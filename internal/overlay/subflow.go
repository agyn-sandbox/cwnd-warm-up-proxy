package overlay

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay/protocol"
)

// Subflow encapsulates a single TLS/TCP connection that carries frames for a
// session. Subflows are used bidirectionally (client↔server).
type Subflow struct {
	ID      int
	Conn    net.Conn
	session *Session
	reader  *tls.Conn
	wMu     sync.Mutex
	closed  chan struct{}

	tx *metrics.SlidingCounter
	rx *metrics.SlidingCounter
}

func newClientSubflow(id int, sess *Session, dialer func() (net.Conn, error)) (*Subflow, error) {
	conn, err := dialer()
	if err != nil {
		return nil, err
	}
	sf := &Subflow{
		ID:      id,
		Conn:    conn,
		session: sess,
		closed:  make(chan struct{}),
		tx:      metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
		rx:      metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
	}
	return sf, nil
}

func newServerSubflow(id int, sess *Session, conn net.Conn) *Subflow {
	return &Subflow{
		ID:      id,
		Conn:    conn,
		session: sess,
		closed:  make(chan struct{}),
		tx:      metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
		rx:      metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
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
		if frame.Type == protocol.FrameData {
			s.rx.Add(int64(len(frame.Payload)))
		}
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
	if err := frame.Encode(s.Conn); err != nil {
		return fmt.Errorf("subflow %d write: %w", s.ID, err)
	}
	if frame.Type == protocol.FrameData {
		s.tx.Add(int64(len(frame.Payload)))
	}
	return nil
}

func (s *Subflow) close() error {
	return s.Conn.Close()
}
