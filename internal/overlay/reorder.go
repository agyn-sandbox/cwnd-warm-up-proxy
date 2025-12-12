package overlay

// ReorderBuffer stores out-of-order data frames until they can be delivered in
// sequence.
type ReorderBuffer struct {
	next   uint64
	buffer map[uint64][]byte
}

func NewReorderBuffer() *ReorderBuffer {
	return &ReorderBuffer{buffer: make(map[uint64][]byte)}
}

// Push inserts a frame for the provided sequence number. It returns any
// contiguous payloads that can be delivered starting at the current read
// position.
func (r *ReorderBuffer) Push(seq uint64, payload []byte) [][]byte {
	if seq < r.next {
		// Duplicate; ignore by not re-delivering.
		return nil
	}
	if _, exists := r.buffer[seq]; exists {
		return nil
	}
	r.buffer[seq] = append([]byte(nil), payload...)
	var ready [][]byte
	for {
		data, ok := r.buffer[r.next]
		if !ok {
			break
		}
		ready = append(ready, data)
		r.next += uint64(len(data))
		delete(r.buffer, r.next-uint64(len(data)))
	}
	return ready
}

func (r *ReorderBuffer) Advance(to uint64) {
	if to <= r.next {
		return
	}
	r.next = to
	for seq := range r.buffer {
		if seq < r.next {
			delete(r.buffer, seq)
		}
	}
}
