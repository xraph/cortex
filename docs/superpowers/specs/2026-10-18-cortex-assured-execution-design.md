# Cortex assured execution design

Status: proposed architecture for the four-stage implementation plan. This document describes work to build; it does not claim that these capabilities are installed or verified.

Date: 2026-10-18. Source baseline: Cortex `685e9ae`. Recheck HEAD and concurrent changes before each work packet.

Roadmap: [Implementation roadmap](../plans/2026-10-18-cortex-assured-execution.md).

## 1. Product contract

You should be able to ask Cortex to perform a task, inspect its plan, interrupt or resume it, and see which outcomes were verified against which inputs. Every claimed result must distinguish execution status, verification status, freshness and remaining uncertainty. Open-ended correctness is not guaranteed. The system enforces declared checks and reports the limits of those checks.

The delivery covers four stages: trustworthy termination and safety; evidence and evaluation; planning and teamwork; adaptive behavior. Each stage ships a usable increment behind explicit capabilities. Existing ReAct callers remain supported. New verification-required workflows cannot silently fall back to unchecked execution.

Required behavior:

- Define acceptance criteria before execution and preserve their revision throughout the run.
- Track a candidate answer separately from its published answer and its verification report.
- Recheck authorization when an operation executes, resumes, exports an artifact or publishes a result.
- Replan when evidence invalidates a prerequisite, while retaining the old plan and reason for revision.
- Keep human checkpoints, tool policy, evaluation criteria and resource limits outside model control.
- Make collaboration accountable through durable tasks, scoped assignments, artifact versions and verifier decisions.
- Run generated code and tests in a qualified isolated environment. Evaluate the final external state when the task requires it.
- Record useful lessons with provenance, expiry, contradiction and revocation handling.
- Measure false-success, task success, recovery, unsafe publication, duplicate effects and cost per verified outcome.

## 2. Current behavior and source anchors

| Area | Existing source | Consequence for implementation |
| --- | --- | --- |
| Runtime | `engine/react.go`, `engine/resume.go`, `suspension/suspension.go` | Sync and streaming loops share continuation data but duplicate loop behavior; centralize policy and finalization before adding more branches |
| Completion | `run/run.go`, `engine/react.go` | Step exhaustion can reach completed; no required acceptance gate |
| Safety | `safety/safety.go`, `safety/shield/adapter.go` | Input/output scanner exists; errors currently continue; streaming publishes tokens before final scan |
| Tools | `authz.go`, `engine/react.go`, `engine/tools.go` | Authorizer and approval suspension exist; registered handlers run inside the host process |
| Context | `engine/prompt.go`, `memory/`, `knowledge/` | Scoped prompt composition and retrieval exist; no immutable evidence-bound execution contract |
| Cognition | `cognitive/cognitive.go`, `persona/persona.go`, `behavior/behavior.go` | Stored phases and preferences are not a functioning phase controller |
| Evaluation | `sentinel/adapter.go`, `sentinel/plugin.go` | Target adapter starts a new agent run; automatic completion evaluation is a TODO |
| Teamwork | `orchestration/`, `a2a/` | Six strategies, messaging, tendering, retries and hop containment exist; a task DAG and verified handoff protocol are additional work |
| Streams | `extension/contract/stream.go`, `a2aremote/stream.go` | Dashboard replay is temporary; remote task streaming primarily follows task state |
| Learning | `integrations/fabriq/plugin.go` | Completion/failure memories exist; completion alone does not establish a verified fact |
| Persistence | `store/{sqlite,postgres,mongo}`, `store/storetest`, `store/scopespy` | All backends need matching semantics; reads currently enforce only the first three scope levels |

These are source observations. Stage gates require executable regression and integration evidence.

The companion [optimization design](2026-10-18-cortex-optimization-design.md) was committed as `c9bee40` during planning. Leave that work under its existing owner. Its prepared execution, recorded Sentinel scoring and Fabriq lesson contracts overlap with this program. The roadmap assigns one owner for each shared contract. This design defines its own runtime contracts; Stage 4 explicitly consumes the companion optimizer deliverables.

## 3. Package and dependency boundaries

Keep interfaces and deterministic policies in the root module. Heavy runtimes stay in their own modules. Import direction is from adapters toward root ports.

