package main

import (
	"crypto/tls"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/overlay"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/tui"
)

type serverFlags struct {
	listen    string
	certFile  string
	keyFile   string
	frameSize int
	window    int
	showTUI   bool
}

func parseServerFlags() serverFlags {
	var cfg serverFlags
	flag.StringVar(&cfg.listen, "listen", ":443", "TLS listen address")
	flag.StringVar(&cfg.certFile, "cert", "server.crt", "TLS certificate path")
	flag.StringVar(&cfg.keyFile, "key", "server.key", "TLS private key path")
	flag.IntVar(&cfg.frameSize, "frame", 32<<10, "frame size in bytes")
	flag.IntVar(&cfg.window, "window", 512<<10, "initial window bytes")
	flag.BoolVar(&cfg.showTUI, "tui", true, "render metrics dashboard")
	flag.Parse()
	return cfg
}

func main() {
	cfg := parseServerFlags()
	certificate, err := tls.LoadX509KeyPair(cfg.certFile, cfg.keyFile)
	if err != nil {
		log.Fatalf("load tls certificate: %v", err)
	}
	server, err := overlay.NewServer(overlay.ServerConfig{
		ListenAddr:        cfg.listen,
		FrameSize:         cfg.frameSize,
		InitialWindow:     uint32(cfg.window),
		HeartbeatInterval: 5 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS13,
		},
		WriteTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("overlay server init: %v", err)
	}

	if cfg.showTUI {
		dashboard := tui.NewDashboard(server, os.Stdout, 500*time.Millisecond)
		dashboard.Start()
		defer dashboard.Stop()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		server.Close()
	}()

	log.Printf("overlay server listening on %s", cfg.listen)
	if err := server.Serve(); err != nil {
		log.Printf("server stopped: %v", err)
	}
}
