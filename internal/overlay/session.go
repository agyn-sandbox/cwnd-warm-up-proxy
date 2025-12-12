package overlay

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
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
	SubflowTarget     int
	EnableChecksums   bool
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

	subflows      map[int]*Subflow
	subflowsMu    sync.RWMutex
	scheduler     *Scheduler
	nextSubflowID int
	pendingSpawns int

	incoming chan inboundFrame
	done     chan struct{}
	closed   atomic.Bool

	streamIDs StreamIDAllocator

	Dialer Dialer

	metricsWindow time.Duration
	metricsStep   time.Duration
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
		metricsStep:   500 * time.Millisecond,
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
	if s.closed.Load() {
		s.subflowsMu.Unlock()
		_ = sf.close()
		return
	}
	if s.subflows == nil {
		s.subflows = make(map[int]*Subflow)
	}
	if sf.ID >= s.nextSubflowID {
		s.nextSubflowID = sf.ID + 1
	}
	if s.pendingSpawns > 0 {
		s.pendingSpawns--
	}
	s.subflows[sf.ID] = sf
	s.subflowsMu.Unlock()
	s.scheduler.Attach(sf)
	go sf.run()
}

func (s *Session) subflowCount() int {
	s.subflowsMu.RLock()
	defer s.subflowsMu.RUnlock()
	return len(s.subflows)
}

func (s *Session) prepareSubflowSpawn() (id int, useInit bool, ok bool) {
	if s.cfg.Role != RoleClient || s.Dialer == nil || s.cfg.SubflowTarget <= 0 {
		return 0, false, false
	}
	s.subflowsMu.Lock()
	defer func() {
		if !ok {
			s.subflowsMu.Unlock()
		}
	}()
	if len(s.subflows)+s.pendingSpawns >= s.cfg.SubflowTarget {
		ok = false
		return 0, false, false
	}
	id = s.nextSubflowID
	s.nextSubflowID++
	useInit = len(s.subflows) == 0 && s.pendingSpawns == 0
	s.pendingSpawns++
	ok = true
	s.subflowsMu.Unlock()
	return id, useInit, true
}

func (s *Session) subflowSpawnFailed() {
	s.subflowsMu.Lock()
	if s.pendingSpawns > 0 {
		s.pendingSpawns--
	}
	s.subflowsMu.Unlock()
}

func (s *Session) spawnReplacementSubflow() {
	id, useInit, ok := s.prepareSubflowSpawn()
	if !ok {
		return
	}
	go s.dialSubflow(id, useInit)
}

func (s *Session) dialSubflow(id int, useInit bool) {
	sf, err := newClientSubflow(id, s, s.Dialer)
	if err != nil {
		s.subflowSpawnFailed()
		time.AfterFunc(time.Second, func() { s.spawnReplacementSubflow() })
		return
	}
	frameType := protocol.ControlSessionJoin
	if useInit {
		frameType = protocol.ControlSessionInit
	}
	hello := &protocol.Frame{
		Type:      protocol.FrameControl,
		SessionID: s.id,
		Control: &protocol.ControlPayload{
			Type:      frameType,
			SessionID: s.id,
			Window:    s.cfg.InitialWindow,
		},
	}
	if err := sf.send(hello); err != nil {
		_ = sf.close()
		s.subflowSpawnFailed()
		time.AfterFunc(time.Second, func() { s.spawnReplacementSubflow() })
		return
	}
	s.registerSubflow(sf)
}

func (s *Session) removeSubflow(id int) *Subflow {
	s.subflowsMu.Lock()
	sf := s.subflows[id]
	delete(s.subflows, id)
	s.subflowsMu.Unlock()
	s.scheduler.Detach(id)
	return sf
}

