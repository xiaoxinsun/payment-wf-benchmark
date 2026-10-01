// payment-temporal is the V1 payment service. MODE selects the deployment shape:
//
//	ingress    ingress only (V1a: workers run as a separate deployment)
//	worker     worker only  (V1a)
//	colocated  ingress and worker in one process on one client, with eager start (V1b)
//
// VARIANT (a|b) selects the workflow type and task queue.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/ingress"
	"github.com/bill/ibps-bench/core/mapping"
	"github.com/bill/ibps-bench/core/obs"
	"github.com/bill/ibps-bench/core/steps"
	"github.com/bill/ibps-bench/engine-temporal/payment"
)

func main() {
	ep := config.EndpointsFromEnv()
	cfg, err := config.Load(ep.ConfigDir)
	if err != nil {
		log.Fatal(err)
	}
	payment.Cfg = cfg
	mode, variant := os.Getenv("MODE"), os.Getenv("VARIANT")
	wfName, tq := payment.WorkflowA, "ibps-a"
	if variant == "b" {
		wfName, tq = payment.WorkflowB, "ibps-b"
	}
	ctx := context.Background()

	store, err := steps.OpenStore(ctx, ep.AppDBURL)
	if err != nil {
		log.Fatal(err)
	}
	deps := &steps.Deps{Cfg: cfg, EP: ep, HTTP: steps.NewHTTPClient(),
		Mapper: mapping.SyntheticMapper{RejectCodes: cfg.RejectCodes}, Store: store}

	logLevel := slog.Level(12) // silent unless LOG_LEVEL=debug
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	logger := tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))
	addr := os.Getenv("TEMPORAL_ADDR")
	if addr == "" {
		addr = "localhost:7233"
	}
	var c client.Client
	for start := time.Now(); ; { // the server may still be starting (e.g. right after a harness reset)
		c, err = client.Dial(client.Options{HostPort: addr, Namespace: "default", Logger: logger})
		if err == nil {
			break
		}
		if time.Since(start) > 2*time.Minute {
			log.Fatal(err)
		}
		time.Sleep(time.Second)
	}
	defer c.Close()
	obs.Serve(ep.MetricsAddr)

	if mode == "worker" || mode == "colocated" {
		w := worker.New(c, tq, worker.Options{})
		w.RegisterWorkflowWithOptions(payment.PaymentWorkflowA, workflow.RegisterOptions{Name: payment.WorkflowA})
		w.RegisterWorkflowWithOptions(payment.PaymentWorkflowB, workflow.RegisterOptions{Name: payment.WorkflowB})
		acts := &payment.Activities{Deps: deps}
		payment.LocalActs = acts
		w.RegisterActivity(acts)
		if err := w.Start(); err != nil {
			log.Fatal(err)
		}
		defer w.Stop()
		log.Printf("temporal worker started (variant=%s task queue=%s mode=%s)", variant, tq, mode)
	}
	if mode == "ingress" || mode == "colocated" {
		h := &ingress.Handler{Deps: deps, Starter: &payment.Starter{
			Client: c, TaskQueue: tq, WorkflowName: wfName, Eager: mode == "colocated"}}
		mux := http.NewServeMux()
		h.Routes(mux)
		log.Printf("ingress listening on %s (mode=%s)", ep.ListenAddr, mode)
		log.Fatal((&http.Server{Addr: ep.ListenAddr, Handler: mux}).ListenAndServe())
	}
	if mode == "worker" {
		select {}
	}
	if mode != "ingress" && mode != "colocated" && mode != "worker" {
		log.Fatal("MODE must be ingress, worker or colocated")
	}
}
