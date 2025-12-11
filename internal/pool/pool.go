package pool

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/upstream"
)

type Pool struct {
	cfg      *config.Config
	dialer   *upstream.Dialer
	counters *metrics.Counters

	sessions []*Session
	next     atomic.Uint64
}

func New(cfg *config.Config, dialer *upstream.Dialer, counters *metrics.Counters) (*Pool, error) {
	if cfg.Pool.PoolSize <= 0 {
		return nil, fmt.Errorf("pool size must be > 0")
	}
	sessions := make([]*Session, 0, cfg.Pool.PoolSize)
	for i := 0; i < cfg.Pool.PoolSize; i++ {
		sessions = append(sessions, NewSession(i, cfg, dialer, counters))
	}
	return &Pool{
		cfg:      cfg,
		dialer:   dialer,
		counters: counters,
		sessions: sessions,
	}, nil
}

func (p *Pool) Start(ctx context.Context) error {
	for _, sess := range p.sessions {
		if err := sess.Start(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pool) Close() {
	for _, sess := range p.sessions {
		sess.Close()
	}
}

func (p *Pool) pickSession() *Session {
	if len(p.sessions) == 1 {
		return p.sessions[0]
	}
	idx := int((p.next.Add(1) - 1) % uint64(len(p.sessions)))
	return p.sessions[idx]
}

func (p *Pool) Sessions() []*Session {
	return p.sessions
}

func (p *Pool) States() []SessionState {
	result := make([]SessionState, 0, len(p.sessions))
	for _, sess := range p.sessions {
		result = append(result, sess.State())
	}
	return result
}

func (p *Pool) NextSession() *Session {
	return p.pickSession()
}

func (p *Pool) Size() int {
	return len(p.sessions)
}
