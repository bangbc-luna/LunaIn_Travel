package httpx

import (
	"net/http"
	"slices"
	"sync"
)

// Router is the registration surface modules need. *http.ServeMux satisfies it.
type Router interface {
	Handle(pattern string, h http.Handler)
	HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request))
}

// RecordingRouter delegates to a ServeMux and records every pattern registered.
type RecordingRouter struct {
	mux      *http.ServeMux
	mu       sync.Mutex
	patterns []string
}

// NewRecordingRouter wraps mux.
func NewRecordingRouter(mux *http.ServeMux) *RecordingRouter {
	return &RecordingRouter{mux: mux}
}

func (r *RecordingRouter) record(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.patterns = append(r.patterns, p)
}

// Handle registers h on the mux and records pattern.
func (r *RecordingRouter) Handle(pattern string, h http.Handler) {
	r.mux.Handle(pattern, h)
	r.record(pattern)
}

// HandleFunc registers h on the mux and records pattern.
func (r *RecordingRouter) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	r.mux.HandleFunc(pattern, h)
	r.record(pattern)
}

// Patterns returns a sorted copy of the recorded patterns.
func (r *RecordingRouter) Patterns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.patterns)
	slices.Sort(out)
	return out
}
