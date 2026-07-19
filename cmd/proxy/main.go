// Command proxy runs the Polygon RPC reverse proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/pablomat/polygon-rpc-proxy/internal/config"
	"github.com/pablomat/polygon-rpc-proxy/internal/proxy"
)

func main() {
	// -healthcheck lets the same static binary act as its own container health
	// probe: it GETs /healthz and exits 0/1. This keeps the runtime image
	// shell-free (distroless) while still supporting a Docker HEALTHCHECK.
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0 (ok) or 1 (fail)")
	flag.Parse()
	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("fatal", slog.Any("err", err))
		os.Exit(1)
	}
}

// runHealthcheck probes the local /healthz endpoint. It reads LISTEN_ADDR the
// same way the server does, so the probe always targets the right port.
func runHealthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: config:", err)
		return 1
	}
	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: listen addr:", err)
		return 1
	}
	if host == "" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: build request:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// An explicit registry (rather than the global default) is what /metrics
	// serves; Go runtime and process collectors are registered alongside the
	// proxy's own.
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	metrics := proxy.NewMetrics(reg)
	srv := newServer(cfg, proxy.New(cfg, newUpstreamClient(cfg.MaxInFlight), log, metrics))

	// Cancel the base context on SIGINT/SIGTERM to trigger graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		upstreamScheme, upstreamHost := upstreamLogTarget(cfg.UpstreamURL)
		log.Info("proxy listening",
			slog.String("addr", cfg.ListenAddr),
			slog.String("upstream_scheme", upstreamScheme),
			slog.String("upstream_host", upstreamHost))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Requests still in flight when the drain window closes are cut;
			// that is the bounded, expected end of a shutdown, not a fault.
			if errors.Is(err, context.DeadlineExceeded) {
				log.Warn("drain window elapsed, closing remaining connections")
				return srv.Close()
			}
			return err
		}
		return nil
	}
}

func upstreamLogTarget(rawURL string) (scheme, host string) {
	u, _ := url.Parse(rawURL) // validated by config.Load before this is called
	return u.Scheme, u.Host
}

// newServer builds the HTTP server with timeouts derived from config. Kept
// separate from run() so it can be constructed and exercised in tests.
func newServer(cfg config.Config, handler *proxy.Handler) *http.Server {
	readTimeout := cfg.UpstreamTimeout + 5*time.Second
	// The write deadline starts after headers are read, before the request body.
	// Budget for the complete body-read window, one upstream round trip, and a
	// small response-write margin so a slow upload cannot consume the RPC slot.
	writeTimeout := readTimeout + cfg.UpstreamTimeout + 5*time.Second
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
	}
}

// newUpstreamClient builds an HTTP client tuned for proxying: connection
// pooling for throughput, a hard cap on concurrent upstream connections, and
// no client-level timeout (per-request deadlines are set via context, which
// cancels correctly mid-flight).
func newUpstreamClient(maxConnsPerHost int) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Setting DialContext disables net/http's automatic HTTP/2; opt back
		// in explicitly, as http.DefaultTransport does.
		ForceAttemptHTTP2: true,
		// Preserve the client's Accept-Encoding and the upstream Content-Encoding
		// without decompression or recompression.
		DisableCompression:  true,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		// Bound total sockets to the upstream so a request flood cannot
		// exhaust file descriptors or the upstream's connection quota.
		MaxConnsPerHost:     maxConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: transport}
}
