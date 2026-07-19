package proxy

import (
	"bytes"
	"encoding/json"
)

// peekMethod extracts a metric label from the request body. It never rejects
// a request: batches use "batch", malformed or unrecognized payloads
// "unknown", and unsafe method names "other".
func peekMethod(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return "batch"
	}
	var probe struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil || probe.Method == "" {
		return "unknown"
	}
	if !validMethodLabel(probe.Method) {
		return "other"
	}
	return probe.Method
}

// validMethodLabel bounds client-controlled metric cardinality before the
// process-wide distinct-method cap is applied.
func validMethodLabel(method string) bool {
	// Leading underscores are reserved for proxy-generated operational labels
	// such as _invalid, _preflight, and _overloaded.
	if method == "" || method[0] == '_' || len(method) > 64 {
		return false
	}
	for i := 0; i < len(method); i++ {
		switch c := method[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}
