package app

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
)

type ctxKey int

const requestIDKey ctxKey = iota

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// withRequestID assigns a correlation ID to every request, reusing an
// inbound X-Request-ID when it looks well-formed so callers can trace a
// request across a proxy chain; otherwise it mints a fresh UUID.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID.MatchString(id) {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

var uuidSegment = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// metricsPath collapses job UUID path segments so per-job cardinality does
// not leak into Prometheus label values or span names.
func metricsPath(p string) string {
	start := 0
	out := make([]byte, 0, len(p))
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			seg := p[start:i]
			if uuidSegment.MatchString(seg) {
				out = append(out, "{id}"...)
			} else {
				out = append(out, seg...)
			}
			if i < len(p) {
				out = append(out, '/')
			}
			start = i + 1
		}
	}
	return string(out)
}

// accessLog logs each request and records HTTP-level Prometheus metrics.
func (a *App) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		dur := time.Since(start)
		path := metricsPath(r.URL.Path)
		a.metrics.httpRequests.WithLabelValues(r.Method, path, strconv.Itoa(rec.status)).Inc()
		a.metrics.httpDuration.WithLabelValues(r.Method, path).Observe(dur.Seconds())
		a.log.Info("request", "request_id", requestIDFromContext(r.Context()), "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration_ms", dur.Milliseconds(), "remote", clientIP(r))
	})
}

func (a *App) rateLimitJobs(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.limiter.allow(clientIP(r)) {
			a.metrics.rateLimited.Inc()
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
			return
		}
		next(w, r)
	}
}
