# Cortex A2A Plan 2: persistence and the engine

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put the `a2a` package behind real storage and wire it into the engine, so an agent can call `agent_send`, `agent_ask` and `agent_inbox` for real.

**Architecture:** `a2a.Store` gets an implementation per backend and folds into the composite `store.Store`. The engine grows a new suspension reason, a builtin tool contract that can pend, three builtin tools, a `WithA2A` option, lifecycle wiring, and three plugin hooks.

**Tech Stack:** Go 1.26, grove (the repo's query builder), sqlite/postgres/mongo backends, the existing `storetest` conformance harness.

**Spec:** [docs/superpowers/specs/2026-08-26-cortex-a2a-messaging-design.md](../specs/2026-08-26-cortex-a2a-messaging-design.md)

**Depends on:** [Plan 1](2026-08-26-cortex-a2a-1-package.md), complete.

## Deviation from the usual plan format, stated up front

Plan 1 carried full code for every step. This one carries full code for the parts where a wrong choice is expensive (the suspension reason, the resume gating, the builtin outcome contract, the tool schemas) and carries interfaces, file lists, test names and the specific gotchas for the parts that are mechanical repetition of an existing pattern (three store backends implementing sixteen methods each). The store work has a reference implementation to copy from in the same repo: [store/sqlite/orchestration.go](../../../store/sqlite/orchestration.go), [store/postgres/orchestration.go](../../../store/postgres/orchestration.go) and [store/mongo/orchestration.go](../../../store/mongo/orchestration.go). Restating 1,100 lines of that pattern in a plan document would not make it more correct.

## Global Constraints

- Everything from Plan 1's global constraints still holds, in particular TDD and lint-clean.
- **Scope is stamped on write and filtered on read.** Every store method reads `cortex.ScopeFromContext`, returns `cortex.ErrNoScope` on a zero scope, and filters with `scopePredicates`. This is not optional: [store/scopespy](../../../store/scopespy) exists because a scope sitting available and unread is how every row ended up in one bucket once already.
- **Scope columns are never updated.** Each backend keeps a `mutable...Columns` whitelist, mirroring `mutableOrchestrationConfigColumns`. Grove builds SET from every model field otherwise, and an update from a broader context would silently widen a row's stored scope.
- **Migrations are additive and idempotent.** New tables only, `CREATE TABLE IF NOT EXISTS`, with a `Down` that drops them. Never edit a shipped migration.
- **Postgres and mongo cannot be tested without Docker.** If containers do not start, say so and do not claim those backends pass.

---

### Task 1: sqlite store

**Files:**
- Create: `store/sqlite/a2a.go`
- Modify: `store/sqlite/models.go` (four models plus to/from converters), `store/sqlite/migrations.go` (one new migration)
- Test: `store/storetest/conformance.go` (new cases, run by all three backends)

**Interfaces:**
- Produces: `*sqlite.Store` satisfying `a2a.Store` in full: the sixteen methods listed in [a2a/store.go](../../../a2a/store.go).

Four tables, following the naming of the existing ones:

| Table | Notes |
|---|---|
| `cortex_a2a_messages` | Envelope. Receivers, reply_to and metadata are JSON columns; the 13 ACL parameters are real columns. Index on `(scope_canon, conversation_id)`. |
| `cortex_a2a_conversations` | Index on `(scope_canon, status)`. |
| `cortex_a2a_deliveries` | Index on `(scope_canon, receiver_agent, state)`, which is the inbox query, and on `state` for the redrive query. |
| `cortex_a2a_pending_asks` | Unique index on `reply_with`. That uniqueness is what makes the claim safe. |

- [ ] **Step 1: Write the failing conformance cases**

Add to `store/storetest/conformance.go`, inside the existing `Conformance` function's subtest list. Every backend runs these, so they are written once:

```go
	t.Run("A2AMessageRoundTrip", func(t *testing.T) { a2aMessageRoundTrip(t, s) })
	t.Run("A2AConversationHops", func(t *testing.T) { a2aConversationHops(t, s) })
	t.Run("A2ADeliveryStates", func(t *testing.T) { a2aDeliveryStates(t, s) })
	t.Run("A2AClaimPendingAskOnce", func(t *testing.T) { a2aClaimPendingAskOnce(t, s) })
	t.Run("A2AExpiredAsks", func(t *testing.T) { a2aExpiredAsks(t, s) })
	t.Run("A2AScopeIsolation", func(t *testing.T) { a2aScopeIsolation(t, s) })
```

The bodies mirror the memStore tests from Plan 1 Task 4, with two additions no in-memory double could prove:

- `a2aClaimPendingAskOnce` runs the two claims **concurrently** from eight goroutines and asserts exactly one success and seven `ErrAskAlreadyClaimed`. Against a real database this exercises the unique index and the UPDATE ... WHERE claimed_at IS NULL, which is the actual mechanism.
- `a2aScopeIsolation` writes under scope A and reads under scope B, asserting nothing crosses. Use the `ctxWithScope` helper already in the file.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./store/sqlite/ -run TestConformance`
Expected: FAIL to compile, `*Store does not implement a2a.Store`.

- [ ] **Step 3: Write the models and the migration**

Follow `orchestrationConfigModel` in [store/sqlite/models.go:951](../../../store/sqlite/models.go) exactly: bun struct tags, the five scope columns (`scope_l0`, `scope_l1`, `scope_l2`, `scope_extra`, `scope_canon`), JSON marshalling for slice and map fields, and a `...FromModel` returning an error when a JSON column fails to decode.

The migration goes at the end of the migration list with the next sequence number, name `create_a2a`, comment "Create cortex_a2a_messages, cortex_a2a_conversations, cortex_a2a_deliveries and cortex_a2a_pending_asks tables". Scope columns are in the CREATE TABLE from the start, so there is no follow-up scope migration.

- [ ] **Step 4: Write the sixteen methods**

Copy the shape of [store/sqlite/orchestration.go](../../../store/sqlite/orchestration.go). The three that are not boilerplate:

```go
// ClaimPendingAsk takes the row only if nobody else has. The WHERE clause
// is the claim: two callers racing both issue this UPDATE, and exactly one
// of them changes a row.
func (s *Store) ClaimPendingAsk(ctx context.Context, replyWith string) (*a2a.PendingAsk, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	now := time.Now().UTC()
	q := s.sdb.NewUpdate(&a2aPendingAskModel{}).
		Set("claimed_at = ?", now).
		Where("reply_with = ?", replyWith).
		Where("claimed_at IS NULL")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex/sqlite: claim pending ask: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("cortex/sqlite: claim pending ask: %w", err)
	}
	if affected == 0 {
		// Nothing changed for one of two reasons, and the caller needs to
		// tell them apart: a claim that lost a race is normal, a claim for
		// a token nobody minted is a bug somewhere upstream.
		exists, existsErr := s.pendingAskExists(ctx, replyWith)
		if existsErr != nil {
			return nil, existsErr
		}
		if exists {
			return nil, a2a.ErrAskAlreadyClaimed
		}
		return nil, a2a.ErrAskNotFound
	}
	return s.getPendingAsk(ctx, replyWith)
}
```

`ClaimDelivery` follows the same shape with `Where("state = ?", a2a.DeliveryQueued)` and `Set("state = ?", a2a.DeliveryDelivering)`, returning `a2a.ErrDeliveryAlreadyClaimed` and `a2a.ErrDeliveryNotFound`.

`ListQueuedDeliveries` deliberately does **not** filter by scope: the dispatcher redrives across every scope in the process, and each delivered message carries its own scope forward. Write that reason in a comment above the method, because it is the one place in this file that breaks the rule the rest of it follows.

- [ ] **Step 5: Run the conformance suite**

Run: `go test ./store/sqlite/ -race -v -run TestConformance`
Expected: PASS, including the concurrent claim case.

- [ ] **Step 6: Commit**

```bash
git add store/sqlite/ store/storetest/
git commit -m "feat(store/sqlite): persist a2a messages, conversations, deliveries and asks"
```

---

### Task 2: Fold `a2a.Store` into the composite

**Files:**
- Modify: `store/store.go`
- Test: the existing `scopespy` completeness test, which enumerates the composite's methods

- [ ] **Step 1: Add the interface**

```go
	orchestration.ConfigStore
	orchestration.RunStore
	a2a.Store