| Package/module | Responsibility | Ownership boundary |
| --- | --- | --- |
| `run/` | Execution state, stop reason, result publication metadata | No scoring or model calls |
| `evidence/` (new root package) | Immutable artifact/context references, digests, claims and storage port | No engine dependency or credential payloads |
| `verification/` (new root package) | Acceptance contracts, verifier port, reports, aggregation and freshness | Does not start agents or import `engine` |
| `execution/` (new root package) | Job receipts, effect records, reservations, shared budgets and recovery contracts | No provider SDK |
| `event/` (new root package) | Public event envelope and durable replay port | Persist only publication-approved content |
| `sandbox/` (new root package) | Isolation policy, job specification, runner interface and capability receipt | No shell/container runtime dependency |
| `planning/` (new root package) | Versioned task DAG, validation, readiness and replan decisions | Scheduler does not own model/provider calls |
| `coordination/` (new root package) | Assignment claims, handoffs, conflicts and durable task ownership | Reuses A2A delivery and orchestration runners |
| `cognitive/` | Runtime capability/assumption snapshots and bounded transition policy | Agent-written scores never become calibrated probabilities |
| `learning/` (new root package) | Evidence-qualified lesson and eligibility contracts | Fabriq remains the durable recall integration |
| `engine/` | Applies all policies to existing run/resume/stream paths | One integrator owns loop and lifecycle wiring |
| `sentinel/` (existing module) | Verification adapter and completion-evaluation worker | Sentinel owns scorer implementations and suite comparisons |
| `integrations/sandbox/` (new module) | Remote client, authenticated broker and Linux OCI adapter | Qualified sandbox runtime required; never fallback to host execution |
| `integrations/fabriq/` | Lesson storage/recall adapter and publication worker | No Cortex types in Fabriq core |
| `api`, `extension`, dashboard plugin | Authorized commands, capabilities and compact review UI | Shared contract transport, host identity and native design system |

The sandbox broker uses an OCI runtime with gVisor `runsc` as the initial Linux qualification target. Host deployments can supply another runner implementing the same conformance suite. Local macOS development uses a remote Linux worker or a qualified Linux VM; a fake runner is labeled fixture-only. Runtime/image versions are pinned in the sandbox module's compatibility manifest during task S2.4, after the API and integration checks there pass. No claim of isolation is made from an interface or mocked test.

## 4. Shared proposed contracts

Names below are proposed additions, not existing APIs. Freeze these contracts in task F0 before independent implementation. The types are deliberately transport-neutral. Use `id.ID` with validated module-owned prefixes for new identities; existing run and agent IDs keep their current aliases.

### Result lifecycle

Keep `run.State` for compatibility and add `run.StopReason`: `final_answer`, `step_limit`, `token_limit`, `cost_limit`, `deadline`, `cancelled`, `safety_block`, `policy_unavailable`, `tool_error`, `persistence_error`, `verification_failed`. Add `StateIncomplete`; old completed rows are never backfilled as verified.

`verification.Status` is `unchecked`, `pending`, `passed`, `failed`, `inconclusive`, or `stale`. Publication status is `withheld`, `provisional`, `committed`, or `blocked`. A committed response can explicitly report an inconclusive task; committed means the message may be published, not that the task succeeded. `ResultSucceeded` requires a terminal successful execution, a current passed report for all required criteria, and a committed result matching the report digest.

`run.Run` gains additive stop reason and result reference fields. Do not put large evidence blobs in `Metadata`. Legacy `completed` continues to mean execution ended; new clients use the separate verification fields. A final output cut off by provider length is incomplete. A step containing pending external calls remains paused with its consumed budget preserved.

### Evidence and verification

```go
// evidence package
// Ref: ID, SHA256, media type, bytes, scope, immutable version, producer,
// source URI, sensitivity, created time, retention policy; credentials excluded.
type Repository interface {
    Put(context.Context, Artifact) (Ref, error)
    Open(context.Context, Ref) (io.ReadCloser, error)
    Delete(context.Context, Ref, string) error
}

// verification package
// Request carries Contract, Candidate ref, Evidence refs, ContextSnapshot,
// exact scope, run ID, deadline and verifier revision, never executable callbacks.
type Verifier interface {
    Verify(context.Context, Request) (Report, error)
}
func Aggregate(contract Contract, reports []Report, at time.Time) Decision
func CheckFreshness(report Report, current Binding, at time.Time) Status
```

`Contract` contains ID/revision/digest, objective, constraints, and ordered `Criterion` records. Each criterion has ID, kind, required flag, verifier name/revision, typed parameters, freshness policy and evidence requirements. Kinds are `schema`, `test`, `state`, `semantic`, `human`, `policy`. Typed parameters have a schema per registered verifier; model text cannot introduce new executable evaluators. Required criteria are all-of; optional weighted scores cannot override a failed or missing required criterion.

