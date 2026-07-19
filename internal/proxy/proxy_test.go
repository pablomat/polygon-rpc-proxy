package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pablomat/polygon-rpc-proxy/internal/config"
)

func testConfig(upstream string) config.Config {
	return config.Config{
		UpstreamURL:     upstream,
		ListenAddr:      ":0",
		UpstreamTimeout: time.Second,
		MaxBodyBytes:    1024,
		MaxInFlight:     8,
		ShutdownTimeout: time.Second,
	}
}

func newTestHandler(t *testing.T, upstream string, timeout time.Duration) (http.Handler, *Metrics) {
	t.Helper()
	cfg := testConfig(upstream)
	cfg.UpstreamTimeout = timeout
	return newHandler(t, cfg, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newHandler(t *testing.T, cfg config.Config, client *http.Client, log *slog.Logger) (http.Handler, *Metrics) {
	t.Helper()
	metrics := NewMetrics(prometheus.NewRegistry())
	return New(cfg, client, log, metrics).Routes(), metrics
}

func TestProxyForwardsOpaquePayloadsWithBoundedLabels(t *testing.T) {
	tests := []struct {
		name, body, label string
	}{
		{"single", `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`, "eth_blockNumber"},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}]`, "batch"},
		{"malformed", `{not json`, "unknown"},
		{"reserved operational namespace", `{"jsonrpc":"2.0","id":1,"method":"_invalid"}`, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var received string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				received = string(body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"accepted":true}`)
			}))
			defer upstream.Close()

			h, metrics := newTestHandler(t, upstream.URL, time.Second)
			rec := do(h, http.MethodPost, "/", tt.body)
			if rec.Code != http.StatusOK || received != tt.body {
				t.Errorf("response status = %d, upstream body = %q", rec.Code, received)
			}
			if got := testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues(tt.label, "200")); got != 1 {
				t.Errorf("requests_total{%q,200} = %v", tt.label, got)
			}
		})
	}
}

func TestProxyPreservesApplicationHeadersWithoutAddingDefaults(t *testing.T) {
	received := make(chan http.Header, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/custom")
		w.Header().Set("X-RateLimit-Remaining", "17")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "accepted")
	}))
	defer upstream.Close()

	h, _ := newTestHandler(t, upstream.URL, time.Second)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	want := map[string]string{
		"Authorization": "Bearer token",
		"Origin":        "https://dapp.example",
		"X-Api-Key":     "key",
		"Content-Type":  "application/custom+json",
		"Accept":        "application/json",
	}
	for name, value := range want {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	gotHeaders := <-received
	for name, value := range want {
		if got := gotHeaders.Get(name); got != value {
			t.Errorf("upstream %s = %q, want %q", name, got, value)
		}
	}
	if rec.Code != http.StatusAccepted || rec.Header().Get("Content-Type") != "application/custom" {
		t.Errorf("response = (%d, %q)", rec.Code, rec.Header().Get("Content-Type"))
	}

	do(h, http.MethodPost, "/", `{}`)
	gotHeaders = <-received
	if gotHeaders.Get("Content-Type") != "" || gotHeaders.Get("Accept") != "" {
		t.Errorf("proxy added content negotiation headers: %v", gotHeaders)
	}
}

func TestProxyDoesNotDefaultResponseContentType(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("plain")),
		}, nil
	})
	h, _ := newHandler(t, testConfig("http://upstream.invalid"), &http.Client{Transport: transport}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := do(h, http.MethodPost, "/", `{}`)
	if got := rec.Header().Get("Content-Type"); got == "application/json" {
		t.Errorf("proxy defaulted response Content-Type to %q", got)
	}
}

func TestProxyPinsExactUpstreamAndRejectsNonRootPaths(t *testing.T) {
	var path, rawPath, query string
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		path, rawPath, query = r.URL.Path, r.URL.RawPath, r.URL.RawQuery
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	h, _ := newTestHandler(t, upstream.URL+"/rpc%2Fv1?api-key=server", time.Second)
	rec := do(h, http.MethodPost, "/?api-key=client", `{}`)
	if rec.Code != http.StatusOK || path != "/rpc/v1" || rawPath != "/rpc%2Fv1" || query != "api-key=server" {
		t.Errorf("status/target = %d %q %q ?%s", rec.Code, path, rawPath, query)
	}
	rec = do(h, http.MethodPost, "/elsewhere?api-key=client", `{}`)
	if rec.Code != http.StatusNotFound || calls.Load() != 1 {
		t.Errorf("non-root response = %d, upstream calls = %d", rec.Code, calls.Load())
	}
}

