package overlay

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay/protocol"
)

var errStreamClosed = errors.New("overlay: stream closed")

const (
	retransmitInterval            = 50 * time.Millisecond
	defaultReorderCapacity uint32 = 256 << 10
)

type pendingEntry struct {
	seq      uint64
	length   uint32
	frame    *protocol.Frame
	lastSend time.Time
}

// Stream represents a bidirectional logical stream multiplexed across the
// session subflows.
type Stream struct {
	session *Session
	id      uint32

	sendWindow *Window
	reorder    *ReorderBuffer

	sendSeq uint64
	recvSeq atomic.Uint64

	pending   []*pendingEntry
	pendingMu sync.Mutex

	inbound chan ReorderedChunk
	done    chan struct{}

	accepted chan struct{}

	errMu sync.RWMutex
	err   error

	target net.Conn // server side target connection
	local  net.Conn // client side local connection

	once      sync.Once
	closeOnce sync.Once

	sendClosed atomic.Bool
	localDone  atomic.Bool
	remoteDone atomic.Bool

	throughput *metrics.SlidingCounter
	inflight   atomic.Uint32
}

func newStream(sess *Session, id uint32) *Stream {
	cap := sess.cfg.InitialWindow
	if cap == 0 {
		cap = defaultReorderCapacity
	}
	if cap > ^uint32(0)/2 {
		cap = ^uint32(0)
	} else {
		cap *= 2
	}
	return &Stream{
		session:    sess,
		id:         id,
		sendWindow: NewWindow(sess.cfg.InitialWindow),
		reorder:    NewReorderBuffer(cap),
		inbound:    make(chan ReorderedChunk, 32),
		done:       make(chan struct{}),
		accepted:   make(chan struct{}),
		throughput: metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
	}
}

func (s *Stream) waitForAccept() error {
	select {
	case <-s.accepted:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("overlay: stream open timed out")
	}
}

func (s *Stream) accept() {
	s.once.Do(func() {
		close(s.accepted)
	})
}

func (s *Stream) fail(err error) {
	if err == nil {
		err = errStreamClosed
	}
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
	s.closeOnce.Do(func() {
		close(s.done)
		s.sendWindow.Close()
	})
	if s.local != nil {
		_ = s.local.Close()
	}
	if s.target != nil {
		_ = s.target.Close()
	}
}

func (s *Stream) close(err error) {
	s.fail(err)
}

func (s *Stream) Err() error {
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.err
}

func (s *Stream) queueFrame(payload []byte, end bool) (*pendingEntry, error) {
	length := uint32(len(payload))
	if end {
		if s.sendClosed.Swap(true) {
			return nil, errStreamClosed
		}
	} else if s.sendClosed.Load() {
		return nil, errStreamClosed
	}
	if !s.sendWindow.Acquire(length) {
		return nil, errStreamClosed
	}
	seq := s.sendSeq
	s.sendSeq += uint64(length)
	frame := &protocol.Frame{
		Type:     protocol.FrameData,
		StreamID: s.id,
		Seq:      seq,
		Payload:  payload,
	}
	if end {
		frame.Flags |= protocol.FlagEndOfStream
	}
	entry := &pendingEntry{seq: seq, length: length, frame: frame}
	s.pendingMu.Lock()
	s.pending = append(s.pending, entry)
	s.pendingMu.Unlock()
	if length > 0 {
		s.throughput.Add(int64(length))
		s.inflight.Add(length)
	}
	return entry, nil
}