`ContextSnapshot` records ordered scope, principal/authority reference, artifact versions, prepared execution digest, source digests/retrieval time, environment image/dataset revision, tool revisions and policy revision. `Report` binds all of these plus contract digest, checked-at time, expiry and per-criterion outcome. Missing evidence, evaluator errors and inaccessible expected state produce inconclusive or failure according to the registered criterion policy, never pass. Retractions are append-only records; deleting bytes retains an authorized audit tombstone.

### Prepared execution and budgets

Reuse the proposed optimizer seam: `Engine.PrepareAgent(ctx, agentID, overrides) (*PreparedAgent, error)` and `Engine.RunPrepared(ctx, prepared, input, PreparedRunOptions) (*run.Run, error)`. Add `StreamPrepared` only through the same prepared-run construction. `PreparedRunOptions` carries explicit session, execution purpose, provenance and acceptance-contract reference. Normal, evaluation, optimization trial and optimization controller purposes are trusted host metadata.

Preparation freezes effective prompt sections, tools including an explicit empty allowlist, model revision, knowledge references, safety profile and provenance. Current authorization still applies at use time. `execution.Budget` contains step, token, deadline, tool-call, fan-out, replan and optional integer currency limits. Reserve usage atomically across parent and children before dispatch. Settle actual usage once; unknown usage remains reserved or pessimistically charged. A provider without a bounded monetary estimate cannot advertise a hard money ceiling.

### Safety and publication

```go
// safety package
type PublicationGate interface {
    Check(context.Context, PublicationRequest) (PublicationDecision, error)
}
// engine package, proposed options
func WithVerification(v verification.Verifier) Option
func WithPublicationGate(g safety.PublicationGate) Option
func WithSandbox(r sandbox.Runner) Option
func WithEventStore(s event.Store) Option
```

Policies are `buffered`, `checked_segments`, and `disabled_explicit`. Buffered is the default when a safety scanner is configured. With no scanner, capability output says unscanned; hosts may require a scanner and reject startup/run preparation if absent. Scanner errors fail closed under required safety policy. Permissive behavior requires explicit host configuration, visible degraded status and audit.

Input checking covers untrusted user input and newly introduced tool/retrieval/peer content, with provenance and size limits. Redacted input must replace the content actually sent. Scanning is defense in depth; prompt injection cannot grant tools, alter evaluation criteria or publish artifacts. Segment mode is available only when the provider declares a supported incremental decision contract; inconclusive boundaries stay buffered. A final scan cannot retract an unsafe segment, so policy must not promise full-response confidentiality under segment mode.

`event.Envelope` contains run ID, monotonic sequence, event ID, schema version, type, public payload, occurrence time and evidence references. Distinguish execution-finished, verification-finished and result-committed. Preserve legacy event mapping without using `done` as an assurance claim. Internal raw candidates have a separate authorized storage path and never enter public replay.

### Sandboxed evaluation

```go
// sandbox package
type Runner interface {
    Capabilities(context.Context) (Capabilities, error)
    Submit(context.Context, Job) (Receipt, error)
    Inspect(context.Context, id.ID) (JobStatus, error)
    Cancel(context.Context, id.ID) error
    Collect(context.Context, id.ID) (Result, error)
}
```

`Job` includes idempotency key, exact scope, principal reference, allowed image digest, command argv, read-only input artifact refs, writable scratch policy, network policy, environment allowlist and finite resource limits. Commands are job data interpreted by the isolated runtime, never interpolated into a host shell. `Receipt` binds the accepted job digest, runtime version, image digest, limits and isolation capabilities. Results contain exit reason, bounded stdout/stderr refs, immutable artifact refs and runner-signed provenance verified by the broker client.

Generated programs and graders run as separate jobs. The orchestrator and scorer credentials stay outside both. Protect authoritative test sources and expected answers; the grader receives a read-only candidate export and cannot promote artifacts. Authoritative verdicts come from an external trusted supervisor. In-process unit tests that import candidate code are themselves exposed to tampering and require independent corroboration, even when test files are read-only. Export is a distinct authorized operation that checks destination, digest, path safety, content policy and current grant. Network defaults to denied, including metadata endpoints, host services and private ranges; approved egress is enforced outside the workload, including DNS and redirects.

### Plans and handoffs

