package config

import (
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// t.Setenv guarantees a clean, isolated environment per test.
	for _, k := range []string{"UPSTREAM_URL", "LISTEN_ADDR", "UPSTREAM_TIMEOUT", "MAX_BODY_BYTES", "MAX_IN_FLIGHT", "SHUTDOWN_TIMEOUT"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with defaults: %v", err)
	}
	if cfg.UpstreamURL != defaultUpstreamURL {
		t.Errorf("UpstreamURL = %q, want %q", cfg.UpstreamURL, defaultUpstreamURL)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
	if cfg.MaxBodyBytes != defaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d, want %d", cfg.MaxBodyBytes, defaultMaxBodyBytes)
	}
	if cfg.MaxInFlight != defaultMaxInFlight {
		t.Errorf("MaxInFlight = %d, want %d", cfg.MaxInFlight, defaultMaxInFlight)
	}
	if cfg.UpstreamTimeout != defaultUpstreamTimeout {
		t.Errorf("UpstreamTimeout = %v, want %v", cfg.UpstreamTimeout, defaultUpstreamTimeout)
	}
	if cfg.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, defaultShutdownTimeout)
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("UPSTREAM_URL", "https://example.invalid/rpc")
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("UPSTREAM_TIMEOUT", "5s")
	t.Setenv("MAX_BODY_BYTES", "2048")
	t.Setenv("MAX_IN_FLIGHT", "7")
	t.Setenv("SHUTDOWN_TIMEOUT", "3s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with overrides: %v", err)
	}
	if cfg.UpstreamURL != "https://example.invalid/rpc" {
		t.Errorf("UpstreamURL = %q", cfg.UpstreamURL)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.UpstreamTimeout != 5*time.Second {
		t.Errorf("UpstreamTimeout = %v", cfg.UpstreamTimeout)
	}
	if cfg.MaxBodyBytes != 2048 {
		t.Errorf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
	if cfg.MaxInFlight != 7 {
		t.Errorf("MaxInFlight = %d", cfg.MaxInFlight)
	}
	if cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
}

func TestLoad_Invalid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"bad url scheme", map[string]string{"UPSTREAM_URL": "ftp://nope"}},
		{"url without host", map[string]string{"UPSTREAM_URL": "https://"}},
		{"url without hostname", map[string]string{"UPSTREAM_URL": "https://:443"}},
		{"bad duration", map[string]string{"UPSTREAM_TIMEOUT": "notaduration"}},
		{"bad bytes", map[string]string{"MAX_BODY_BYTES": "lots"}},
		{"bytes with trailing garbage", map[string]string{"MAX_BODY_BYTES": "1MB"}},
		{"bad in-flight limit", map[string]string{"MAX_IN_FLIGHT": "many"}},
		{"zero bytes", map[string]string{"MAX_BODY_BYTES": "0"}},
		{"zero in-flight limit", map[string]string{"MAX_IN_FLIGHT": "0"}},
		{"negative in-flight limit", map[string]string{"MAX_IN_FLIGHT": "-1"}},
		{"zero upstream timeout", map[string]string{"UPSTREAM_TIMEOUT": "0s"}},
		{"negative shutdown timeout", map[string]string{"SHUTDOWN_TIMEOUT": "-1s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("Load(%v) = nil error, want error", tt.env)
			}
		})
	}
}
