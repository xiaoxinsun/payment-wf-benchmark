// Package simx holds what every simulator shares: base latency, the /faults API and control endpoints.
package simx

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Fault is one injected misbehaviour on a named target (an endpoint group) of a simulator.
type Fault struct {
	Target     string  `json:"target"`
	Mode       string  `json:"mode"`
	Rate       float64 `json:"rate"`       // 0..1, default 1
	DurationMs int     `json:"durationMs"` // 0 = until cleared
	LatencyMs  int     `json:"latencyMs"`
	until      time.Time
}

// Sim provides base latency (with 20% jitter) and the fault registry.
type Sim struct {
	latencyNs atomic.Int64
	mu        sync.Mutex
	faults    []Fault
}

func New(baseLatency time.Duration) *Sim {
	s := &Sim{}
	s.latencyNs.Store(int64(baseLatency))
	return s
}

func (s *Sim) SetLatency(d time.Duration) { s.latencyNs.Store(int64(d)) }

// Sleep applies the base latency; call at the top of every business endpoint.
func (s *Sim) Sleep() {
	base := time.Duration(s.latencyNs.Load())
	if base <= 0 {
		return
	}
	jitter := time.Duration(rand.Int64N(int64(base)/5+1)) - base/10
	time.Sleep(base + jitter)
}

// Add registers a fault.
func (s *Sim) Add(f Fault) {
	if f.Rate <= 0 {
		f.Rate = 1
	}
	if f.DurationMs > 0 {
		f.until = time.Now().Add(time.Duration(f.DurationMs) * time.Millisecond)
	}
	s.mu.Lock()
	s.faults = append(s.faults, f)
	s.mu.Unlock()
}

func (s *Sim) Clear() {
	s.mu.Lock()
	s.faults = nil
	s.mu.Unlock()
}

// Pick returns an active fault for the target that fires on this call, or nil.
func (s *Sim) Pick(target string) *Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	live := s.faults[:0]
	var hit *Fault
	for i := range s.faults {
		f := s.faults[i]
		if !f.until.IsZero() && now.After(f.until) {
			continue
		}
		live = append(live, f)
		if hit == nil && f.Target == target && (f.Rate >= 1 || rand.Float64() < f.Rate) {
			c := f
			hit = &c
		}
	}
	s.faults = live
	return hit
}

// Control registers POST/DELETE /faults, GET /faults and POST /config/latency on mux.
func (s *Sim) Control(mux *http.ServeMux) {
	mux.HandleFunc("POST /faults", func(w http.ResponseWriter, r *http.Request) {
		var f Fault
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil || f.Target == "" || f.Mode == "" {
			http.Error(w, "need target and mode", http.StatusBadRequest)
			return
		}
		s.Add(f)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("DELETE /faults", func(w http.ResponseWriter, r *http.Request) {
		s.Clear()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /faults", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		JSON(w, http.StatusOK, s.faults)
	})
	mux.HandleFunc("POST /config/latency", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Ms float64 `json:"ms"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil || b.Ms < 0 {
			http.Error(w, "need ms >= 0", http.StatusBadRequest)
			return
		}
		s.SetLatency(time.Duration(b.Ms * float64(time.Millisecond)))
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

// JSON writes v as JSON with the given status.
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve runs the handler with sane server timeouts.
func Serve(addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second}
	return srv.ListenAndServe()
}
