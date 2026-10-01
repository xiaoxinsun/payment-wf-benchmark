# Workflow data isolation: don't store the domain object in Temporal's own store

**TL;DR:** Pass the workflow an identifier (and a few small, immutable control-flow values), not the full domain object. Let activities load and persist the actual business data from your own database. This is not a house style preference — it's Temporal's own documented pattern ("Claim Check"), with a specific compliance and performance rationale behind it.

## The question

If Temporal (or any durable-workflow engine) is the payment processing engine, should the workflow carry the full domain object (e.g. a `Payment` with names, account numbers, amounts, remarks) as its input/output, or should it carry only the minimum — an ID plus whatever state is strictly needed for workflow control flow — with the real domain object owned and accessed separately by the activity handlers?

**Answer: the latter.** Minimum identifying data in the workflow; the domain object lives in your own database, read and written by activities via direct DB access.

## Why: Temporal durably persists whatever crosses the workflow boundary

This isn't a style choice — it falls directly out of how Temporal guarantees durability. Every workflow input, activity input and activity output is recorded as an event in the workflow's **Event History**, persisted in Temporal's own database, for the full length of the namespace's retention period after the workflow closes. There's no way to pass data through a workflow/activity call without it durably landing there.

Two consequences follow directly, and Temporal's own docs call out both:

### 1. Size and performance — the Claim Check pattern

From [Troubleshoot payload and gRPC message size limit errors](https://docs.temporal.io/troubleshooting/blob-size-limit-error):

> A Workflow Execution may be terminated if any single payload exceeds 2 MB or if the entire Event History exceeds 50 MB.
>
> Pass references to the stored payloads within the Workflow instead of the actual data... Retrieve the payloads from the object store when needed during execution.

This is explicitly named the **Claim Check pattern** (the same pattern used with Kafka and other messaging systems: store the payload externally, pass a token/ID through the pipeline, fetch it when needed). It's built into the SDKs as External Storage, or implementable yourself via a custom Payload Codec. Temporal's guidance goes further than "only do this if you're near the limit":

> Consider implementing the claim check pattern for Workflows and Activities that have the potential to receive or return large payloads, even if they are currently within the limit.

### 2. Compliance and exposure — Activity isolation as a security boundary

From the official Temporal blog, [Using Activity isolation as a security boundary](https://temporal.io/blog/using-activity-isolation-as-a-security-boundary):

> Data passed between Activities will be visible in the Temporal UI, which might be against company policy.

Their worked example processes a document inside *one* activity (PII scanning, classification, storage) and passes only a `file_id` onward:

> By keeping the PII scan, classification, and storage inside one Activity, raw complaint text never crosses an Activity boundary and never appears in the Temporal UI. The only thing passed to the next Activity is a `file_id`.

For a payment workflow this maps directly: account numbers, customer names and amounts are exactly the kind of data that shouldn't ride through workflow/activity payloads if it can be avoided, because:

- it duplicates regulated data into an infrastructure datastore that isn't your system of record, with its own (likely different) retention, encryption-at-rest, access-control and right-to-erasure posture;
- it's visible in the Temporal Web UI, CLI output and logs to anyone with namespace access;
- it entangles your domain model's schema with workflow **replay determinism** — change a field later, and old in-flight histories still contain the old shape;
- it isn't queryable as a business record anyway (Temporal's visibility store indexes a handful of search attributes, not your domain data), so you need a real database for that regardless — making the duplication pure downside.

### 3. The same rule applies to Workflow IDs specifically

From [Workflow Id and Run Id](https://docs.temporal.io/workflow-execution/workflowid-runid):

> Do not include sensitive data, secrets, or personally identifiable information (PII) as a Workflow Id.

Workflow IDs are plaintext, bypass any Payload Codec, and are visible in the Web UI, CLI, Event History and system logs. Worth checking whatever dedup/business key you use to construct a workflow ID — transaction/message references are fine; customer names or account numbers are not.

## Recommended shape

1. **Ingress** parses/validates the inbound message and writes the canonical domain object into your own database — the one system of record you actually control for retention, audit and redaction.
2. **Start the workflow with just an identifier** (and a dedup key, if different) — plus, if genuinely needed, a handful of small, immutable, already-known-safe control values. Even these are optional if you're willing to pay one extra lookup.
3. **Every activity loads what it needs directly from your database**, by ID, inside the activity function — not via workflow/activity parameters.
4. **Activity outputs stay small**: decision codes, reason codes, timestamps — never an echo of the domain object.
5. **State transitions are written directly to your own database** from inside the activity, independent of whatever the engine itself persists.

## The trade-off, honestly

This costs one extra database round-trip per activity that needs the data (a plain indexed lookup against your own Postgres — typically sub-millisecond to a few milliseconds) versus zero extra round-trips if the data simply rides along in the payload. For a workflow with four or five steps, that's four or five extra fast local reads. Given the compliance and architectural benefits above, it's a trade worth making by default, not an edge-case optimization.

## Sources

- [Troubleshoot payload and gRPC message size limit errors](https://docs.temporal.io/troubleshooting/blob-size-limit-error) — the Claim Check pattern, size limits, External Storage / Payload Codec
- [Using Activity isolation as a security boundary](https://temporal.io/blog/using-activity-isolation-as-a-security-boundary) — official Temporal blog; PII handling, activity isolation, pass-a-reference pattern
- [Workflow Id and Run Id](https://docs.temporal.io/workflow-execution/workflowid-runid) — no PII/sensitive data in Workflow IDs
- [Worker deployment and performance](https://docs.temporal.io/best-practices/worker)
- [Workflow cost optimization](https://docs.temporal.io/best-practices/cost-optimization)
