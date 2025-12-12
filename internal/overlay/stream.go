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

type pendingEntry struct {
	seq    uint64
	length uint32
}

// Stream represents a bidirectional logical stream multiplexed across the
// session subflows.
type Stream struct {
	session *Session
	id      uint32

	sendWindow *Window
	reorder    *ReorderBuffer

	sendSeq uint64
	recvSeq uint64

	pending   []pendingEntry
	pendingMu sync.Mutex

	inbound chan []byte
	done    chan struct{}

	accepted chan struct{}

	errMu sync.RWMutex
	err   error

	target net.Conn // server side target connection
	local  net.Conn // client side local connection

	once      sync.Once
	closeOnce sync.Once

	throughput *metrics.SlidingCounter
	inflight   atomic.Uint32
}

func newStream(sess *Session, id uint32) *Stream {
	return &Stream{
		session:    sess,
		id:         id,
		sendWindow: NewWindow(sess.cfg.InitialWindow),
		reorder:    NewReorderBuffer(),
		inbound:    make(chan []byte, 32),
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

func (s *Stream) reserveSequence(length uint32) (uint64, error) {
	if !s.sendWindow.Acquire(length) {
		return 0, errStreamClosed
	}
	s.pendingMu.Lock()
	seq := s.sendSeq
	s.sendSeq += uint64(length)
	s.pending = append(s.pending, pendingEntry{seq: seq, length: length})
	s.pendingMu.Unlock()
	s.throughput.Add(int64(length))
	s.inflight.Add(length)
	return seq, nil
}

func (s *Stream) onAck(ack *protocol.AckPayload) {
	s.pendingMu.Lock()
	trimIdx := 0
	for _, entry := range s.pending {
		end := entry.seq + uint64(entry.length)
		if ack.AckSeq >= end {
			trimIdx++
		} else {
			break
		}
	}
	if trimIdx > 0 {
		s.pending = s.pending[trimIdx:]
	}
	s.pendingMu.Unlock()
	if ack.Credit > 0 {
		s.sendWindow.Release(ack.Credit)
		s.inflight.Add(^uint32(ack.Credit - 1))
	}
}

func (s *Stream) onData(frame *protocol.Frame) {
	ready := s.reorder.Push(frame.Seq, frame.Payload)
	for _, payload := range ready {
		select {
		case s.inbound <- payload:
		case <-s.done:
		}
		if s.session.cfg.Role == RoleServer {
			// Server acks after data is forwarded to target in pump loop.
		} else {
			s.recvSeq += uint64(len(payload))
			_ = s.session.sendAck(s.id, s.recvSeq, uint32(len(payload)))
		}
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
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if err := s.session.sendData(s, chunk); err != nil {
				s.fail(err)
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.fail(err)
			}
			_ = s.session.sendFrame(&protocol.Frame{
				Type:      protocol.FrameControl,
				SessionID: s.session.id,
				StreamID:  s.id,
				Control:   &protocol.ControlPayload{Type: protocol.ControlStreamClose},
			})
			return
		}
	}
}

func (s *Stream) pumpOverlayToLocal() {
	for {
		select {
		case payload := <-s.inbound:
			if _, err := s.local.Write(payload); err != nil {
				s.fail(err)
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
		case payload := <-s.inbound:
			if _, err := s.target.Write(payload); err != nil {
				s.fail(err)
				return
			}
			s.recvSeq += uint64(len(payload))
			_ = s.session.sendAck(s.id, s.recvSeq, uint32(len(payload)))
		case <-s.done:
			return
		}
	}
}

func (s *Stream) pumpTargetToOverlay() {
	buf := make([]byte, s.session.cfg.FrameSize)
	for {
		n, err := s.target.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if err := s.session.sendData(s, chunk); err != nil {
				s.fail(err)
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.fail(err)
			}
			_ = s.session.sendFrame(&protocol.Frame{
				Type:      protocol.FrameControl,
				SessionID: s.session.id,
				StreamID:  s.id,
				Control:   &protocol.ControlPayload{Type: protocol.ControlStreamClose},
			})
			return
		}
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
