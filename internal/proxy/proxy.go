// Package proxy implements a bounded HTTP reverse proxy for Polygon JSON-RPC.
// JSON-RPC payloads remain opaque application data; method inspection is used
// only for bounded telemetry labels.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pablomat/polygon-rpc-proxy/internal/config"
)

// Handler forwards requests to one configured upstream endpoint.
type Handler struct {
	cfg     config.Config
	log     *slog.Logger
	metrics *Metrics
	sem     chan struct{}
	proxy   *httputil.ReverseProxy
}

type stateKey struct{}

// reqState carries the method label and any proxy failure back to handleRPC,
// which emits the single per-request log line.
type reqState struct {
	method           string
	transportErr     error
	responseWriteErr error
	cause            string
}

// New constructs a Handler. cfg.UpstreamURL and positive limits are expected
// to have been validated by config.Load.
func New(cfg config.Config, client *http.Client, log *slog.Logger, metrics *Metrics) *Handler {
	target, _ := url.Parse(cfg.UpstreamURL)
	h := &Handler{
		cfg:     cfg,
		log:     log,
		metrics: metrics,
		sem:     make(chan struct{}, cfg.MaxInFlight),
	}

	h.proxy = &httputil.ReverseProxy{
		Transport: client.Transport,
		ErrorLog:  slog.NewLogLogger(slog.DiscardHandler, slog.LevelError),
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Pin the complete configured endpoint. Inbound paths and query
			// parameters can never redirect the outbound request.
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.User = target.User
			pr.Out.URL.Path = target.Path
			pr.Out.URL.RawPath = target.RawPath
			pr.Out.URL.RawQuery = target.RawQuery
			pr.Out.URL.ForceQuery = target.ForceQuery
			pr.Out.Host = target.Host
			// The middleware owns this header; drop any inbound value.
			pr.Out.Header.Del(requestIDHeader)
			if id := requestID(pr.Out.Context()); id != "" {
				pr.Out.Header.Set(requestIDHeader, id)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// The middleware owns the response correlation ID.
			resp.Header.Del(requestIDHeader)
			// Transport errors after the headers arrive bypass ErrorHandler.
			// Observe body-read failures so a truncated upstream response is
			// still logged and counted as a transport failure. A 101 response
			// must retain its original read-write body for protocol upgrades.
			if resp.StatusCode != http.StatusSwitchingProtocols && resp.Body != nil && resp.Request != nil {
				ctx := resp.Request.Context()
				if state, ok := ctx.Value(stateKey{}).(*reqState); ok {
					resp.Body = &observedReadCloser{
						ReadCloser: resp.Body,
						onError: func(err error) {
							_, cause := classifyUpstreamError(ctx, err)
							if cause == "unreachable" {
								cause = "response_read"
							}
							state.transportErr = err
							state.cause = cause
							h.metrics.UpstreamErrors.WithLabelValues(cause).Inc()
						},
					}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status, cause := classifyUpstreamError(r.Context(), err)
			if st, ok := r.Context().Value(stateKey{}).(*reqState); ok {
				st.transportErr = err
				st.cause = cause
			}
			h.metrics.UpstreamErrors.WithLabelValues(cause).Inc()
			message := "upstream unavailable"
			if status == http.StatusGatewayTimeout {
				message = "upstream timeout"
			}
			http.Error(w, message, status)
		},
	}
	return h
}

// Routes wires the root RPC endpoint and the operational routes.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handleHealth)
	metricsHandler := promhttp.HandlerFor(h.metrics.registry, promhttp.HandlerOpts{
		MaxRequestsInFlight: 1,
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		metricsHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		h.handleRPC(w, r)
	})
	return withRequestID(mux)
}

// handleHealth reports process liveness without contacting the upstream.
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func (h *Handler) handleRPC(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	state := &reqState{method: "_invalid"}
	recorder := &statusRecorder{ResponseWriter: w}
	defer func() {
		state.responseWriteErr = recorder.WriteError()
		duration := time.Since(started)
		status := recorder.Status()
		h.metrics.RequestSeconds.WithLabelValues(state.method).Observe(duration.Seconds())
		h.metrics.RequestsTotal.WithLabelValues(state.method, strconv.Itoa(status)).Inc()
		h.logRequest(r.Context(), r.Method, state, status, duration)
	}()

	switch r.Method {
	case http.MethodPost:
	case http.MethodOptions:
		state.method = "_preflight"
	default:
		recorder.Header().Set("Allow", "POST, OPTIONS")
		http.Error(recorder, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !h.tryAcquire() {
		state.method = "_overloaded"
		recorder.Header().Set("Retry-After", "1")
		http.Error(recorder, "proxy at capacity", http.StatusServiceUnavailable)
		return
	}
	defer h.release()

	// Give MaxBytesReader the server's writer directly so net/http can mark an
	// oversized request for connection close. Errors are still emitted through
	// the recorder so status telemetry remains accurate.
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
	body, ok := h.bufferBody(recorder, r)
	if !ok {
		state.method = "_invalid"
		return
	}
	if r.Method == http.MethodPost {
		state.method = h.metrics.methodLabel(peekMethod(body))
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.UpstreamTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, stateKey{}, state)
	h.proxy.ServeHTTP(recorder, r.WithContext(ctx))
}

// bufferBody reads the already-limited RPC or preflight body, then restores
// the bytes for ReverseProxy's transport.
func (h *Handler) bufferBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return nil, false
		}
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return nil, false
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return body, true
}

// observedReadCloser reports the first non-EOF upstream body-read failure.
// ReverseProxy still owns the read loop, close, and its standard abort path.
type observedReadCloser struct {
	io.ReadCloser
	onError  func(error)
	reported bool
}

func (r *observedReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && err != io.EOF && !r.reported {
		r.reported = true
		r.onError(err)
	}
	return n, err
}

func (h *Handler) tryAcquire() bool {
	select {
	case h.sem <- struct{}{}:
		h.metrics.InFlight.Inc()
		return true
	default:
		return false
	}
}

func (h *Handler) release() {
	h.metrics.InFlight.Dec()
	<-h.sem
}

func (h *Handler) logRequest(ctx context.Context, httpMethod string, state *reqState, status int, duration time.Duration) {
	attrs := []slog.Attr{
		slog.String("request_id", requestID(ctx)),
		slog.String("http_method", httpMethod),
		slog.String("method", state.method),
		slog.Int("status", status),
		slog.Duration("duration", duration),
	}
	if state.transportErr == nil && state.responseWriteErr != nil {
		attrs = append(attrs,
			slog.String("cause", "downstream_write"),
			slog.Any("err", state.responseWriteErr),
		)
		h.log.LogAttrs(ctx, slog.LevelError, "response write failed", attrs...)
		return
	}
	if state.transportErr != nil {
		attrs = append(attrs,
			slog.String("cause", state.cause),
			slog.Any("err", state.transportErr),
		)
		level := slog.LevelError
		message := "upstream request failed"
		if state.cause == "canceled" {
			level = slog.LevelInfo
			message = "upstream request canceled"
		}
		h.log.LogAttrs(ctx, level, message, attrs...)
		return
	}
	h.log.LogAttrs(ctx, slog.LevelInfo, "request completed", attrs...)
}

func classifyUpstreamError(ctx context.Context, err error) (status int, cause string) {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout"
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return http.StatusGatewayTimeout, "canceled"
	case errors.As(err, &netErr) && netErr.Timeout():
		return http.StatusGatewayTimeout, "timeout"
	default:
		return http.StatusBadGateway, "unreachable"
	}
}

// statusRecorder captures the final status and lets ResponseController unwrap
// to the original writer for streaming, trailers, and protocol upgrades.
type statusRecorder struct {
	http.ResponseWriter
	status   int
	writeErr error
}

func (s *statusRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		s.ResponseWriter.WriteHeader(code)
		return
	}
	if s.status != 0 {
		return
	}
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	if n != len(p) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil && s.writeErr == nil {
		s.writeErr = err
	}
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Status() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

func (s *statusRecorder) WriteError() error { return s.writeErr }