func TestProxyRelaysCORSPreflight(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions || r.Header.Get("Origin") != "https://dapp.example" {
			t.Errorf("preflight = %s, Origin %q", r.Method, r.Header.Get("Origin"))
		}
		w.Header().Set("Access-Control-Allow-Origin", "https://dapp.example")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	h, metrics := newTestHandler(t, upstream.URL, time.Second)
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://dapp.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://dapp.example" {
		t.Errorf("preflight response = %d %v", rec.Code, rec.Header())
	}
	if got := testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("_preflight", "204")); got != 1 {
		t.Errorf("preflight counter = %v", got)
	}
}

func TestProxyRemovesHopByHopHeaders(t *testing.T) {
	var requestHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHeader = r.Header.Clone()
		w.Header().Set("Connection", "X-Upstream-Hop")
		w.Header().Set("X-Upstream-Hop", "remove")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-End-To-End", "preserve")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	h, _ := newTestHandler(t, upstream.URL, time.Second)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Connection", "X-Client-Hop")
	req.Header.Set("X-Client-Hop", "remove")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("X-End-To-End", "preserve")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, name := range []string{"Connection", "X-Client-Hop", "Keep-Alive"} {
		if requestHeader.Get(name) != "" {
			t.Errorf("upstream request retained %s", name)
		}
	}
	for _, name := range []string{"Connection", "X-Upstream-Hop", "Keep-Alive"} {
		if rec.Header().Get(name) != "" {
			t.Errorf("client response retained %s", name)
		}
	}
	if requestHeader.Get("X-End-To-End") != "preserve" || rec.Header().Get("X-End-To-End") != "preserve" {
		t.Error("end-to-end header was not preserved")
	}
}

func TestProxyPreservesCompressionNegotiation(t *testing.T) {
	const payload = `{"result":"0x1"}`
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(zw, payload)
	_ = zw.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer upstream.Close()

	h, _ := newTestHandler(t, upstream.URL, time.Second)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil || string(decoded) != payload {
		t.Errorf("decoded = %q, %v", decoded, err)
	}
}

