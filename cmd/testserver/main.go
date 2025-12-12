package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/testserver"
)

func main() {
	port := flag.Int("port", 8081, "port to listen on (0 for random)")
	disableHTTP1 := flag.Bool("require_h2", false, "require HTTP/2 for clients")
	enableH2C := flag.Bool("h2c", true, "enable h2c (HTTP/2 over cleartext)")
	flag.Parse()

	cfg := testserver.Config{
		Port:          *port,
		SupportHTTP11: !*disableHTTP1,
		SupportH2C:    *enableH2C,
	}

	server, err := testserver.New(cfg)
	if err != nil {
		log.Fatalf("create server: %v", err)
	}

	server.Renderer().SetOutput(os.Stdout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	go func() {
		<-server.Ready()
		fmt.Printf("Test server listening on %s\n", server.Address())
	}()

	if err := server.Run(ctx); err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			return
		}
		log.Fatalf("server error: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	signal.Stop(sigCh)
	close(sigCh)

	io.WriteString(os.Stdout, "Server stopped.\n")
}
