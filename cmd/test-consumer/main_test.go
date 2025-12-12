package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestPerformUploadDirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()
		resp := uploadResponse{BytesReceived: int64(len(body)), ServerTimeMS: 5}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	file := tempFile(t, 2048)
	client, err := newHTTPClient(consumerFlags{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, elapsed, err := performUpload(ctx, client, server.URL, file, nil)
	if err != nil {
		t.Fatalf("perform upload: %v", err)
	}
	if resp.BytesReceived != 2048 {
		t.Fatalf("bytes received: %d", resp.BytesReceived)
	}
	if elapsed <= 0 {
		t.Fatalf("expected elapsed > 0")
	}
}

func TestPerformUploadViaSocks(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()
		resp := uploadResponse{BytesReceived: int64(len(body)), ServerTimeMS: 7}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer backend.Close()

	socksAddr, stop := startTestSocksProxy(t)
	defer stop()

	client, err := newHTTPClient(consumerFlags{useSocks: true, socksAddr: socksAddr})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	file := tempFile(t, 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, _, err := performUpload(ctx, client, backend.URL, file, nil)
	if err != nil {
		t.Fatalf("perform upload: %v", err)
	}
	if resp.BytesReceived != 1024 {
		t.Fatalf("wrong byte count: %d", resp.BytesReceived)
	}
}

func tempFile(t *testing.T, size int) string {
	t.Helper()
	f, err := os.CreateTemp("", "consumer-test-*.bin")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer f.Close()
	payload := bytes.Repeat([]byte("x"), size)
	if _, err := f.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() {
		os.Remove(f.Name())
	})
	return f.Name()
}

func startTestSocksProxy(t *testing.T) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
					t.Logf("accept error: %v", err)
				}
				return
			}
			go handleSocksConn(t, conn)
		}
	}()
	addr := ln.Addr().String()
	return addr, func() {
		close(stop)
		ln.Close()
	}
}

func handleSocksConn(t *testing.T, conn net.Conn) {
	defer conn.Close()
	var greeting [3]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		t.Logf("read greeting: %v", err)
		return
	}
	if greeting[0] != 0x05 {
		t.Log("bad version")
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		t.Logf("write method: %v", err)
		return
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Logf("read head: %v", err)
		return
	}
	addrType := head[3]
	var host string
	switch addrType {
	case 0x01:
		var buf [4]byte
		if _, err := io.ReadFull(conn, buf[:]); err != nil {
			t.Logf("ipv4: %v", err)
			return
		}
		host = net.IP(buf[:]).String()
	case 0x03:
		var ln [1]byte
		if _, err := io.ReadFull(conn, ln[:]); err != nil {
			t.Logf("domain len: %v", err)
			return
		}
		name := make([]byte, ln[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			t.Logf("domain: %v", err)
			return
		}
		host = string(name)
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Logf("ipv6: %v", err)
			return
		}
		host = net.IP(buf).String()
	default:
		t.Logf("unsupported addr type %d", addrType)
		return
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(conn, portBuf[:]); err != nil {
		t.Logf("port: %v", err)
		return
	}
	port := int(portBuf[0])<<8 | int(portBuf[1])
	dest := fmt.Sprintf("%s:%d", host, port)
	upstream, err := net.Dial("tcp", dest)
	if err != nil {
		t.Logf("dial dest: %v", err)
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Logf("write success: %v", err)
		return
	}
	done := make(chan struct{})
	go func() {
		io.Copy(upstream, conn)
		close(done)
	}()
	io.Copy(conn, upstream)
	<-done
}
