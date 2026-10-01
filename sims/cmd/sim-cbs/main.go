package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/obs"
	"github.com/bill/ibps-bench/sims/cbs"
	"github.com/bill/ibps-bench/sims/simx"
)

func main() {
	ep := config.EndpointsFromEnv()
	url := os.Getenv("CBS_DB_URL")
	if url == "" {
		url = "postgres://bench:bench@localhost:5434/cbs?sslmode=disable&pool_max_conns=30"
	}
	ctx := context.Background()
	srv, err := cbs.Open(ctx, url, simx.New(time.Duration(config.EnvInt("LATENCY_MS", 5))*time.Millisecond))
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Seed(ctx, config.EnvInt("ACCOUNTS", 50000)); err != nil {
		log.Fatal(err)
	}
	obs.Serve(ep.MetricsAddr)
	mux := http.NewServeMux()
	srv.Routes(mux)
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8001"
	}
	log.Printf("sim-cbs listening on %s", addr)
	log.Fatal(simx.Serve(addr, mux))
}