```

- [ ] **Step 2: Run**

Run: `go build ./... && go test ./store/...`
Expected: postgres and mongo now fail to compile, which is the correct signal and is what Tasks 3 and 4 fix. Sqlite passes.

- [ ] **Step 3: Commit after Tasks 3 and 4**

The tree does not build between here and Task 4, so this task's changes are committed together with them.

---

### Task 3: postgres store

Same as Task 1, against [store/postgres/orchestration.go](../../../store/postgres/orchestration.go). Differences that matter:

- Migrations are SQL files under `store/postgres/migrations/`, not inline strings. Follow the numbering already there.
- JSON columns are `jsonb`.
- `ClaimPendingAsk` can use `UPDATE ... RETURNING`, which collapses the claim and the read into one statement. Prefer it.

Run: `go test ./store/postgres/ -race -run TestConformance`. Postgres containers start fine in this environment, so this one is genuinely verifiable.

---

### Task 4: mongo store

Same again, against [store/mongo/orchestration.go](../../../store/mongo/orchestration.go). Differences that matter:

- Mongo has no migrations in the SQL sense; index creation lives in [store/mongo/migrations.go](../../../store/mongo/migrations.go).
- The claim is `FindOneAndUpdate` with a filter of `{reply_with: x, claimed_at: nil}` and `ReturnDocument: After`. That is atomic, so no separate existence check is needed for the success path.
- The unique index on `reply_with` must be created, or the claim's atomicity rests on nothing.

Run: `go test ./store/mongo/ -race -run TestConformance`. Same caveat about Docker.

Commit Tasks 2, 3 and 4 together, because the tree does not build in between:

```bash
git add store/
git commit -m "feat(store): persist a2a across postgres and mongo, and fold it into the composite"
```

---

### Task 5: `ReasonAgentReply` and resume gating

This is the load-bearing engine change. A host must not be able to forge a peer's reply.

**Files:**
- Modify: `suspension/suspension.go`, `engine/resume.go`
- Test: `engine/a2a_resume_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestResumeRefusesAnAgentReplyPause(t *testing.T) {
	// A run paused waiting on a peer is not the caller's to answer. The
	// public Resume must refuse it exactly like an approval pause.
	e, runID := suspendedOnAgentReply(t)
	_, err := e.Resume(ctx, runID, ResumeInput{Results: []ToolResult{{CallID: "call-1", Result: "forged"}}})
	if !errors.Is(err, ErrNotResumable) {
		t.Fatalf("err = %v, want ErrNotResumable", err)
	}
}

