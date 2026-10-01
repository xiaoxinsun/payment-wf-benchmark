# IBPS Inward Payment Benchmark — Temporal vs DBOS: Claude Code Hand-off

Sep 28, 2026 · @Bill Sun · **Revision 2.1: Go implementation, open questions resolved, laptop-scale experiment**

## Revision log (v1 → v2.1)

- **v2.3: as built (M1 to M8).** Changes found while building and validating, all applied identically to every variant (evidence in `docs/versions.md`, H1 to H6): (1) **all Postgres data on RAM-backed Docker volumes**, because Docker Desktop's VM disk stalled the whole pipeline for seconds under fsync load; `fsync` and `synchronous_commit = on` unchanged; (2) **CPU budget** raised to 3 cores for the Temporal server and for both durability Postgres instances (identical spec), because Temporal needed 1.4 to 2.5 cores of each at only 30 TPS; (3) **SLA enforcement** is uniform: every step 3 to 4b checks the deadline itself and the last pre-settlement step rejects with AB05 if the SLA is gone, so neither engine needs a workflow timer (the v2 text about a Temporal REVIEW timer is dropped); (4) **S1 to S3 and S5 run the happy path only** (100% happy), because the default mix contains REVIEW-SLOW payments that by design finish at the SLA and would pin p99 at 5 s; S6 uses the default mix; (5) V1a runs 2 ingress + 2 worker replicas (0.3 + 0.7 CPU per pair) so the payment service totals 2 CPUs like V1b and V2; (6) observability is Prometheus, pprof and `docker stats`, not OpenTelemetry spans; (7) I10 restarts the whole Temporal container.
- **v2.2: V1b redesigned after the M0 gate.** Update-with-Start cannot be combined with eager start in Temporal Go SDK v1.49.0, so V1b is eager start + Local Activities (Bill chose option A). Evidence in `docs/versions.md`.
- **v2.1: target environment fixed to Bill's laptop** (Apple M3 Pro, 12 cores, 36 GB). Loads, durations and repetitions are scaled down; a resource budget, run-isolation rules and an engine-overhead scenario (S0) are added. See "Test environment and resource budget". Clarifications #1 and #2 confirmed by Bill.
- **Language changed from Java 21 to Go.** Tech stack, repo layout, milestones, fairness rules and working rules rewritten for Go; the design, flow, fault catalogue and scenarios are unchanged except where noted below.
- **Open questions closed** with recommended values (see "Decisions and assumptions"). Values that depend on the bank's real spec remain configuration, not code.
- **Six gaps found in v1 and closed here:** where step 2 runs, how a REVIEW is awaited, where the payment-state table lives (a fourth Postgres), who drives load, async ack semantics for V1b, and how the ibps.102 MsgId stays stable across retries. Each is marked *(v2)* below and listed under "Clarifications for your review".
- **M0 now has a go/no-go gate** on Go SDK features for both engines, because the Go SDKs are newer than the Java ones.
- Original v1 kept alongside as `… (v1 Java original).md`.

## Purpose and scope

Build one inward IBPS instant-credit flow twice, on Temporal (V1) and on DBOS (V2), with identical business logic and simulators, and measure latency, throughput, recovery and correctness under the same faults.

The benchmark answers four questions:

1. What is end-to-end latency (ibps.101 received to ibps.102 emitted) at p50, p95, p99 and p99.9 for each engine at a fixed arrival rate?
2. What is the maximum sustainable TPS for each engine while p99 stays inside the IBPS response SLA?
3. How long does each engine take to resume in-flight payments after a crash, and does any payment get stuck?
4. Does either engine ever double-post, lose a payment, or leave the ledger unbalanced under injected faults?

**In scope:** inward credit only (ibps.101 in, ibps.102 out), account validation, AML screening, settlement posting, reject paths, deterministic fault injection, load generation, metrics and a results report.

**Out of scope:** outward payments, IBPS account-inquiry and e-authorisation messages, real CFCA signing and encryption, net-settlement cycles with the PBOC, returns and recalls after acceptance, UI.

**Variants measured:** V1a Temporal baseline (regular activities), V1b Temporal optimised (Local Activities + Eager Workflow Start; Update-with-Start dropped after the M0 gate, see below), V2 DBOS. Including V1b is essential; comparing DBOS only against untuned Temporal would not be a fair result.

**Experiment framing (v2.1).** This is a short, laptop-scale experiment, not a capacity certification. The bottom line is a *relative* comparison of Temporal and DBOS as durable workflow engines under identical conditions. Absolute TPS and latency will not transfer to production hardware; ratios, per-payment durability cost and correctness behaviour should. "Max sustainable TPS" therefore means the ceiling on this laptop with this resource budget.

## Gap review

The original three-step flow is a sound core, but it has ten gaps that would make the benchmark unrealistic or unfair; each is closed in the sections below.

