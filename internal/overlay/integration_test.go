package overlay

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

func TestOverlayEchoIntegration(t *testing.T) {
	certPEM, keyPEM := mustSelfSignedCert(t)
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}

	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoListener.Close()
	go runEchoServer(echoListener)

	server, err := NewServer(ServerConfig{
		ListenAddr:        "127.0.0.1:0",
		FrameSize:         32 << 10,
		InitialWindow:     512 << 10,
		HeartbeatInterval: time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
		},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer server.Close()
	go func() {
		_ = server.Serve()
	}()

	client, err := NewClient(ClientConfig{
		ServerAddr:        server.Addr().String(),
		Subflows:          3,
		FrameSize:         16 << 10,
		InitialWindow:     256 << 10,
		HeartbeatInterval: time.Second,
		DialTimeout:       3 * time.Second,
		TLSConfig:         &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	sess := client.Session()

	stream, err := sess.OpenStream(echoListener.Addr().String())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	local, remote := net.Pipe()
	stream.BindClient(local)
	defer remote.Close()

	payload := make([]byte, 5*1024*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	go func() {
		_, _ = remote.Write(payload)
	}()
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(remote, received); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("payload mismatch")
	}
	remote.Close()
	<-stream.Done()
}

func runEchoServer(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 64<<10)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					if _, werr := c.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}(conn)
	}
}

func mustSelfSignedCert(t *testing.T) ([]byte, []byte) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "overlay-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM
}
