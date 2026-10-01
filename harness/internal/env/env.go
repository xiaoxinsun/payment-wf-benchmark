// Package env controls the benchmark stack: docker compose, chaos primitives, Toxiproxy, database access,
// simulator admin and resource sampling. It is the only place that talks to docker.
package env

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Variant describes one engine deployment.
type Variant struct {
	Name       string   // v1a | v1b | v2
	Files      []string // compose files (relative to deploy/compose)
	Ingress    []string // services that take ingress traffic
	Workers    []string // services that only run workflow workers (V1a)
	Replicas   []string // every payment container (kill targets)
	Durability string   // durability DB service name
	DurPort    int
	Proxy      string // toxiproxy name of the durability link
	Engine     string // temporal | dbos
}

var Variants = map[string]Variant{
	"v1a": {Name: "v1a", Files: []string{"shared.yml", "v1.yml", "v1a.yml"}, Ingress: []string{"ingress-1", "ingress-2"},
		Workers: []string{"worker-1", "worker-2"}, Replicas: []string{"ingress-1", "ingress-2", "worker-1", "worker-2"},
		Durability: "temporal-db", DurPort: 5432, Proxy: "temporal", Engine: "temporal"},
	"v1b": {Name: "v1b", Files: []string{"shared.yml", "v1.yml", "v1b.yml"}, Ingress: []string{"payment-1", "payment-2"},
		Replicas: []string{"payment-1", "payment-2"}, Durability: "temporal-db", DurPort: 5432, Proxy: "temporal", Engine: "temporal"},
	"v2": {Name: "v2", Files: []string{"shared.yml", "v2.yml"}, Ingress: []string{"payment-1", "payment-2"},
		Replicas: []string{"payment-1", "payment-2"}, Durability: "dbos-db", DurPort: 5433, Proxy: "dbosdb", Engine: "dbos"},
}

// Env is a handle on the running stack.
type Env struct {
	Root    string // repo root
	V       Variant
	Shards  int
	Extra   map[string]string // extra environment for compose (e.g. CODE_FAULT_SUFFIX)
	dbs     map[string]*pgxpool.Pool
	dbsMu   sync.Mutex
	HTTP    *http.Client
	NPCURL  string
	CBSURL  string
	AMLURL  string
	LBURL   string
	ToxiURL string
}

func New(root string, v Variant) *Env {
	return &Env{Root: root, V: v, Shards: 16, dbs: map[string]*pgxpool.Pool{},
		HTTP:   &http.Client{Timeout: 30 * time.Second},
		NPCURL: "http://localhost:9000", CBSURL: "http://localhost:8001", AMLURL: "http://localhost:8002",
		LBURL: "http://localhost:8080", ToxiURL: "http://localhost:8474"}
}

func (e *Env) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = e.Root
	cmd.Env = append(os.Environ(), "SETTLEMENT_SHARDS="+strconv.Itoa(e.Shards))
	for k, v := range e.Extra {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

func (e *Env) compose(ctx context.Context, files []string, args ...string) (string, error) {
	a := []string{"compose"}
	for _, f := range files {
		a = append(a, "-f", filepath.Join("deploy/compose", f))
	}
	return e.run(ctx, "docker", append(a, args...)...)
}

// Container returns the docker container name of a compose service.
func Container(service string) string { return "ibps-" + service + "-1" }

// Down stops every variant stack (only one may run at a time). Volumes are kept.
func (e *Env) Down(ctx context.Context) error {
	for _, v := range Variants {
		if _, err := e.compose(ctx, v.Files, "down", "--remove-orphans"); err != nil {
			return err
		}
	}
	return nil
}

// Up starts the variant's stack with the current SETTLEMENT_SHARDS and waits until it serves traffic.
func (e *Env) Up(ctx context.Context) error {
	if _, err := e.compose(ctx, e.V.Files, "up", "-d", "--remove-orphans"); err != nil {
		return err
	}
	return e.WaitReady(ctx, 180*time.Second)
}

// Recreate force-recreates the payment containers (used to change SETTLEMENT_SHARDS).
func (e *Env) Recreate(ctx context.Context) error {
	if _, err := e.compose(ctx, e.V.Files, append([]string{"up", "-d", "--force-recreate", "--no-deps"}, e.V.Replicas...)...); err != nil {
		return err
	}
	return e.WaitReady(ctx, 120*time.Second)
}

// WaitReady waits for the LB to reach a healthy ingress and for every replica container to be running.
func (e *Env) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		last = e.ready(ctx)
		if last == nil {
			time.Sleep(2 * time.Second) // let workers register their pollers
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("stack not ready after %v: %w", timeout, last)
}

func (e *Env) ready(ctx context.Context) error {
	for _, s := range e.V.Replicas {
		out, err := e.run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", Container(s))
		if err != nil || strings.TrimSpace(out) != "true" {
			return fmt.Errorf("%s not running", s)
		}
	}
	for _, u := range []string{e.LBURL + "/healthz", e.CBSURL + "/healthz", e.AMLURL + "/healthz", e.NPCURL + "/healthz"} {
		resp, err := e.HTTP.Get(u)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("%s: %d", u, resp.StatusCode)
		}
	}
	return nil
}

