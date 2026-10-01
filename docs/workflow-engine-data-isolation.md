# Should a workflow engine store the full business object, or just the minimum?

**Scope:** this generalizes [`temporal-workflow-data-isolation.md`](./temporal-workflow-data-isolation.md) beyond Temporal. For Temporal-specific quotes and the determinism argument, see that file; this one surveys the same question across six workflow/orchestration engines plus the foundational pattern literature it traces back to.

**TL;DR:** Across every workflow engine surveyed — Temporal, Azure Durable Functions, Camunda, Netflix Conductor, AWS Step Functions, Apache Airflow — the convergent, independently-arrived-at practice is the same: pass an identifier or reference through the engine, and load/persist the actual business data from your own system of record inside the task/activity that needs it. The engines mostly justify this as a size/performance concern. One of them — Azure Durable Functions — states the compliance rationale explicitly and directly, which none of the others quite do. Stepping outside workflow-engine vendor docs entirely and into regulation and security standards (below), the same conclusion is reached for a third, independent reason: it's close to a *legal requirement* under GDPR, and it's literally how the payments industry already treats this exact problem (PCI DSS tokenization). This document is honest about which claim comes from which kind of source.

## The question, and the honest caveat up front

If a durable-workflow/orchestration engine is driving a business process, should the engine's own input/output/state carry the full domain object (customer details, amounts, account numbers — whatever the business entity actually is), or just an identifier plus whatever small state is strictly needed for control flow, with the real object living in — and only in — your own database?

**This is not a settled, undisputed rule.** Martin Fowler's distinction between **Event Notification** (an event carries "just some id information and a link back to the sender that can be queried for more information") and **Event-Carried State Transfer** (the event carries the full changed state, so "recipients... don't need to contact the source system in order to do further work") describes a real spectrum with real trade-offs on both sides — not a one-directional "always minimize" principle. [Fat events have genuine advantages](https://martinfowler.com/articles/201701-event-driven.html): better resilience when the source system is down, lower latency, less load on the source. The cost is duplication and eventual consistency.

Workflow engines, as it turns out, land almost unanimously on the "thin" side of that spectrum for their *own* persisted state — but mostly for reasons specific to how they work (replay, history size, retention), not because thin-is-always-better in general.

## Survey by engine

### Azure Durable Functions — the most direct statement found anywhere in this research

