// payment-dbos is the V2 payment service: ingress and the DBOS workflow engine in one process.
package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/ingress"
	"github.com/bill/ibps-bench/core/mapping"
	"github.com/bill/ibps-bench/core/obs"
	"github.com/bill/ibps-bench/core/steps"
	"github.com/bill/ibps-bench/engine-dbos/payment"
)

func main() {
	ep := config.EndpointsFromEnv()
	cfg, err := config.Load(ep.ConfigDir)
	if err != nil {
		log.Fatal(err)
	}
	payment.Cfg = cfg
	ctx := context.Background()

	store, err := steps.OpenStore(ctx, ep.AppDBURL)
	if err != nil {
		log.Fatal(err)
	}
	deps := &steps.Deps{Cfg: cfg, EP: ep, HTTP: steps.NewHTTPClient(),
		Mapper: mapping.SyntheticMapper{RejectCodes: cfg.RejectCodes}, Store: store}
	payment.Impl = deps

	var out io.Writer = io.Discard // DBOS logs every step retry at error level; silent unless LOG_LEVEL=debug
	if os.Getenv("LOG_LEVEL") == "debug" {
		out = os.Stderr
	}
	dbURL := os.Getenv("DBOS_DB_URL")
	if dbURL == "" {
		dbURL = "postgres://dbos:dbos@localhost:5433/dbos_sys?sslmode=disable&pool_max_conns=20"
	}
	// Two replicas can start migrations concurrently on a fresh schema (docker restarts them together);
	// retry the race instead of crashing the container.
	var dctx dbos.Context
	for start := time.Now(); ; {
		dctx, err = dbos.NewContext(ctx, dbos.Config{
			AppName: "ibps", DatabaseURL: dbURL, ApplicationVersion: "bench-1",
			ExecutorID: os.Getenv("DBOS__VMID"), // a stable ID per replica is what makes restart-recovery work (I7/I8)
			Logger:     slog.New(slog.NewTextHandler(out, nil)),
		})
		if err == nil {
			break
		}
		if time.Since(start) > time.Minute {
			log.Fatal(err)
		}
		log.Printf("dbos.NewContext: %v; retrying", err)
		time.Sleep(time.Second)
	}
	payment.Register(dctx)
	if err := dbos.Launch(dctx); err != nil {
		log.Fatal(err)
	}
	defer dbos.Shutdown(dctx, 5*time.Second)
	obs.Serve(ep.MetricsAddr)

	h := &ingress.Handler{Deps: deps, Starter: payment.NewStarter(dctx, config.EnvInt("DBOS_INFLIGHT_CAP", 512), cfg.SLA.Budget)}
	mux := http.NewServeMux()
	h.Routes(mux)
	log.Printf("payment-dbos ingress listening on %s (executor %q)", ep.ListenAddr, os.Getenv("DBOS__VMID"))
	log.Fatal((&http.Server{Addr: ep.ListenAddr, Handler: mux}).ListenAndServe())
}