// ---- chaos primitives -------------------------------------------------------------------------------

func (e *Env) Kill(ctx context.Context, service string) error {
	_, err := e.run(ctx, "docker", "kill", Container(service))
	return err
}

func (e *Env) Start(ctx context.Context, service string) error {
	_, err := e.run(ctx, "docker", "start", Container(service))
	return err
}

func (e *Env) Restart(ctx context.Context, service string) error {
	_, err := e.run(ctx, "docker", "restart", "-t", "0", Container(service))
	return err
}

// RestartReplicas restarts every payment container (fresh heaps and pools for each run).
func (e *Env) RestartReplicas(ctx context.Context) error {
	for _, s := range e.V.Replicas {
		if _, err := e.run(ctx, "docker", "restart", "-t", "2", Container(s)); err != nil {
			return err
		}
	}
	return e.WaitReady(ctx, 120*time.Second)
}

// ---- HTTP helpers -----------------------------------------------------------------------------------

func (e *Env) Do(ctx context.Context, method, url string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, err
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: bad json: %w (%s)", method, url, err, string(data))
		}
	}
	return resp.StatusCode, nil
}

// Toxiproxy helpers.
func (e *Env) ProxyEnable(ctx context.Context, name string, enabled bool) error {
	code, err := e.Do(ctx, "POST", e.ToxiURL+"/proxies/"+name, map[string]any{"enabled": enabled}, nil)
	if err == nil && code != 200 {
		err = fmt.Errorf("toxiproxy %s: HTTP %d", name, code)
	}
	return err
}

func (e *Env) AddToxic(ctx context.Context, proxy string, toxic map[string]any) error {
	code, err := e.Do(ctx, "POST", e.ToxiURL+"/proxies/"+proxy+"/toxics", toxic, nil)
	if err == nil && code != 200 {
		err = fmt.Errorf("toxiproxy add toxic %s: HTTP %d", proxy, code)
	}
	return err
}

func (e *Env) ClearToxics(ctx context.Context) error {
	var proxies map[string]struct {
		Toxics []struct {
			Name string `json:"name"`
		} `json:"toxics"`
	}
	if _, err := e.Do(ctx, "GET", e.ToxiURL+"/proxies", nil, &proxies); err != nil {
		return err
	}
	for name, p := range proxies {
		for _, t := range p.Toxics {
			if _, err := e.Do(ctx, "DELETE", e.ToxiURL+"/proxies/"+name+"/toxics/"+t.Name, nil, nil); err != nil {
				return err
			}
		}
		if err := e.ProxyEnable(ctx, name, true); err != nil {
			return err
		}
	}
	return nil
}

// Simulator admin.
func (e *Env) SimFault(ctx context.Context, sim string, fault map[string]any) error {
	code, err := e.Do(ctx, "POST", e.simURL(sim)+"/faults", fault, nil)
	if err == nil && code != 200 {
		err = fmt.Errorf("%s fault: HTTP %d", sim, code)
	}
	return err
}

func (e *Env) simURL(sim string) string {
	switch sim {
	case "cbs":
		return e.CBSURL
	case "aml":
		return e.AMLURL
	}
	return e.NPCURL
}

func (e *Env) SimLatency(ctx context.Context, sim string, ms float64) error {
	_, err := e.Do(ctx, "POST", e.simURL(sim)+"/config/latency", map[string]float64{"ms": ms}, nil)
	return err
}

