package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/obs"
	"github.com/bill/ibps-bench/sims/aml"
	"github.com/bill/ibps-bench/sims/simx"
)

func main() {
	ep := config.EndpointsFromEnv()
	srv := aml.New(simx.New(time.Duration(config.EnvInt("LATENCY_MS", 10)) * time.Millisecond))
	srv.FastReview = time.Duration(config.EnvInt("REVIEW_FAST_MS", 1000)) * time.Millisecond
	srv.SlowReview = time.Duration(config.EnvInt("REVIEW_SLOW_MS", 30000)) * time.Millisecond
	obs.Serve(ep.MetricsAddr)
	mux := http.NewServeMux()
	srv.Routes(mux)
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8002"
	}
	log.Printf("sim-aml listening on %s", addr)
	log.Fatal(simx.Serve(addr, mux))
}
