package overlay

import (
	"errors"
	"sort"
	"sync"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay/protocol"
)

// ReorderedChunk represents data that has become contiguous and ready for
// delivery.
type ReorderedChunk struct {
	Data []byte
	End  bool
}

type reorderEntry struct {
	payload []byte
	end     bool
}

// ReorderBuffer stores out-of-order data frames until they can be delivered in
// sequence while enforcing a bounded capacity for backpressure.
type ReorderBuffer struct {
	mu       sync.Mutex
	next     uint64
	buffer   map[uint64]reorderEntry
	buffered uint32
	capacity uint32
	terminal bool
	endSeq   uint64
}

// ErrReorderOverflow indicates the buffer capacity would be exceeded.
var ErrReorderOverflow = errors.New("overlay: reorder buffer overflow")

func NewReorderBuffer(capacity uint32) *ReorderBuffer {
	return &ReorderBuffer{
		buffer:   make(map[uint64]reorderEntry),
		capacity: capacity,
	}
}

// Push inserts a frame for the provided sequence number, returning any data
// that has become contiguous as a result. Payloads are copied to isolate from
// caller mutation.
func (r *ReorderBuffer) Push(seq uint64, payload []byte, end bool) ([]ReorderedChunk, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq < r.next {
		if end && !r.terminal {
			r.terminal = true
			r.endSeq = seq + uint64(len(payload))
		}
		return nil, nil
	}
	if existing, ok := r.buffer[seq]; ok {
		if end && !existing.end {
			r.buffer[seq] = reorderEntry{payload: existing.payload, end: true}
		}
		return nil, nil
	}
	length := uint32(len(payload))
	if length > 0 {
		if r.buffered+length > r.capacity {
			return nil, ErrReorderOverflow
		}
		r.buffered += length
	}
	copyPayload := append([]byte(nil), payload...)
	r.buffer[seq] = reorderEntry{payload: copyPayload, end: end}
	if end {
		r.terminal = true
		r.endSeq = seq + uint64(len(payload))
	}
	return r.popReadyLocked(), nil
}

// popReadyLocked drains any newly contiguous chunks from the buffer. Caller
// must hold r.mu.
func (r *ReorderBuffer) popReadyLocked() []ReorderedChunk {
	if len(r.buffer) == 0 {
		return nil
	}
	var ready []ReorderedChunk
	for {
		entry, ok := r.buffer[r.next]
		if !ok {
			break
		}
		delete(r.buffer, r.next)
		if len(entry.payload) > 0 {
			r.buffered -= uint32(len(entry.payload))
		}
		ready = append(ready, ReorderedChunk{Data: entry.payload, End: entry.end})
		r.next += uint64(len(entry.payload))
		if entry.end {
			r.next = maxUint64(r.next, r.endSeq)
		}
	}
	return ready
}

// BufferedBytes reports the currently buffered bytes.
func (r *ReorderBuffer) BufferedBytes() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buffered
}

// NextSeq exposes the next expected sequence number.
func (r *ReorderBuffer) NextSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

// HasTerminal indicates whether an END_OF_STREAM flag has been observed.
func (r *ReorderBuffer) HasTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminal
}

// SACKRanges returns sorted, coalesced ranges of buffered data beyond the
// cumulative ack point for inclusion in ACK frames.
func (r *ReorderBuffer) SACKRanges() []protocol.SACKRange {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buffer) == 0 {
		return nil
	}
	keys := make([]uint64, 0, len(r.buffer))
	for seq, entry := range r.buffer {
		if len(entry.payload) == 0 {
			continue
		}
		keys = append(keys, seq)
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	ranges := make([]protocol.SACKRange, 0, len(keys))
	for _, seq := range keys {
		entry := r.buffer[seq]
		start := seq
		end := seq + uint64(len(entry.payload))
		if len(ranges) == 0 {
			ranges = append(ranges, protocol.SACKRange{Start: start, End: end})
			continue
		}
		last := &ranges[len(ranges)-1]
		if start <= last.End {
			if end > last.End {
				last.End = end
			}
			continue
		}
		ranges = append(ranges, protocol.SACKRange{Start: start, End: end})
	}
	if len(ranges) > protocol.MaxSACKRanges {
		ranges = ranges[:protocol.MaxSACKRanges]
	}
	return ranges
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
