package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

type Target struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	TLS      bool   `json:"tls"`
}

type Pool struct {
	PoolSize             int               `json:"pool_size"`
	BandwidthMbps        float64           `json:"bandwidth_mbps"`
	WarmUpIntervalMS     int               `json:"warm_up_interval_ms"`
	WarmUpSizeBytes      int               `json:"warm_up_size_bytes"`
	WarmupPath           string            `json:"warmup_path"`
	WarmupMethod         string            `json:"warmup_method"`
	WarmUpHeaders        map[string]string `json:"warm_up_headers"`
	PerConnectionDwellMS int               `json:"per_connection_dwell_ms"`
}

type Server struct {
	Port           int   `json:"port"`
	SupportHTTP11  *bool `json:"support_http1_1"`
	SupportH2C     *bool `json:"support_h2c"`
	ReadTimeoutMS  int   `json:"read_timeout_ms"`
	WriteTimeoutMS int   `json:"write_timeout_ms"`
	IdleTimeoutMS  int   `json:"idle_timeout_ms"`
}

type Config struct {
	Target Target `json:"target"`
	Pool   Pool   `json:"pool"`
	Server Server `json:"server"`

	perConnectionTargetBPS int
	supportHTTP11          bool
	supportH2C             bool
	readTimeout            time.Duration
	writeTimeout           time.Duration
	idleTimeout            time.Duration
	warmupMethod           string
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validateAndDerive(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validateAndDerive() error {
	if err := c.validateTarget(); err != nil {
		return err
	}
	if err := c.validatePool(); err != nil {
		return err
	}
	if err := c.validateServer(); err != nil {
		return err
	}
	if c.Pool.PoolSize <= 0 {
		return errors.New("pool.pool_size must be > 0")
	}
	if c.Pool.BandwidthMbps <= 0 {
		return errors.New("pool.bandwidth_mbps must be > 0")
	}
	perConn := int((c.Pool.BandwidthMbps * 1_000_000) / float64(c.Pool.PoolSize))
	if perConn <= 0 {
		return errors.New("derived per-connection target bps <= 0")
	}
	c.perConnectionTargetBPS = perConn

	c.warmupMethod = strings.ToUpper(c.Pool.WarmupMethod)
	if c.warmupMethod != httpMethodPost && c.warmupMethod != httpMethodPut {
		return fmt.Errorf("pool.warmup_method must be POST or PUT, got %q", c.Pool.WarmupMethod)
	}
	if c.Pool.WarmupPath == "" {
		return errors.New("pool.warmup_path is required")
	}
	if c.Pool.WarmUpIntervalMS <= 0 {
		return errors.New("pool.warm_up_interval_ms must be > 0")
	}
	if c.Pool.WarmUpSizeBytes <= 0 {
		return errors.New("pool.warm_up_size_bytes must be > 0")
	}
	if c.Pool.PerConnectionDwellMS < 0 {
		return errors.New("pool.per_connection_dwell_ms must be >= 0")
	}

	if len(c.Pool.WarmUpHeaders) > 0 {
		normalized := make(map[string]string, len(c.Pool.WarmUpHeaders))
		for k, v := range c.Pool.WarmUpHeaders {
			key := http.CanonicalHeaderKey(strings.TrimSpace(k))
			if key == "" {
				return errors.New("pool.warm_up_headers cannot contain empty keys")
			}
			normalized[key] = strings.TrimSpace(v)
		}
		c.Pool.WarmUpHeaders = normalized
	}

	c.supportHTTP11 = true
	if c.Server.SupportHTTP11 != nil {
		c.supportHTTP11 = *c.Server.SupportHTTP11
	}
	if c.Server.SupportH2C != nil {
		c.supportH2C = *c.Server.SupportH2C
	} else {
		c.supportH2C = !c.Target.TLS
	}
	if c.Server.Port <= 0 {
		return errors.New("server.port must be > 0")
	}

	c.readTimeout = durationFromMS(c.Server.ReadTimeoutMS)
	c.writeTimeout = durationFromMS(c.Server.WriteTimeoutMS)
	c.idleTimeout = durationFromMS(c.Server.IdleTimeoutMS)

	return nil
}

const (
	httpMethodPost = "POST"
	httpMethodPut  = "PUT"
)

func durationFromMS(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func (c *Config) validateTarget() error {
	if strings.ToLower(c.Target.Protocol) != "http2" {
		return fmt.Errorf("target.protocol must be 'http2', got %q", c.Target.Protocol)
	}
	if c.Target.Host == "" {
		return errors.New("target.host is required")
	}
	if c.Target.Port <= 0 {
		return errors.New("target.port must be > 0")
	}
	return nil
}

func (c *Config) validatePool() error {
	if c.Pool.PoolSize <= 0 {
		return errors.New("pool.pool_size must be > 0")
	}
	return nil
}

func (c *Config) validateServer() error {
	if c.Server.Port <= 0 {
		return errors.New("server.port must be > 0")
	}
	return nil
}

func (c *Config) PerConnectionTargetBPS() int {
	return c.perConnectionTargetBPS
}

func (c *Config) WarmupMethod() string {
	return c.warmupMethod
}

func (c *Config) WarmupHeaders() map[string]string {
	if c.Pool.WarmUpHeaders == nil {
		return map[string]string{}
	}
	return c.Pool.WarmUpHeaders
}

func (c *Config) SupportHTTP11() bool {
	return c.supportHTTP11
}

func (c *Config) SupportH2C() bool {
	return c.supportH2C
}

func (c *Config) ReadTimeout() time.Duration {
	return c.readTimeout
}

func (c *Config) WriteTimeout() time.Duration {
	return c.writeTimeout
}

func (c *Config) IdleTimeout() time.Duration {
	return c.idleTimeout
}

func (c *Config) TargetAuthority() string {
	return fmt.Sprintf("%s:%d", c.Target.Host, c.Target.Port)
}

func (c *Config) TargetScheme() string {
	if c.Target.TLS {
		return "https"
	}
	return "http"
}

func (c *Config) WarmupURL() string {
	return fmt.Sprintf("%s://%s%s", c.TargetScheme(), c.TargetAuthority(), c.WarmupPath())
}

func (c *Config) WarmupPath() string {
	warmPath := c.Pool.WarmupPath
	if warmPath == "" {
		return "/"
	}
	if !strings.HasPrefix(warmPath, "/") {
		warmPath = "/" + warmPath
	}
	return path.Clean(warmPath)
}
