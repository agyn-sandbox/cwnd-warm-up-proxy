package proxy

import (
	"net/http"
	"testing"
)

func TestStripHopByHopHeaders(t *testing.T) {
	header := http.Header{}
	header.Add("Connection", "keep-alive, X-Custom")
	header.Add("Keep-Alive", "timeout=5")
	header.Add("X-Custom", "true")
	header.Add("Transfer-Encoding", "chunked")
	header.Add("TE", "trailers, compress")

	stripHopByHop(header)

	if header.Get("Connection") != "" {
		t.Fatalf("expected Connection header stripped, got %q", header.Get("Connection"))
	}
	if header.Get("Keep-Alive") != "" {
		t.Fatalf("expected Keep-Alive stripped")
	}
	if header.Get("X-Custom") != "" {
		t.Fatalf("expected Connection token header stripped")
	}
	if header.Get("Transfer-Encoding") != "" {
		t.Fatalf("expected Transfer-Encoding stripped")
	}
	if got := header.Get("Te"); got != "trailers" {
		t.Fatalf("expected TE reduced to trailers, got %q", got)
	}
}
