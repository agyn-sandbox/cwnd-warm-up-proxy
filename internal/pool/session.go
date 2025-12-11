package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/time/rate"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/upstream"
)

type WarmupPhase string

const (
	WarmupPhaseIdle         WarmupPhase = "idle"
	WarmupPhaseContinuous   WarmupPhase = "continuous"
	WarmupPhaseBurst        WarmupPhase = "burst"
	WarmupPhasePaused       WarmupPhase = "paused"
	WarmupPhaseReconnecting WarmupPhase = "reconnecting"
	WarmupPhaseError        WarmupPhase = "error"
)

type SessionState struct {
	ID          int
	Phase       WarmupPhase
	LastError   string
	Paused      bool
	TargetBPS   int
	Connected   bool
	LastUpdated time.Time
}

type Session struct {
	id       int
	cfg      *config.Config
	dialer   *upstream.Dialer
	counters *metrics.Counters

	mu     sync.RWMutex
	conn   net.Conn
	client *http2.ClientConn
	closed bool

	controlCh chan controlMessage
	state     atomic.Value // SessionState

	warmupWG   sync.WaitGroup
	realActive atomic.Int64
}

type controlType int

const (
	controlPause controlType = iota
	controlResume
	controlStop
)

type controlMessage struct {
	typ controlType
}

func NewSession(id int, cfg *config.Config, dialer *upstream.Dialer, counters *metrics.Counters) *Session {
	s := &Session{
		id:        id,
		cfg:       cfg,
		dialer:    dialer,
		counters:  counters,
		controlCh: make(chan controlMessage, 8),
	}
	s.state.Store(SessionState{
		ID:        id,
		Phase:     WarmupPhaseIdle,
		TargetBPS: cfg.PerConnectionTargetBPS(),
	})
	return s
}

func (s *Session) Start(ctx context.Context) error {
	if err := s.reconnect(ctx); err != nil {
		return err
	}
	s.warmupWG.Add(1)
	go s.warmupLoop(ctx)
	return nil
}

func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.controlCh <- controlMessage{typ: controlStop}
	s.warmupWG.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

func (s *Session) State() SessionState {
	state := s.state.Load().(SessionState)
	return state
}

func (s *Session) markState(update func(*SessionState)) {
	current := s.state.Load().(SessionState)
	update(&current)
	current.LastUpdated = time.Now()
	s.state.Store(current)
}

func (s *Session) reconnect(ctx context.Context) error {
	s.markState(func(st *SessionState) {
		st.Phase = WarmupPhaseReconnecting
		st.Paused = false
		st.Connected = false
		st.LastError = ""
	})

	conn, err := s.dialer.Dial(ctx)
	if err != nil {
		s.markState(func(st *SessionState) {
			st.Phase = WarmupPhaseError
			st.LastError = err.Error()
			st.Connected = false
		})
		return err
	}

	s.mu.Lock()
	if s.client != nil {
		s.client.Close()
	}
	if s.conn != nil {
		s.conn.Close()
	}
	s.conn = conn.RawConn
	s.client = conn.Client
	s.mu.Unlock()

	s.markState(func(st *SessionState) {
		st.Phase = WarmupPhaseContinuous
		st.LastError = ""
		st.Connected = true
	})
	return nil
}

func (s *Session) warmupLoop(ctx context.Context) {
	defer s.warmupWG.Done()
	for {
		if s.isClosed() {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		client := s.currentClient()
		if client == nil {
			if err := s.reconnect(ctx); err != nil {
				select {
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					return
				}
				continue
			}
			client = s.currentClient()
			if client == nil {
				continue
			}
		}

		if err := s.runWarmupOnce(ctx, client); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			s.markState(func(st *SessionState) {
				st.Phase = WarmupPhaseError
				st.LastError = err.Error()
				st.Connected = false
			})
			s.reconnect(ctx)
			continue
		}
	}
}

func (s *Session) currentClient() *http2.ClientConn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

func (s *Session) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

func (s *Session) ensureClient(ctx context.Context) (*http2.ClientConn, error) {
	client := s.currentClient()
	if client != nil {
		return client, nil
	}
	if err := s.reconnect(ctx); err != nil {
		return nil, err
	}
	client = s.currentClient()
	if client == nil {
		return nil, errors.New("no http2 client after reconnect")
	}
	return client, nil
}

func (s *Session) beginReal() {
	if s.realActive.Add(1) == 1 {
		s.trySend(controlMessage{typ: controlPause})
	}
}

func (s *Session) endReal() {
	if s.realActive.Add(-1) == 0 {
		s.trySend(controlMessage{typ: controlResume})
	}
}

func (s *Session) trySend(msg controlMessage) {
	select {
	case s.controlCh <- msg:
	default:
	}
}

func (s *Session) Forward(ctx context.Context, req *http.Request) (*http.Response, error) {
	client, err := s.ensureClient(ctx)
	if err != nil {
		return nil, err
	}
	s.beginReal()
	defer s.endReal()

	resp, err := client.RoundTrip(req)
	if err != nil {
		s.markState(func(st *SessionState) {
			st.LastError = err.Error()
			st.Connected = false
		})
		_ = s.reconnect(ctx)
		return nil, err
	}

	s.markState(func(st *SessionState) {
		st.LastError = ""
		st.Connected = true
	})
	return resp, nil
}

