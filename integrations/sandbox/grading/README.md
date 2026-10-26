# Protected sandbox grading

You can install a JSON equality supervisor and a recoverable sandbox evaluation adapter from this package. The adapter uses the existing verification executor and sandbox test verifier. It does not rerun the agent, expose effect tools, or execute a host shell.

`NewEqualitySupervisor` accepts a bounded list of sealed manifests and reserved assessment artifact IDs. Supply the same registrations after a restart. Every manifest must pin `EqualitySupervisorRevision`, use restricted JSON artifacts, and have no state probes. Unsupported protocols and probes fail registration or assessment.

The protected test bundle and result schema each declare the exact ordered check names:

```json
{"protocol":"cortex.json-equality.v1","checks":["answer"]}
```

The protected fixture dataset contains the expected values. The candidate export uses the same shape for actual values:

```json
{"protocol":"cortex.json-equality.v1","values":{"answer":42}}
```

The separate grader export reports each check:

```json
{"protocol":"cortex.json-equality.v1","checks":{"answer":"passed"}}
```

The supervisor independently compares each named value and requires the grader's status to agree. A matching value passes; a different value fails. Missing or additional checks, duplicate JSON keys, unknown envelope fields, invalid UTF-8, and nesting beyond 32 levels fail closed. Each artifact is limited to one MiB. Numbers retain their JSON spelling, so `1` and `1.0` are different values under this protocol. Candidate text and stdout cannot supply instructions or authority.

An assessment is an immutable restricted artifact bound to the exact manifest, candidate job, grader job, exported artifact references, and individual results. Reports carry the candidate export, grader export, and assessment in `ObservationRefs`. These references are included in the report digest without changing its original input binding. Report save, result commit, and current result reads load their bytes again. A deleted observation blocks current result access; historical report metadata remains available.

## Host lifecycle

Construct `EvaluationConfig` with the existing `SandboxTestConfig`, the same manifest roster, a private journal directory, its owning UID, a polling interval, and a conservative allowance for both exact jobs. The directory must already exist, have mode `0700`, and pass the host path ownership checks. The adapter creates immutable mode `0400` journal records and holds an exclusive process lock while running. One adapter process owns a journal at a time.

Call `Start` with your authorized service context before passing the adapter to `verification.NewExecutor`. Call `Stop` during shutdown and wait for it to finish. A timed-out stop keeps the process lock until a later stop joins the worker. This package does not automatically attach its phase service to an engine lifecycle.

The host supplies a pinned price revision and an upper bound covering both jobs' enforced resource ceilings. Money usage remains unknown because the sandbox result protocol does not contain authenticated billing. The existing evaluator ledger keeps the full allowance charged. Explicit token-only policy is supported with a zero money allowance and no price revision.

`Evaluate` retains the original call admission before the service starts its phases. It may return `ErrCallUncertain` while work is pending. The existing verification worker can later call `Reconcile`, which reads and validates retained output without submitting, cancelling, collecting, or running another evaluator.

Before each first job submission, the service writes an immutable intent. A retry inspects the original job ID; it never submits that identity again. After a restart, a completed candidate can allow the original grader to start if its submission intent was not consumed. A crash after consuming an intent but before the broker accepted it remains uncertain without an original broker receipt. There is no replacement ID, inferred success, or refund. Call IDs cannot move to another manifest within the journal.

Keep the journal and manifest registrations for as long as their admissions must be recoverable. Journal records contain references, contracts, and reports, not candidate or export artifact bodies. Apply your retention and access policy to this metadata. Artifact deletion still goes through the evidence repository and prevents cached results from becoming usable again.

## Verification boundary

The tests exercise protected files, signed fixture broker responses, process locking, restart recovery, the existing evaluator executor, and real SQLite persistence. Shared observation conformance also runs against the Postgres and Mongo stores when their disposable containers are available. These tests do not qualify a live Linux worker. Runtime qualification, source and artifact grants, current deployment identity, and provisioned runner credentials remain host requirements.
