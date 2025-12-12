package testserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func startServer(t *testing.T, cfg Config) (*Server, context.CancelFunc, <-chan error) {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.Renderer().SetOutput(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Run(ctx)
	}()

	select {
	case <-srv.Ready():
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatalf("server did not become ready")
	}

	return srv, cancel, errCh
}

func buildMultipartPayload(t *testing.T, fieldName, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(fieldName, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func stopServer(t *testing.T, cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server run error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for server shutdown")
	}
}

func TestIndexServesHTML(t *testing.T) {
	cfg := Config{Port: 0, SupportHTTP11: true, SupportH2C: true}
	srv, cancel, errCh := startServer(t, cfg)
	defer stopServer(t, cancel, errCh)

	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()

	resp, err := client.Get("http://" + srv.Address() + "/")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Contains(body, []byte("<form")) {
		t.Fatalf("expected form in response, got %s", string(body))
	}
}

func TestUploadUpdatesStats(t *testing.T) {
	cfg := Config{Port: 0, SupportHTTP11: true, SupportH2C: true}
	srv, cancel, errCh := startServer(t, cfg)
	defer stopServer(t, cancel, errCh)

	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()

	payload := []byte("hello world")
	buf, contentType := buildMultipartPayload(t, "file", "test.txt", payload)

	req, err := http.NewRequest(http.MethodPost, "http://"+srv.Address()+"/upload", buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post upload: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	var output struct {
		Bytes      uint64  `json:"bytes"`
		DurationMS float64 `json:"duration_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&output); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if output.Bytes != uint64(len(payload)) {
		t.Fatalf("expected bytes %d, got %d", len(payload), output.Bytes)
	}
	if output.DurationMS <= 0 {
		t.Fatalf("expected positive duration_ms")
	}

	// Allow sampler to capture the update.
	time.Sleep(600 * time.Millisecond)

	snap := srv.Stats().Snapshot()
	if snap.TotalUploads != 1 {
		t.Fatalf("expected total uploads 1, got %d", snap.TotalUploads)
	}
	if snap.TotalBytes != uint64(len(payload)) {
		t.Fatalf("expected total bytes %d, got %d", len(payload), snap.TotalBytes)
	}
	if snap.LastSize != uint64(len(payload)) {
		t.Fatalf("expected last size %d, got %d", len(payload), snap.LastSize)
	}
	if snap.LastDuration <= 0 {
		t.Fatalf("expected positive last duration")
	}

	window := srv.Sampler().Window()
	if window.SampleCount < 1 {
		t.Fatalf("expected window samples, got %d", window.SampleCount)
	}
	if window.Bytes == 0 {
		t.Fatalf("expected window bytes > 0")
	}
}

func TestH2CSupportsUpload(t *testing.T) {
	cfg := Config{Port: 0, SupportHTTP11: true, SupportH2C: true}
	srv, cancel, errCh := startServer(t, cfg)
	defer stopServer(t, cancel, errCh)

	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: 2 * time.Second}
			return dialer.Dial(network, addr)
		},
	}

	client := &http.Client{Timeout: 2 * time.Second, Transport: transport}
	defer client.CloseIdleConnections()
	defer transport.CloseIdleConnections()

	payload := []byte("upload over h2c")
	buf, contentType := buildMultipartPayload(t, "file", "h2c.txt", payload)
	req, err := http.NewRequest(http.MethodPost, "http://"+srv.Address()+"/upload", buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("h2c upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("expected HTTP/2 response, got %s", resp.Proto)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	var output struct {
		Bytes uint64 `json:"bytes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&output); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if output.Bytes != uint64(len(payload)) {
		t.Fatalf("expected bytes %d, got %d", len(payload), output.Bytes)
	}

	snap := srv.Stats().Snapshot()
	if snap.TotalUploads != 1 {
		t.Fatalf("expected total uploads 1, got %d", snap.TotalUploads)
	}
	if snap.TotalBytes != uint64(len(payload)) {
		t.Fatalf("expected total bytes %d, got %d", len(payload), snap.TotalBytes)
	}
}