func TestTransportFailureLogging(t *testing.T) {
	tests := []struct {
		name, cause, body string
		status            int
		transport         http.RoundTripper
	}{
		{
			"unreachable", "unreachable", "upstream unavailable\n", http.StatusBadGateway,
			roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial failed") }),
		},
		{
			"network timeout", "timeout", "upstream timeout\n", http.StatusGatewayTimeout,
			roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, timeoutError{} }),
		},
		{
			"context deadline", "timeout", "upstream timeout\n", http.StatusGatewayTimeout,
			roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig("http://upstream.invalid")
			cfg.UpstreamTimeout = 20 * time.Millisecond
			var logs bytes.Buffer
			h, metrics := newHandler(t, cfg, &http.Client{Transport: tt.transport}, slog.New(slog.NewJSONHandler(&logs, nil)))
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"method":"eth_chainId"}`))
			req.Header.Set(requestIDHeader, "request-failure")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.status || rec.Body.String() != tt.body {
				t.Errorf("response = (%d, %q)", rec.Code, rec.Body.String())
			}
			if got := testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("eth_chainId", strconv.Itoa(tt.status))); got != 1 {
				t.Errorf("actual-method request counter = %v", got)
			}
			if got := testutil.ToFloat64(metrics.UpstreamErrors.WithLabelValues(tt.cause)); got != 1 {
				t.Errorf("upstream error counter = %v", got)
			}
			events := decodeLogs(t, &logs)
			if len(events) != 1 {
				t.Fatalf("log events = %d: %s", len(events), logs.String())
			}
			event := events[0]
			if event["level"] != "ERROR" || event["msg"] != "upstream request failed" ||
				event["request_id"] != "request-failure" || event["method"] != "eth_chainId" ||
				event["cause"] != tt.cause || event["status"] != float64(tt.status) {
				t.Errorf("unexpected transport log: %#v", event)
			}
		})
	}
}

func TestProxyRecordsTruncatedUpstreamResponse(t *testing.T) {
	// Exceed net/http's response buffer so the downstream receives the 200
	// headers and partial body before ReverseProxy aborts the connection.
	partial := strings.Repeat("x", 4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(partial)+10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, partial)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	h, metrics := newHandler(t, testConfig(upstream.URL), &http.Client{}, slog.New(slog.NewJSONHandler(&logs, nil)))
	server := httptest.NewServer(h)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/", strings.NewReader(`{"method":"eth_chainId"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(requestIDHeader, "request-truncated")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) == 0 || len(body) > len(partial) || readErr == nil {
		t.Errorf("truncated response = status %d, body bytes %d/%d, read error %v", resp.StatusCode, len(body), len(partial), readErr)
	}
	if got := testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("eth_chainId", "200")); got != 1 {
		t.Errorf("request counter = %v", got)
	}
	if got := testutil.ToFloat64(metrics.UpstreamErrors.WithLabelValues("response_read")); got != 1 {
		t.Errorf("upstream response-read counter = %v", got)
	}
	if got := testutil.CollectAndCount(metrics.RequestSeconds, "rpc_proxy_request_duration_seconds"); got != 1 {
		t.Errorf("duration series = %d, want 1", got)
	}
	events := decodeLogs(t, &logs)
	if len(events) != 1 || events[0]["level"] != "ERROR" || events[0]["msg"] != "upstream request failed" ||
		events[0]["request_id"] != "request-truncated" || events[0]["cause"] != "response_read" ||
		events[0]["status"] != float64(http.StatusOK) {
		t.Errorf("truncated response logs = %#v", events)
	}
}

