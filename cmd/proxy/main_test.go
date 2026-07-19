package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pablomat/polygon-rpc-proxy/internal/config"
	"github.com/pablomat/polygon-rpc-proxy/internal/proxy"
)

func testServer(t *testing.T) *http.Server {
	t.Helper()
	cfg := config.Config{
		UpstreamURL:     "http://upstream.invalid",
		ListenAddr:      "127.0.0.1:0",
		UpstreamTimeout: time.Second,
		MaxBodyBytes:    1024,
		MaxInFlight:     8,
		ShutdownTimeout: time.Second,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := proxy.New(cfg, &http.Client{}, log, proxy.NewMetrics(prometheus.NewRegistry()))
	return newServer(cfg, h)
}

func TestNewServer_WiresRoutesAndTimeouts(t *testing.T) {
	srv := testServer(t)
	if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 6*time.Second ||
		srv.WriteTimeout != 12*time.Second || srv.IdleTimeout != 120*time.Second {
		t.Errorf("server timeouts = header %v, read %v, write %v, idle %v",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}

	// The router should serve the operational endpoints.
	for _, path := range []string{"/healthz", "/metrics"} {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
	}
}

func TestNewUpstreamClient_HasPooledTransport(t *testing.T) {
	c := newUpstreamClient(17)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.MaxIdleConnsPerHost == 0 {
		t.Error("connection pool not configured")
	}
	if tr.MaxConnsPerHost != 17 {
		t.Errorf("MaxConnsPerHost = %d, want 17", tr.MaxConnsPerHost)
	}
	if !tr.DisableCompression {
		t.Error("transport may alter upstream compression")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("transport does not attempt HTTP/2")
	}
	if c.Timeout != 0 {
		t.Errorf("client Timeout = %v, want request-context deadlines", c.Timeout)
	}
}

func TestUpstreamLogTarget_OmitsCredentialsPathAndQuery(t *testing.T) {
	scheme, host := upstreamLogTarget("https://user:secret@example.invalid:8443/private?token=secret")
	if scheme != "https" || host != "example.invalid:8443" {
		t.Errorf("target = (%q, %q), want sanitized scheme and host", scheme, host)
	}
}

func TestRunHealthcheck(t *testing.T) {
	// Healthy: stand up the real router and point the probe at it.
	srv := testServer(t)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	t.Setenv("LISTEN_ADDR", ts.Listener.Addr().String())
	if code := runHealthcheck(); code != 0 {
		t.Errorf("healthcheck against live server = %d, want 0", code)
	}

	// Unhealthy: nothing listening on this port.
	t.Setenv("LISTEN_ADDR", "127.0.0.1:1") // unassigned/privileged, connection refused
	if code := runHealthcheck(); code != 1 {
		t.Errorf("healthcheck against dead server = %d, want 1", code)
	}
}

func TestRun_ForcesCloseWhenDrainWindowElapses(t *testing.T) {
	// A request that outlives SHUTDOWN_TIMEOUT must not turn a routine
	// deployment into a fatal exit: run() closes the remaining connections
	// and returns nil.
	inFlight := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(inFlight)
		select { // hold the request well past the drain window
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	t.Setenv("LISTEN_ADDR", addr)
	t.Setenv("UPSTREAM_URL", slow.URL)
	t.Setenv("UPSTREAM_TIMEOUT", "5s")
	t.Setenv("SHUTDOWN_TIMEOUT", "100ms")

	done := make(chan error, 1)
	go func() {
		done <- run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Put a request in flight, then signal shutdown while it is stuck.
	go func() {
		resp, err := http.Post("http://"+addr+"/", "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the upstream")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v after an overrun drain, want nil (forced close)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after the drain window elapsed")
	}
}

func TestRun_DrainsAndExitsCleanlyOnSIGTERM(t *testing.T) {
	// Reserve a free port, release it, and hand it to run() via the env.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	t.Setenv("LISTEN_ADDR", addr)
	t.Setenv("UPSTREAM_URL", "http://upstream.invalid")

	done := make(chan error, 1)
	go func() {
		done <- run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	// Wait until the server answers, then signal shutdown.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v after SIGTERM, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after SIGTERM")
	}
}
