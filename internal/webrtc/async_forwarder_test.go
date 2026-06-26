package webrtc

import (
	"sync"
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
				select { case <-idle.C: default: }
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
