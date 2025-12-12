package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPayloadSize = 5 * 1024 * 1024
)

type consumerFlags struct {
	target    string
	filePath  string
	useSocks  bool
	socksAddr string
	timeout   time.Duration
}

func parseFlags() consumerFlags {
	var cfg consumerFlags
	flag.StringVar(&cfg.target, "target", "http://127.0.0.1:9000/upload", "target upload URL")
	flag.StringVar(&cfg.filePath, "file", "", "path to payload file (optional)")
	flag.BoolVar(&cfg.useSocks, "use-socks", false, "route traffic through SOCKS5 proxy")
	flag.StringVar(&cfg.socksAddr, "socks", "127.0.0.1:1080", "SOCKS5 proxy address")
	flag.DurationVar(&cfg.timeout, "timeout", 60*time.Second, "request timeout")
	flag.Parse()
	return cfg
}

type uploadResponse struct {
	BytesReceived int64 `json:"bytes_received"`
	ServerTimeMS  int64 `json:"server_time_ms"`
}

func main() {
	cfg := parseFlags()
	payloadFile, err := ensurePayload(cfg.filePath)
	if err != nil {
		log.Fatalf("prepare payload: %v", err)
	}
	if cfg.filePath == "" {
		log.Printf("generated payload file: %s", payloadFile)
	}
	client, err := newHTTPClient(cfg)
	if err != nil {
		log.Fatalf("http client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	resp, elapsed, err := performUpload(ctx, client, cfg.target, payloadFile)
	if err != nil {
		log.Fatalf("upload failed: %v", err)
	}
	mode := "direct"
	if cfg.useSocks {
		mode = fmt.Sprintf("SOCKS %s", cfg.socksAddr)
	}
	log.Printf("mode: %s", mode)
	log.Printf("client_time_ms=%d", elapsed.Milliseconds())
	if elapsed > 0 {
		throughput := float64(resp.BytesReceived) / (1024 * 1024) / elapsed.Seconds()
		log.Printf("throughput_mibps=%.2f", throughput)
	}
	log.Printf("server_bytes=%d server_time_ms=%d", resp.BytesReceived, resp.ServerTimeMS)
}

func ensurePayload(path string) (string, error) {
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if errors.Is(err, os.ErrNotExist) {
			if err := writePayloadFile(path); err != nil {
				return "", err
			}
			return path, nil
		} else {
			return "", err
		}
	}
	name := fmt.Sprintf("test-consumer-%d.txt", time.Now().UnixNano())
	tmpPath := filepath.Join(os.TempDir(), name)
	if err := writePayloadFile(tmpPath); err != nil {
		return "", err
	}
	return tmpPath, nil
}

func writePayloadFile(path string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	pattern := []byte("The quick brown fox jumps over the lazy dog. ")
	written := 0
	for written < defaultPayloadSize {
		remain := defaultPayloadSize - written
		chunk := pattern
		if remain < len(pattern) {
			chunk = pattern[:remain]
		}
		if _, err := file.Write(chunk); err != nil {
			return err
		}
		written += len(chunk)
	}
	return file.Sync()
}

func newHTTPClient(cfg consumerFlags) (*http.Client, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
	}
	if cfg.useSocks {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialViaSocks(ctx, network, cfg.socksAddr, addr)
		}
	}
	return &http.Client{Transport: transport}, nil
}

func performUpload(ctx context.Context, client *http.Client, target, filePath string) (uploadResponse, time.Duration, error) {
	var out uploadResponse
	file, err := os.Open(filePath)
	if err != nil {
		return out, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return out, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, file)
	if err != nil {
		return out, 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = info.Size()
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return out, 0, err
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return out, elapsed, fmt.Errorf("unexpected status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, elapsed, err
	}
	return out, elapsed, nil
}

func dialViaSocks(ctx context.Context, network, socksAddr, destAddr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, socksAddr)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	if err := sendSocksGreeting(conn); err != nil {
		return nil, err
	}
	if err := requestSocksConnect(conn, destAddr); err != nil {
		return nil, err
	}
	success = true
	return conn, nil
}

func sendSocksGreeting(rw io.ReadWriter) error {
	if _, err := rw.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	var resp [2]byte
	if _, err := io.ReadFull(rw, resp[:]); err != nil {
		return err
	}
	if resp[0] != 0x05 {
		return errors.New("socks: unexpected version")
	}
	if resp[1] != 0x00 {
		return errors.New("socks: authentication required")
	}
	return nil
}

func requestSocksConnect(conn net.Conn, dest string) error {
	host, portStr, err := net.SplitHostPort(dest)
	if err != nil {
		return err
	}
	port, err := parsePort(portStr)
	if err != nil {
		return err
	}
	buf := []byte{0x05, 0x01, 0x00}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		buf = append(buf, 0x03)
		buf = append(buf, byte(len(host)))
		buf = append(buf, host...)
	case ip.To4() != nil:
		buf = append(buf, 0x01)
		buf = append(buf, ip.To4()...)
	default:
		buf = append(buf, 0x04)
		buf = append(buf, ip.To16()...)
	}
	buf = append(buf, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(buf); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[0] != 0x05 {
		return errors.New("socks: unexpected version in reply")
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks: connect failed with code %d", head[1])
	}
	addrType := head[3]
	var discard int
	switch addrType {
	case 0x01:
		discard = 4
	case 0x03:
		var ln [1]byte
		if _, err := io.ReadFull(conn, ln[:]); err != nil {
			return err
		}
		discard = int(ln[0])
	case 0x04:
		discard = 16
	default:
		return errors.New("socks: unknown address type")
	}
	if discard > 0 {
		trash := make([]byte, discard)
		if _, err := io.ReadFull(conn, trash); err != nil {
			return err
		}
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return err
	}
	return nil
}

func parsePort(port string) (uint16, error) {
	if port == "" {
		return 0, errors.New("empty port")
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, errors.New("port must be greater than zero")
	}
	return uint16(value), nil
}