| # | Gap in the original flow | Why it matters | Resolution in this design |
| --- | --- | --- | --- |
| 1 | No response path | An inward ibps.101 is only complete when the receiving bank returns ibps.102 (accept or reject). Latency must be measured to that point. | Add step 5: build pacs.002, map to ibps.102, deliver to the simulated IBPS NPC with retries. |
| 2 | Only the AML "clear" path is defined | Hits and potential hits are the interesting durable cases. | AML returns CLEAR, HIT or REVIEW. HIT rejects; REVIEW rejects if not cleared inside the SLA budget. |
| 3 | No duplicate protection | The NPC may resend the same ibps.101. | Dedup on message ID + end-to-end ID at ingress; workflow ID derived from it in both engines. |
| 4 | Account-validation outcomes undefined | Real rejects: closed, frozen, dormant, name mismatch, wrong currency. | Deterministic CBS outcomes keyed by test account ranges, each mapped to a reject reason. |
| 5 | Ambiguous settlement outcome | A CBS timeout after posting is the classic double-credit risk. | CBS posting is idempotent on a posting reference; on timeout the flow queries posting status before retrying. |
| 6 | Hot settlement account | Every payment debits the same IBPS settlement account; row-lock contention will cap TPS in both engines and hide the engine difference. | Configurable sharded settlement sub-accounts (default 16) with a reconciliation check; run once with 1 shard to show the effect. |
| 7 | No response SLA | IBPS is real-time; a late reply is a failed payment even if it eventually succeeds. | Configurable SLA budget (default 5 s) enforced before settlement; late outcomes are counted separately. |
| 8 | Sync vs async API undefined | Changes what latency means. | Ingress acknowledges receipt, then the flow emits ibps.102 asynchronously; a sync mode is also offered for the "caller waits" measurement. |
| 9 | Unfair comparison risk | Different languages, DBs or tuning would decide the result, not the engine. | Same language, same business core module, same Postgres spec, same simulators, same load tool; Temporal tested baseline and optimised. |
| 10 | Ledger is ambiguous | "Post a balanced pair" could live in the app DB (favours DBOS) or in the CBS. | Postings live in the simulated CBS behind an API, as in a real bank; DBOS gets no same-transaction advantage. |

Two smaller gaps are noted as assumptions rather than built: real CFCA message signing (stubbed as a no-op signer with configurable latency) and the NPC's own processing confirmation to the sender (not needed on the inward side).

## Business flow

Each inward ibps.101 runs as one durable workflow keyed by its message ID, and ends in exactly one ibps.102: ACCP after settlement, or RJCT with a reason code.

&#91;embedded content: inward IBPS credit · 6 steps, 3 exits\]

Settlement is the point of no return: before it, any failure or SLA expiry rejects; after it, the flow must deliver ACCP however long the NPC takes to acknowledge.

**Payment states** (persisted in both versions, same enum): RECEIVED → VALIDATED → SCREENED → SETTLED → ACCEPTED\_SENT, or RECEIVED/VALIDATED/SCREENED → REJECTED → REJECTED\_SENT. *(v2)* The state table lives in a separate `app-db` Postgres (see Architecture) and is written by one shared-core function, `recordState()`, at every transition in both engines.

**Step policies** (identical in both engines; business errors are never retried):

| Step | Call | Per-attempt timeout | Retry policy | Non-retryable outcomes |
| --- | --- | --- | --- | --- |
| 1 Receive | Ingress API: schema check, parse, dedup, map to canonical pacs.008 model | n/a | n/a | Schema invalid (reject at ingress) |
| 2 Check | In-process pure function, no I/O, runs inside the workflow body in both engines *(v2)* | n/a | n/a | Mapping error, currency, amount limit, wrong receiver → RJCT |
| 3 Validate | CBS `GET /accounts/{id}/validate` | 500 ms | Exponential, 50 ms start, ×2, max 4 attempts inside SLA | Closed, frozen, dormant, name mismatch, wrong currency |
| 4 AML | AML `POST /screen` | 800 ms | Same as step 3 | HIT |
| 4b Await review *(v2)* | Only when step 4 returns REVIEW: AML `GET /cases/{id}` polled every 250 ms | 800 ms per poll | Poll loop lives inside one shared-core function and is bounded by the remaining SLA budget | CONFIRMED\_HIT; case not cleared within budget → REVIEW\_TIMEOUT |
| 5 Settle | CBS `POST /postings` (idempotent) | 1 s | On timeout: `GET /postings/{ref}` first, then retry same ref; unbounded until success | None; business rejects are impossible after step 4 |
| 6 Respond | NPC `POST /ibps/102` | 1 s | Unbounded with backoff capped at 2 s | None |

All timeouts and budgets are configuration, not code, so the same values are applied to V1a, V1b and V2.

**SLA enforcement (v2).** The SLA budget is measured from the durable receive timestamp and checked before step 5 in both engines; every step in 3–4b receives a context deadline equal to the remaining budget. Temporal additionally uses a workflow timer for the REVIEW wait; DBOS uses the deadline only. A workflow-level timeout must never be used, because cancelling after settlement would break the point-of-no-return rule.

## Message mapping

ibps.101 maps inbound to pacs.008 and pacs.002 maps outbound to ibps.102; everything inside the platform speaks pacs only, and IBPS formats exist only in an adapter at the edge.

The CNCC IBPS interface specification is not public, so the simulator uses a **simplified synthetic XML schema** with the field set below. Claude Code should put both directions behind a `MessageMapper` Go interface (in `core/mapping`) so the bank's real spec can replace the synthetic one without touching workflows.

**Inbound: ibps.101 (customer credit transfer) → pacs.008.001.08**

