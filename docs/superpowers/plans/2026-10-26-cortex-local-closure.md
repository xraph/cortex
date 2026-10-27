# Cortex local closure follow-up

Continue the assured-execution design without running or claiming live Linux qualification. Work directly on main, preserve concurrent edits, and commit and push verified changes.

Spec: `../specs/2026-10-18-cortex-assured-execution-design.md`. The existing authority wire protocol in `integrations/sandbox/cmd/cortex-sandboxd/README.md` is binding.

## Global constraints

The authority cannot qualify itself from a request or a fixture. It must authenticate the caller, verify a trusted producer's signed retained record and its probe output, bind the exact host boot and deployment, and check current policy on every request. Keep all resource use bounded. Linux execution remains deferred.

## Review focus

Forged producer signatures, passing-looking logs with failed or missing terminal results, cross-host evidence, changed retained files, policy revocation during reads, expired decisions, mTLS identity confusion, unbounded connections and incomplete shutdown.

### Task 1: Retained evidence authority

Interfaces: consume the existing qualification request/response and host filesystem pins. Provide a protected authority handler that validates signed records and retained Go test JSON probe results. Reuse the bounded TLS server lifecycle.

1. Add failing authority tests for a complete signed record, revoked policy, tampered files, failed/partial/fixture evidence and mismatched identities.
2. Run focused tests. Expected: new authority APIs are absent.
3. Implement strict record validation, protected policy reads, current authorization and bounded decisions. No evidence is generated from request fields.
4. Run authority and client tests with race detection. Expected: pass.
5. Format, lint, build, test and commit the coherent authority implementation.

### Task 2: Installed command and closure record

Interfaces: consume Task 1 through a bounded installed mTLS server command. Keep credential loading and transport lifecycle shared with the broker.

1. Add command/configuration tests and observe failure before implementation.
2. Implement the installed command and verify a real local TLS client/authority exchange, current revocation, restart and shutdown.
3. Correct stale sandbox documentation and record exact local evidence and deployment-only checks. Review release and CI coverage.
4. Run final formatting, all-module lint, sandbox race tests, Linux cross-builds and command tests. Expected: pass; live qualification is not run.
5. Obtain an independent review, address consequential findings, commit and push. Verify CI on the pushed commit.