func TestResumeAgentReplyContinuesTheRun(t *testing.T) {
	// The bus's own path does resume it, and the model sees the reply as
	// the tool result.
	e, runID := suspendedOnAgentReply(t)
	if err := e.resumeAgentReply(ctx, runID, "call-1", `{"content":"all clear"}`); err != nil {
		t.Fatalf("resumeAgentReply: %v", err)
	}
	// assert the run completed and the fake LLM saw a tool message
	// carrying "all clear"
}
```

- [ ] **Step 2: Implement**

In `suspension/suspension.go`:

```go
	// ReasonAgentReply means the run is waiting on another agent's answer.
	//
	// It is not ReasonExternalTool even though both wait on something
	// outside the loop, because the two say different things about who
	// acts next. External says the CALLER executes the call and reports
	// back. Agent-reply says cortex itself is waiting on a peer, and a
	// caller answering it would be forging a message the peer never sent.
	ReasonAgentReply SuspendReason = "agent_reply"
```

In `engine/resume.go`, `claimForResume` already refuses an approval pause. Extend that check so a public `Resume` refuses `ReasonAgentReply` too, and add the internal entry point the bus uses:

```go
// resumeAgentReply continues a run that was waiting on a peer. It is the
// only path allowed to answer a ReasonAgentReply pause, and it is not
// exported: the correlation ledger is what decides a reply is genuine, and
// a public caller has no ledger row to prove it with.
func (e *Engine) resumeAgentReply(ctx context.Context, runID id.AgentRunID, callID, result string) (*run.Run, error) {
	return e.resume(ctx, runID, ResumeInput{Results: []ToolResult{{CallID: callID, Result: result}}}, resumeSourceAgentReply)
}
```

`resume` currently takes `approved bool`. Two callers with two different privileges was already a boolean; three is where a boolean stops being honest. Replace it with a small `resumeSource` enum (`resumeSourcePublic`, `resumeSourceApproval`, `resumeSourceAgentReply`) and switch on it. Update the two existing call sites.

- [ ] **Step 3: Run and commit**

Run: `go test ./engine/ ./suspension/ -race`

```bash
git add engine/ suspension/
git commit -m "feat(engine): add the agent-reply suspension reason and gate its resume"
```

---

### Task 6: The builtin outcome contract, and the three tools

**Files:**
- Modify: `engine/tools.go`, `engine/react.go`, `engine/options.go`
- Create: `engine/a2a_tools.go`
- Test: `engine/a2a_tools_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestAgentAskSuspendsTheRun(t *testing.T)                       // reason is ReasonAgentReply
func TestAgentAskSiblingToolCallStillCompletes(t *testing.T)        // one step, two calls, one suspend
func TestAgentSendDoesNotSuspend(t *testing.T)                      // returns a result, run continues
func TestAgentInboxReturnsDeliveredMessages(t *testing.T)
func TestA2AToolsAbsentWithoutWithA2A(t *testing.T)                 // no option, no tools in the list
func TestAuthorizerDenialNeverReachesTheBus(t *testing.T)           // no message written
func TestErrRequiresApprovalOnAgentAskOpensACheckpoint(t *testing.T)
```

- [ ] **Step 2: Change the builtin contract**

`executeBuiltinTool` returns `(string, bool)` today, so a builtin can only complete. It becomes:

```go
// executeBuiltinTool attempts to execute a built-in tool. The second return
// says whether this call was handled here at all; the outcome says how it
// ended, because agent_ask does not complete: it pends, and the loop
// suspends the step around it.
func (e *Engine) executeBuiltinTool(ctx context.Context, inv cortex.Invocation) (string, toolOutcome, bool)
```

`dispatchTool` propagates the outcome instead of assuming `outcomeCompleted`, and `executeTool` maps a pending builtin to `suspension.ReasonAgentReply` the way it maps an external tool to `ReasonExternalTool`.

- [ ] **Step 3: Write the tools**

Schemas, matching the spec §7.2. `agent_ask` calls `bus.Ask` with `inv.Subject.RunID` and `inv.Call.ID`, then returns `("", outcomePending, true)`. `agent_send` and `agent_inbox` return JSON results and `outcomeCompleted`.

The tools appear in `builtinTools()` only when `e.a2a != nil`, mirroring the knowledge gate.

- [ ] **Step 4: Add the option**

```go
// WithA2A turns on agent-to-agent messaging. The three tools appear only
// when it is set, so a host that does not configure it sees no new tools
// and no new tables touched.
func WithA2A(opts a2a.Options) Option
```

It builds the bus from `e.store` (which satisfies `a2a.Store` after Task 2), the engine's own runner adapter, a resumer wrapping `resumeAgentReply`, and a hooks adapter over `e.extensions`. It errors if there is no store.

- [ ] **Step 5: Run and commit**

Run: `go test ./engine/ -race`

```bash
git add engine/
git commit -m "feat(engine): let a builtin pend, and add the three a2a tools"
```

---

### Task 7: Lifecycle and hooks

**Files:**
- Modify: `engine/engine.go` (Start/Stop), `plugin/plugin.go`, `plugin/registry.go`
- Test: `engine/a2a_lifecycle_test.go`, `plugin/registry_test.go`

- [ ] **Step 1: Wire the lifecycle**

`Engine.Start` starts the bus dispatcher and redrives orphaned deliveries; `Engine.Stop` stops it and waits, joining rather than signalling, the way `stopSweeper` already does. The ask sweep runs on the bus's own interval, ahead of the engine's suspension sweep.

- [ ] **Step 2: Add the hooks**

Three new per-hook interfaces beside `AgentHandoff`, plus their registry emitters and type-cached dispatch:

```go
// MessageSent fires when an envelope is accepted and queued.
type MessageSent interface {
	OnMessageSent(ctx context.Context, msgID id.MessageID, from, to, performative string)
}

