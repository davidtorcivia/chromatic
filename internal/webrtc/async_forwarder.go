package webrtc

import (
	"io"
	"log"
	"sync"

	"github.com/pion/rtp"
)

// asyncForwarder decouples an RTP producer (the WHIP ingest Read loop) from a
// potentially-blocking consumer (TrackLocalStaticRTP.WriteRTP, which pion
// implements as a synchronous per-subscriber encrypt + UDP socket write). Without
// this decoupling, one congested subscriber's full OS send buffer blocks the
// single ingest Read goroutine, which backs up OBS's own read buffer and stalls
// media — and keyframes — for EVERY viewer.
//
// Design:
//   - The producer calls write() and never blocks (beyond a short mutex). When
//     the bounded buffer is full, the OLDEST packet is dropped (correct for live
//     video — a stale frame is worthless; never block the live ingest).
//   - A single drain goroutine calls the consumer (WriteRTP) sequentially. We
//     don't add per-subscriber queues because pion's TrackLocalStaticRTP already
//     fans one WriteRTP out to all bound subscribers synchronously; the win here
//     is isolating the *ingest read* from a transient write stall and bounding
//     the resulting latency to the buffer depth instead of "unbounded / until
//     OBS drops the keyframe".
//   - Drop-oldest keeps latency bounded: under sustained congestion the buffer
//     can't grow without limit, and the dropped frames are exactly the stalest.
//
// bufSize is chosen per-track kind: video tolerates drops and has large frames,
// audio is small so a deeper buffer avoids dropping talk spurts. Callers pass the
// capacity; see whip.go for the chosen values.
type asyncForwarder struct {
	buf   [][]byte
	head  int // index of the oldest packet
	count int
	// free is a bounded LIFO of recycled packet buffers. At video bitrates the
	// ingest produces hundreds of packets/sec; without reuse each write() would
	// heap-allocate a ~1.4KB slice, churning the GC on the hottest media path.
	// Buffers are recycled by the drain goroutine after WriteRTP returns (it
	// copies the payload out synchronously, so the buffer is free to reuse) and
	// by write() when drop-oldest discards a stale packet. Capped at len(buf)
	// so the free list can't grow unbounded.
	free      [][]byte
	mu        sync.Mutex
	cond      *sync.Cond
	closeOnce sync.Once
	writeFn   func(*rtp.Packet) error
	dropped   int64
	closed    bool
	// doneCh is closed by Close so observers (e.g. the WHIP drop-diagnostic
	// goroutine) can end with the forwarder instead of outliving it.
	doneCh chan struct{}
}

// newAsyncForwarder creates a forwarder with the given buffer capacity (in
// packets). write is the consumer invoked by the drain goroutine for each
// queued packet (typically localTrack.WriteRTP).
func newAsyncForwarder(bufSize int, write func(*rtp.Packet) error) *asyncForwarder {
	if bufSize < 1 {
		bufSize = 1
	}
	f := &asyncForwarder{
		buf:     make([][]byte, bufSize),
		writeFn: write,
		doneCh:  make(chan struct{}),
	}
	f.cond = sync.NewCond(&f.mu)
	go f.drain()
	return f
}

// write queues a packet for asynchronous fan-out. It never blocks on the
// consumer: if the buffer is full the oldest packet is dropped (drop-oldest),
// keeping the live ingest read loop moving. The packet bytes are copied since
// the caller reuses its read buffer; the copy reuses a recycled buffer when one
// is available (see free) to avoid a heap allocation per packet.
func (f *asyncForwarder) write(pkt []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	if f.count == len(f.buf) {
		// Buffer full: drop the oldest packet to keep latency bounded and the
		// producer unblocked. For live video a stale frame is worthless; for
		// audio this is the lesser evil vs. blocking OBS. Recycle its buffer.
		old := f.buf[f.head]
		f.buf[f.head] = nil
		f.head = (f.head + 1) % len(f.buf)
		f.count--
		f.dropped++
		f.recycleLocked(old)
	}
	cp := f.getBufLocked(len(pkt))
	copy(cp, pkt)
	tail := (f.head + f.count) % len(f.buf)
	f.buf[tail] = cp
	f.count++
	f.cond.Signal()
}

// getBufLocked returns a byte slice of length n, reusing a recycled buffer when
// one with sufficient capacity is available. Caller must hold f.mu.
func (f *asyncForwarder) getBufLocked(n int) []byte {
	if k := len(f.free); k > 0 {
		b := f.free[k-1]
		f.free[k-1] = nil
		f.free = f.free[:k-1]
		if cap(b) >= n {
			return b[:n]
		}
		// Too small (a larger packet than this buffer held): let it go and
		// allocate a right-sized one below.
	}
	return make([]byte, n)
}

// recycleLocked returns a buffer to the free list for reuse, bounding the list
// to the ring capacity so it can't grow without limit. Caller must hold f.mu.
func (f *asyncForwarder) recycleLocked(b []byte) {
	if b == nil || len(f.free) >= len(f.buf) {
		return
	}
	f.free = append(f.free, b[:0])
}

// drain is the consumer loop. It calls write() for each queued packet, exiting
// when Close is called and the buffer is drained.
func (f *asyncForwarder) drain() {
	// p is reused across iterations. drain is single-goroutine and writeFn
	// (WriteRTP) does not retain the packet past the call, so one packet struct
	// serves every iteration instead of allocating one per packet. It is NOT
	// zeroed between iterations: Unmarshal overwrites every field and reuses
	// the CSRC/Extensions backing arrays, so zeroing would just discard that
	// capacity and force a fresh allocation per packet.
	var p rtp.Packet
	// used holds the previous iteration's buffer so it can be recycled once we
	// re-acquire the lock — writeFn has returned by then, so the bytes are free.
	var used []byte
	for {
		f.mu.Lock()
		if used != nil {
			f.recycleLocked(used)
		}
		for f.count == 0 && !f.closed {
			f.cond.Wait()
		}
		if f.count == 0 && f.closed {
			f.mu.Unlock()
			return
		}
		pkt := f.buf[f.head]
		f.buf[f.head] = nil
		f.head = (f.head + 1) % len(f.buf)
		f.count--
		f.mu.Unlock()

		if err := p.Unmarshal(pkt); err != nil {
			used = pkt
			continue
		}
		stripHeaderExtensions(&p.Header)
		if err := f.writeFn(&p); err != nil {
			if err != io.ErrClosedPipe && err != io.EOF {
				log.Printf("async forwarder write error: %v", err)
			}
		}
		used = pkt
	}
}

// Dropped returns the count of packets dropped due to a full buffer.
func (f *asyncForwarder) Dropped() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dropped
}

// Done returns a channel closed when the forwarder is closed.
func (f *asyncForwarder) Done() <-chan struct{} {
	return f.doneCh
}

// Close stops accepting packets and exits the drain goroutine once queued work
// is flushed. Safe to call multiple times.
func (f *asyncForwarder) Close() {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.closed = true
		f.cond.Broadcast()
		f.mu.Unlock()
		close(f.doneCh)
	})
}
