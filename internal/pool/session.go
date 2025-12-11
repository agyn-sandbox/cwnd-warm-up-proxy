package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
	counters *metrics.Counters

	mu     sync.RWMutex
	conn   net.Conn
	client sessionClient
	closed bool

	controlCh chan controlMessage
	state     atomic.Value // SessionState

	warmupWG   sync.WaitGroup
	realActive atomic.Int64
	burstReady atomic.Bool

	dial func(context.Context) (*dialResult, error)
}

type sessionClient interface {
	RoundTrip(*http.Request) (*http.Response, error)
	Close() error
}

type dialResult struct {
	rawConn net.Conn
	client  sessionClient
}

var (
	errWarmupStream = errors.New("warmup stream failure")
	errWarmupConn   = errors.New("warmup connection failure")
)

const warmupRetryBackoff = 500 * time.Millisecond

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
		counters:  counters,
		controlCh: make(chan controlMessage, 8),
	}
	if dialer != nil {
		s.dial = func(ctx context.Context) (*dialResult, error) {
			conn, err := dialer.Dial(ctx)
			if err != nil {
				return nil, err
			}
			return &dialResult{rawConn: conn.RawConn, client: conn.Client}, nil
		}
	} else {
		s.dial = func(context.Context) (*dialResult, error) {
			return nil, errors.New("dial function not configured")
		}
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

	result, err := s.dial(ctx)
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
	s.conn = result.rawConn
	s.client = result.client
	s.burstReady.Store(false)
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
			if s.handleWarmupError(ctx, err) {
				return
			}
			continue
		}
	}
}

func (s *Session) currentClient() sessionClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

func (s *Session) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

func (s *Session) ensureClient(ctx context.Context) (sessionClient, error) {
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

func (s *Session) runWarmupOnce(ctx context.Context, client sessionClient) error {
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
	if s.isClosed() {
		return nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		classification := wrapWarmupError(err)
		if res.err != nil && !errors.Is(res.err, context.Canceled) && !errors.Is(res.err, context.DeadlineExceeded) && isConnectionError(res.err) {
			classification = wrapWarmupError(res.err)
		}
		return classification
	}
	if res.err != nil {
		if errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded) {
			return res.err
		}
		return wrapWarmupError(res.err)
	}
	return nil
}

func (s *Session) streamWarmup(ctx context.Context, pipeW *io.PipeWriter) error {
	targetBytesPerSecond := int(math.Ceil(float64(s.cfg.PerConnectionTargetBPS()) / 8.0))
	if targetBytesPerSecond <= 0 {
		return errors.New("derived per-connection byte rate must be > 0")
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
		dwellTimer *time.Timer
		dwellCh    <-chan time.Time
		ticker     *time.Ticker
	)
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
		if dwellTimer != nil {
			dwellTimer.Stop()
		}
	}()

	paused.Store(false)

	chunkBuf := make([]byte, warmupChunkSize)
	for i := range chunkBuf {
		chunkBuf[i] = 0xff
	}

	state := WarmupPhaseContinuous
	if s.burstReady.Load() || dwell <= 0 {
		state = WarmupPhaseBurst
		ticker = time.NewTicker(interval)
		s.burstReady.Store(true)
		s.markState(func(st *SessionState) {
			st.Phase = state
			st.Paused = false
		})
	} else {
		dwellTimer = time.NewTimer(dwell)
		dwellCh = dwellTimer.C
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
			if dwellCh != nil {
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
				case <-dwellCh:
					dwellCh = nil
					if dwellTimer != nil {
						dwellTimer.Stop()
						dwellTimer = nil
					}
					state = WarmupPhaseBurst
					s.burstReady.Store(true)
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

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Session) handleWarmupError(ctx context.Context, err error) bool {
	switch {
	case errors.Is(err, errWarmupStream):
		s.markState(func(st *SessionState) {
			if s.burstReady.Load() {
				st.Phase = WarmupPhaseBurst
			} else {
				st.Phase = WarmupPhaseContinuous
			}
			st.LastError = err.Error()
			st.Paused = false
			st.Connected = true
		})
		if !sleepWithContext(ctx, warmupRetryBackoff) {
			return true
		}
		return false
	case errors.Is(err, errWarmupConn):
		s.markState(func(st *SessionState) {
			st.Phase = WarmupPhaseError
			st.LastError = err.Error()
			st.Connected = false
		})
		if recErr := s.reconnect(ctx); recErr != nil {
			if errors.Is(recErr, context.Canceled) || errors.Is(recErr, context.DeadlineExceeded) {
				return true
			}
		}
		return false
	default:
		s.markState(func(st *SessionState) {
			st.Phase = WarmupPhaseError
			st.LastError = err.Error()
			st.Connected = false
		})
		if recErr := s.reconnect(ctx); recErr != nil {
			if errors.Is(recErr, context.Canceled) || errors.Is(recErr, context.DeadlineExceeded) {
				return true
			}
			if !sleepWithContext(ctx, 500*time.Millisecond) {
				return true
			}
		}
		return false
	}
}

func wrapWarmupError(err error) error {
	if err == nil {
		return nil
	}
	if isStreamError(err) {
		return fmt.Errorf("%w: %w", errWarmupStream, err)
	}
	if isConnectionError(err) {
		return fmt.Errorf("%w: %w", errWarmupConn, err)
	}
	return fmt.Errorf("%w: %w", errWarmupStream, err)
}

func isStreamError(err error) bool {
	if err == nil {
		return false
	}
	var se http2.StreamError
	if errors.As(err, &se) {
		return true
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	return false
}

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if isStreamError(err) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var goAway *http2.GoAwayError
	if errors.As(err, &goAway) {
		return true
	}
	var connErr http2.ConnectionError
	if errors.As(err, &connErr) {
		return true
	}
	return false
}
