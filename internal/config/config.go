// Package config loads proxy configuration from the environment.
//
// Every value has a default so the proxy runs with zero configuration while
// remaining fully overridable for containerized deployments.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config holds the runtime configuration for the proxy. It is populated once
// at startup and then treated as immutable.
type Config struct {
	// UpstreamURL is the blockchain RPC endpoint requests are forwarded to.
	UpstreamURL string
	// ListenAddr is the host:port the proxy's HTTP server binds to.
	ListenAddr string
	// UpstreamTimeout bounds a single upstream request (dial + response).
	UpstreamTimeout time.Duration
	// MaxBodyBytes caps the accepted request body size to protect the proxy
	// and the upstream from oversized payloads.
	MaxBodyBytes int64
	// MaxInFlight caps concurrent RPC and preflight requests per process.
	MaxInFlight int
	// ShutdownTimeout bounds how long graceful shutdown waits for in-flight
	// requests to drain.
	ShutdownTimeout time.Duration
}

// Default values used when the corresponding environment variable is unset.
const (
	defaultUpstreamURL     = "https://polygon.drpc.org"
	defaultListenAddr      = ":8080"
	defaultUpstreamTimeout = 30 * time.Second
	defaultMaxBodyBytes    = 1 << 20 // 1 MiB
	defaultMaxInFlight     = 128
	defaultShutdownTimeout = 10 * time.Second
)

// Load reads configuration from the environment, applying defaults for any
// unset variable and validating the result. It returns a descriptive error
// (wrapping the underlying cause) if any value is malformed.
func Load() (Config, error) {
	cfg := Config{
		UpstreamURL: getString("UPSTREAM_URL", defaultUpstreamURL),
		ListenAddr:  getString("LISTEN_ADDR", defaultListenAddr),
	}

	var err error
	if cfg.UpstreamTimeout, err = getDuration("UPSTREAM_TIMEOUT", defaultUpstreamTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxBodyBytes, err = getBytes("MAX_BODY_BYTES", defaultMaxBodyBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxInFlight, err = getInt("MAX_IN_FLIGHT", defaultMaxInFlight); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = getDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return Config{}, err
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate ensures the loaded configuration is internally consistent.
func (c Config) validate() error {
	u, err := url.Parse(c.UpstreamURL)
	if err != nil {
		return fmt.Errorf("parse UPSTREAM_URL %q: %w", c.UpstreamURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("UPSTREAM_URL must be http or https, got %q", c.UpstreamURL)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("UPSTREAM_URL must include a host, got %q", c.UpstreamURL)
	}
	if c.ListenAddr == "" {
		return errors.New("LISTEN_ADDR must not be empty")
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("MAX_BODY_BYTES must be positive, got %d", c.MaxBodyBytes)
	}
	if c.MaxInFlight <= 0 {
		return fmt.Errorf("MAX_IN_FLIGHT must be positive, got %d", c.MaxInFlight)
	}
	if c.UpstreamTimeout <= 0 {
		return fmt.Errorf("UPSTREAM_TIMEOUT must be positive, got %s", c.UpstreamTimeout)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("SHUTDOWN_TIMEOUT must be positive, got %s", c.ShutdownTimeout)
	}
	return nil
}

func getString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", key, v, err)
	}
	return d, nil
}

func getBytes(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", key, v, err)
	}
	return n, nil
}

func getInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", key, v, err)
	}
	return n, nil
}
