package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey int

const ctxRequestID ctxKey = 1

// requestIDFromContext returns the request ID stored on ctx, or "" if absent.
func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// RecoverAndLog wraps next with panic recovery, request-ID injection, and
// one access log line per request. Exported so main.go can apply it to
// the top-level handler.
func RecoverAndLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			rid := newRequestID()
			ctx := context.WithValue(r.Context(), ctxRequestID, rid)

			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("http.panic",
						"request_id", rid,
						"path", r.URL.Path,
						"err", rec,
						"stack", string(debug.Stack()),
					)
					if !rw.wroteHeader {
						http.Error(rw, `{"error":"internal error"}`, http.StatusInternalServerError)
					}
				}
				logger.Info("http_request",
					"method", r.Method,
					"path", r.URL.Path,
					"status", rw.status,
					"duration_ms", time.Since(start).Milliseconds(),
					"remote_addr", r.RemoteAddr,
					"request_id", rid,
				)
			}()

			rw.Header().Set("X-Request-Id", rid)
			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
