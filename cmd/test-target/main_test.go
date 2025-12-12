package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUploadEndpoint(t *testing.T) {
	server := httptest.NewServer(newMux(nil))
	defer server.Close()
	payload := bytes.Repeat([]byte("a"), 1024)
	resp, err := http.Post(server.URL+"/upload", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %s", resp.Status)
	}
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.BytesReceived != int64(len(payload)) {
		t.Fatalf("bytes received mismatch: got %d want %d", out.BytesReceived, len(payload))
	}
	if out.ServerTimeMS < 0 {
		t.Fatalf("unexpected server time: %d", out.ServerTimeMS)
	}
}

func TestHealthEndpoint(t *testing.T) {
	server := httptest.NewServer(newMux(nil))
	defer server.Close()
	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %s", resp.Status)
	}
}
