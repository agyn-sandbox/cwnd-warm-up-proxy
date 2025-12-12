package overlay

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay/protocol"
)

// Subflow encapsulates a single TCP connection that carries frames for a
// session. Subflows are used bidirectionally (client↔server).
type Subflow struct {
	ID      int
	Conn    net.Conn
	session *Session
	wMu     sync.Mutex
	closed  chan struct{}

	txReal  *metrics.SlidingCounter
	txDummy *metrics.SlidingCounter
	rxReal  *metrics.SlidingCounter
	rxDummy *metrics.SlidingCounter
}

func newClientSubflow(id int, sess *Session, dialer func() (net.Conn, error)) (*Subflow, error) {
	conn, err := dialer()
	if err != nil {
		return nil, err
	}
	return newSubflow(id, sess, conn), nil
}

func newServerSubflow(id int, sess *Session, conn net.Conn) *Subflow {
	return newSubflow(id, sess, conn)
}

func newSubflow(id int, sess *Session, conn net.Conn) *Subflow {
	window := sess.metricsWindow
	step := sess.metricsStep
	return &Subflow{
		ID:      id,
		Conn:    conn,
		session: sess,
		closed:  make(chan struct{}),
		txReal:  metrics.NewSlidingCounter(window, step),
		txDummy: metrics.NewSlidingCounter(window, step),
		rxReal:  metrics.NewSlidingCounter(window, step),
		rxDummy: metrics.NewSlidingCounter(window, step),
	}
}

func (s *Subflow) run() {
	defer close(s.closed)
	for {
		frame, err := protocol.ReadFrame(s.Conn)
		if err != nil {
			s.session.onSubflowError(s.ID, err)
			return
		}
		if s.session.cfg.EnableChecksums && frame.Flags&protocol.FlagChecksumPresent == 0 {
			s.session.onSubflowError(s.ID, fmt.Errorf("overlay: checksum required"))
			return
		}
		s.recordReceive(frame)
		s.session.handleFrame(s, frame)
	}
}

func (s *Subflow) send(frame *protocol.Frame) error {
	s.wMu.Lock()
	defer s.wMu.Unlock()
	if deadline := s.session.cfg.WriteTimeout; deadline > 0 {
		_ = s.Conn.SetWriteDeadline(time.Now().Add(deadline))
	}
	if s.session.cfg.EnableChecksums {
		frame.Flags |= protocol.FlagChecksumPresent
	}
	cw := &protocolCountingWriter{Conn: s.Conn}
	if err := frame.Encode(cw); err != nil {
		return fmt.Errorf("subflow %d write: %w", s.ID, err)
	}
	s.recordSend(frame, cw.BytesWritten())
	return nil
}

func (s *Subflow) close() error {
	return s.Conn.Close()
}

func (s *Subflow) recordSend(frame *protocol.Frame, bytes int) {
	frame.WireLength = bytes
	head := headerBytes(frame, bytes)
	if head > 0 {
		s.txDummy.Add(head)
	}
	if frame.Type == protocol.FrameData {
		payload := len(frame.Payload)
		if payload > 0 {
			if frame.IsDuplicate {
				s.txDummy.Add(int64(payload))
			} else {
				s.txReal.Add(int64(payload))
			}
		}
	}
}

func (s *Subflow) recordReceive(frame *protocol.Frame) {
	wire := frame.WireLength
	head := headerBytes(frame, wire)
	if head > 0 {
		s.rxDummy.Add(head)
	}
	if frame.Type == protocol.FrameData {
		if payload := len(frame.Payload); payload > 0 {
			s.rxReal.Add(int64(payload))
		}
	}
}

func headerBytes(frame *protocol.Frame, total int) int64 {
	if total <= 0 {
		return 0
	}
	if frame.Type == protocol.FrameData {
		payload := len(frame.Payload)
		if payload >= total {
			return 0
		}
		return int64(total - payload)
	}
	return int64(total)
}

type protocolCountingWriter struct {
	Conn net.Conn
	cnt  int
}

func (w *protocolCountingWriter) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	w.cnt += n
	return n, err
}

func (w *protocolCountingWriter) BytesWritten() int {
	return w.cnt
}
