// Package obs exposes Prometheus metrics and pprof on a side port for every service.
package obs

import (
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// StepSeconds is the latency of the shared-core step bodies (excludes engine scheduling and checkpoint
// overhead, which is what the benchmark isolates).
var StepSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "ibps_step_seconds",
	Help:    "Duration of a shared-core step body.",
	Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
}, []string{"step"})

// PaymentsTotal counts final outcomes recorded by this replica.
var PaymentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "ibps_payments_total", Help: "Payments by final state.",
}, []string{"state"})

func init() { prometheus.MustRegister(StepSeconds, PaymentsTotal) }

// ObserveStep records the duration since start for a step.
func ObserveStep(step string, start time.Time) {
	StepSeconds.WithLabelValues(step).Observe(time.Since(start).Seconds())
}

// Serve starts /metrics and /debug/pprof on addr in the background.
func Serve(addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	go func() {
		_ = (&http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
	}()
}
