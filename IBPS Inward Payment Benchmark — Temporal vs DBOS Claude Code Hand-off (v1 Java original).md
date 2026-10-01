# IBPS Inward Payment Benchmark — Temporal vs DBOS: Claude Code Hand-off

Sep 28, 2026 · @Bill Sun

## Purpose and scope

Build one inward IBPS instant-credit flow twice, on Temporal (V1) and on DBOS (V2), with identical business logic and simulators, and measure latency, throughput, recovery and correctness under the same faults.

The benchmark answers four questions:

1. What is end-to-end latency (ibps.101 received to ibps.102 emitted) at p50, p95, p99 and p99.9 for each engine at a fixed arrival rate?
2. What is the maximum sustainable TPS for each engine while p99 stays inside the IBPS response SLA?
3. How long does each engine take to resume in-flight payments after a crash, and does any payment get stuck?
4. Does either engine ever double-post, lose a payment, or leave the ledger unbalanced under injected faults?

**In scope:** inward credit only (ibps.101 in, ibps.102 out), account validation, AML screening, settlement posting, reject paths, deterministic fault injection, load generation, metrics and a results report.

**Out of scope:** outward payments, IBPS account-inquiry and e-authorisation messages, real CFCA signing and encryption, net-settlement cycles with the PBOC, returns and recalls after acceptance, UI.

**Variants measured:** V1a Temporal baseline (regular activities), V1b Temporal optimised (Local Activities + Eager Workflow Start + Update-with-Start), V2 DBOS. Including V1b is essential; comparing DBOS only against untuned Temporal would not be a fair result.

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
| 7 | No response SLA | IBPS is real-time; a late reply is a failed payment even if it eventually succeeds. | Configurable SLA budget (default 5 s, to confirm) enforced by a workflow timer; late outcomes are counted separately. |
| 8 | Sync vs async API undefined | Changes what latency means. | Ingress acknowledges receipt, then the flow emits ibps.102 asynchronously; a sync mode is also offered for the "caller waits" measurement. |
| 9 | Unfair comparison risk | Different languages, DBs or tuning would decide the result, not the engine. | Same language, same business core module, same Postgres spec, same simulators, same load tool; Temporal tested baseline and optimised. |
| 10 | Ledger is ambiguous | "Post a balanced pair" could live in the app DB (favours DBOS) or in the CBS. | Postings live in the simulated CBS behind an API, as in a real bank; DBOS gets no same-transaction advantage. |

Two smaller gaps are noted as assumptions rather than built: real CFCA message signing (stubbed as a no-op signer with configurable latency) and the NPC's own processing confirmation to the sender (not needed on the inward side).

## Business flow

Each inward ibps.101 runs as one durable workflow keyed by its message ID, and ends in exactly one ibps.102: ACCP after settlement, or RJCT with a reason code.

&#91;embedded content: inward IBPS credit · 6 steps, 3 exits\]

Settlement is the point of no return: before it, any failure or SLA expiry rejects; after it, the flow must deliver ACCP however long the NPC takes to acknowledge.

**Payment states** (persisted in both versions, same enum): RECEIVED → VALIDATED → SCREENED → SETTLED → ACCEPTED\_SENT, or RECEIVED/VALIDATED/SCREENED → REJECTED → REJECTED\_SENT.

**Step policies** (identical in both engines; business errors are never retried):

| Step | Call | Per-attempt timeout | Retry policy | Non-retryable outcomes |
| --- | --- | --- | --- | --- |
| 1 Receive | Ingress API | n/a | n/a | Schema invalid (reject at ingress) |
| 2 Map | In-process | n/a | n/a | Mapping error → RJCT |
| 3 Validate | CBS `GET /accounts/{id}/validate` | 500 ms | Exponential, 50 ms start, ×2, max 4 attempts inside SLA | Closed, frozen, dormant, name mismatch, wrong currency |
| 4 AML | AML `POST /screen` | 800 ms | Same as step 3 | HIT; REVIEW not cleared within budget |
| 5 Settle | CBS `POST /postings` (idempotent) | 1 s | On timeout: `GET /postings/{ref}` first, then retry same ref; unbounded until success | None; business rejects are impossible after step 4 |
| 6 Respond | NPC `POST /ibps/102` | 1 s | Unbounded with backoff capped at 2 s | None |

All timeouts and budgets are configuration, not code, so the same values are applied to V1a, V1b and V2.

## Message mapping

ibps.101 maps inbound to pacs.008 and pacs.002 maps outbound to ibps.102; everything inside the platform speaks pacs only, and IBPS formats exist only in an adapter at the edge.

