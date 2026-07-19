package proxy

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// methodLabelCap bounds distinct client-supplied method series over one process
// lifetime. First-seen valid names consume the available slots; later unseen
// names collapse to "other".
const methodLabelCap = 100

// Metrics bundles the Prometheus collectors the proxy reports. It is passed
// into the handler as a dependency so tests can supply an isolated registry
// and the wiring stays explicit.
type Metrics struct {
	RequestsTotal  *prometheus.CounterVec
	RequestSeconds *prometheus.HistogramVec
	UpstreamErrors *prometheus.CounterVec
	InFlight       prometheus.Gauge

	// registry backs the /metrics endpoint, so the handler serves exactly
	// the registry its collectors were registered on.
	registry *prometheus.Registry

	mu          sync.Mutex
	seenMethods map[string]struct{}
}

// NewMetrics registers the proxy collectors on the given registry. Using a
// caller-supplied registry (rather than the global default) keeps tests
// hermetic and avoids duplicate-registration panics.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	factory := promauto.With(reg)
	return &Metrics{
		registry:    reg,
		seenMethods: make(map[string]struct{}, methodLabelCap),
		RequestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "rpc_proxy_requests_total",
			Help: "Total RPC endpoint requests, labeled by JSON-RPC method and response status code.",
		}, []string{"method", "code"}),
		RequestSeconds: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rpc_proxy_request_duration_seconds",
			Help:    "End-to-end proxy request latency in seconds, labeled by JSON-RPC method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method"}),
		UpstreamErrors: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "rpc_proxy_upstream_errors_total",
			Help: "Total upstream transport failures, labeled by cause.",
		}, []string{"cause"}),
		InFlight: factory.NewGauge(prometheus.GaugeOpts{
			Name: "rpc_proxy_in_flight_requests",
			Help: "Current admitted RPC and preflight requests.",
		}),
	}
}

// methodLabel returns the metric label for a peeked method name, admitting at
// most methodLabelCap distinct names per process lifetime; anything new beyond
// the cap collapses to "other".
func (m *Metrics) methodLabel(name string) string {
	if len(name) > 0 && name[0] == '_' {
		return "other"
	}
	switch name {
	case "batch", "unknown", "other":
		return name
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seenMethods[name]; ok {
		return name
	}
	if len(m.seenMethods) >= methodLabelCap {
		return "other"
	}
	m.seenMethods[name] = struct{}{}
	return name
}
