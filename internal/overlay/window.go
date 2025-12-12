package overlay

import "sync"

// Window implements a simple byte-credit window that bounds the number of
// in-flight bytes for a stream. The window starts with an initial credit and is
// replenished as ACKs arrive from the peer.
type Window struct {
	mu        sync.Mutex
	cond      *sync.Cond
	available uint32
	closed    bool
}

// NewWindow creates a window with the provided initial credit.
func NewWindow(initial uint32) *Window {
	w := &Window{available: initial}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// Acquire blocks until at least n bytes of credit are available. If the window
// is closed, Acquire returns immediately with false.
func (w *Window) Acquire(n uint32) bool {
	if n == 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if w.closed {
			return false
		}
		if w.available >= n {
			w.available -= n
			return true
		}
		w.cond.Wait()
	}
}

// Release adds the specified number of bytes back to the available window and
// wakes any waiting goroutines.
func (w *Window) Release(n uint32) {
	if n == 0 {
		return
	}
	w.mu.Lock()
	w.available += n
	w.mu.Unlock()
	w.cond.Broadcast()
}

// Close wakes any waiters and prevents further acquisitions.
func (w *Window) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cond.Broadcast()
}