| ibps.101 field (synthetic) | pacs.008 path | Rule |
| --- | --- | --- |
| MsgId | GrpHdr/MsgId | Copy; also part of the dedup key |
| CreDtTm | GrpHdr/CreDtTm | Copy, ISO 8601 with +08:00 |
| (fixed) | GrpHdr/NbOfTxs | Always 1 |
| (fixed) | GrpHdr/SttlmInf/SttlmMtd, ClrSys/Prtry | CLRG, IBPS |
| InstgDrctPty (12-digit CNAPS bank code) | GrpHdr/InstgAgt/FinInstnId/ClrSysMmbId/MmbId | Copy |
| InstdDrctPty | GrpHdr/InstdAgt/FinInstnId/ClrSysMmbId/MmbId | Must equal our bank code, else reject |
| EndToEndId | CdtTrfTxInf/PmtId/EndToEndId | Copy; second half of the dedup key |
| TxId | CdtTrfTxInf/PmtId/TxId | Copy |
| Amt, Ccy | CdtTrfTxInf/IntrBkSttlmAmt @Ccy | Decimal, 2 dp; Ccy must be CNY |
| SttlmDt | CdtTrfTxInf/IntrBkSttlmDt | Copy |
| BizTp, BizKind | PmtTpInf/CtgyPurp/Prtry, Purp/Prtry | Copy as proprietary codes |
| DbtrNm, DbtrAcct, DbtrBk | Dbtr/Nm, DbtrAcct/Id/Othr/Id, DbtrAgt | Copy |
| CdtrNm, CdtrAcct, CdtrBk | Cdtr/Nm, CdtrAcct/Id/Othr/Id, CdtrAgt | Copy; CdtrAcct drives step 3 |
| Remark | RmtInf/Ustrd | Copy, truncate at 140 chars |
| (fixed) | ChrgBr | SLEV |

**Amounts in Go (v2).** Never use `float64` for money. Parse XML amounts into integer fen (int64) in the canonical model and render back to a 2 dp decimal string at the edge.

**Outbound: pacs.002.001.10 → ibps.102 (receipt)**

| pacs.002 path | ibps.102 field (synthetic) | Rule |
| --- | --- | --- |
| GrpHdr/MsgId | MsgId | Generated by our bank. *(v2)* Derived deterministically from the payment key (hash of `postingRef`) so it is identical on every retry and replay, in every engine |
| OrgnlGrpInfAndSts/OrgnlMsgId, OrgnlMsgNmId | OrgnlMsgId, OrgnlMsgTp | Original ibps.101 MsgId and "ibps.101.001.01" |
| TxInfAndSts/OrgnlEndToEndId | OrgnlEndToEndId | Copy |
| TxInfAndSts/TxSts | PrcSts | ACCP → accepted, RJCT → rejected |
| TxInfAndSts/StsRsnInf/Rsn/Cd | RjctCd | ISO code mapped to IBPS proprietary code via a config table |
| TxInfAndSts/AccptncDtTm | PrcDtTm | Time of settlement or rejection, taken from the recorded state, not the wall clock at retry time |