The CNCC IBPS interface specification is not public, so the simulator uses a **simplified synthetic XML schema** with the field set below. Claude Code should put both directions behind a `MessageMapper` interface so the bank's real spec can replace the synthetic one without touching workflows.

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

**Outbound: pacs.002.001.10 → ibps.102 (receipt)**

| pacs.002 path | ibps.102 field (synthetic) | Rule |
| --- | --- | --- |
| GrpHdr/MsgId | MsgId | New ID generated by our bank |
| OrgnlGrpInfAndSts/OrgnlMsgId, OrgnlMsgNmId | OrgnlMsgId, OrgnlMsgTp | Original ibps.101 MsgId and "ibps.101.001.01" |
| TxInfAndSts/OrgnlEndToEndId | OrgnlEndToEndId | Copy |
| TxInfAndSts/TxSts | PrcSts | ACCP → accepted, RJCT → rejected |
| TxInfAndSts/StsRsnInf/Rsn/Cd | RjctCd | ISO code mapped to IBPS proprietary code via a config table |
| TxInfAndSts/AccptncDtTm | PrcDtTm | Time of settlement or rejection |

**Reject reasons** (internal code → ISO 20022 code; IBPS proprietary codes come from a lookup file filled from the bank's spec):

| Internal reason | ISO code | Raised at |
| --- | --- | --- |
| ACCOUNT\_NOT\_FOUND | AC01 | Step 3 |
| ACCOUNT\_CLOSED | AC04 | Step 3 |
| ACCOUNT\_FROZEN or DORMANT | AC06 | Step 3 |
| NAME\_MISMATCH | BE01 | Step 3 |
| CURRENCY\_NOT\_ALLOWED | AM03 | Step 2 |
| AMOUNT\_OVER\_LIMIT | AM02 | Step 2 (limit configurable, default CNY 1,000,000, to confirm) |
| AML\_HIT or AML\_REVIEW\_TIMEOUT | RR04 | Step 4 |
| SLA\_TIMEOUT | AB05 | Timer, before step 5 |
| WRONG\_RECEIVER | AGNT | Step 2 |
| FORMAT\_INVALID | FF01 | Ingress |

## System architecture

Both versions run the same payment service binary shape, the same business core and the same simulators; only the durability layer differs.

&#91;embedded content: benchmark topology · V1 and V2 share everything but the durability layer\]

The V1 Temporal server box is the extra network hop the benchmark measures; in V2 the same checkpoints go straight from the process to Postgres.

**Shared business core.** A plain library with no engine imports: `validateAccount()`, `screenAml()`, `postSettlement()`, `sendReceipt()`, the mappers and the reason-code table. Each engine binding is a thin wrapper that calls these as activities (V1) or steps (V2). This keeps any latency difference attributable to the engine.

**V1a, Temporal baseline.** Ingress calls `startWorkflow` and returns; the workflow runs steps 3 to 6 as regular activities on a separate task queue. Workers run as a separate deployment from ingress.

**V1b, Temporal optimised.** Ingress and worker share one process and client connection. Ingress uses Update-with-Start with eager start requested, steps 3 and 4 run as Local Activities, and the update returns once step 5 has posted or a reject is decided. Step 6 runs as a regular activity so it can retry indefinitely.

**V2, DBOS.** Ingress calls the DBOS workflow directly in-process with the payment's dedup key as workflow ID. Steps 3 to 6 are DBOS steps with retries configured per the step table. DBOS queues cap in-flight workflows per instance.

**Scaling shape for both.** Payment service runs N identical replicas behind a load balancer (start with 3). Temporal server runs frontend, history and matching with numHistoryShards fixed before the first run (512 suggested). DBOS relies on Postgres; connection pool size is tuned per replica and recorded.

## Simulators

Three simulators stand in for external systems; all are deterministic from test data so that every run of a scenario produces the same business outcome, and all expose a latency distribution setting so the network cost of a real bank can be dialled in.

**IBPS NPC simulator.** Generates ibps.101 messages from a seeded account set, POSTs them to the payment service ingress, receives ibps.102 on a callback endpoint, and records both timestamps. It can resend an already-sent MsgId (duplicate test) and can delay or refuse its ibps.102 acknowledgement (response-path faults).

| Endpoint | Direction | Purpose |
| --- | --- | --- |
| `POST /ingress/ibps101` (on payment service) | NPC → bank | Deliver ibps.101; bank replies 202 with a receipt ID |
| `POST /npc/ibps102` (on simulator) | Bank → NPC | Deliver ibps.102; simulator replies 200 ack |
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
| `GET /cases/{id}` | Poll a REVIEW | OPEN, CLEARED or CONFIRMED\_HIT; auto-resolves after a configurable delay |

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
- No duplicate `postingRef` and no payment with more than one ibps.102 recorded at the NPC simulator.
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
| B6 | Business | AML REVIEW cleared inside SLA | 4 | data | ACCP, latency includes review wait |
| B7 | Business | AML REVIEW past SLA | 4 | data | RJCT RR04 at SLA expiry, no posting |
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

**Note on I8.** Temporal reassigns an orphaned workflow to any live worker automatically. DBOS open-source recovery is tied to the executor that owned the workflow, so Claude Code must check the current DBOS docs for how a permanently lost replica's workflows are recovered (for example a stable executor ID on redeploy, or DBOS Conductor) and record the result as an operational difference, not only a latency number.

**Code-fault probe (optional, C1).** Throw an unexpected exception in the workflow body (not inside an activity or step) right after step 4 for one payment. Temporal retries a failing workflow task indefinitely by default, while a DBOS workflow records the error; the benchmark should record how each surfaces the stuck payment to operators.

## Benchmark methodology

The headline number is end-to-end latency from ibps.101 sent to ibps.102 received, both stamped by the NPC simulator, measured under an open-model constant arrival rate so queueing delay is not hidden.

**Fairness rules**

1. Same language, runtime version and JVM or GC flags for V1a, V1b and V2.
2. Same business core module, same simulators, same step timeouts and retry policies (all from one config file).
3. Durability Postgres for V1 and V2 on identical instance specs and Postgres settings (`synchronous_commit = on` for both; never relax durability for one side).
4. Same replica count and CPU/memory limits for the payment service; Temporal server resources reported separately and added to V1's total footprint.
5. Simulator latency set to a realistic baseline (CBS 5 ms, AML 10 ms, NPC 3 ms, to adjust) so engine overhead is compared against real network cost, not zero.
6. Each scenario run 3 times after a 5-minute warm-up; report the median run and the spread.

**Scenarios**

| ID | Scenario | Load | Faults | Answers |
| --- | --- | --- | --- | --- |
| S1 | Latency at fixed rates | 100, 500, 1,000 TPS for 10 min each | None | Latency percentiles per variant |
| S2 | Capacity step-up | +100 TPS every 2 min until p99 breaches SLA or errors exceed 0.1% | None | Max sustainable TPS |
| S3 | Soak | 60% of S2 max for 60 min | None | Stability, DB growth, GC or vacuum effects |
| S4 | Fault matrix | 50% of S2 max, one fault per run | B1–B8, I1–I12 | Correct outcomes, recovery time, latency impact |
| S5 | Hot account | S2 repeated with settlement shards N = 1 | None | Share of the ceiling caused by the ledger |
| S6 | Business mix | 60% of S2 max, default reject mix | B1–B7 via data | Latency of reject paths vs happy path |

**Metrics collected**

| Metric | Source | Unit |
| --- | --- | --- |
| End-to-end latency p50, p95, p99, p99.9, max | NPC simulator timestamps | ms |
| Ingress acknowledgement latency | k6 | ms |
| Per-step latency (steps 3–6) and engine overhead between steps | OpenTelemetry spans | ms |
| Achieved TPS and SLA breach rate | NPC simulator | payments/s, % |
| Outcomes by reason code | Payment state table | count |
| Recovery time after I7–I11 | First and last stalled payment timestamps | s |
| Durability DB writes and WAL volume per payment | `pg_stat_statements`, `pg_stat_wal` | rows, bytes |
| CPU and memory, per component | cAdvisor / Prometheus | cores, GiB |

**Pass criteria for any variant:** every ledger invariant holds in every run, zero non-terminal payments 60 s after load stops, and zero payments with two ibps.102 outcomes. A variant that fails correctness is reported as failed regardless of latency.

## Build plan for Claude Code

Build in eight milestones as one Gradle multi-module repo on Java 21, one Docker Compose stack, with each milestone ending in green tests before the next starts.

**Tech stack**

| Concern | Choice | Note |
| --- | --- | --- |
| Language | Java 21, virtual threads | Both SDKs available; confirm DBOS Java SDK maturity in M0 |
| Web framework | Spring Boot 3 | Same for all services |
| V1 engine | Temporal Java SDK + Temporal Server via Docker (Postgres persistence) | Pin versions; server must support Eager Workflow Start |
| V2 engine | DBOS Transact Java | Pin version |
| ISO 20022 | Prowide ISO 20022 (pacs.008.001.08, pacs.002.001.10) | IBPS synthetic XML via JAXB |
| Databases | Postgres 16: temporal-db, dbos-db, cbs-db | Identical settings for the first two |
| Faults | Toxiproxy + simulator fault API |  |
| Load | k6, constant-arrival-rate executor |  |
| Observability | OpenTelemetry, Prometheus, Grafana | One shared dashboard for all variants |

**Repo layout**

```
ibps-bench/
  core/                 # shared business logic, no engine imports
    mapping/            # ibps101<->pacs008, pacs002<->ibps102, reason codes
    steps/              # validateAccount, screenAml, postSettlement, sendReceipt
    model/              # Payment, PaymentState, RejectReason
  engine-temporal/      # V1a + V1b workflow, activities, worker, ingress
  engine-dbos/          # V2 workflow, steps, ingress
  sim-npc/              # IBPS NPC simulator + results collector
  sim-cbs/              # accounts, ledger, idempotent postings, fault API
  sim-aml/              # screening, cases, fault API
  harness/
    k6/                 # S1-S6 scripts
    chaos/              # chaos.sh, toxiproxy profiles per fault ID
    checks/             # ledger and outcome invariant checker
    report/             # results -> markdown report + charts
  deploy/
    compose/            # docker-compose.v1.yml, docker-compose.v2.yml, shared.yml
    config/             # timeouts.yml, retries.yml, mix.yml, sla.yml
  docs/HANDOFF.md       # this document
```

**Milestones**

- [ ] **M0 Skeleton.** Repo, Gradle modules, Compose with three Postgres, Toxiproxy, Prometheus, Grafana; SDK version check for Temporal and DBOS Java recorded in `docs/versions.md`.
- [ ] **M1 Simulators.** sim-npc, sim-cbs, sim-aml with endpoints, seeded test data (1M accounts), fault API and `/ledger/invariants`; contract tests for every documented response.
- [ ] **M2 Shared core.** Mappers with golden-file tests for all four message directions, reason-code table, HTTP clients with configured timeouts, ambiguous-posting resolver; 100% unit coverage on mapping and reject rules.
- [ ] **M3 V1a Temporal baseline.** Workflow, regular activities, separate worker deployment, SLA timer, dedup via workflow ID; B1–B8 pass end to end.
- [ ] **M4 V1b Temporal optimised.** Co-located client and worker, Update-with-Start with eager start, Local Activities for steps 3–4; same B1–B8 pass.
- [ ] **M5 V2 DBOS.** Workflow and steps calling the same core, workflow ID = dedup key, queue concurrency limits, SLA timer; same B1–B8 pass.
- [ ] **M6 Harness.** k6 scripts S1–S6, results collector, invariant checker wired to fail the run, report generator.
- [ ] **M7 Fault automation.** One script per fault ID I1–I12 (and C1) with automatic assertion of the expected outcome for all three variants.
- [ ] **M8 Run and report.** Execute S1–S6 on the target environment and produce `results/REPORT.md` with latency charts, max TPS, recovery times and invariant results per variant.

**Working rules for Claude Code:** keep engine imports out of `core`; put every timeout, retry, SLA and mix value in `deploy/config`; never tune one variant without applying the equivalent setting to the others; and stop to ask when a DBOS or Temporal feature named here behaves differently in the pinned SDK version.

## Open questions and assumptions

Seven items are assumed so work can start; each should be confirmed before M8, and none blocks M0–M2.

- [ ] **Language.** Java 21 is assumed because both SDKs exist and it is common in banks. If the DBOS Java SDK proves immature in M0, switch both versions to Go or Python together.
- [ ] **IBPS response SLA.** 5 s end to end is assumed; replace with the timeout in the bank's CNCC interface spec.
- [ ] **IBPS message schema and reject codes.** The synthetic schema and ISO-only codes are placeholders for the real ibps.101/ibps.102 spec and proprietary code list.
- [ ] **Amount limit.** CNY 1,000,000 per payment is assumed; confirm against current CNCC rules.
- [ ] **Target TPS.** S1 rates (100, 500, 1,000) are placeholders; set them from the bank's peak inward IBPS volume with 3× headroom.
- [ ] **Temporal hosting.** Self-hosted server is assumed for like-for-like infra control; a Temporal Cloud run can be added as V1c if the bank would use Cloud in production.
- [ ] **AML REVIEW policy.** Rejecting when a REVIEW is not cleared within the SLA is assumed; some banks instead accept and hold funds, which would change step 5.
