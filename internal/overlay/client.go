package overlay

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay/protocol"
)

// ClientConfig defines how the overlay client connects to the server.
type ClientConfig struct {
	ServerAddr        string
	Subflows          int
	FrameSize         int
	InitialWindow     uint32
	HeartbeatInterval time.Duration
	DialTimeout       time.Duration
	TLSConfig         *tls.Config
	WriteTimeout      time.Duration
	Plaintext         bool
}

type Client struct {
	cfg     ClientConfig
	session *Session
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Subflows <= 0 {
		cfg.Subflows = 1
	}
	if cfg.FrameSize == 0 {
		cfg.FrameSize = 32 << 10
	}
	if cfg.InitialWindow == 0 {
		cfg.InitialWindow = 512 << 10
	}
	sessCfg := SessionConfig{
		Role:              RoleClient,
		FrameSize:         cfg.FrameSize,
		InitialWindow:     cfg.InitialWindow,
		HeartbeatInterval: cfg.HeartbeatInterval,
		WriteTimeout:      cfg.WriteTimeout,
		SubflowTarget:     cfg.Subflows,
		EnableChecksums:   cfg.Plaintext,
	}
	client := &Client{cfg: cfg}
	var dialer func() (net.Conn, error)
	if cfg.Plaintext {
		dialer = func() (net.Conn, error) {
			d := &net.Dialer{Timeout: cfg.DialTimeout}
			return d.Dial("tcp", cfg.ServerAddr)
		}
	} else {
		if cfg.TLSConfig == nil {
			cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true}
		}
		dialer = func() (net.Conn, error) {
			d := &net.Dialer{Timeout: cfg.DialTimeout}
			return tls.DialWithDialer(d, "tcp", cfg.ServerAddr, cfg.TLSConfig)
		}
	}
	sess, err := NewClientSession(sessCfg, dialer)
	if err != nil {
		return nil, err
	}
	client.session = sess
	if err := client.establishSubflows(); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *Client) establishSubflows() error {
	for i := 0; i < c.cfg.Subflows; i++ {
		sf, err := newClientSubflow(i, c.session, c.session.Dialer)
		if err != nil {
			return fmt.Errorf("overlay: dial subflow: %w", err)
		}
		frameType := protocol.ControlSessionJoin
		if i == 0 {
			frameType = protocol.ControlSessionInit
		}
		hello := &protocol.Frame{
			Type:      protocol.FrameControl,
			SessionID: c.session.id,
			Control: &protocol.ControlPayload{
				Type:      frameType,
				SessionID: c.session.id,
				Window:    c.cfg.InitialWindow,
			},
		}
		if err := sf.send(hello); err != nil {
			return err
		}
		c.session.registerSubflow(sf)
	}
	return nil
}

func (c *Client) Session() *Session {
	return c.session
}
