package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/http2"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
)

type SessionConn struct {
	RawConn   net.Conn
	Client    *http2.ClientConn
	Transport *http2.Transport
}

type Dialer struct {
	cfg         *config.Config
	dialTimeout time.Duration
}

func NewDialer(cfg *config.Config) *Dialer {
	return &Dialer{cfg: cfg, dialTimeout: 5 * time.Second}
}

func (d *Dialer) Dial(ctx context.Context) (*SessionConn, error) {
	ctx, cancel := context.WithTimeout(ctx, d.dialTimeout)
	defer cancel()

	address := d.cfg.TargetAuthority()
	netDialer := &net.Dialer{}
	conn, err := netDialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}

	if d.cfg.Target.TLS {
		tlsCfg := &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
		}
		if d.cfg.Target.Host != "" {
			tlsCfg.ServerName = d.cfg.Target.Host
		}
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		if proto := tlsConn.ConnectionState().NegotiatedProtocol; proto != "h2" {
			tlsConn.Close()
			return nil, fmt.Errorf("tls handshake: upstream negotiated %q instead of h2", proto)
		}
		conn = tlsConn
	}

	transport := &http2.Transport{}
	if d.cfg.Target.TLS {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
			ServerName:         d.cfg.Target.Host,
		}
	} else {
		transport.AllowHTTP = true
	}

	clientConn, err := transport.NewClientConn(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("http2 client conn: %w", err)
	}

	return &SessionConn{
		RawConn:   conn,
		Client:    clientConn,
		Transport: transport,
	}, nil
}
