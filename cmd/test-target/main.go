package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type serverFlags struct {
	listen string
}

func parseFlags() serverFlags {
	var cfg serverFlags
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:9000", "address to listen on")
	flag.Parse()
	return cfg
}

func main() {
	cfg := parseFlags()
	mux := newMux()
	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("received signal %s, shutting down", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("test-target listening on %s", cfg.listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

func newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/upload", http.HandlerFunc(handleUpload))
	return mux
}

type uploadResponse struct {
	BytesReceived int64 `json:"bytes_received"`
	ServerTimeMS  int64 `json:"server_time_ms"`
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	defer r.Body.Close()
	bytesRead, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		log.Printf("upload read error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp := uploadResponse{
		BytesReceived: bytesRead,
		ServerTimeMS:  time.Since(start).Milliseconds(),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode response: %v", err)
	}
}
