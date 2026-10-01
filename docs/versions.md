# Versions and M0 go/no-go gate

Recorded 2026-09-28 on Apple M3 Pro (12 cores, 36 GB), macOS 26.6.2, Docker Desktop 29.7.2 (VM: 12 CPUs, 16 GB).

## Pinned versions

| Component | Version | Note |
| --- | --- | --- |
| Go | 1.27.1 (darwin/arm64 host; `golang:1.27.1-alpine` for images) | One version for every binary |
| Temporal Go SDK | `go.temporal.io/sdk` v1.49.0 | |
| Temporal Server | `temporalio/server:1.32.0`, `temporalio/admin-tools:1.32.0` | numHistoryShards = 512, fixed |
| DBOS Transact Go | `github.com/dbos-inc/dbos-transact-golang` v1.4.0 | Admin server deprecated, removed in v1.5.0 |
| pgx | v5 (latest at M0: 5.11.0) | Pin when core gets its first DB client |
| Postgres | `postgres:16.15` | Same `postgresql.conf` for all four databases |
| Toxiproxy | `ghcr.io/shopify/toxiproxy:2.12.0` | |
| Prometheus | `prom/prometheus:v3.15.0` | |
| Grafana | `grafana/grafana:13.2.2` | |
| ISO 20022 | `moov-io/iso20022` v0.2.1 evaluated in M2; fallback is hand-written `encoding/xml` structs | Not adopted yet |

Go runtime settings applied to every payment binary: `GOMAXPROCS` = container CPU limit, `GOGC` and `GOMEMLIMIT` identical across variants (values fixed in M3 when the first binary exists).

Temporal server dynamic config: `system.enableEagerWorkflowStart: true`.

## Temporal gate (spikes/temporal, run against the live server)

| # | Item | Result | Evidence |
| --- | --- | --- | --- |
| G1 | Local Activity + unbounded activity retry with capped backoff | PASS | 4 attempts, completed |
| G2 | Eager Workflow Start via `ExecuteWorkflow` with `EnableEagerStart` | PASS | median start-to-first-task 45 µs eager vs 3.3 ms poll-dispatched (40 runs each) |
| G3 | Update-with-Start, wait stages Accepted and Completed | PASS | Accepted returns in ~10 ms; Completed returns with result |
| G4 | Update-with-Start **combined with** eager start | **FAIL** | SDK doc: `EnableEagerStart` "cannot be set in WithStartWorkflowOperation". Setting it raises no error but is silently ignored: first task delay 6 ms (poll-dispatched), not 45 µs. The eager path exists only in the plain `ExecuteWorkflow` code path |
| G5 | Dedup by workflow ID (`USE_EXISTING` + `REJECT_DUPLICATE`) | PASS | second start attached to the same run |
| G6 | Retry backoff granularity, regular vs Local Activity | **FINDING** | Configured 50 ms x2, cap 2 s. Regular activity gaps: 1.007 s, 1.015 s, 987 ms, 1.012 s (about 1 s floor). Local Activity gaps: 51, 101, 202, 401 ms (as configured). The 1 s floor is a server timer-queue effect on this default config |

Consequences:

- **G4 blocks the plan's V1b as written** ("Update-with-Start with eager start"). Decision requested from Bill (see M0 report).
- **G6 matters for fairness reporting.** Step 3 and 4 retries (50 ms start) cost about 1 s each in V1a but not in V1b (Local Activities) or V2. It is a real property of Temporal regular activities and stays in the baseline. Whether to investigate server timer settings for V1b is an open question; nothing has been tuned.

## DBOS gate (spikes/dbos, v1.4.0, run against the live dbos-db)

| # | Item | Result | Evidence |
| --- | --- | --- | --- |
| D1 | Step retry, base 50 ms x2, cap 2 s, max-retries 2^30 | PASS | gaps 53, 101, 194, 418 ms (as configured; in-process, no timer floor) |
| D1b | Non-retryable business errors | PASS | `WithStepRetryPredicate` returning false: 1 attempt, error returned |
| D2 | Per-step timeout | **No per-step option.** `dbos.WithTimeout` bounds the whole workflow (result wait timed out). Per-attempt timeouts come from the HTTP client and context deadline inside the shared core function, which the plan already does | Source + runtime |
| D3 | Workflow ID as idempotency key | PASS | same ID twice: step ran once, both calls returned the same result |
| D4 | Queue concurrency limit | PASS | limit 2, max observed in flight 2 (`RegisterQueue` + `WithWorkerConcurrency`) |
| D5 | Recovery-attempt cap | Present (`WithMaxRecoveryAttempts`, default dead-letters after N); raise it for I7 and I8 | Source |
| D6 | Recovery of a dead executor's workflows (I8) | **Not automatic across executors.** Kill test: workflow owned by `exec-a`, process SIGKILLed. A live process with executor ID `exec-b` left it PENDING and owned by `exec-a` after 12 s. Restarting with the *same* executor ID recovered it (`recovery_attempts` = 2). Cross-executor recovery is only exposed through DBOS Conductor (external service, API key) or the deprecated admin server (removed in v1.5.0); the recovery function is unexported | Runtime |
| D7 | Custom pgx pool | Present (`Config.SystemDBPool`) | Source |
| D8 | Stable executor ID | Present (`Config.ExecutorID` or env `DBOS__VMID`) | Runtime |