**Reject reasons** (internal code → ISO 20022 code; IBPS proprietary codes come from a lookup file filled from the bank's spec):

| Internal reason | ISO code | Raised at |
| --- | --- | --- |
| ACCOUNT\_NOT\_FOUND | AC01 | Step 3 |
| ACCOUNT\_CLOSED | AC04 | Step 3 |
| ACCOUNT\_FROZEN or DORMANT | AC06 | Step 3 |
| NAME\_MISMATCH | BE01 | Step 3 |
| CURRENCY\_NOT\_ALLOWED | AM03 | Step 2 |
| AMOUNT\_OVER\_LIMIT | AM02 | Step 2 (limit configurable, default CNY 1,000,000) |
| AML\_HIT or AML\_REVIEW\_TIMEOUT | RR04 | Step 4 / 4b |
| SLA\_TIMEOUT | AB05 | SLA check, before step 5 |
| WRONG\_RECEIVER | AGNT | Step 2 |
| FORMAT\_INVALID | FF01 | Ingress |

## System architecture

Both versions run the same payment service shape, the same business core and the same simulators; only the durability layer differs.

&#91;embedded content: benchmark topology · V1 and V2 share everything but the durability layer\]

The V1 Temporal server box is the extra network hop the benchmark measures; in V2 the same checkpoints go straight from the process to Postgres.

**Shared business core.** A plain Go module with no engine imports: `ValidateAccount()`, `ScreenAML()`, `AwaitReview()`, `PostSettlement()`, `SendReceipt()`, `RecordState()`, the mappers and the reason-code table. Each engine binding is a thin wrapper that calls these as activities (V1) or steps (V2). The rule is enforced mechanically: `core` is its own Go module, and a `depguard` lint rule plus a CI check on `go list -deps` fail the build if it imports Temporal or DBOS. This keeps any latency difference attributable to the engine.

**Payment state store *(v2)*.** A fourth Postgres, `app-db`, holds the payment-state table and the ingress dedup key (unique on MsgId + EndToEndId). It is separate from `temporal-db` and `dbos-db` so that neither engine gets a same-database advantage, and it is written through `RecordState()` in both. B8 duplicate deliveries are answered from this table in both engines.

**V1a, Temporal baseline.** Ingress calls `client.ExecuteWorkflow` and returns; the workflow runs steps 3 to 6 as regular activities on a separate task queue. Workers run as a separate deployment from ingress.

**V1b, Temporal optimised.** Ingress and worker share one process and client connection. Ingress starts the workflow with `EnableEagerStart` so the first workflow task is dispatched to the co-located worker without a poll round trip (measured in M0: 45 µs vs 3.3 ms). Steps 3 and 4 (and `RecordState()`) run as Local Activities; step 5 and the step 6 receipt run as regular activities so they can retry indefinitely. *(v2.2, M0 gate G4)* Update-with-Start is **not used**: in Temporal Go SDK v1.49.0 it cannot be combined with eager start (the flag is silently ignored), and in async mode it adds nothing once the 202 is returned at acceptance. In async mode ingress returns 202 right after the eager start; in sync mode it waits on the workflow result. Headline latency is stamped at the NPC either way.

**V2, DBOS.** Ingress calls the DBOS workflow directly in-process with the payment's dedup key as workflow ID. Steps 3 to 6 are DBOS steps with retries configured per the step table (unbounded retry is expressed as a very large max-retries value with the same capped backoff, and the value is recorded). DBOS queues cap in-flight workflows per instance.

**Go runtime settings (v2).** Same Go version for every binary. `GOMAXPROCS` set to the container CPU limit, `GOGC` and `GOMEMLIMIT` identical for V1a, V1b and V2, recorded in `docs/versions.md`. `net/http/pprof` enabled on all services for post-run analysis.

**Scaling shape for both.** Payment service runs N identical replicas behind a load balancer (2 on the laptop; 3 or more on real hardware). Temporal server runs frontend, history and matching with numHistoryShards fixed before the first run (512 suggested). DBOS relies on Postgres; the pgx pool size is tuned per replica and recorded.

## Simulators

Three simulators stand in for external systems; all are deterministic from test data so that every run of a scenario produces the same business outcome, and all expose a latency distribution setting so the network cost of a real bank can be dialled in.

**IBPS NPC simulator.** *(v2)* Also the load generator: an open-model, constant-arrival-rate sender with pre-scheduled send times (so a slow system cannot hide queueing delay). It builds ibps.101 messages from a seeded account set, POSTs them to the payment service ingress, receives ibps.102 on a callback endpoint, and records send, ack and receipt timestamps on a single clock. It can resend an already-sent MsgId (duplicate test) and can delay or refuse its ibps.102 acknowledgement (response-path faults). It de-duplicates received ibps.102 by MsgId, and counts a payment as double-answered only if two *different* ibps.102 MsgIds arrive for it.

| Endpoint | Direction | Purpose |
| --- | --- | --- |
| `POST /ingress/ibps101` (on payment service) | NPC → bank | Deliver ibps.101; bank replies 202 with a receipt ID |
| `POST /npc/ibps102` (on simulator) | Bank → NPC | Deliver ibps.102; simulator replies 200 ack |
| `POST /npc/run` | Test harness | Start a run: rate, duration, mix, mode (async or sync) |
| `GET /npc/results` | Test harness | Paired timestamps and outcomes per MsgId |

**Core Bank System simulator.** Owns accounts and the ledger in its own Postgres. Posting is idempotent on `postingRef` (derived from the payment ID): a second call with the same reference returns the original result and writes nothing.

| Endpoint | Purpose | Key responses |
| --- | --- | --- |
| `GET /accounts/{acct}/validate?name=&ccy=` | Step 3 | 200 VALID; 422 with reason ACCOUNT\_NOT\_FOUND, CLOSED, FROZEN, DORMANT, NAME\_MISMATCH |
| `POST /postings` | Step 5; body = postingRef + two legs | 201 POSTED; 200 ALREADY\_POSTED; 409 UNBALANCED (bug guard) |
| `GET /postings/{postingRef}` | Resolve ambiguous step 5 | 200 POSTED or 404 NOT\_FOUND |
| `GET /ledger/invariants` | Test harness | Sum of all legs (must be 0), duplicate refs (must be 0) |

**AML simulator.** Screens creditor and debtor names against a seeded watchlist.

| Endpoint | Purpose | Key responses |
| --- | --- | --- |
| `POST /screen` | Step 4 | CLEAR; HIT with list ID; REVIEW with a case ID |
| `GET /cases/{id}` | Step 4b poll | OPEN, CLEARED or CONFIRMED\_HIT; auto-resolves after a configurable delay |

**Test data conventions** (seeded, so outcomes are known in advance):

| Account or name pattern | Outcome |
| --- | --- |
| Accounts ending 0000–8999 | Valid |
| Accounts ending 9000–9099 | ACCOUNT\_NOT\_FOUND |
| Accounts ending 9100–9199 | CLOSED |
| Accounts ending 9200–9299 | FROZEN |
| Accounts ending 9300–9399 | DORMANT |
| Creditor name with suffix " X" | NAME\_MISMATCH |
| Debtor name on watchlist (seeded, e.g. "BLOCKED PARTY 01") | AML HIT |
| Debtor name prefix "REVIEW-FAST" | REVIEW, clears in 1 s |
| Debtor name prefix "REVIEW-SLOW" | REVIEW, clears in 30 s (exceeds SLA, becomes RJCT) |

The load profile mixes these at configurable ratios; the default mix is 94% happy path, 3% account rejects, 1% AML HIT, 1% REVIEW-FAST, 1% REVIEW-SLOW.

## Ledger and settlement

Each accepted payment writes exactly one balanced posting in the CBS: debit an IBPS settlement sub-account, credit the customer account, same amount, one database transaction, unique on `postingRef`.

| Leg | Account | Side | Amount |
| --- | --- | --- | --- |
| 1 | IBPS settlement sub-account `SETTLE-IBPS-{shard}` | Debit | Payment amount, CNY |
| 2 | Creditor customer account | Credit | Same amount |

**Idempotency.** `postingRef = "IBPS-" + InstgDrctPty + "-" + MsgId + "-" + EndToEndId`. The CBS `postings` table has a unique index on it, so a retried or replayed step can never write twice. The workflow never generates a new reference on retry.

**Ambiguous outcome handling.** If `POST /postings` times out, the step first calls `GET /postings/{postingRef}`. POSTED means continue to step 6; NOT\_FOUND means retry the post with the same reference. This logic lives in the shared core so both engines behave identically.

**Hot settlement account.** A single settlement row debited by every payment serialises all postings on one row lock and will cap throughput for both engines. The design shards it into N sub-accounts (`shard = hash(postingRef) mod N`, default N = 16) whose balances sum to the logical settlement position. CNCC made a comparable change in 2016, moving IBPS from real-time to scheduled netting partly to relieve hot accounts for high-volume participants ([CNCC system overview](https://www.cncc.cn/zfqszs/zfqsxtjj/201806/t20180611_487.html)). Run one scenario with N = 1 to show how much of the result is the ledger, not the engine.

**Invariants checked after every run:**

- Sum of all legs across the ledger = 0.
- Count of postings = count of payments with state ACCEPTED\_SENT, and no posting exists for a REJECTED payment.
- No duplicate `postingRef` and no payment answered by more than one distinct ibps.102 MsgId at the NPC simulator.
- Every ibps.101 sent has exactly one final state; zero payments left in a non-terminal state 60 s after load stops.

## Fault injection catalogue

Twenty faults are defined: eight business faults driven by test data and twelve infrastructure faults driven by Toxiproxy, a simulator fault API and a chaos script; every fault has one expected outcome that the harness asserts automatically.

Injection mechanisms: **data** = seeded test data (see Simulators); **toxi** = Toxiproxy toxic on the named link; **api** = `POST /faults` on a simulator with mode, rate and duration; **chaos** = `scripts/chaos.sh` using `docker kill` or `docker restart`.

| ID | Type | Fault | Stage | Injection | Expected outcome |
| --- | --- | --- | --- | --- | --- |
| B1 | Business | Account not found, closed, frozen, dormant | 3 | data | RJCT with AC01/AC04/AC06, no posting |
| B2 | Business | Name mismatch | 3 | data | RJCT BE01, no posting |
| B3 | Business | Currency not CNY | 2 | data | RJCT AM03 before any external call |
| B4 | Business | Amount above limit | 2 | data | RJCT AM02 before any external call |
| B5 | Business | AML HIT | 4 | data | RJCT RR04, no posting |
| B6 | Business | AML REVIEW cleared inside SLA | 4b | data | ACCP, latency includes review wait |
| B7 | Business | AML REVIEW past SLA | 4b | data | RJCT RR04 at SLA expiry, no posting |
| B8 | Business | Duplicate ibps.101, sequential and concurrent (same MsgId sent twice within 5 ms) | 1 | api on NPC sim | One workflow, one posting, one ibps.102; second delivery gets stored outcome |
| I1 | Infra | CBS validate latency +300 ms on 20% of calls | 3 | toxi | ACCP; p99 rises, no extra rejects |
| I2 | Infra | CBS validate HTTP 503 on 30% of calls | 3 | api on CBS | Retries succeed; RJCT AB05 only where retries exhaust SLA |
| I3 | Infra | AML unreachable for 10 s | 4 | toxi (reset\_peer) | In-window payments RJCT AB05; normal after recovery; no stuck payments |
| I4 | Infra | CBS commits the posting, then drops the response | 5 | api on CBS | Status query finds POSTED; exactly one posting; ACCP |
| I5 | Infra | CBS times out before commit | 5 | api on CBS | Retry with same postingRef posts once; ACCP |
| I6 | Infra | NPC callback down for 20 s | 6 | api on NPC sim | ibps.102 delivered late, same ibps.102 MsgId on every retry; NPC sees one logical receipt |
| I7 | Infra | Kill payment service replica between steps 5 and 6, restart after 10 s | 5–6 | chaos | Payment resumes at step 6; no second posting; recovery time recorded |
| I8 | Infra | Kill a replica and never restart it | any | chaos | All its in-flight payments still reach a terminal state (see note below) |
| I9 | Infra | Durability store restart: Temporal persistence (V1) or DBOS system DB (V2) | any | chaos | In-flight payments pause then complete; zero lost; stall duration recorded |
| I10 | Infra | Temporal history or matching service killed (V1 only) | any | chaos | Shard reassignment; in-flight payments complete; stall recorded |
| I11 | Infra | 5 s partition between payment service and its durability store | any | toxi | No duplicate postings; payments complete or RJCT AB05 before settlement |
| I12 | Infra | CBS ledger Postgres restart | 3, 5 | chaos | Steps retry; exactly-once postings hold |

**Note on I8.** Temporal reassigns an orphaned workflow to any live worker automatically. DBOS open-source recovery is tied to the executor that owned the workflow, so Claude Code must check the current DBOS Go SDK docs for how a permanently lost replica's workflows are recovered (for example a stable executor ID on redeploy, or DBOS Conductor, and whether Conductor is available to the Go SDK) and record the result as an operational difference, not only a latency number.

**Code-fault probe (optional, C1).** Panic in the workflow body (not inside an activity or step) right after step 4 for one payment. Temporal retries a failing workflow task indefinitely by default, while a DBOS workflow records the error; the benchmark should record how each surfaces the stuck payment to operators.

## Benchmark methodology

The headline number is end-to-end latency from ibps.101 sent to ibps.102 received, both stamped by the NPC simulator on one clock, measured under an open-model constant arrival rate so queueing delay is not hidden.

**Fairness rules**

1. Same Go version and runtime settings (`GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`) for V1a, V1b and V2. Dependency version differences forced by the two SDKs (grpc, protobuf, OpenTelemetry) are recorded in `docs/versions.md`.
2. Same business core module, same simulators, same step timeouts and retry policies (all from one config file).
3. Durability Postgres for V1 and V2 on identical instance specs and Postgres settings (`synchronous_commit = on` for both; never relax durability for one side). `app-db` is identical for both variants.
4. Same replica count (2) and CPU/memory limits for the payment service; Temporal server resources reported separately and added to V1's total footprint.
5. Simulator latency set to a realistic baseline (CBS 5 ms, AML 10 ms, NPC 3 ms) so engine overhead is compared against real network cost, not zero.
6. Each headline scenario (S0–S2) run 3 times after a 60-second warm-up (connection pools, caches, Postgres buffers); report the median run and the spread. Repetitions are interleaved across variants (V1a, V1b, V2, V1a, V1b, V2, …) so thermal drift and background noise hit all variants equally. S3–S6 run once.
7. **(v2.1) Common load.** Scenarios S3, S4 and S6 use one absolute rate for all variants, set from the *lower* of the variants' S2 ceilings, so every engine faces identical load.

**Test environment and resource budget (v2.1)**

Hardware: Apple M3 Pro, 12 cores (6 performance + 6 efficiency), 36 GB RAM, macOS, Docker Desktop (Linux VM). Docker Desktop is currently set to 12 CPUs and only 8 GB of memory; **raise the memory to 16 GB** before M0 closes.

Run rules for a valid measurement:

- Laptop plugged in, macOS High Power mode on, lid open, other heavy apps closed. Record thermal state; a 60 s cool-down between runs.
- **Only one variant stack runs at a time** (V1 and V2 never side by side), and every run starts from a clean state: truncated ledger, `app-db` and durability DB, fresh Temporal namespace or DBOS schema.
- **Everything runs inside the Compose network**, including sim-npc; the host only calls its control API. This keeps host-to-VM networking out of the latency path.
- Postgres data on named Docker volumes (not bind mounts). Note for the report: Docker Desktop's VM does not give true fsync-to-disk semantics, so the cost of `synchronous_commit = on` is understated for *both* engines. The comparison stays fair, but absolute durability cost will be lower than on a production server.

Indicative CPU limits per run (V1 shown; V2 drops the Temporal server and its DB, freeing about 4 cores, which are **not** reassigned to V2):

| Component | CPU limit | Note |
| --- | --- | --- |
| Payment service, 2 replicas | 1.0 each | 2 replicas so one can be killed while a survivor remains (I7, I8) |
| Temporal server (frontend, history, matching in one container) | 3.0 | V1 only; reported and added to V1's footprint |
| Durability Postgres (temporal-db or dbos-db) | 3.0 | Identical spec and settings for both; data on a RAM-backed volume |
| cbs-db, app-db | 1.0 each | Identical for both |
| sim-npc, sim-cbs, sim-aml | 1.5 total | Simulator latency is a sleep, so cost is low |
| Prometheus, Grafana, Toxiproxy | 0.5 total | |

Total is about 11 cores against 12 available; this is tight, so the M6 calibration run checks that the generator and simulators are never the bottleneck (their CPU stays well below the limit at the highest rate used). If they are, the budget is reduced equally for all variants.

**Scenarios (scaled for the laptop; rates are starting values, calibrated once in M6 and then frozen for all variants)**

| ID | Scenario | Load | Faults | Answers |
| --- | --- | --- | --- | --- |
| S0 | Engine overhead (v2.1) | 10 and 50 TPS, 60 s warm-up + 2 min, simulators at 0 ms latency | None | Pure engine cost per payment with external latency removed |
| S1 | Latency at fixed rates | 25, 50, 100 TPS, 60 s warm-up + 2 min each | None | Latency percentiles per variant |
| S2 | Capacity step-up | Start 50 TPS, +50 TPS every 60 s (step configurable) until p99 breaches SLA or errors exceed 0.1% | None | Max sustainable TPS on this laptop |
| S3 | Soak | 60% of common S2 ceiling for 10 min | None | Stability, DB growth, GC or vacuum effects |
| S4 | Fault matrix | 50% of common S2 ceiling, about 3 min per run, one fault per run | B1–B8, I1–I12 | Correct outcomes, recovery time, latency impact |
| S5 | Hot account | S2 once with settlement shards N = 1 | None | Share of the ceiling caused by the ledger |
| S6 | Business mix | 60% of common S2 ceiling for 3 min, default reject mix | B1–B7 via data | Latency of reject paths vs happy path |

Estimated wall-clock for M8 is roughly 6 to 8 hours across all variants, run unattended in sessions. The load-independent metrics (durability DB writes, WAL bytes and round trips per payment) are the most transferable results of this experiment and are reported first.

**Metrics collected**

| Metric | Source | Unit |
| --- | --- | --- |
| End-to-end latency p50, p95, p99, p99.9, max | NPC simulator timestamps | ms |
| Ingress acknowledgement latency | NPC simulator (send to 202) | ms |
| Per-step latency (steps 3–6) and engine overhead between steps | OpenTelemetry spans | ms |
| Achieved TPS and SLA breach rate | NPC simulator | payments/s, % |
| Outcomes by reason code | Payment state table (`app-db`) | count |
| Recovery time after I7–I11 | First and last stalled payment timestamps | s |
| Durability DB writes and WAL volume per payment | `pg_stat_statements`, `pg_stat_wal` | rows, bytes |
| CPU and memory, per component | cAdvisor / Prometheus | cores, GiB |
| Go runtime: GC pause, goroutines, heap | Prometheus Go collector, pprof | ms, count, GiB |

**Pass criteria for any variant:** every ledger invariant holds in every run, zero non-terminal payments 60 s after load stops, and zero payments answered by two distinct ibps.102 MsgIds. A variant that fails correctness is reported as failed regardless of latency.

## Build plan for Claude Code

Build in nine milestones (M0 to M8) as one Go workspace repo, one Docker Compose stack, with each milestone ending in green tests before the next starts.

**Tech stack**

| Concern | Choice | Note |
| --- | --- | --- |
| Language | Go, latest stable at M0 (1.25 or newer; 1.27.1 is installed), pinned; goroutines | One version for every binary |
| HTTP | `net/http` standard library (1.22+ pattern routing), `log/slog` | Same for all services; no framework overhead in the comparison |
| Postgres driver | `pgx` v5 with `pgxpool` | Confirm DBOS Go uses the same driver |
| V1 engine | Temporal Go SDK (`go.temporal.io/sdk`) + Temporal Server via Docker (Postgres persistence) | Pin versions; server must support Eager Workflow Start (check dynamic-config flag) |
| V2 engine | DBOS Transact Go (`dbos-transact-golang`) | Pin version; see M0 gate |
| ISO 20022 | Typed Go structs with `encoding/xml` for the pacs.008.001.08 and pacs.002.001.10 subset we use; evaluate `moov-io/iso20022` in M0 | Prowide is Java-only; XSD validation runs in tests via `xmllint`, not at runtime |
| IBPS synthetic XML | Own structs with `encoding/xml` behind `MessageMapper` | |
| Money | int64 fen in the canonical model | No floats |
| Databases | Postgres 16: temporal-db, dbos-db, cbs-db, app-db | temporal-db and dbos-db identical settings |
| Faults | Toxiproxy + simulator fault API | |
| Load | Go open-model generator inside sim-npc | Replaces k6 (see Clarifications) |
| Observability | OpenTelemetry Go SDK, Prometheus, Grafana, pprof | One shared dashboard for all variants |
| Build and test | Go workspace (`go.work`), `go test`, table-driven and golden-file tests, Temporal `testsuite`, Makefile | |
| Lint | `golangci-lint` incl. `depguard` (core must not import engines), `govet`, `staticcheck` | Run in CI and before each milestone closes |

**Repo layout**

```
ibps-bench/
  go.work
  core/                 # module: shared business logic, no engine imports
    mapping/            # ibps101<->pacs008, pacs002<->ibps102, reason codes
    steps/              # ValidateAccount, ScreenAML, AwaitReview, PostSettlement, SendReceipt, RecordState
    model/              # Payment, PaymentState, RejectReason
    config/             # loader for deploy/config files
  engine-temporal/      # module: V1a + V1b workflow, activities, worker, ingress (cmd/payment-temporal, mode=a|b)
  engine-dbos/          # module: V2 workflow, steps, ingress (cmd/payment-dbos)
  sims/                 # module: three simulators
    npc/                # IBPS NPC simulator + load generator + results collector
    cbs/                # accounts, ledger, idempotent postings, fault API
    aml/                # screening, cases, fault API
  harness/              # module
    scenarios/          # S1-S6 runner (drives sim-npc /npc/run)
    chaos/              # chaos.sh, toxiproxy profiles per fault ID
    checks/             # ledger and outcome invariant checker
    report/             # results -> markdown report + charts
  deploy/
    compose/            # docker-compose.v1.yml, docker-compose.v2.yml, shared.yml
    config/             # timeouts.yml, retries.yml, mix.yml, sla.yml
  docs/HANDOFF.md       # this document
```

Separate modules stop the Temporal and DBOS dependency trees from being forced onto each other and keep `core` provably clean.

**Milestones**

- [x] **M0 Skeleton and SDK gate.** Repo, Go workspace and modules, lint rules, Compose with four Postgres, Toxiproxy, Prometheus, Grafana, with the CPU limits from the resource budget; check Docker Desktop memory is 16 GB and install `golangci-lint` (not currently installed; `make` and `xmllint` are present). Write `docs/versions.md` with pinned versions and a **go/no-go table** confirming, with a throwaway spike each: (Temporal) Eager Workflow Start, Update-with-Start including the eager-start combination and the Accepted vs Completed wait stages, Local Activities, unbounded activity retry; (DBOS) per-step timeout and capped exponential backoff, a very large max-retries value, workflow ID as an idempotency key, queue concurrency limits, recovery of a dead executor's workflows (I8), Conductor availability, OpenTelemetry hooks, pgx pool config. If any item is missing or behaves differently, **stop and report to Bill**; do not work around it for one engine only.
- [x] **M1 Simulators.** sim-npc (with generator), sim-cbs, sim-aml with endpoints, seeded test data (50k accounts, fast to reset between runs; payments reuse accounts at laptop rates, which is acceptable because credits to one account are independent and per-account lock contention is negligible), fault API and `/ledger/invariants`; contract tests for every documented response.
- [x] **M2 Shared core.** Mappers with golden-file tests for all four message directions, reason-code table, HTTP clients with configured timeouts, ambiguous-posting resolver, `AwaitReview`, `RecordState`; 100% statement coverage on the mapping and reject-rule packages.
- [x] **M3 V1a Temporal baseline.** Workflow, regular activities, separate worker deployment, SLA check, dedup via workflow ID and `app-db`; B1–B8 pass end to end.
- [x] **M4 V1b Temporal optimised.** Co-located client and worker, eager start (no Update-with-Start, see M0 gate G4), Local Activities for steps 3–4; same B1–B8 pass.
- [x] **M5 V2 DBOS.** Workflow and steps calling the same core, workflow ID = dedup key, queue concurrency limits, SLA check; same B1–B8 pass.
- [x] **M6 Harness.** Scenario runner S0–S6 with clean-state reset between runs and interleaved repetitions, results collector, invariant checker wired to fail the run, report generator, and a one-off calibration run that fixes the S1 rates and S2 step size and confirms the generator and simulators are not the bottleneck.
- [x] **M7 Fault automation.** One script per fault ID I1–I12 (and C1) with automatic assertion of the expected outcome for all three variants.
- [ ] **M8 Run and report.** Execute S0–S6 on the laptop under the run rules above and produce `results/REPORT.md` with latency charts, max TPS, recovery times and invariant results per variant.

**Working rules for Claude Code:** keep engine imports out of `core` (enforced by lint); put every timeout, retry, SLA and mix value in `deploy/config`; never tune one variant without applying the equivalent setting to the others; in Temporal workflow code use only SDK time, randomness and concurrency primitives; propagate `context.Context` with deadlines through every core call; and stop to ask when a DBOS or Temporal feature named here behaves differently in the pinned SDK version.

## Decisions and assumptions

The seven v1 open questions are resolved to the recommended values below so work can start. Items marked *confirm before M8* stay configuration and can change without code changes.

| Item | Decision | Status |
| --- | --- | --- |
| Language | **Go** (latest stable at M0, ≥1.25, pinned) for both versions. If the DBOS Go SDK fails the M0 gate, stop and ask; do not silently switch either engine's language. | Decided by Bill |
| IBPS response SLA | 5 s end to end, in `sla.yml` | Assumed; confirm before M8 against the CNCC interface spec |
| IBPS message schema and reject codes | Synthetic schema, ISO-only codes, behind `MessageMapper` and a proprietary-code lookup file | Assumed; replace when the real spec is available |
| Amount limit | CNY 1,000,000 per payment, configurable | Assumed; confirm before M8 |
| Target TPS | Scaled to the laptop: S1 at 25 / 50 / 100; S2 from 50 in steps of 50 (configurable); calibrated once in M6, then frozen. Revisit only if the experiment is repeated on production-like hardware | Decided (v2.1) |
| Target environment | Bill's laptop (M3 Pro, 12 cores, 36 GB), Docker Desktop at 16 GB, short runs, relative comparison only | Decided by Bill (v2.1) |
| Temporal hosting | Self-hosted server only. Temporal Cloud (V1c) is out of scope for this benchmark and can be added later | Decided (recommended) |
| AML REVIEW policy | Reject (RR04) if not cleared within the SLA budget; no accept-and-hold path | Decided (recommended) |

## Clarifications for your review

These are gaps in v1 that I closed with a default; each changes the design slightly, so please confirm or override.

1. **Payment-state store (`app-db`). Confirmed by Bill.** v1 lists three Postgres instances but also says state is "persisted in both versions". I added a fourth, engine-neutral `app-db`, written at every state transition by the same core function in both engines, and used for ingress dedup and B8. Alternative: write only milestone states (RECEIVED, SETTLED, terminal) to cut about four writes per payment; that would shrink the engine-specific share of the cost. Default kept: every transition.
2. **Who drives load. Confirmed by Bill.** v1 had both the NPC simulator and k6 sending traffic. I made sim-npc the sole generator (open-model, pre-scheduled sends, one clock for both timestamps) and dropped k6. Alternative: keep k6 and have it call a sim-npc send endpoint, at the cost of an extra hop and split timestamps.
3. **Step 2 location.** Parsing and mapping to the canonical model happen at ingress; the pure business checks (currency, amount, receiver) run inside the workflow body with no activity or step, so they cost neither engine a durable round trip.
4. **REVIEW handling.** Split into step 4b: poll every 250 ms, bounded by the remaining SLA budget, inside one core function. No durable timers per poll in either engine.
5. **V1b ack semantics.** In async mode the 202 is returned at the Update *Accepted* stage; in sync mode at *Completed*. Headline latency is unaffected.
6. **Stable ibps.102 MsgId.** Derived by hash from the payment key so it is identical on every retry and replay in every engine.
7. **Target environment: resolved (v2.1).** Bill chose the laptop. The plan is scaled to it (see "Test environment and resource budget"). Two things need your action or awareness: Docker Desktop memory must be raised from 8 GB to 16 GB, and results are relative only.
8. **Common load across variants (v2.1).** S3, S4 and S6 run every variant at the same absolute rate, taken from the lower S2 ceiling, rather than each at its own percentage. Alternative: per-variant percentages, which compare each engine at equal *relative* stress but at different absolute loads.
9. **DBOS unbounded retry.** If the DBOS Go SDK caps max retries as an int, steps 5 and 6 use a very large value with the same capped backoff rather than a per-attempt loop of separate steps, which would change checkpoint volume. The value used is recorded in `docs/versions.md`.
