package webrtc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// minimalRTP builds a valid, unmarshable RTP packet with a distinct sequence
// number so tests can identify which packets were delivered vs dropped.
func minimalRTP(seq uint16) []byte {
	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, PayloadType: 96},
		Payload: []byte{0x01, 0x02},
	}
	b, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

// TestAsyncForwarder_SlowConsumerDoesNotBlockProducer is the core regression for
// the WHIP C4 head-of-line blocking fix. With a synchronous WriteRTP fan-out, a
// congested subscriber blocks the ingest Read goroutine (pion's WriteRTP does a
// synchronous per-binding encrypt + UDP write). The forwarder must ensure the
// producer's write() returns promptly even while the consumer is stalled.
func TestAsyncForwarder_SlowConsumerDoesNotBlockProducer(t *testing.T) {
	// Consumer gate: hold every WriteRTP until released.
	block := make(chan struct{}, 16)
	release := make(chan struct{})
	var mu sync.Mutex
	var written []uint16

	f := newAsyncForwarder(8, func(p *rtp.Packet) error {
		// Block this drain call until the test releases, then signal it was
		// served so the test can count deliveries deterministically.
		<-release
		mu.Lock()
		written = append(written, p.Header.SequenceNumber)
		mu.Unlock()
		block <- struct{}{}
		return nil
	})
	defer f.Close()

	// Synchronously produce many more packets than the buffer holds while the
	// consumer is stalled. write() is non-blocking, so calling it directly here
	// (no goroutine) both keeps the produce order deterministic AND proves the
	// producer never blocks — a blocking write() would hang the test.
	const n = 60
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			f.write(minimalRTP(uint16(i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("producer writes blocked on a stalled consumer (head-of-line regression)")
	}

	// Drop-oldest must have discarded stale packets. The exact count depends on
	// the race between drain-dequeue and producer-fill (the drain dequeues some
	// packets before the buffer fills), so assert the invariant instead: the
	// newest packet (n-1) is NEVER dropped (drop-oldest preserves the tail), and
	// total produced = delivered + dropped.
	released := false
	close(release) // unblock the drain goroutines; released may already be closed
	_ = released

	// Collect deliveries until the drain goes idle (no packets for a grace
	// period) or we hit n.
	idle := time.NewTimer(120 * time.Millisecond)
	defer idle.Stop()
	for {
		mu.Lock()
		got := len(written)
		mu.Unlock()
		if got == n {
			break
		}
		select {
		case <-block:
			// a delivery happened; reset the idle timer
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(120 * time.Millisecond)
		case <-idle.C:
			// drain has gone idle
			goto done
		}
	}
done:

	mu.Lock()
	defer mu.Unlock()
	if len(written) == 0 {
		t.Fatal("no packets were delivered")
	}
	// The newest packet must always be among the deliveries — drop-oldest never
	// drops the tail.
	last := written[len(written)-1]
	if last != n-1 {
		t.Errorf("newest packet (seq %d) was dropped; drop-oldest must preserve the tail (last delivered=%d)", n-1, last)
	}
	// Conservation: produced = delivered + dropped.
	if got := f.Dropped(); int64(len(written))+got != int64(n) {
		t.Errorf("conservation broken: delivered(%d) + dropped(%d) != produced(%d)", len(written), got, n)
	}
	// Delivered packets are strictly increasing (drain processes in dequeue
	// order; drop-oldest keeps a contiguous tail so deliveries are monotonic).
	for i := 1; i < len(written); i++ {
		if written[i] <= written[i-1] {
			t.Errorf("deliveries not monotonic at %d: %d after %d", i, written[i], written[i-1])
		}
	}
}

// TestAsyncForwarder_RecyclesBuffersUnderVaryingSizes exercises the buffer
// free-list across packets larger than a previously-recycled buffer, ensuring
// getBufLocked reallocates when capacity is insufficient and never returns a
// short slice (which would corrupt the copied payload).
func TestAsyncForwarder_RecyclesBuffersUnderVaryingSizes(t *testing.T) {
	var mu sync.Mutex
	got := map[uint16]int{} // seq -> payload length observed

	f := newAsyncForwarder(4, func(p *rtp.Packet) error {
		mu.Lock()
		got[p.Header.SequenceNumber] = len(p.Payload)
		mu.Unlock()
		return nil
	})
	defer f.Close()

	// Build packets with growing payloads so recycled small buffers must be
	// grown, then shrinking so large buffers are reused for small packets.
	sizes := []int{2, 64, 8, 512, 16, 512, 4}
	build := func(seq uint16, payloadLen int) []byte {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq, PayloadType: 96},
			Payload: make([]byte, payloadLen),
		}
		b, err := pkt.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for i, n := range sizes {
		// Serialize produce+drain by waiting for delivery, so the buffer is
		// recycled before the next write and the reuse path is exercised.
		f.write(build(uint16(i), n))
		deadline := time.After(2 * time.Second)
		for {
			mu.Lock()
			_, ok := got[uint16(i)]
			mu.Unlock()
			if ok {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("packet %d never delivered", i)
			case <-time.After(time.Millisecond):
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for i, n := range sizes {
		if got[uint16(i)] != n {
			t.Errorf("packet %d: payload len = %d, want %d (buffer reuse corrupted the copy)", i, got[uint16(i)], n)
		}
	}
}

// BenchmarkAsyncForwarder_Throughput drives the produce/drain path at a video-
// like packet rate. With buffer recycling the steady state should allocate far
// less than one ~1.4KB slice per packet; run with -benchmem to observe B/op.
func BenchmarkAsyncForwarder_Throughput(b *testing.B) {
	var delivered int64
	f := newAsyncForwarder(256, func(p *rtp.Packet) error {
		atomic.AddInt64(&delivered, 1)
		return nil
	})
	defer f.Close()

	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 1, PayloadType: 96},
		Payload: make([]byte, 1200),
	}
	raw, err := pkt.Marshal()
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.write(raw)
	}
	// Under drop-oldest not every packet is delivered; wait until the drain has
	// caught up so delivered + dropped == produced before stopping the timer.
	deadline := time.After(10 * time.Second)
	for atomic.LoadInt64(&delivered)+f.Dropped() < int64(b.N) {
		select {
		case <-deadline:
			b.Fatalf("drain did not converge: delivered=%d dropped=%d want=%d",
				atomic.LoadInt64(&delivered), f.Dropped(), b.N)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestAsyncForwarder_FlushesAllWhenNotFull verifies that under normal (non-full)
// operation every packet is delivered in order with no drops.
func TestAsyncForwarder_FlushesAllWhenNotFull(t *testing.T) {
	var mu sync.Mutex
	var written []uint16

	f := newAsyncForwarder(64, func(p *rtp.Packet) error {
		mu.Lock()
		written = append(written, p.Header.SequenceNumber)
		mu.Unlock()
		return nil
	})
	defer f.Close()

	for i := 0; i < 20; i++ {
		f.write(minimalRTP(uint16(i)))
	}

	// Give the drain goroutine time to flush.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := len(written)
		mu.Unlock()
		if got == 20 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("drained %d/20 packets", got)
		case <-time.After(2 * time.Millisecond):
		}
	}

	if got := f.Dropped(); got != 0 {
		t.Errorf("expected 0 drops under capacity, got %d", got)
	}
}

// TestAsyncForwarder_CloseStopsDrain verifies Close is idempotent and lets the
// drain goroutine exit after flushing queued work.
func TestAsyncForwarder_CloseStopsDrain(t *testing.T) {
	var written int
	var mu sync.Mutex
	f := newAsyncForwarder(16, func(p *rtp.Packet) error {
		mu.Lock()
		written++
		mu.Unlock()
		return nil
	})

	f.write(minimalRTP(1))
	f.Close()
	f.Close() // idempotent

	// Allow the drain to flush the one queued packet and exit.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := written
		mu.Unlock()
		if got == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("queued packet not flushed on close: %d/1", got)
		case <-time.After(2 * time.Millisecond):
		}
	}

	// Writes after close are dropped (consumer is gone).
	f.write(minimalRTP(2))
	mu.Lock()
	final := written
	mu.Unlock()
	if final != 1 {
		t.Errorf("writes after close should be dropped; got %d writes, want 1", final)
	}
}
