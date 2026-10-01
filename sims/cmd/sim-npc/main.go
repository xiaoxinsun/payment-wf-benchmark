package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/mapping"
	"github.com/bill/ibps-bench/core/obs"
	"github.com/bill/ibps-bench/sims/npc"
	"github.com/bill/ibps-bench/sims/simx"
)

func main() {
	ep := config.EndpointsFromEnv()
	cfg, err := config.Load(ep.ConfigDir)
	if err != nil {
		log.Fatal(err)
	}
	sim := simx.New(time.Duration(config.EnvInt("LATENCY_MS", 3)) * time.Millisecond)
	srv := npc.New(sim, mapping.SyntheticMapper{RejectCodes: cfg.RejectCodes}, config.EnvInt("ACCOUNTS", 50000),
		cfg.SLA.OurBankCode, cfg.SLA.AmountLimitFen)
	obs.Serve(ep.MetricsAddr)
	mux := http.NewServeMux()
	srv.Routes(mux)
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":9000"
	}
	log.Printf("sim-npc listening on %s", addr)
	log.Fatal(simx.Serve(addr, mux))
}