// MessageDelivered fires when an envelope reaches a receiver.
type MessageDelivered interface {
	OnMessageDelivered(ctx context.Context, msgID id.MessageID, to string)
}

// MessageRefused fires when delivery is refused: an exhausted hop budget,
// an unroutable address, a failed delivery.
type MessageRefused interface {
	OnMessageRefused(ctx context.Context, msgID id.MessageID, to, reason string)
}
```

`AgentHandoff` is untouched. Orchestration handoffs and ACL messages are different events, and collapsing them would lie to every existing subscriber.

- [ ] **Step 3: Run and commit**

Run: `go test ./... -race` (mongo excluded if Docker is down)

```bash
git add engine/ plugin/
git commit -m "feat(engine): run the message bus with the engine, and emit its hooks"
```

---

### Task 8: The end-to-end test

One test that proves the whole thing, with a fake LLM and a real sqlite store: agent A's model calls `agent_ask`, A's run suspends with `ReasonAgentReply`, the dispatcher runs B, B's output comes back as a reply, A resumes and its model sees the answer as the tool result.

**Files:**
- Test: `engine/a2a_e2e_test.go`

This is the test that would have caught every integration mistake the unit tests cannot see, so it is worth writing even though every piece under it is already covered.

```bash
git add engine/
git commit -m "test(engine): prove the ask, run, reply, resume loop end to end"
```

---

## Self-review

**Spec coverage.** §6 to Tasks 1 through 4, §7.5 to Task 6, §9.5 to Task 5, §10.1 to Task 6, §10.2 and §10.3 to Task 5, §10.4 to Task 6, §10.5 and §10.6 to Task 7, §10.7 and §10.8 landed in Plan 1.

**Carried forward from Plan 1.** The stale-`delivering` gap: a delivery claimed by a process that dies is never redriven. Task 1 gives the delivery row a `claimed_at`, and the redrive query picks up rows in `delivering` older than a threshold. That is only testable against a real store, which is why it lands here and not in Plan 1.
