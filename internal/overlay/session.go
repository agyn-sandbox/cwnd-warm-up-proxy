package overlay

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay/protocol"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/tui"
)

// Role identifies which side of the overlay a session instance operates on.
type Role int

const (
	RoleClient Role = iota
	RoleServer
)

// SessionConfig bundles tunables shared across client and server sessions.
type SessionConfig struct {
	Role              Role
	SessionID         uint32
	FrameSize         int
	InitialWindow     uint32
	HeartbeatInterval time.Duration
	WriteTimeout      time.Duration
}

// Dialer abstracts the transport used to create subflows on the client.
type Dialer func() (net.Conn, error)

// StreamIDAllocator yields monotonically increasing stream identifiers.
type StreamIDAllocator struct {
	value uint32
}

func (a *StreamIDAllocator) Next() uint32 {
	return atomic.AddUint32(&a.value, 1)
}

// Session models a logical overlay session spanning K subflows.
type Session struct {
	cfg       SessionConfig
	id        uint32
	streams   map[uint32]*Stream
	streamsMu sync.RWMutex

	subflows   map[int]*Subflow
	subflowsMu sync.RWMutex
	scheduler  *Scheduler

	incoming chan inboundFrame
	done     chan struct{}
	closed   atomic.Bool

	streamIDs StreamIDAllocator

	Dialer Dialer

	metricsWindow time.Duration
}

type inboundFrame struct {
	subflow *Subflow
	frame   *protocol.Frame
}

// NewClientSession constructs a session in client role. The dialer is used to
// create new TLS subflows on demand.
func NewClientSession(cfg SessionConfig, dialer Dialer) (*Session, error) {
	if cfg.Role != RoleClient {
		return nil, fmt.Errorf("overlay: NewClientSession expects client role")
	}
	return newSession(cfg, dialer), nil
}

// NewServerSession constructs a server-side session. Session IDs are chosen by
// the client; the server is told about them via control frames before any data
// flows.
func NewServerSession(cfg SessionConfig) *Session {
	return newSession(cfg, nil)
}

func newSession(cfg SessionConfig, dialer Dialer) *Session {
	s := &Session{
		cfg:           cfg,
		id:            cfg.SessionID,
		streams:       make(map[uint32]*Stream),
		subflows:      make(map[int]*Subflow),
		scheduler:     NewScheduler(),
		incoming:      make(chan inboundFrame, 64),
		done:          make(chan struct{}),
		Dialer:        dialer,
		metricsWindow: 10 * time.Second,
	}
	if s.id == 0 {
		s.id = randomSessionID()
	}
	go s.loop()
	return s
}

func randomSessionID() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}

func (s *Session) loop() {
	for {
		select {
		case inbound := <-s.incoming:
			s.dispatchFrame(inbound.subflow, inbound.frame)
		case <-s.done:
			return
		}
	}
}

func (s *Session) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.done)
	s.subflowsMu.Lock()
	for _, sf := range s.subflows {
		_ = sf.close()
	}
	s.subflows = nil
	s.subflowsMu.Unlock()
	s.streamsMu.Lock()
	for _, st := range s.streams {
		st.close(io.EOF)
	}
	s.streams = nil
	s.streamsMu.Unlock()
	return nil
}

func (s *Session) registerSubflow(sf *Subflow) {
	s.subflowsMu.Lock()
	s.subflows[sf.ID] = sf
	s.subflowsMu.Unlock()
	s.scheduler.Attach(sf)
	go sf.run()
}

func (s *Session) removeSubflow(id int) {
	s.subflowsMu.Lock()
	delete(s.subflows, id)
	s.subflowsMu.Unlock()
	s.scheduler.Detach(id)
}

func (s *Session) onSubflowError(id int, err error) {
	s.removeSubflow(id)
	// Notify streams about failure so they can tear down gracefully.
	s.streamsMu.RLock()
	for _, stream := range s.streams {
		stream.fail(err)
	}
	s.streamsMu.RUnlock()
}

func (s *Session) handleFrame(sf *Subflow, frame *protocol.Frame) {
	select {
	case s.incoming <- inboundFrame{subflow: sf, frame: frame}:
	case <-s.done:
	}
}

func (s *Session) dispatchFrame(sf *Subflow, frame *protocol.Frame) {
	switch frame.Type {
	case protocol.FrameData:
		s.handleDataFrame(frame)
	case protocol.FrameAck:
		s.handleAckFrame(frame)
	case protocol.FrameControl:
		s.handleControlFrame(sf, frame)
	case protocol.FrameHeartbeat:
		// For now heartbeat frames only refresh liveness; acks will flow separately.
	default:
		// Unknown frame type → ignore for forward compatibility during prototype.
	}
}

func (s *Session) handleDataFrame(frame *protocol.Frame) {
	stream := s.getStream(frame.StreamID)
	if stream == nil {
		return
	}
	stream.onData(frame)
}

func (s *Session) handleAckFrame(frame *protocol.Frame) {
	stream := s.getStream(frame.StreamID)
	if stream == nil || frame.Ack == nil {
		return
	}
	stream.onAck(frame.Ack)
}