func (e *Env) ClearSimFaults(ctx context.Context) error {
	for _, s := range []string{"cbs", "aml", "npc"} {
		if _, err := e.Do(ctx, "DELETE", e.simURL(s)+"/faults", nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// ---- databases ---------------------------------------------------------------------------------------

type dbInfo struct{ url string }

func (e *Env) dbURL(name string) string {
	switch name {
	case "app":
		return "postgres://bench:bench@localhost:5435/app?sslmode=disable&connect_timeout=5"
	case "cbs":
		return "postgres://bench:bench@localhost:5434/cbs?sslmode=disable&connect_timeout=5"
	case "dbos":
		return "postgres://dbos:dbos@localhost:5433/dbos_sys?sslmode=disable&connect_timeout=5"
	case "temporal":
		return "postgres://temporal:temporal@localhost:5432/temporal?sslmode=disable&connect_timeout=5"
	case "temporal_visibility":
		return "postgres://temporal:temporal@localhost:5432/temporal_visibility?sslmode=disable&connect_timeout=5"
	}
	panic("unknown db " + name)
}

// DB returns a pooled connection to one of app | cbs | dbos | temporal.
func (e *Env) DB(ctx context.Context, name string) (*pgxpool.Pool, error) {
	e.dbsMu.Lock()
	defer e.dbsMu.Unlock()
	if p := e.dbs[name]; p != nil {
		if p.Ping(ctx) == nil {
			return p, nil
		}
		p.Close()
		delete(e.dbs, name)
	}
	p, err := pgxpool.New(ctx, e.dbURL(name))
	if err != nil {
		return nil, err
	}
	e.dbs[name] = p
	return p, p.Ping(ctx)
}

// DurabilityDB is the engine's durability database name (dbos or temporal).
func (e *Env) DurabilityDB() string {
	return map[string]string{"dbos": "dbos", "temporal": "temporal"}[e.V.Engine]
}

// ---- resource sampling ---------------------------------------------------------------------------------

// Sample is one docker stats reading.
type Sample struct {
	At     time.Time
	Name   string
	CPU    float64 // percent of one core
	MemMiB float64
}

func parseMem(s string) float64 { // "12.5MiB / 1GiB"
	s = strings.TrimSpace(strings.SplitN(s, "/", 2)[0])
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "GiB"):
		mult, s = 1024, strings.TrimSuffix(s, "GiB")
	case strings.HasSuffix(s, "MiB"):
		s = strings.TrimSuffix(s, "MiB")
	case strings.HasSuffix(s, "KiB"):
		mult, s = 1.0/1024, strings.TrimSuffix(s, "KiB")
	case strings.HasSuffix(s, "B"):
		mult, s = 1.0/1048576, strings.TrimSuffix(s, "B")
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f * mult
}

// SampleStats reads docker stats once for every ibps container.
func (e *Env) SampleStats(ctx context.Context) ([]Sample, error) {
	out, err := e.run(ctx, "docker", "stats", "--no-stream", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var res []Sample
	now := time.Now()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var r struct{ Name, CPUPerc, MemUsage string }
		if json.Unmarshal([]byte(line), &r) != nil || !strings.HasPrefix(r.Name, "ibps-") {
			continue
		}
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(r.CPUPerc, "%"), 64)
		res = append(res, Sample{At: now, Name: strings.TrimSuffix(strings.TrimPrefix(r.Name, "ibps-"), "-1"), CPU: cpu, MemMiB: parseMem(r.MemUsage)})
	}
	return res, nil
}

// ResetTemporal returns the Temporal cluster to an empty state (like truncating the DBOS system tables):
// stop the server, truncate every workflow, task, shard and visibility table while keeping the schema,
// cluster metadata and the default namespace, then start it again and wait until it serves.
func (e *Env) ResetTemporal(ctx context.Context) error {
	if _, err := e.run(ctx, "docker", "stop", "-t", "3", Container("temporal")); err != nil {
		return err
	}
	keep := map[string][]string{
		"temporal":            {"schema_version", "schema_update_history", "cluster_metadata_info", "namespaces", "namespace_metadata", "cluster_membership", "queue_metadata"},
		"temporal_visibility": {"schema_version", "schema_update_history"},
	}
	for db, k := range keep {
		p, err := e.DB(ctx, db)
		if err != nil {
			return err
		}
		rows, err := p.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE' AND NOT (table_name = ANY($1))`, k)
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var t string
			rows.Scan(&t)
			tables = append(tables, `"`+t+`"`)
		}
		rows.Close()
		if len(tables) > 0 {
			if _, err := p.Exec(ctx, `TRUNCATE `+strings.Join(tables, ", ")+` CASCADE`); err != nil {
				return fmt.Errorf("truncate %s: %w", db, err)
			}
		}
	}
	if _, err := e.run(ctx, "docker", "start", Container("temporal")); err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", "localhost:7233", time.Second); err == nil {
			c.Close()
			time.Sleep(10 * time.Second) // shard acquisition after a cold start
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("temporal did not come back after reset")
}