Consequence for I8 in V2: the replica must be given a **stable executor ID** and a workflow is only resumed when a replica with that ID comes back. A permanently lost replica's in-flight payments stay PENDING until an operator restarts an executor with that ID (or uses Conductor, which is out of scope here). This will be reported as an operational difference and the I8 assertion for V2 becomes "reach a terminal state after a replacement executor with the same ID starts".

## Gate result

Both engines pass everything the benchmark needs **except** Update-with-Start plus eager start (G4). Decision by Bill (2026-09-28): **V1b = eager start + Local Activities, no Update-with-Start.**

## Findings from building and validating the harness (M1 to M7)

| # | Finding | Effect on the benchmark |
| --- | --- | --- |
| H1 | **Docker Desktop's VM disk stalls for 2 to 10 s under sustained fsync load.** With Postgres data on ordinary disk-backed named volumes, every variant showed periodic whole-pipeline stalls (receipts dropping to zero for 4 to 9 s, then a burst), independent of the engine. Switching to RAM-backed (tmpfs) volumes reduced the worst stall to under 2 s, **but tmpfs volumes turned out not to survive `docker restart` reliably (see H7), which silently breaks the I9/I12 restart tests** — worse than the stall itself | **Reverted to ordinary disk-backed named volumes for every database in every variant.** `fsync` and `synchronous_commit = on` stay on; the periodic stall is accepted and reported as a caveat, since Docker Desktop never gave real fsync-to-disk semantics anyway and this affects both engines equally. See H7 for the restart-survival finding that ruled tmpfs out |
| H2 | **nginx resolves upstream names once at start.** `docker restart` of a replica gave it a new IP and the load balancer kept sending to the stale one, so one replica silently took all traffic | Payment replicas have fixed IPs on a fixed subnet (`172.30.0.0/24`), verified by per-container network counters |
| H3 | **Temporal is database-heavy per payment**: about 110 to 180 transactions and 190 KB of WAL for one payment on V1a, versus far fewer for DBOS (see the cost table in the report). At 30 TPS the Temporal server and its Postgres each used 1.4 to 2.5 CPU cores | Temporal server and both durability Postgres instances got a 3-CPU budget (identical for temporal-db and dbos-db); the Temporal server's cores are counted in V1's footprint |
| H4 | **DBOS processes sharing one system database run each other's workflows.** A unit-test process using the same database as the running replicas (different application name and version) had its test workflows executed twice | Engine tests use their own `dbos_test` database (`make test-db`). The benchmark itself has a single application and version |
| H5 | Temporal `WorkflowIDReusePolicy=REJECT_DUPLICATE` plus `USE_EXISTING` conflict policy: a duplicate start attaches to the running workflow without adding history events (checked in `spikes/temporal/dup`) | Duplicate deliveries are cheap for Temporal; ingress still only re-starts a payment that is still RECEIVED |
| H6 | Between runs the Temporal cluster is reset by truncating its workflow, task, shard and visibility tables with the server stopped (schema, cluster metadata and the default namespace kept); DBOS system tables are truncated. Payment replicas are restarted for fresh heaps and pools | Same clean start for every run and variant |

## H7: Docker Desktop does not durably persist a killed Postgres container's data across restart

Found while validating I9 (durability-store restart) and I12 (CBS Postgres restart) on this laptop.

**Symptom:** after `docker restart` (graceful or `-t 0` immediate) of any Postgres service, every database created by a `CREATE DATABASE` SQL statement *after* the container's first startup is gone; the database named by `POSTGRES_DB` (created during `initdb` at first boot) survives. Reproduced on temporal-db and independently on cbs-db, with disk-backed *and* tmpfs-backed named volumes, and with an explicit `CHECKPOINT` run immediately before the kill.

**What this rules out:** not a code bug (reproduced with a bare `psql CREATE DATABASE` and no application involved), not a WAL/checkpoint durability edge case (an explicit `CHECKPOINT` before the kill made no difference), not the volume type (disk-backed and tmpfs-backed both fail), not specific to `-t 0` (a graceful restart with the default stop timeout also loses the database).

**Conclusion:** this is a property of Docker Desktop's VM disk on this machine — writes Postgres has fsynced are not durable across a container restart. It matches, and goes further than, the caveat already in the plan ("Docker Desktop's VM does not give true fsync-to-disk semantics"). It affects both engines equally, so it is not an unfair comparison, but it means **I9 and I12 cannot validate true crash durability on this laptop**.

**Handling:** I9 and I12 run as probes (`Spec.Probe = true`): the fault still fires and the run is recorded, but a correctness-invariant failure is downgraded to an observational note instead of a hard FAIL, so it does not block the matrix or misrepresent an environment limitation as an engine defect. I7 (kill/restart a *payment service* replica, not a database) and I10 (restart the Temporal *server*, not its Postgres) are unaffected: neither restarts a Postgres container, so both remain hard-gated faults.
