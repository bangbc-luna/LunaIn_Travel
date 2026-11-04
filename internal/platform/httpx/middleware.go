package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// Chain wraps h with mws; the first middleware is the outermost.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

type ctxKey struct{}

// RequestIDFrom returns the request id stored by RequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// RequestID reads X-Request-ID or generates a uuid v7, and echoes it back.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 128 {
			if u, err := uuid.NewV7(); err == nil {
				id = u.String()
			} else {
				id = uuid.NewString()
			}
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Logger logs one line per request.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration", time.Since(start),
				"request_id", RequestIDFrom(r.Context()))
		})
	}
}

// Recover turns a panic into a logged ErrInternal response.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.Error("panic recovered",
					"panic", rec,
					"stack", string(debug.Stack()),
					"request_id", RequestIDFrom(r.Context()))
				WriteError(w, r, ErrInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ClientIPFunc returns a client-IP extractor for use as a RateLimit key.
//
// Trust model: clients can put anything in X-Forwarded-For, but each trusted
// proxy appends the address it saw to the right end. With trustedProxyHops == 0
// the header is ignored and the TCP peer (RemoteAddr host) is used. With N > 0
// the N-th entry from the right is used, i.e. the address seen by the outermost
// trusted proxy; anything the client forged sits further left and is ignored.
// If the header has fewer than N entries, it falls back to RemoteAddr.
func ClientIPFunc(trustedProxyHops int) func(*http.Request) string {
	return func(r *http.Request) string {
		if trustedProxyHops > 0 {
			var entries []string
			for _, v := range r.Header.Values("X-Forwarded-For") {
				for _, part := range strings.Split(v, ",") {
					if p := strings.TrimSpace(part); p != "" {
						entries = append(entries, p)
					}
				}
			}
			if len(entries) >= trustedProxyHops {
				return entries[len(entries)-trustedProxyHops]
			}
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return host
	}
}

const (
	limiterIdleTTL     = 10 * time.Minute
	limiterSweepPeriod = time.Minute
)

type limiterEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// RateLimit is an in-memory per-key token bucket (perMinute refill, burst
// capacity). Keys idle for 10 minutes are swept lazily on access, so there is
// no background goroutine. Single-instance only.
func RateLimit(perMinute int, burst int, key func(*http.Request) string) func(http.Handler) http.Handler {
	var (
		mu        sync.Mutex
		entries   = map[string]*limiterEntry{}
		lastSweep = time.Now()
	)
	limit := rate.Limit(float64(perMinute) / 60.0)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := key(r)
			now := time.Now()

			mu.Lock()
			if now.Sub(lastSweep) >= limiterSweepPeriod {
				for ek, e := range entries {
					if now.Sub(e.lastSeen) >= limiterIdleTTL {
						delete(entries, ek)
					}
				}
				lastSweep = now
			}
			e, ok := entries[k]
			if !ok {
				e = &limiterEntry{lim: rate.NewLimiter(limit, burst)}
				entries[k] = e
			}
			e.lastSeen = now
			res := e.lim.ReserveN(now, 1)
			allowed := res.OK() && res.DelayFrom(now) == 0
			var delay time.Duration
			if !allowed {
				if res.OK() {
					delay = res.DelayFrom(now)
				} else {
					delay = time.Minute
				}
				res.CancelAt(now) // do not consume a token for a rejected request
			}
			mu.Unlock()

			if !allowed {
				secs := int(delay.Seconds()) + 1
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				WriteError(w, r, ErrTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