From [Data Persistence and Serialization in Durable Functions](https://learn.microsoft.com/en-us/azure/durable-task/durable-functions/durable-functions-serialization-and-persistence), under a section literally titled **"Work with sensitive data"**:

> Inputs and outputs (including exceptions) to and from Durable Functions APIs are durably persisted in your storage provider of choice. If those inputs, outputs, or exceptions contain sensitive data (such as secrets, connection strings, or personally identifiable information), anyone with read access to your storage provider's resources could obtain them.
>
> To safely handle sensitive data, **fetch that data within activity functions** from either Azure Key Vault or environment variables, and **never communicate that data directly to or from orchestrators or entities**. This approach helps prevent sensitive data from leaking into your storage resources.

This is the one source in this entire survey — across both this document and the Temporal-specific one — that states the architectural principle directly, as a named recommendation, for a reason beyond payload size. It's also explicit that the orchestrator's history is a trust boundary: write access to it "can be used to alter application behavior, including triggering arbitrary code execution," and the same page separately gives the size-performance rationale:

> You can run into memory issues if you provide large inputs and outputs to and from Durable Functions APIs... The best practice for dealing with large data is to keep it in external storage and materialize that data only inside activities, when needed.

### Camunda — architectural framing, not just a size workaround

From [Handling data in processes](https://docs.camunda.io/docs/components/best-practices/development/handling-data-in-processes/):

> If a process requires an attribute from a business object, the object should be retrieved via a key stored in the process variable from the business object's persistence store.

The general rule of thumb given is to store "as few variables as possible" in the process engine, and — where a leading system already owns the business data — "store references only (e.g. IDs) to the objects stored there." This is framed as lean design generally, not only as a reaction to hitting a size limit, though size limits (a rule-of-thumb ceiling in the low single-digit MB per process) are also cited as a reason.

### Temporal — covered in depth separately

See [`temporal-workflow-data-isolation.md`](./temporal-workflow-data-isolation.md) for the full treatment. In brief: the strongest *directly*-stated fact is the determinism constraint (DB/network calls must happen in Activities, not workflow code); the Claim Check pattern and the PII/activity-isolation blog post are real but narrower than they first appear (size-triggered; PII-containment-to-one-activity, respectively); the no-PII-in-Workflow-ID rule is direct but narrow.

### Netflix Conductor — minimalism as a stated principle, performance as the reason

From the [Conductor best practices guide](https://conductor-oss.github.io/conductor/devguide/bestpractices.html):

> Return only data that downstream tasks need... Store large files in S3/GCS and pass the URI.

Explicitly warns against dumping entire API responses into task output or passing file contents as base64. The stated recommended limit is well under their hard payload cap (under 64 KB recommended, 1 MB hard limit), so like most of the others, the underlying rationale is size/performance rather than a compliance argument.

### AWS Step Functions — size-limit workaround, no general framing

From [Best practices for Step Functions](https://docs.aws.amazon.com/step-functions/latest/dg/sfn-best-practices.html), under "Using Amazon S3 ARNs instead of passing large payloads":

> Executions that pass large payloads of data between states can be terminated. If the data you are passing between states might grow to over 256 KiB, use Amazon S3 to store the data, and parse the ARN... in the Payload parameter.

This is explicitly and only a response to the hard 256 KiB per-state payload limit. There's no broader architectural statement here about identifiers vs. domain objects independent of size.

### Apache Airflow — size-limit framing, confirmed from the official docs

From the [XComs documentation](https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/xcoms.html):

> They are only designed for small amounts of data; do not use them to pass around large values, like dataframes.

The metadata database backing XComs "works well for small values but can cause issues with large values or a high volume of XComs"; the recommended alternative for larger data is an object-storage-backed XCom backend. No PII/compliance framing is present in the official docs — this is purely a performance/storage concern, same shape as AWS Step Functions.

## The PII/sensitive-data-handling lens

Everything above comes from workflow-engine vendors, and (Azure aside) they mostly justify "pass a reference" as a size/performance concern. Stepping into regulation and security standards instead — sources that have nothing to do with workflow engines and never mention them — the same conclusion gets reached independently, for reasons that are compliance-first, not performance-first.

### GDPR Article 25 — this is closer to a legal requirement than a best practice

For any system touching EU residents' personal data, **data minimization by design isn't a recommendation — it's binding law.** From the regulation's actual text ([Article 25, Data protection by design and by default](https://gdpr-text.com/read/article-25/)):

> The controller shall implement appropriate technical and organisational measures for ensuring that, by default, **only personal data which are necessary for each specific purpose of the processing are processed**. That obligation applies to the amount of personal data collected, the extent of their processing, the period of their storage and their accessibility.

And Article 25(1) names data minimisation specifically as an example of the "appropriate technical and organisational measures" controllers must implement at the point they *design* the system — not bolt on afterward. Applied directly to this question: if a workflow engine's event history/task hub/process-variable store is a place personal data *could* flow through but doesn't need to, Article 25 is a standing argument against putting it there, independent of whether the engine's docs say anything about it. This is the strongest source in either document, in the sense that it's enforceable law in relevant jurisdictions, not vendor guidance.

### PCI DSS tokenization — the payments industry already solved this exact problem

Given this project is a payment system, this is the most directly on-point source of all. The PCI Security Standards Council's own tokenization guidance describes exactly the "reference in, real data in a vault, fetched only where needed" pattern — for cardholder data specifically, decades before any workflow engine existed:

> Tokenization secures cardholder data (CHD) by replacing PANs with meaningless or "surrogate" values, also called tokens. [Reducing the amount of CHD stored within a cardholder data environment (CDE)] is a key benefit... Tokenization and de-tokenization processes do not reveal sensitive PAN to any application, user, system, or network outside a defined CDE.

(Quoted via [RSI Security's PCI DSS tokenization guide](https://blog.rsisecurity.com/how-to-meet-tokenization-pci-dss-requirements/), which closely tracks the [official PCI SSC Tokenization Guidelines supplement](https://listings.pcisecuritystandards.org/documents/Tokenization_Guidelines_Info_Supplement.pdf); the PDF itself didn't extract cleanly through automated fetching, so this is sourced via a reputable secondary summary rather than a direct quote of the primary document — worth a manual read of the PDF if this needs to go in front of an auditor.)

The scope-reduction mechanism is the practical payoff: **systems that only ever see a token, never the real PAN, fall outside PCI DSS audit scope entirely.** That's a direct, mechanical incentive — not just a risk-reduction preference — to keep real account/payment data confined to as few systems as possible. A workflow engine's own durable store (event history, system DB, task hub) is exactly the kind of system you'd want to keep out of that scope if it doesn't need the real data to do its job of orchestrating control flow.

### NIST SP 800-122 and OWASP — the general security-engineering consensus

NIST's own abstract for [SP 800-122, *Guide to Protecting the Confidentiality of PII*](https://csrc.nist.gov/pubs/sp/800/122/final), frames the whole document as "practical, context-based guidance for identifying PII and determining what level of protection is appropriate for each instance of PII," aimed at protecting PII "from inappropriate access, use, and disclosure." (The specific line "minimize the use, collection, and retention of PII to what is strictly necessary," widely cited as this document's guidance, comes from secondary sources rather than text I could independently extract from the primary PDF — flagged here rather than presented as a confirmed direct quote.)

[OWASP's Developer Guide](https://devguide.owasp.org/en/04-design/02-web-app-checklist/08-protect-data/) is more directly quotable and blunter:

> Avoid storing sensitive data when at all possible.

— alongside classifying data by sensitivity, applying least-privilege access, and purging sensitive data once it's no longer required. This is the general application-security-engineering version of the same instinct: the safest sensitive data is the data that was never copied somewhere it didn't need to be.

### What this adds

None of GDPR, PCI DSS, NIST or OWASP say anything about workflow engines specifically — they're general-purpose data-protection sources, cited here because they independently justify the *same architectural choice* this document is about, for a *third* reason beyond "performance" (vendor docs) and "one vendor's explicit compliance statement" (Azure): it's close to legally required (GDPR, for EU personal data), it's the standing industry pattern for exactly this kind of data (PCI DSS tokenization, for payment data), and it's the general security-engineering default (NIST, OWASP). For a payment workflow specifically — account numbers, names, amounts — all three apply simultaneously, which is a stronger combined case than any single workflow-engine's documentation makes on its own.

## The pattern's origin: Claim Check, and why it's framed the way it is

The mechanism every engine above converges on — pass a small reference, store the real payload externally, fetch it when needed — has a name older than any of these products: the **Claim Check pattern**, from Hohpe & Woolf's *Enterprise Integration Patterns* (2003). Its own stated intent:

> How can we reduce the data volume of a message sent across the system without sacrificing information content?

Note the framing: *data volume*, not *data sensitivity or ownership*. The pattern was designed to solve a messaging-system performance problem (don't make every hop carry a multi-megabyte payload), and every workflow engine surveyed here has independently re-derived the same mechanism for the same underlying reason — their own persistence layer (event history, process variables, task hub, XCom table) isn't built to hold large or frequently-duplicated data cheaply.

## What this means for the original question

Putting the survey together:

1. **Every engine checked converges on the same mechanism** (reference in, fetch in the task/activity) — that convergence is real and meaningful, independent of why each vendor states it.
2. **The stated rationale is overwhelmingly about size and performance**, not business-data ownership as a principle. If your domain object is small (a few hundred bytes to low kilobytes — a typical payment record, say), you are nowhere near triggering any of these size-driven recommendations on their own terms.
3. **Azure Durable Functions is the one *workflow-engine* source that states the compliance/ownership argument directly and separately from size** — "never communicate [sensitive] data directly to or from orchestrators." That's the closest thing to a documented version of the exact principle asked about from within the workflow-engine world specifically.
4. **Outside workflow engines entirely, the PII lens makes the case independently and more strongly.** GDPR Article 25 makes data minimization close to a legal requirement, not a preference, for EU personal data. PCI DSS tokenization is the payments industry's own long-standing version of exactly this pattern, with a concrete payoff (audit scope reduction) rather than just risk reduction. NIST and OWASP state the same default from a general security-engineering standpoint. None of these mention workflow engines, which if anything makes the convergence more significant — it means the recommendation doesn't depend on any particular engine's design choices.
5. **The general distributed-systems literature (Fowler) treats this as a genuine trade-off, not a one-way rule.** Event-Carried State Transfer — the "fat" side — has real, cited advantages (resilience, latency, decoupling from the source's availability). The case for keeping a workflow engine's own state thin rests on engine-specific facts (replay/history mechanics, a shared multi-tenant storage/UI surface, retention policy mismatches with your system of record) plus, for personal/payment data specifically, the regulatory and industry-standard arguments above — not a universal claim that thin is always better everywhere.

**So: for a workflow engine specifically — as opposed to messaging/events in general — the recommendation to keep the engine's own store to an identifier plus minimal control-flow state, with the domain object owned by your own database and loaded by the task/activity, is well supported by convergent independent practice across engines, directly stated for the compliance case by at least one major vendor (Azure), reinforced independently and more strongly by data-protection law and payments-industry standards once PII/financial data is in play, and consistent with the oldest named version of the mechanism (Claim Check).**

## Sources

- [Azure Durable Functions: Data Persistence and Serialization](https://learn.microsoft.com/en-us/azure/durable-task/durable-functions/durable-functions-serialization-and-persistence) — direct statement on sensitive data; "fetch within activity functions... never communicate directly to or from orchestrators"
- [Camunda: Handling data in processes](https://docs.camunda.io/docs/components/best-practices/development/handling-data-in-processes/) — store references, retrieve business objects via key from their own persistence store
- [Netflix Conductor: Best Practices](https://conductor-oss.github.io/conductor/devguide/bestpractices.html) — "return only data that downstream tasks need"
- [AWS Step Functions: Best practices](https://docs.aws.amazon.com/step-functions/latest/dg/sfn-best-practices.html) — S3 ARN pattern, framed around the 256 KiB limit
- [Apache Airflow: XComs](https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/xcoms.html) — "only designed for small amounts of data"
- [GDPR Article 25: Data protection by design and by default](https://gdpr-text.com/read/article-25/) — binding legal text; data minimization by default
- [PCI SSC Tokenization Guidelines (official supplement, PDF)](https://listings.pcisecuritystandards.org/documents/Tokenization_Guidelines_Info_Supplement.pdf); quoted here via [RSI Security's summary](https://blog.rsisecurity.com/how-to-meet-tokenization-pci-dss-requirements/) — scope reduction through tokenization
- [NIST SP 800-122: Guide to Protecting the Confidentiality of PII](https://csrc.nist.gov/pubs/sp/800/122/final) — official abstract confirmed; the specific "minimize collection/retention" line is from secondary sources, flagged as such in the text above
- [OWASP Developer Guide: Protect Data Everywhere](https://devguide.owasp.org/en/04-design/02-web-app-checklist/08-protect-data/) — "avoid storing sensitive data when at all possible"
- Gregor Hohpe & Bobby Woolf, *Enterprise Integration Patterns* (2003) — origin of the Claim Check pattern
- [Martin Fowler: What do you mean by "Event-Driven"?](https://martinfowler.com/articles/201701-event-driven.html) — Event Notification vs. Event-Carried State Transfer, the general trade-off this sits inside
- [`temporal-workflow-data-isolation.md`](./temporal-workflow-data-isolation.md) — Temporal-specific treatment (determinism constraint, Claim Check, Workflow ID rules)

**Checked and found not to directly support this claim** (worth knowing, so nobody re-finds these expecting a direct statement): the canonical Saga pattern reference ([microservices.io](https://microservices.io/patterns/data/saga.html)) describes orchestrator/participant message flow functionally but gives no explicit guidance on minimal orchestrator state vs. full business entities. Separately, a search-tool summary claimed Temporal Cloud's own security docs ([docs.temporal.io/cloud/security](https://docs.temporal.io/cloud/security)) state that "reference-based communication... keeps sensitive workflows compliant with PCI DSS" — fetching the actual page directly found no such sentence; the page confirms Temporal Cloud is SOC 2/GDPR/HIPAA-aligned and describes client-side payload encryption via Data Converters, but makes no reference-vs-payload recommendation. Not citing it as a Temporal statement because it isn't one.
