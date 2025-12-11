package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/pool"
)

type Handler struct {
	cfg      *config.Config
	pool     *pool.Pool
	counters *metrics.Counters
}

func NewHandler(cfg *config.Config, pool *pool.Pool, counters *metrics.Counters) *Handler {
	return &Handler{cfg: cfg, pool: pool, counters: counters}
}

var hopByHopHeaders = func() map[string]struct{} {
	keys := []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	}
	canonical := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		canonical[http.CanonicalHeaderKey(k)] = struct{}{}
	}
	return canonical
}()

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var bodyTemplate []byte
	var err error

	idempotent := isIdempotentMethod(r.Method)

	if idempotent && r.Body != nil {
		bodyTemplate, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()
	}

	if !idempotent && r.Body != nil {
		defer r.Body.Close()
	}

	attempts := 1
	if idempotent {
		attempts = 2
	}

	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		sess := h.pool.NextSession()
		if sess == nil {
			http.Error(w, "no available sessions", http.StatusServiceUnavailable)
			return
		}

		bodyReader, ok := h.bodyForAttempt(r, bodyTemplate, attempt)
		if !ok {
			break
		}

		upstreamReq, err := h.buildUpstreamRequest(ctx, r, bodyReader)
		if err != nil {
			http.Error(w, "build upstream request failed", http.StatusBadGateway)
			return
		}

		resp, err := sess.Forward(ctx, upstreamReq)
		if err != nil {
			if upstreamReq.Body != nil {
				upstreamReq.Body.Close()
			}
			lastErr = err
			if !idempotent {
				break
			}
			continue
		}

		writeErr := h.writeResponse(w, resp)
		resp.Body.Close()
		if writeErr != nil {
			lastErr = writeErr
			break
		}
		return
	}

	if lastErr == nil {
		lastErr = errors.New("upstream request failed")
	}
	http.Error(w, "upstream error: "+lastErr.Error(), http.StatusBadGateway)
}

func (h *Handler) bodyForAttempt(r *http.Request, template []byte, attempt int) (io.ReadCloser, bool) {
	if template != nil {
		return io.NopCloser(bytes.NewReader(template)), true
	}
	if attempt == 1 {
		if r.Body == nil {
			return http.NoBody, true
		}
		return r.Body, true
	}
	if r.Body == nil {
		return http.NoBody, true
	}
	return nil, false
}

func (h *Handler) buildUpstreamRequest(ctx context.Context, original *http.Request, body io.ReadCloser) (*http.Request, error) {
	targetURL := &url.URL{
		Scheme:   h.cfg.TargetScheme(),
		Host:     h.cfg.TargetAuthority(),
		Path:     original.URL.Path,
		RawPath:  original.URL.RawPath,
		RawQuery: original.URL.RawQuery,
		Fragment: "",
	}

	req, err := http.NewRequestWithContext(ctx, original.Method, targetURL.String(), nil)
	if err != nil {
		return nil, err
	}

	req.Header = cloneHeaders(original.Header)
	stripHopByHop(req.Header)

	if body != nil {
		req.Body = &countingReadCloser{ReadCloser: body, onRead: h.counters.AddRealTx}
	} else {
		req.Body = http.NoBody
	}

	req.ContentLength = original.ContentLength
	req.Host = h.cfg.TargetAuthority()

	return req, nil
}

func (h *Handler) writeResponse(w http.ResponseWriter, resp *http.Response) error {
	copyHeaders(w.Header(), resp.Header)
	stripHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)

	if resp.Body == nil {
		return nil
	}

	writer := &countingWriter{Writer: w, onWrite: h.counters.AddRealRx}
	_, err := io.Copy(writer, resp.Body)
	return err
}

func cloneHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	return dst
}

func stripHopByHop(h http.Header) {
	if h == nil {
		return
	}
	if values := h.Values("Connection"); len(values) > 0 {
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				key := http.CanonicalHeaderKey(strings.TrimSpace(token))
				if key == "" {
					continue
				}
				if strings.EqualFold(key, "Te") {
					filterTE(h)
					continue
				}
				h.Del(key)
			}
		}
		h.Del("Connection")
	}
	for k := range hopByHopHeaders {
		if k == "Te" {
			filterTE(h)
			continue
		}
		h.Del(k)
	}
	filterTE(h)
}

type countingReadCloser struct {
	io.ReadCloser
	onRead func(uint64)
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 && c.onRead != nil {
		c.onRead(uint64(n))
	}
	return n, err
}

type countingWriter struct {
	io.Writer
	onWrite func(uint64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.Writer.Write(p)
	if n > 0 && c.onWrite != nil {
		c.onWrite(uint64(n))
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func copyHeaders(dst, src http.Header) {
	for k := range dst {
		dst.Del(k)
	}
	for k, vv := range src {
		if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isIdempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodDelete, http.MethodTrace, http.MethodPut:
		return true
	default:
		return false
	}
}

func filterTE(h http.Header) {
	values := h.Values("Te")
	if len(values) == 0 {
		return
	}
	keepTrailers := false
	for _, v := range values {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "trailers") {
				keepTrailers = true
			}
		}
	}
	if keepTrailers {
		h.Set("Te", "trailers")
	} else {
		h.Del("Te")
	}
}