func (s *Stream) onSendFailure(entry *pendingEntry) {
	s.pendingMu.Lock()
	for i, pending := range s.pending {
		if pending == entry {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	s.pendingMu.Unlock()
	if entry.length > 0 {
		s.sendWindow.Release(entry.length)
		s.inflight.Add(^uint32(entry.length - 1))
	}
}

func (s *Stream) onAck(ack *protocol.AckPayload) {
	if ack == nil {
		return
	}
	holes := computeAckHoles(ack)
	now := time.Now()
	var resend []*pendingEntry
	s.pendingMu.Lock()
	filtered := s.pending[:0]
	for _, entry := range s.pending {
		if ackCoversEntry(ack, entry) {
			continue
		}
		if shouldRetransmitEntry(entry, holes, now) {
			resend = append(resend, entry)
		}
		filtered = append(filtered, entry)
	}
	s.pending = filtered
	s.pendingMu.Unlock()
	if ack.Credit > 0 {
		s.sendWindow.Release(ack.Credit)
		s.inflight.Add(^uint32(ack.Credit - 1))
	}
	for _, entry := range resend {
		s.session.retransmit(s, entry, now)
	}
}

func (s *Stream) onData(frame *protocol.Frame) {
	end := frame.Flags&protocol.FlagEndOfStream != 0
	for {
		chunks, err := s.reorder.Push(frame.Seq, frame.Payload, end)
		if err != nil {
			if errors.Is(err, ErrReorderOverflow) {
				s.handleReorderOverflow()
				time.Sleep(5 * time.Millisecond)
				continue
			}
			s.fail(err)
			return
		}
		if len(chunks) == 0 {
			if err := s.sendAck(0); err != nil {
				s.fail(err)
			}
			return
		}
		for _, chunk := range chunks {
			select {
			case s.inbound <- chunk:
			case <-s.done:
				return
			}
		}
		return
	}
}

// BindClient attaches a local connection (SOCKS client side) to the overlay
// stream and begins bidirectional forwarding.
func (s *Stream) BindClient(conn net.Conn) {
	s.local = conn
	go s.pumpLocalToOverlay()
	go s.pumpOverlayToLocal()
}

// BindServer attaches the remote target connection and begins forwarding data
// between target and overlay stream.
func (s *Stream) BindServer(conn net.Conn) {
	s.target = conn
	go s.pumpOverlayToTarget()
	go s.pumpTargetToOverlay()
}

func (s *Stream) pumpLocalToOverlay() {
	buf := make([]byte, s.session.cfg.FrameSize)
	for {
		n, err := s.local.Read(buf)
		end := errors.Is(err, io.EOF)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if sendErr := s.session.sendData(s, chunk, end); sendErr != nil {
				if errors.Is(sendErr, errStreamClosed) {
					s.localDone.Store(true)
					s.maybeFinalize()
					return
				}
				s.fail(sendErr)
				return
			}
		} else if end {
			if sendErr := s.session.sendData(s, nil, true); sendErr != nil && !errors.Is(sendErr, errStreamClosed) {
				s.fail(sendErr)
				return
			}
		}
		if err != nil {
			if !end {
				s.fail(err)
				return
			}
			s.localDone.Store(true)
			s.maybeFinalize()
			return
		}
	}
}

func (s *Stream) pumpOverlayToLocal() {
	for {
		select {
		case chunk := <-s.inbound:
			if len(chunk.Data) > 0 {
				if _, err := s.local.Write(chunk.Data); err != nil {
					s.fail(err)
					return
				}
				s.recvSeq.Add(uint64(len(chunk.Data)))
				if err := s.sendAck(uint32(len(chunk.Data))); err != nil {
					s.fail(err)
					return
				}
			} else if err := s.sendAck(0); err != nil {
				s.fail(err)
				return
			}
			if chunk.End {
				s.remoteDone.Store(true)
				s.halfCloseWrite(s.local)
				s.maybeFinalize()
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *Stream) pumpOverlayToTarget() {
	for {
		select {
		case chunk := <-s.inbound:
			if len(chunk.Data) > 0 {
				if _, err := s.target.Write(chunk.Data); err != nil {
					s.fail(err)
					return
				}
				s.recvSeq.Add(uint64(len(chunk.Data)))
				if err := s.sendAck(uint32(len(chunk.Data))); err != nil {
					s.fail(err)
					return
				}
			} else if err := s.sendAck(0); err != nil {
				s.fail(err)
				return
			}
			if chunk.End {
				s.remoteDone.Store(true)
				s.halfCloseWrite(s.target)
				s.maybeFinalize()
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *Stream) pumpTargetToOverlay() {
	buf := make([]byte, s.session.cfg.FrameSize)
	for {
		n, err := s.target.Read(buf)
		end := errors.Is(err, io.EOF)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if sendErr := s.session.sendData(s, chunk, end); sendErr != nil {
				if errors.Is(sendErr, errStreamClosed) {
					s.localDone.Store(true)
					s.maybeFinalize()
					return
				}
				s.fail(sendErr)
				return
			}
		} else if end {
			if sendErr := s.session.sendData(s, nil, true); sendErr != nil && !errors.Is(sendErr, errStreamClosed) {
				s.fail(sendErr)
				return
			}
		}
		if err != nil {
			if !end {
				s.fail(err)
				return
			}
			s.localDone.Store(true)
			s.maybeFinalize()
			return
		}
	}
}

func (s *Stream) sendAck(credit uint32) error {
	ranges := s.reorder.SACKRanges()
	if credit == 0 && len(ranges) == 0 && !s.reorder.HasTerminal() {
		return nil
	}
	ack := &protocol.AckPayload{
		AckSeq: s.recvSeq.Load(),
		Credit: credit,
		Ranges: ranges,
	}
	return s.session.sendAck(s.id, ack)
}

func (s *Stream) handleReorderOverflow() {
	if err := s.sendAck(0); err != nil {
		s.fail(err)
	}
}

func (s *Stream) halfCloseWrite(conn net.Conn) {
	if conn == nil {
		return
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

func (s *Stream) maybeFinalize() {
	if s.localDone.Load() && s.remoteDone.Load() {
		s.close(io.EOF)
	}
}

func (s *Stream) openServerSide(target string) error {
	conn, err := net.Dial("tcp", target)
	if err != nil {
		return err
	}
	s.BindServer(conn)
	return nil
}

// Done returns a channel that is closed once the stream completes or fails.
func (s *Stream) Done() <-chan struct{} {
	return s.done
}

func computeAckHoles(ack *protocol.AckPayload) []protocol.SACKRange {
	if ack == nil || len(ack.Ranges) == 0 {
		return nil
	}
	cur := ack.AckSeq
	holes := make([]protocol.SACKRange, 0, len(ack.Ranges))
	for _, r := range ack.Ranges {
		if r.Start > cur {
			holes = append(holes, protocol.SACKRange{Start: cur, End: r.Start})
		}
		if r.End > cur {
			cur = r.End
		}
	}
	return holes
}

func ackCoversEntry(ack *protocol.AckPayload, entry *pendingEntry) bool {
	end := entry.seq + uint64(entry.length)
	if ack.AckSeq >= end {
		return true
	}
	for _, r := range ack.Ranges {
		if entry.seq >= r.Start && end <= r.End {
			return true
		}
	}
	return false
}

func shouldRetransmitEntry(entry *pendingEntry, holes []protocol.SACKRange, now time.Time) bool {
	if entry.length == 0 || len(holes) == 0 {
		return false
	}
	if !entry.lastSend.IsZero() && now.Sub(entry.lastSend) < retransmitInterval {
		return false
	}
	end := entry.seq + uint64(entry.length)
	for _, hole := range holes {
		if entry.seq < hole.End && end > hole.Start {
			return true
		}
	}
	return false
}