type warmupResult struct {
	err error
}

func (s *Session) runWarmupOnce(ctx context.Context, client *http2.ClientConn) error {
	pipeR, pipeW := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, s.cfg.WarmupMethod(), s.cfg.WarmupURL(), pipeR)
	if err != nil {
		pipeR.Close()
		pipeW.Close()
		return fmt.Errorf("new warmup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	for k, v := range s.cfg.WarmupHeaders() {
		req.Header.Set(k, v)
	}

	resultCh := make(chan warmupResult, 1)

	go func() {
		resp, err := client.RoundTrip(req)
		if err != nil {
			resultCh <- warmupResult{err: err}
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		resultCh <- warmupResult{err: nil}
	}()

	err = s.streamWarmup(ctx, pipeW)
	if err != nil {
		pipeW.CloseWithError(err)
	} else {
		pipeW.Close()
	}

	res := <-resultCh
	if err != nil {
		return err
	}
	return res.err
}

func (s *Session) streamWarmup(ctx context.Context, pipeW *io.PipeWriter) error {
	targetBytesPerSecond := s.cfg.PerConnectionTargetBPS() / 8
	if targetBytesPerSecond <= 0 {
		targetBytesPerSecond = 1
	}
	continuousLimiter := rate.NewLimiter(rate.Limit(targetBytesPerSecond), warmupChunkSize)
	dwell := time.Duration(s.cfg.Pool.PerConnectionDwellMS) * time.Millisecond
	interval := time.Duration(s.cfg.Pool.WarmUpIntervalMS) * time.Millisecond
	burstSize := s.cfg.Pool.WarmUpSizeBytes

	if burstSize <= 0 {
		return errors.New("warm_up_size_bytes must be > 0")
	}
	if interval <= 0 {
		return errors.New("warm_up_interval_ms must be > 0")
	}

	var (
		paused     atomic.Bool
		dwellTimer <-chan time.Time
		ticker     *time.Ticker
	)
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()

	paused.Store(false)

	chunkBuf := make([]byte, warmupChunkSize)
	for i := range chunkBuf {
		chunkBuf[i] = 0xff
	}

	state := WarmupPhaseContinuous
	if dwell > 0 {
		dwellTimer = time.After(dwell)
		s.markState(func(st *SessionState) {
			st.Phase = state
			st.Paused = false
		})
	} else {
		state = WarmupPhaseBurst
		ticker = time.NewTicker(interval)
		s.markState(func(st *SessionState) {
			st.Phase = state
			st.Paused = false
		})
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-s.controlCh:
			switch msg.typ {
			case controlPause:
				paused.Store(true)
				s.markState(func(st *SessionState) {
					st.Phase = WarmupPhasePaused
					st.Paused = true
				})
			case controlResume:
				paused.Store(false)
				// Restore phase depending on state
				s.markState(func(st *SessionState) {
					st.Paused = false
					st.Phase = state
				})
			case controlStop:
				return nil
			}
			continue
		default:
		}

		if paused.Load() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case msg := <-s.controlCh:
				switch msg.typ {
				case controlPause:
					// already paused
				case controlResume:
					paused.Store(false)
					s.markState(func(st *SessionState) {
						st.Paused = false
						st.Phase = state
					})
				case controlStop:
					return nil
				}
			}
			continue
		}

		if state == WarmupPhaseContinuous {
			if dwellTimer != nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case msg := <-s.controlCh:
					switch msg.typ {
					case controlPause:
						paused.Store(true)
						s.markState(func(st *SessionState) {
							st.Phase = WarmupPhasePaused
							st.Paused = true
						})
						continue
					case controlResume:
						continue
					case controlStop:
						return nil
					}
				case <-dwellTimer:
					dwellTimer = nil
					state = WarmupPhaseBurst
					s.markState(func(st *SessionState) {
						st.Phase = state
					})
					ticker = time.NewTicker(interval)
					continue
				default:
				}
			}

			if err := continuousLimiter.WaitN(ctx, warmupChunkSize); err != nil {
				return err
			}
			if paused.Load() {
				continue
			}
			if err := writeFull(pipeW, chunkBuf); err != nil {
				return err
			}
			s.counters.AddDummyTx(uint64(warmupChunkSize))
		} else {
			if ticker == nil {
				ticker = time.NewTicker(interval)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case msg := <-s.controlCh:
				switch msg.typ {
				case controlPause:
					paused.Store(true)
					s.markState(func(st *SessionState) {
						st.Phase = WarmupPhasePaused
						st.Paused = true
					})
				case controlResume:
					// already active
				case controlStop:
					ticker.Stop()
					return nil
				}
			case <-ticker.C:
				remaining := burstSize
				for remaining > 0 {
					if paused.Load() {
						break
					}
					chunk := warmupChunkSize
					if remaining < chunk {
						chunk = remaining
					}
					if err := writeFull(pipeW, chunkBuf[:chunk]); err != nil {
						ticker.Stop()
						return err
					}
					remaining -= chunk
					s.counters.AddDummyTx(uint64(chunk))
				}
			}
		}
	}
}

const warmupChunkSize = 16 * 1024

func writeFull(w io.Writer, data []byte) error {
	total := 0
	for total < len(data) {
		n, err := w.Write(data[total:])
		if err != nil {
			return err
		}
		total += n
	}
	return nil
}
