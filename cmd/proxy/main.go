package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/app"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "", "Path to configuration JSON file")
	flag.Parse()

	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "--config path is required")
		os.Exit(2)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	application, err := app.New(cfg)
	if err != nil {
		log.Fatalf("init app: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := application.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
}