func (s *Session) handleControlFrame(sf *Subflow, frame *protocol.Frame) {
	if frame.Control == nil {
		return
	}
	switch frame.Control.Type {
	case protocol.ControlSessionInit:
		if s.cfg.Role != RoleServer {
			return
		}
		s.id = frame.SessionID
		s.registerSubflow(sf)
		ack := &protocol.Frame{
			Type:      protocol.FrameControl,
			SessionID: s.id,
			Control: &protocol.ControlPayload{
				Type:      protocol.ControlSessionAccept,
				SessionID: s.id,
				Window:    s.cfg.InitialWindow,
			},
		}
		_ = sf.send(ack)
	case protocol.ControlSessionJoin:
		if s.cfg.Role != RoleServer {
			return
		}
		s.registerSubflow(sf)
	case protocol.ControlSessionAccept:
		if s.cfg.Role != RoleClient {
			return
		}
		// Nothing else for prototype besides acknowledging reception.
	case protocol.ControlStreamOpen:
		s.handleStreamOpen(sf, frame)
	case protocol.ControlStreamAccept:
		stream := s.getStream(frame.StreamID)
		if stream != nil {
			stream.accept()
		}
	case protocol.ControlStreamClose:
		stream := s.getStream(frame.StreamID)
		if stream != nil {
			stream.close(io.EOF)
		}
	case protocol.ControlStreamReset:
		stream := s.getStream(frame.StreamID)
		if stream != nil {
			stream.fail(errors.New("remote reset"))
		}
	}
}

func (s *Session) handleStreamOpen(sf *Subflow, frame *protocol.Frame) {
	if s.cfg.Role != RoleServer {
		return
	}
	meta := frame.Control.Metadata
	addr, _ := meta["target"].(string)
	streamID := frame.StreamID
	stream := s.createStream(streamID)
	if err := stream.openServerSide(addr); err != nil {
		reset := &protocol.Frame{
			Type:      protocol.FrameControl,
			SessionID: s.id,
			StreamID:  streamID,
			Control:   &protocol.ControlPayload{Type: protocol.ControlStreamReset},
		}
		_ = sf.send(reset)
		return
	}
	ack := &protocol.Frame{
		Type:      protocol.FrameControl,
		SessionID: s.id,
		StreamID:  streamID,
		Control: &protocol.ControlPayload{
			Type:   protocol.ControlStreamAccept,
			Window: s.cfg.InitialWindow,
		},
	}
	_ = s.sendFrame(ack)
	stream.accept()
}

func (s *Session) getStream(id uint32) *Stream {
	s.streamsMu.RLock()
	defer s.streamsMu.RUnlock()
	return s.streams[id]
}

func (s *Session) createStream(id uint32) *Stream {
	stream := newStream(s, id)
	s.streamsMu.Lock()
	s.streams[id] = stream
	s.streamsMu.Unlock()
	return stream
}

func (s *Session) sendFrame(frame *protocol.Frame) error {
	sub := s.scheduler.Next()
	if sub == nil {
		return errors.New("overlay: no subflow available")
	}
	frame.SessionID = s.id
	return sub.send(frame)
}

// OpenStream is used on the client to create a new logical stream towards the
// server. Metadata must include the "target" address required for the server to
// connect to.
func (s *Session) OpenStream(target string) (*Stream, error) {
	if s.cfg.Role != RoleClient {
		return nil, fmt.Errorf("overlay: OpenStream only usable on client")
	}
	id := s.streamIDs.Next()
	stream := s.createStream(id)
	open := &protocol.Frame{
		Type:      protocol.FrameControl,
		SessionID: s.id,
		StreamID:  id,
		Control: &protocol.ControlPayload{
			Type:     protocol.ControlStreamOpen,
			StreamID: id,
			Metadata: map[string]any{"target": target},
		},
	}
	if err := s.sendFrame(open); err != nil {
		return nil, err
	}
	if err := stream.waitForAccept(); err != nil {
		return nil, err
	}
	return stream, nil
}

// sendData transmits bytes for a stream respecting flow control limits.
func (s *Session) sendData(stream *Stream, payload []byte) error {
	seq, err := stream.reserveSequence(uint32(len(payload)))
	if err != nil {
		return err
	}
	frame := &protocol.Frame{
		Type:      protocol.FrameData,
		SessionID: s.id,
		StreamID:  stream.id,
		Seq:       seq,
		Payload:   payload,
	}
	return s.sendFrame(frame)
}

// sendAck sends an ack frame for the specified stream.
func (s *Session) sendAck(streamID uint32, ackSeq uint64, credit uint32) error {
	frame := &protocol.Frame{
		Type:      protocol.FrameAck,
		SessionID: s.id,
		StreamID:  streamID,
		Ack: &protocol.AckPayload{
			AckSeq: ackSeq,
			Credit: credit,
		},
	}
	return s.sendFrame(frame)
}

// Snapshot implements tui.Provider.
func (s *Session) Snapshot() tui.Snapshot {
	snap := tui.Snapshot{Timestamp: time.Now()}
	s.subflowsMu.RLock()
	for _, sf := range s.subflows {
		throughput := float64(sf.tx.Sum()) / s.metricsWindow.Seconds()
		snap.Subflows = append(snap.Subflows, tui.SubflowStat{
			ID:         sf.ID,
			RTT:        0,
			Throughput: throughput,
		})
	}
	s.subflowsMu.RUnlock()
	s.streamsMu.RLock()
	for id, stream := range s.streams {
		throughput := float64(stream.throughput.Sum()) / s.metricsWindow.Seconds()
		outstanding := stream.inflight.Load()
		snap.Streams = append(snap.Streams, tui.StreamStat{
			ID:          id,
			Outstanding: outstanding,
			Throughput:  throughput,
		})
	}
	s.streamsMu.RUnlock()
	return snap
}
