# Workflow data isolation: don't store the domain object in Temporal's own store

**TL;DR:** Pass the workflow an identifier (and a few small, immutable control-flow values), not the full domain object. Let activities load and persist the actual business data from your own database.

**On sourcing, up front:** no single official Temporal document states this exact principle — "separate workflow/orchestration data from business domain data" — as a named rule. What follows is a pattern assembled from several real, independently-verified pieces of Temporal guidance, each of which supports part of the recommendation from a different angle (determinism, payload size, PII containment). None of them individually makes the general claim; the synthesis is mine. Each source below is quoted directly, and the gap between what it says and what it's being used to support is called out explicitly.

## The question

If Temporal (or any durable-workflow engine) is the payment processing engine, should the workflow carry the full domain object (e.g. a `Payment` with names, account numbers, amounts, remarks) as its input/output, or should it carry only the minimum — an ID plus whatever state is strictly needed for workflow control flow — with the real domain object owned and accessed separately by the activity handlers?

**Recommendation: the latter.** Minimum identifying data in the workflow; the domain object lives in your own database, read and written by activities via direct DB access.

## What's actually, directly documented

### 1. Workflow code cannot do the fetch itself — only an Activity can

This is the most load-bearing fact, and it's stated plainly in Temporal's docs on [Workflow Definition](https://docs.temporal.io/workflow-definition):

> To handle non-deterministic operations like API calls, LLM/AI invocations, database queries, and other external interactions, put them in Activities.

Workflow code must be deterministic to support replay, so it cannot make network calls or database queries directly — that's exactly what Activities exist for. This establishes one half of the architecture directly: *if* the workflow needs to load the domain object, that load must happen inside an Activity, never in workflow code.

**What this does *not* say:** it doesn't say the workflow shouldn't *receive* the domain object as an argument from outside (e.g. ingress already fetched it and passes it in). The constraint is about where a fetch can happen, not about what a caller may hand to `ExecuteWorkflow`.

### 2. The Claim Check pattern — a size mitigation, not an identity/domain-object rule

From [Troubleshoot payload and gRPC message size limit errors](https://docs.temporal.io/troubleshooting/blob-size-limit-error):

> A Workflow Execution may be terminated if any single payload exceeds 2 MB or if the entire Event History exceeds 50 MB.
>
> Pass references to the stored payloads within the Workflow instead of the actual data... Retrieve the payloads from the object store when needed during execution.

Named the **Claim Check pattern**. Temporal does nudge toward applying it proactively, not only once you're near the limit:

> Consider implementing the claim check pattern for Workflows and Activities that have the potential to receive or return large payloads, even if they are currently within the limit.

**What this does and doesn't establish:** this is framed throughout as a payload-*size* concern — avoiding limit breaches and event-history bloat. It doesn't make a general claim that business/domain data shouldn't flow through the workflow boundary regardless of size. A small `Payment` struct (a few hundred bytes to low kilobytes) is nowhere near the 2 MB/payload or 50 MB/history limits this guidance is written for.

### 3. Activity isolation for PII — narrower than it first looks

From the official Temporal blog, [Using Activity isolation as a security boundary](https://temporal.io/blog/using-activity-isolation-as-a-security-boundary):

> Data passed between Activities will be visible in the Temporal UI, which might be against company policy.

Their worked example keeps PII scanning, classification and storage inside *one* activity, and passes only a `file_id` to the *next* activity:

> By keeping the PII scan, classification, and storage inside one Activity, raw complaint text never crosses an Activity boundary and never appears in the Temporal UI. The only thing passed to the next Activity is a `file_id`.

**What this does and doesn't establish:** this is a pattern for containing the blast radius of PII handling to a single activity, and for keeping data *between activities* reference-based. It does not say the workflow itself — or the first activity, which still has to receive *something* to identify the work — shouldn't be started with a domain object. It's a narrower, adjacent pattern, not a direct statement of "workflow owns identity only, domain object lives elsewhere."

### 4. Workflow IDs specifically: no PII — this one is direct and unambiguous

From [Workflow Id and Run Id](https://docs.temporal.io/workflow-execution/workflowid-runid):

> Do not include sensitive data, secrets, or personally identifiable information (PII) as a Workflow Id.

Workflow IDs are plaintext, bypass any Payload Codec, and are visible in the Web UI, CLI, Event History and system logs. This is the one piece of guidance here that's a direct, explicit, narrowly-scoped rule rather than an inference — but it only covers the ID, not the full input payload.

### 5. A Temporal maintainer's forum answer — still size-framed

From a [Temporal community forum thread](https://community.temporal.io/t/determining-the-best-practices-for-business-logic-placement-within-temporal-activities-and-workflows/8951), Maxim (Temporal maintainer):

> You cannot pass large payloads as activity and workflow arguments and results.

Same shape as #2 — a size constraint, not an architectural identity/domain-object principle.

## Why the synthesis still holds, even though no single doc says it

Put together, #1 through #5 add up to a reasonable, defensible design even though none of them individually mandates it:

- The workflow can't fetch data itself (#1) — so *something* has to hand it the domain object or an identifier.
- Whatever crosses the boundary durably persists in Temporal's own store for the retention window, is visible in the Web UI/CLI/logs, and becomes subject to the determinism/replay constraints on schema change (not quoted above verbatim from an official source in this pass, but a direct mechanical consequence of how Event History works, same as the data a Claim Check payload would otherwise bloat).
- Temporal's own size and PII guidance (#2, #3) both reach for the same mechanism — pass a reference, fetch inside an Activity — even though each is solving a narrower problem (size limits; PII blast radius) than the general one.
- For a regulated domain like payments, the same reasoning that motivates #2 and #3 (reduce what's exposed in the engine's own store, reduce what's duplicated outside your system of record) applies to the domain object as a whole, not just to oversized or PII-specific payloads.

This is a pattern I'd recommend and would defend in a design review — but it should be presented as "assembled from Temporal's determinism model plus its size/PII guidance, applied to a regulated domain," not as "Temporal's documented best practice for this."

## Recommended shape

1. **Ingress** parses/validates the inbound message and writes the canonical domain object into your own database — the one system of record you actually control for retention, audit and redaction.
2. **Start the workflow with just an identifier** (and a dedup key, if different) — plus, if genuinely needed, a handful of small, immutable, already-known-safe control values. Even these are optional if you're willing to pay one extra lookup.
3. **Every activity loads what it needs directly from your database**, by ID, inside the activity function — consistent with #1 above (database access belongs in Activities regardless).
4. **Activity outputs stay small**: decision codes, reason codes, timestamps — never an echo of the domain object.
5. **State transitions are written directly to your own database** from inside the activity, independent of whatever the engine itself persists.

## The trade-off, honestly

This costs one extra database round-trip per activity that needs the data (a plain indexed lookup against your own Postgres — typically sub-millisecond to a few milliseconds) versus zero extra round-trips if the data simply rides along in the payload. For a workflow with four or five steps, that's four or five extra fast local reads. Given the compliance and architectural reasoning above, it's a trade worth making by default — but it's a judgment call, not a documented requirement.

## Sources

- [Workflow Definition](https://docs.temporal.io/workflow-definition) — determinism constraint; DB/network calls must happen in Activities
- [Troubleshoot payload and gRPC message size limit errors](https://docs.temporal.io/troubleshooting/blob-size-limit-error) — the Claim Check pattern, size limits, External Storage / Payload Codec
- [Using Activity isolation as a security boundary](https://temporal.io/blog/using-activity-isolation-as-a-security-boundary) — official Temporal blog; PII handling, activity isolation, pass-a-reference pattern
- [Workflow Id and Run Id](https://docs.temporal.io/workflow-execution/workflowid-runid) — no PII/sensitive data in Workflow IDs
- [Temporal community forum: Business logic placement](https://community.temporal.io/t/determining-the-best-practices-for-business-logic-placement-within-temporal-activities-and-workflows/8951) — maintainer comment on payload size
- [Worker deployment and performance](https://docs.temporal.io/best-practices/worker)
- [Workflow cost optimization](https://docs.temporal.io/best-practices/cost-optimization)
