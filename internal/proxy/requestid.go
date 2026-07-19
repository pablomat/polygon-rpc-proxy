package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// requestIDHeader is the header used to carry a correlation ID between the
// client, this proxy, and the upstream.
const requestIDHeader = "X-Request-Id"

// maxRequestIDLen bounds an inbound correlation ID. Anything longer, or
// containing non-printable bytes, is replaced with a generated ID so callers
// cannot inflate log volume or smuggle junk through the correlation chain.
const maxRequestIDLen = 128

type ctxKey int

const requestIDKey ctxKey = iota

// withRequestID wraps a handler so every request has a correlation ID. A
// well-formed inbound X-Request-Id is honored so the same value is available
// to the client, proxy logs, and upstream; otherwise one is generated. The ID
// is stored in the request context and echoed on the response header.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validRequestID reports whether an inbound correlation ID is safe to adopt:
// non-empty, within the length bound, and printable ASCII with no spaces.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] > '~' {
			return false
		}
	}
	return true
}

// requestID returns the correlation ID stored on the context, or "" if none.
func requestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// newRequestID returns a random 128-bit hex identifier. crypto/rand.Read always
// fills the buffer or terminates the process, so there is no recoverable error.
func newRequestID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
