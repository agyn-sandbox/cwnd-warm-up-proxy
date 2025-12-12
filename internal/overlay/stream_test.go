package overlay

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay/protocol"
)

func TestStreamOnAckTriggersRetransmitForSACKHole(t *testing.T) {
	sess := newSession(SessionConfig{Role: RoleClient, InitialWindow: 1024, SubflowTarget: 1}, nil)
	defer sess.Close()

	conn := &stubConn{}
	sf := &Subflow{
		ID:      0,
		Conn:    conn,
		session: sess,
		closed:  make(chan struct{}),
		txReal:  metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
		txDummy: metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
		rxReal:  metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
		rxDummy: metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
	}
	sess.scheduler.Attach(sf)

	stream := newStream(sess, 1)
	payload1 := []byte("first-chunk")
	payload2 := []byte("second-chunk")
	if err := sess.sendData(stream, payload1, false); err != nil {
		t.Fatalf("send data 1: %v", err)
	}
	if err := sess.sendData(stream, payload2, false); err != nil {
		t.Fatalf("send data 2: %v", err)
	}

	frames, err := decodeRecordedFrames(conn.buffer.Bytes())
	if err != nil {
		t.Fatalf("decode frames: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(frames))
	}

	stream.pendingMu.Lock()
	for _, entry := range stream.pending {
		entry.lastSend = time.Now().Add(-time.Second)
	}
	stream.pendingMu.Unlock()

	ack := &protocol.AckPayload{
		AckSeq: 0,
		Credit: 0,
		Ranges: []protocol.SACKRange{{
			Start: uint64(len(payload1)),
			End:   uint64(len(payload1) + len(payload2)),
		}},
	}
	stream.onAck(ack)

	stream.pendingMu.Lock()
	if len(stream.pending) != 1 {
		stream.pendingMu.Unlock()
		t.Fatalf("expected 1 pending entry after ack, got %d", len(stream.pending))
	}
	remaining := stream.pending[0]
	stream.pendingMu.Unlock()

	if remaining.seq != 0 {
		t.Fatalf("expected remaining sequence 0, got %d", remaining.seq)
	}
	frames, err = decodeRecordedFrames(conn.buffer.Bytes())
	if err != nil {
		t.Fatalf("decode retransmit frame: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames after retransmit, got %d", len(frames))
	}
	retry := frames[2]
	if retry.Type != protocol.FrameData {
		t.Fatalf("expected data frame, got %v", retry.Type)
	}
	if string(retry.Payload) != string(payload1) {
		t.Fatalf("unexpected retransmit payload: %q", retry.Payload)
	}
}

type stubConn struct {
	buffer bytes.Buffer
}

func (c *stubConn) Read(p []byte) (int, error) { return 0, io.EOF }

func (c *stubConn) Write(p []byte) (int, error) {
	return c.buffer.Write(p)
}

func (c *stubConn) Close() error { return nil }

func (c *stubConn) LocalAddr() net.Addr  { return stubAddr("local") }
func (c *stubConn) RemoteAddr() net.Addr { return stubAddr("remote") }

func (c *stubConn) SetDeadline(time.Time) error      { return nil }
func (c *stubConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stubConn) SetWriteDeadline(time.Time) error { return nil }

type stubAddr string

func (a stubAddr) Network() string { return string(a) }

func (a stubAddr) String() string { return string(a) }

func decodeRecordedFrames(data []byte) ([]*protocol.Frame, error) {
	reader := bytes.NewReader(data)
	var frames []*protocol.Frame
	for reader.Len() > 0 {
		frame, err := protocol.Decode(reader)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func TestSessionSpawnReplacementSubflow(t *testing.T) {
	var (
		dialMu    sync.Mutex
		dialCount int
		conns     []*passiveConn
	)
	dialer := func() (net.Conn, error) {
		pc := newPassiveConn()
		dialMu.Lock()
		dialCount++
		conns = append(conns, pc)
		dialMu.Unlock()
		return pc, nil
	}
	sess := newSession(SessionConfig{Role: RoleClient, InitialWindow: 1024, SubflowTarget: 1}, dialer)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
		sess.Close()
	}()

	sess.subflowsMu.Lock()
	sess.subflows[0] = &Subflow{ID: 0, Conn: &stubConn{}, session: sess}
	sess.subflowsMu.Unlock()

	sess.onSubflowError(0, errors.New("boom"))
	deadline := time.After(200 * time.Millisecond)
	for {
		dialMu.Lock()
		count := dialCount
		dialMu.Unlock()
		if count > 0 && sess.subflowCount() == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("replacement subflow not ready: dial=%d subflows=%d", count, sess.subflowCount())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

type passiveConn struct {
	closed chan struct{}
}

func newPassiveConn() *passiveConn {
	return &passiveConn{closed: make(chan struct{})}
}

func (c *passiveConn) Read(p []byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *passiveConn) Write(p []byte) (int, error) {
	return len(p), nil
}

func (c *passiveConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *passiveConn) LocalAddr() net.Addr  { return stubAddr("passive-local") }
func (c *passiveConn) RemoteAddr() net.Addr { return stubAddr("passive-remote") }

func (c *passiveConn) SetDeadline(time.Time) error      { return nil }
func (c *passiveConn) SetReadDeadline(time.Time) error  { return nil }
func (c *passiveConn) SetWriteDeadline(time.Time) error { return nil }