func (s *Session) onSubflowError(id int, err error) {
	sf := s.removeSubflow(id)
	if sf != nil {
		_ = sf.close()
	}
	if s.cfg.Role == RoleClient && s.Dialer != nil && s.cfg.SubflowTarget > 0 {
		s.spawnReplacementSubflow()
	}
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
func (s *Session) sendData(stream *Stream, payload []byte, end bool) error {
	entry, err := stream.queueFrame(payload, end)
	if err != nil {
		return err
	}
	entry.frame.SessionID = s.id
	entry.frame.IsDuplicate = false
	if err := s.sendFrame(entry.frame); err != nil {
		stream.onSendFailure(entry)
		return err
	}
	entry.lastSend = time.Now()
	return nil
}

// sendAck sends an ack frame for the specified stream.
func (s *Session) sendAck(streamID uint32, ack *protocol.AckPayload) error {
	frame := &protocol.Frame{
		Type:      protocol.FrameAck,
		SessionID: s.id,
		StreamID:  streamID,
		Ack:       ack,
	}
	return s.sendFrame(frame)
}

func (s *Session) retransmit(stream *Stream, entry *pendingEntry, now time.Time) {
	entry.frame.SessionID = s.id
	entry.frame.IsDuplicate = true
	if err := s.sendFrame(entry.frame); err != nil {
		stream.fail(err)
		return
	}
	entry.lastSend = now
}

func (s *Session) snapshotTitle() string {
	switch s.cfg.Role {
	case RoleClient:
		return "overlay client"
	case RoleServer:
		return "overlay server"
	default:
		return "overlay session"
	}
}

// Snapshot implements tui.Provider.
func (s *Session) Snapshot() tui.Snapshot {
	windowSeconds := s.metricsWindow.Seconds()
	if windowSeconds <= 0 {
		windowSeconds = 1
	}
	rate := func(c *metrics.SlidingCounter) float64 {
		if c == nil {
			return 0
		}
		return float64(c.Sum()) / windowSeconds
	}

	s.subflowsMu.RLock()
	subflows := make([]*Subflow, 0, len(s.subflows))
	for _, sf := range s.subflows {
		subflows = append(subflows, sf)
	}
	s.subflowsMu.RUnlock()
	sort.Slice(subflows, func(i, j int) bool { return subflows[i].ID < subflows[j].ID })

	var totalTxReal, totalTxDummy, totalRxReal, totalRxDummy float64
	lines := make([]string, 0, len(subflows)+8)
	for _, sf := range subflows {
		txReal := rate(sf.txReal)
		txDummy := rate(sf.txDummy)
		rxReal := rate(sf.rxReal)
		rxDummy := rate(sf.rxDummy)
		totalTxReal += txReal
		totalTxDummy += txDummy
		totalRxReal += rxReal
		totalRxDummy += rxDummy
		lines = append(lines, fmt.Sprintf(
			"subflow %d | tx real=%s dummy=%s | rx real=%s dummy=%s",
			sf.ID,
			tui.FormatRate(txReal),
			tui.FormatRate(txDummy),
			tui.FormatRate(rxReal),
			tui.FormatRate(rxDummy),
		))
	}

	summary := fmt.Sprintf(
		"totals | tx real=%s dummy=%s | rx real=%s dummy=%s",
		tui.FormatRate(totalTxReal),
		tui.FormatRate(totalTxDummy),
		tui.FormatRate(totalRxReal),
		tui.FormatRate(totalRxDummy),
	)
	lines = append([]string{summary, fmt.Sprintf("subflows active=%d", len(subflows))}, lines...)

	s.streamsMu.RLock()
	streamIDs := make([]uint32, 0, len(s.streams))
	for id := range s.streams {
		streamIDs = append(streamIDs, id)
	}
	s.sortStreamsUnlocked(streamIDs)
	var totalOutstanding uint32
	var totalStreamThroughput float64
	for _, id := range streamIDs {
		stream := s.streams[id]
		throughput := float64(stream.throughput.Sum()) / windowSeconds
		outstanding := stream.inflight.Load()
		totalOutstanding += outstanding
		totalStreamThroughput += throughput
		lines = append(lines, fmt.Sprintf(
			"stream %d | outstanding=%d throughput=%s",
			id,
			outstanding,
			tui.FormatRate(throughput),
		))
	}
	s.streamsMu.RUnlock()
	if len(streamIDs) > 0 {
		lines = append(lines, fmt.Sprintf(
			"streams summary | active=%d inflight=%d throughput=%s",
			len(streamIDs),
			totalOutstanding,
			tui.FormatRate(totalStreamThroughput),
		))
	} else {
		lines = append(lines, "streams active=0")
	}

	return tui.Snapshot{
		Timestamp: time.Now(),
		Title:     s.snapshotTitle(),
		Lines:     lines,
	}
}

func (s *Session) sortStreamsUnlocked(ids []uint32) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}
