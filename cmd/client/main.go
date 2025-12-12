package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/socks"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/tui"
)

type clientFlags struct {
	server    string
	listen    string
	subflows  int
	frameSize int
	window    int
	showTUI   bool
}

func parseFlags() clientFlags {
	var cfg clientFlags
	flag.StringVar(&cfg.server, "server", "127.0.0.1:443", "overlay server address")
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:1080", "SOCKS5 listen address")
	flag.IntVar(&cfg.subflows, "subflows", 6, "number of parallel TLS subflows")
	flag.IntVar(&cfg.frameSize, "frame", 32<<10, "frame size in bytes")
	flag.IntVar(&cfg.window, "window", 512<<10, "initial flow control window (bytes)")
	flag.BoolVar(&cfg.showTUI, "tui", true, "render metrics dashboard")
	flag.Parse()
	return cfg
}

func main() {
	cfg := parseFlags()
	client, err := overlay.NewClient(overlay.ClientConfig{
		ServerAddr:        cfg.server,
		Subflows:          cfg.subflows,
		FrameSize:         cfg.frameSize,
		InitialWindow:     uint32(cfg.window),
		HeartbeatInterval: 5 * time.Second,
		DialTimeout:       5 * time.Second,
	})
	if err != nil {
		log.Fatalf("overlay client init: %v", err)
	}
	sess := client.Session()

	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("SOCKS5 listening on %s", cfg.listen)

	if cfg.showTUI {
		dashboard := tui.NewDashboard(sess, os.Stdout, 500*time.Millisecond)
		dashboard.Start()
		defer dashboard.Stop()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		listener.Close()
		sess.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			return
		}
		go handleSOCKS(conn, sess)
	}
}

func handleSOCKS(conn net.Conn, sess *overlay.Session) {
	defer conn.Close()
	addr, err := socks.Handshake(conn)
	if err != nil {
		log.Printf("socks handshake failed: %v", err)
		return
	}
	stream, err := sess.OpenStream(addr)
	if err != nil {
		log.Printf("stream open failed: %v", err)
		return
	}
	stream.BindClient(conn)
	<-stream.Done()
}
