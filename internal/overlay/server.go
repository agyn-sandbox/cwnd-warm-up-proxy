package overlay

import (
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay/protocol"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/tui"
)

// ServerConfig controls the overlay server listener behaviour.
type ServerConfig struct {
	ListenAddr        string
	FrameSize         int
	InitialWindow     uint32
	HeartbeatInterval time.Duration
	TLSConfig         *tls.Config
	WriteTimeout      time.Duration
	Plaintext         bool
}

type Server struct {
	cfg      ServerConfig
	listener net.Listener

	sessions   map[uint32]*Session
	sessionsMu sync.Mutex

	subflowIDs atomicCounter
}

type atomicCounter struct {
	mu sync.Mutex
	vl int
}

func (c *atomicCounter) Next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	val := c.vl
	c.vl++
	return val
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.FrameSize == 0 {
		cfg.FrameSize = 32 << 10
	}
	if cfg.InitialWindow == 0 {
		cfg.InitialWindow = 512 << 10
	}
	var ln net.Listener
	var err error
	if cfg.Plaintext {
		ln, err = net.Listen("tcp", cfg.ListenAddr)
	} else {
		if cfg.TLSConfig == nil {
			return nil, errors.New("overlay: server TLS configuration required")
		}
		ln, err = tls.Listen("tcp", cfg.ListenAddr, cfg.TLSConfig)
	}
	if err != nil {
		return nil, err
	}
	server := &Server{
		cfg:      cfg,
		listener: ln,
		sessions: make(map[uint32]*Session),
	}
	return server, nil
}

func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

// Addr returns the listening address.
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

func (s *Server) handleConn(conn net.Conn) {
	frame, err := protocol.ReadFrame(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	if frame.Type != protocol.FrameControl || frame.Control == nil {
		_ = conn.Close()
		return
	}
	if s.cfg.Plaintext && frame.Flags&protocol.FlagChecksumPresent == 0 {
		_ = conn.Close()
		return
	}
	sessionID := frame.SessionID
	var sess *Session
	s.sessionsMu.Lock()
	sess = s.sessions[sessionID]
	if sess == nil && frame.Control.Type == protocol.ControlSessionInit {
		sessCfg := SessionConfig{
			Role:              RoleServer,
			SessionID:         sessionID,
			FrameSize:         s.cfg.FrameSize,
			InitialWindow:     s.cfg.InitialWindow,
			HeartbeatInterval: s.cfg.HeartbeatInterval,
			WriteTimeout:      s.cfg.WriteTimeout,
			EnableChecksums:   s.cfg.Plaintext,
		}
		sess = NewServerSession(sessCfg)
		s.sessions[sessionID] = sess
	}
	s.sessionsMu.Unlock()
	if sess == nil {
		_ = conn.Close()
		return
	}
	sf := newServerSubflow(s.subflowIDs.Next(), sess, conn)
	sess.handleFrame(sf, frame)
}

func (s *Server) Close() error {
	return s.listener.Close()
}

// Snapshot implements tui.Provider to surface aggregate metrics.
func (s *Server) Snapshot() tui.Snapshot {
	snap := tui.Snapshot{Timestamp: time.Now()}
	s.sessionsMu.Lock()
	for _, sess := range s.sessions {
		ss := sess.Snapshot()
		snap.Subflows = append(snap.Subflows, ss.Subflows...)
		snap.Streams = append(snap.Streams, ss.Streams...)
	}
	s.sessionsMu.Unlock()
	return snap
}