func TestProxyLogsDownstreamWriteFailure(t *testing.T) {
	writeErr := errors.New("client connection closed")
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(`{"result":"0x89"}`)),
			ContentLength: 17,
			Request:       r,
		}, nil
	})
	var logs bytes.Buffer
	h, metrics := newHandler(
		t,
		testConfig("http://upstream.invalid"),
		&http.Client{Transport: transport},
		slog.New(slog.NewJSONHandler(&logs, nil)),
	)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"method":"eth_chainId"}`))
	req.Header.Set(requestIDHeader, "request-write-failure")
	h.ServeHTTP(&errorResponseWriter{header: make(http.Header), err: writeErr}, req)

	if got := testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("eth_chainId", "200")); got != 1 {
		t.Errorf("request counter = %v", got)
	}
	if got := testutil.CollectAndCount(metrics.UpstreamErrors); got != 0 {
		t.Errorf("downstream write created %d upstream error series", got)
	}
	events := decodeLogs(t, &logs)
	if len(events) != 1 || events[0]["level"] != "ERROR" || events[0]["msg"] != "response write failed" ||
		events[0]["request_id"] != "request-write-failure" || events[0]["cause"] != "downstream_write" ||
		events[0]["status"] != float64(http.StatusOK) {
		t.Errorf("downstream-write logs = %#v", events)
	}
}

func TestProxyCancellationIsInformationalAndReleasesAdmission(t *testing.T) {
	started := make(chan struct{})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	cfg := testConfig("http://upstream.invalid")
	cfg.MaxInFlight = 1
	var logs bytes.Buffer
	h, metrics := newHandler(t, cfg, &http.Client{Transport: transport}, slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"method":"eth_blockNumber"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	waitFor(t, started, "transport start")
	cancel()
	waitFor(t, done, "canceled handler")

	if rec.Code != http.StatusGatewayTimeout || testutil.ToFloat64(metrics.InFlight) != 0 {
		t.Errorf("status/in-flight = %d/%v", rec.Code, testutil.ToFloat64(metrics.InFlight))
	}
	if got := testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("eth_blockNumber", "504")); got != 1 {
		t.Errorf("actual-method cancellation counter = %v", got)
	}
	events := decodeLogs(t, &logs)
	if len(events) != 1 || events[0]["level"] != "INFO" || events[0]["msg"] != "upstream request canceled" {
		t.Errorf("cancellation logs = %#v", events)
	}
}

func TestProxyGenericLocalErrors(t *testing.T) {
	h, metrics := newTestHandler(t, "http://upstream.invalid", time.Second)
	tests := []struct {
		name, method, body, wantBody string
		status                       int
	}{
		{"method", http.MethodGet, "", "method not allowed\n", http.StatusMethodNotAllowed},
		{"oversized post", http.MethodPost, strings.Repeat("x", 1025), "request body too large\n", http.StatusRequestEntityTooLarge},
		{"oversized options", http.MethodOptions, strings.Repeat("x", 1025), "request body too large\n", http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(h, tt.method, "/", tt.body)
			if rec.Code != tt.status || rec.Body.String() != tt.wantBody {
				t.Errorf("response = (%d, %q)", rec.Code, rec.Body.String())
			}
			if rec.Header().Get(requestIDHeader) == "" {
				t.Error("generic response has no request ID")
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/", errorReader{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "cannot read request body\n" {
		t.Errorf("read failure = (%d, %q)", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("_invalid", "400")); got != 1 {
		t.Errorf("invalid read counter = %v", got)
	}
	if got := testutil.CollectAndCount(metrics.RequestSeconds, "rpc_proxy_request_duration_seconds"); got != 1 {
		t.Errorf("local-error duration series = %d, want 1", got)
	}
}

func TestAdmissionLimit(t *testing.T) {
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			once.Do(func() { close(reached) })
			<-release
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	cfg := testConfig(upstream.URL)
	cfg.MaxInFlight = 1
	h, metrics := newHandler(t, cfg, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- do(h, http.MethodPost, "/", `{"method":"eth_chainId"}`) }()
	waitFor(t, reached, "first upstream request")
	if got := testutil.ToFloat64(metrics.InFlight); got != 1 {
		t.Fatalf("in-flight gauge = %v, want 1", got)
	}

	probe := &readProbe{}
	req := httptest.NewRequest(http.MethodPost, "/", probe)
	req.Header.Set(requestIDHeader, "request-overload")
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		secondDone <- rec
	}()
	var overloaded *httptest.ResponseRecorder
	select {
	case overloaded = <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("overloaded request waited for capacity")
	}
	if overloaded.Code != http.StatusServiceUnavailable || overloaded.Header().Get("Retry-After") != "1" ||
		overloaded.Header().Get(requestIDHeader) != "request-overload" || probe.reads.Load() != 0 {
		t.Errorf("overload response = %d %v, body reads = %d", overloaded.Code, overloaded.Header(), probe.reads.Load())
	}
	if calls.Load() != 1 || testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("_overloaded", "503")) != 1 {
		t.Errorf("calls/overload counter = %d/%v", calls.Load(), testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("_overloaded", "503")))
	}
	if rec := do(h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("health under saturation = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/metrics", ""); rec.Code != http.StatusOK {
		t.Errorf("metrics under saturation = %d", rec.Code)
	}
	if got := testutil.ToFloat64(metrics.InFlight); got != 1 {
		t.Errorf("operational routes changed RPC gauge to %v", got)
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case rec := <-firstDone:
		if rec.Code != http.StatusOK {
			t.Errorf("first response = %d", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish")
	}
	if got := testutil.ToFloat64(metrics.InFlight); got != 0 {
		t.Errorf("released gauge = %v", got)
	}
	if rec := do(h, http.MethodPost, "/", `{"method":"eth_chainId"}`); rec.Code != http.StatusOK || calls.Load() != 2 {
		t.Errorf("post-release response/calls = %d/%d", rec.Code, calls.Load())
	}
}

func TestMetricsHasIndependentSingleScrapeLimit(t *testing.T) {
	cfg := testConfig("http://upstream.invalid")
	cfg.MaxInFlight = 1
	h, metrics := newHandler(t, cfg, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	collector := newBlockingCollector()
	metrics.registry.MustRegister(collector)
	t.Cleanup(collector.release)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- do(h, http.MethodGet, "/metrics", "") }()
	waitFor(t, collector.entered, "first metrics scrape")
	second := do(h, http.MethodGet, "/metrics", "")
	if second.Code != http.StatusServiceUnavailable {
		t.Errorf("concurrent metrics scrape = %d, want 503", second.Code)
	}
	if got := testutil.ToFloat64(metrics.InFlight); got != 0 {
		t.Errorf("metrics scrape consumed RPC admission: %v", got)
	}
	collector.release()
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Errorf("first metrics scrape = %d", first.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first metrics scrape did not finish")
	}
}

func TestHealthAndMetricsRoutesAreExact(t *testing.T) {
	h, _ := newTestHandler(t, "http://upstream.invalid", time.Second)
	if rec := do(h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}` {
		t.Errorf("health = (%d, %q)", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/healthz", `{}`); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Errorf("POST health = %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := do(h, http.MethodPost, "/not-root", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("non-root = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/metrics", ""); rec.Code != http.StatusOK {
		t.Errorf("metrics = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/metrics", ""); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Errorf("POST metrics = %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	for _, path := range []string{"/healthz/extra", "/metrics/extra"} {
		if rec := do(h, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestProxyRequestIDPolicy(t *testing.T) {
	var upstreamID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamID = r.Header.Get(requestIDHeader)
		w.Header().Set(requestIDHeader, "upstream-imposter")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	h, _ := newTestHandler(t, upstream.URL, time.Second)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(requestIDHeader, "request-abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if upstreamID != "request-abc" || rec.Header().Get(requestIDHeader) != "request-abc" {
		t.Errorf("request IDs upstream/response = %q/%q", upstreamID, rec.Header().Get(requestIDHeader))
	}

	for _, id := range []string{"", strings.Repeat("x", 129), "request space", "request-ü"} {
		req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		req.Header.Set(requestIDHeader, id)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get(requestIDHeader); len(got) != 32 || got == id {
			t.Errorf("replacement for %q = %q", id, got)
		}
	}
}

func TestProxyRewriteDropsRequestIDWithoutContextValue(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	handler := New(
		testConfig("https://polygon.drpc.org"),
		&http.Client{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics,
	)
	in := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	in.Header.Set(requestIDHeader, "client-controlled")
	out := in.Clone(context.Background())

	handler.proxy.Rewrite(&httputil.ProxyRequest{In: in, Out: out})

	if got := out.Header.Get(requestIDHeader); got != "" {
		t.Errorf("outbound request ID without context = %q, want empty", got)
	}
}

func TestMethodLabelBoundsCardinality(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	for _, reserved := range []string{"_invalid", "_preflight", "_overloaded", "_client_defined"} {
		if got := metrics.methodLabel(reserved); got != "other" {
			t.Errorf("reserved label %q = %q, want other", reserved, got)
		}
	}
	if len(metrics.seenMethods) != 0 {
		t.Fatalf("reserved labels consumed %d client-method slots", len(metrics.seenMethods))
	}
	for _, sentinel := range []string{"batch", "unknown", "other"} {
		if got := metrics.methodLabel(sentinel); got != sentinel {
			t.Errorf("sentinel %q = %q", sentinel, got)
		}
	}
	if len(metrics.seenMethods) != 0 {
		t.Fatalf("fixed sentinels consumed %d client-method slots", len(metrics.seenMethods))
	}
	for i := 0; i < methodLabelCap; i++ {
		name := "method_" + strconv.Itoa(i)
		if got := metrics.methodLabel(name); got != name {
			t.Fatalf("method %d = %q", i, got)
		}
	}
	if got := metrics.methodLabel("overflow"); got != "other" {
		t.Errorf("overflow = %q", got)
	}
	if got := metrics.methodLabel("method_0"); got != "method_0" {
		t.Errorf("previous method = %q", got)
	}

	for _, tt := range []struct{ body, want string }{
		{`{"method":"eth_blockNumber"}`, "eth_blockNumber"},
		{`[{"method":"eth_chainId"}]`, "batch"},
		{`{not json`, "unknown"},
		{`{"method":"eth.call"}`, "other"},
		{`{"method":"_invalid"}`, "other"},
		{`{"method":"_preflight"}`, "other"},
		{`{"method":"_overloaded"}`, "other"},
		{`{"method":"_client_defined"}`, "other"},
	} {
		if got := peekMethod([]byte(tt.body)); got != tt.want {
			t.Errorf("peekMethod(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestMethodLabelConcurrentAdmission(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	var wg sync.WaitGroup
	errs := make(chan string, methodLabelCap*2)
	for i := 0; i < methodLabelCap*2; i++ {
		name := "concurrent_" + strconv.Itoa(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := metrics.methodLabel(name); got != name && got != "other" {
				errs <- got
			}
		}()
	}
	wg.Wait()
	close(errs)
	for got := range errs {
		t.Errorf("unexpected concurrent label %q", got)
	}
	if len(metrics.seenMethods) != methodLabelCap {
		t.Errorf("admitted methods = %d, want %d", len(metrics.seenMethods), methodLabelCap)
	}
}

func TestStatusRecorderForwardsOnlyFirstFinalStatus(t *testing.T) {
	underlying := &headerRecorder{header: make(http.Header)}
	recorder := &statusRecorder{ResponseWriter: underlying}
	recorder.WriteHeader(http.StatusEarlyHints)
	recorder.WriteHeader(http.StatusAccepted)
	recorder.WriteHeader(http.StatusInternalServerError)
	if recorder.Status() != http.StatusAccepted {
		t.Errorf("recorded status = %d", recorder.Status())
	}
	if len(underlying.codes) != 2 || underlying.codes[0] != http.StatusEarlyHints || underlying.codes[1] != http.StatusAccepted {
		t.Errorf("forwarded statuses = %v", underlying.codes)
	}
}

func TestProxyWritesOneStructuredCompletionLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer upstream.Close()
	var logs bytes.Buffer
	h, _ := newHandler(t, testConfig(upstream.URL), &http.Client{}, slog.New(slog.NewJSONHandler(&logs, nil)))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"method":"eth_chainId"}`))
	req.Header.Set(requestIDHeader, "request-success")
	h.ServeHTTP(httptest.NewRecorder(), req)
	events := decodeLogs(t, &logs)
	if len(events) != 1 || events[0]["msg"] != "request completed" || events[0]["method"] != "eth_chainId" ||
		events[0]["http_method"] != http.MethodPost || events[0]["request_id"] != "request-success" || events[0]["status"] != float64(http.StatusAccepted) {
		t.Errorf("completion logs = %#v", events)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type timeoutError struct{}

func (timeoutError) Error() string   { return "network timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type readProbe struct{ reads atomic.Int32 }

func (r *readProbe) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, io.EOF
}

type headerRecorder struct {
	header http.Header
	codes  []int
}

func (r *headerRecorder) Header() http.Header         { return r.header }
func (r *headerRecorder) WriteHeader(code int)        { r.codes = append(r.codes, code) }
func (r *headerRecorder) Write(p []byte) (int, error) { return len(p), nil }

type errorResponseWriter struct {
	header http.Header
	err    error
}

func (w *errorResponseWriter) Header() http.Header       { return w.header }
func (w *errorResponseWriter) WriteHeader(int)           {}
func (w *errorResponseWriter) Write([]byte) (int, error) { return 0, w.err }

type blockingCollector struct {
	desc        *prometheus.Desc
	entered     chan struct{}
	releaseCh   chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newBlockingCollector() *blockingCollector {
	return &blockingCollector{
		desc:      prometheus.NewDesc("test_blocking_metric", "test", nil, nil),
		entered:   make(chan struct{}),
		releaseCh: make(chan struct{}),
	}
}

func (c *blockingCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }
func (c *blockingCollector) Collect(ch chan<- prometheus.Metric) {
	c.enterOnce.Do(func() { close(c.entered) })
	<-c.releaseCh
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1)
}
func (c *blockingCollector) release() { c.releaseOnce.Do(func() { close(c.releaseCh) }) }

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func waitFor(t *testing.T, ch <-chan struct{}, event string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", event)
	}
}

func decodeLogs(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(output.String())
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}