```go
// planning package
func Validate(plan Plan, limits Limits) error
func Ready(plan Plan, tasks []Task) []id.ID
func ApplyRevision(current Plan, proposal Revision, expected int64) (Plan, error)
// coordination package
type Scheduler interface {
    Tick(context.Context, id.ID) error
}
```

A `Plan` has objective, acceptance-contract reference, immutable revision, scope, budget and task graph. A `Task` has ID, owner capability, dependencies, allowed resources, expected artifacts, verification requirements, deadline, effect class and retry/compensation policy. Task states: pending, ready, claimed, running, waiting, verifying, succeeded, failed, blocked, cancelled. Persist lease owner, lease expiry, fencing token and expected revision with every claim.

A `Handoff` references plan/task revision, assignment ID, sender/receiver identities, authority attenuation, artifact digests, acceptance criteria and deadline. Acceptance does not mean success. Worker success requires verified task outputs. Existing A2A envelopes carry a versioned structured content type; retain legacy text support. Untrusted remote claims require local verification or an explicitly trusted verifier identity.

Effects have a logical operation key that survives retries. Read-only and declared idempotent effects may retry under policy. An unknown result from a non-idempotent external action enters reconciliation; do not promise universal exactly-once execution. Compensation is an explicit authorized action, not automatic reversal of any tool call.

### Adaptive behavior and learning

`cognitive.Snapshot` holds observable capabilities, permission snapshot, assumptions, unresolved questions, contradictory evidence, task progress and remaining budget. Store short decision summaries and evidence references, not hidden chain-of-thought. `cognitive.Decide(snapshot, policy) Decision` chooses continue, retrieve, verify, replan, delegate, ask or stop from deterministic policy plus an optional bounded structured model proposal. All actions reenter the normal authorization and budget paths.

`learning.Lesson` contains statement, applicability, evidence class, source reports, scope, domain, expiry, revocation and supersession. Observation, validated result and failed approach are distinct classes. Claims about reliability come from versioned evaluation cohorts with sample counts and uncertainty; sparse evidence means unknown. Improvement proposals are versioned offline experiments, with separate discovery/holdout sets, explicit activation and rollback. Reuse the optimization workstream when available; do not create a second optimizer here.

## 5. Persistence, rollout and compatibility

Use optional capability interfaces (`verification.Store`, `event.Store`, `planning.Store`, `coordination.Store`, `execution.Store`, `learning.Store`) implemented by all three built-in stores. Keep the existing composite `store.Store` source-compatible initially. Engine options validate that requested features have their required stores. Missing capability returns a typed unavailable error, never an in-memory production fallback. Capability conformance helpers and `scopespy` must cover every new call.

Atomic store operations group result/report/event publication, claims with fences, budget reservations with attempts, and accepted lessons with outbox entries. No transaction spans an LLM or remote tool call. SQLite uses a write transaction; Postgres uses transactions and row/CAS locking; Mongo uses conditional updates and single-document embedded outboxes for these aggregates. If an operation needs multiple Mongo documents atomically, require a transaction-capable deployment explicitly. Do not implement best-effort equivalents.

Stage 1 rejects unsupported depth greater than three and ambiguous scope encodings before execution or storage access. Inventory existing deep rows and quarantine them for authorized migration. Full arbitrary-depth filtering is a separate compatibility decision, not a silent change to the current ordered-prefix contract. Existing one-to-three-level list prefix access still requires host authorization; resource mutations verify exact authorized identity.

Use additive migrations, legacy decode fixtures and current/previous reader tests. Defaults never turn old completed rows into verified successes. New runtime paths are opt-in until their gates pass. You can disable scheduling and adaptation without disabling safety checks or hiding existing evidence. An old binary that cannot honor a persisted new policy must refuse to resume that run.

## 6. Threat model and validation principles

The model, generated code, tool outputs, retrieved documents, peer messages and candidate artifacts can all be wrong or hostile. The host identity resolver, policy configuration, store and broker form the trusted control plane. Sandbox isolation does not grant authority to publish, and policy checks do not establish OS isolation.

Test prompt injection, evaluator tampering, artifact substitution, tenant leakage, stale evidence, duplicate effects, crash windows, resource exhaustion, unavailable scanners, disconnected streams and revoked permissions. Use a fake clock for deterministic deadline/lease tests and Linux process/container tests for real isolation. Keep fixture, backend-integrated, browser-verified and deployed evidence separate.

External references used for design direction, not Cortex capability claims: [agent evaluation guidance](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents), [gVisor security model](https://gvisor.dev/docs/architecture_guide/security/). Recheck runtime documentation and versions during qualification.
