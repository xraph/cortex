# Sandbox evidence authority

Build from `integrations/sandbox` and install on a protected Linux authority host:

```sh
GOWORK=off go build -trimpath -o cortex-sandbox-authority ./cmd/cortex-sandbox-authority
cortex-sandbox-authority --config /etc/cortex/authority.json
```

Run the service as root. It serves the existing broker qualification protocol over
TLS 1.3 with required client certificates. SIGINT and SIGTERM stop admission,
close connections and join active handlers. Startup and shutdown diagnostics never
include credentials, request contents or retained evidence paths.

This service verifies retained evidence. It does not run workloads or probes, sign
probe reports, or turn local fixtures into Linux qualification. You still need to
select the worker and run the live acceptance checks before enabling admission.

## Configuration

Install canonical JSON for `service.QualificationAuthorityServerConfig` in a
root-owned, single-link, mode-0600 file. Configuration and credential paths must
be absolute, clean, distinct and protected against replacement by other users.
Use the Go structs to marshal configuration and policy without a trailing newline.
Their definitions live in `internal/service/qualification_authority_server_unix.go`
and `qualification_authority_unix.go` within this module.

| Field | Meaning |
| --- | --- |
| `schema_version` | `1` |
| `authority.schema_version` | `1` |
| `authority.source` | The authority identity pinned by the broker client |
| `authority.policy` | Protected current policy file |
| `authority.max_evidence_age_ns` | One minute through 24 hours |
| `authority.decision_ttl_ns` | One second through one minute |
| `authority.timeout_ns` | One through ten seconds per decision |
| `authority.readers` | One through 32 concurrent evidence reads |
| `listen` | Explicit IP address and nonzero port, for example `127.0.0.1:9444` |
| `certificate`, `tls_key`, `client_cas` | Protected `hostfs.Pin` values for server certificate, PKCS8 private key and client CA bundle |
| `limits` | Existing bounded `sandboxbroker.ServerLimits`; request timeout must equal the authority timeout |

Each pin contains `path`, lowercase SHA256 in `sha256`, exact length in `bytes`
and numeric `mode: 384` (0600). TLS keys and CA bundles use the same strict PEM
loader as the broker client. No listener opens until configuration, policy and
credentials pass validation. Changing configuration or credential bytes closes
admission until a restart. HTTP/2, session resumption and unauthenticated requests
are unavailable. The authority uses the broker's bounded connection lifecycle.

## Current policy and revocation

`QualificationAuthorityPolicy` contains `schema_version: 1`, `source`, an RFC3339
`valid_until`, and up to 64 `grants`. An empty grant list denies every request.
Each grant contains:

- `client_sha256`: SHA256 of the authenticated client's leaf certificate DER.
- `host`: the exact machine digest, boot ID, kernel release/version and architecture.
- `deployment`: the exact broker identity, profile digest and runtime inventory digest.
- `producer_key`: the approved Ed25519 evidence producer public key, encoded as base64.
- `record`: the protected pin of one retained signed record.

Duplicate grants for the same client, host and deployment are rejected. Install
policy updates with an atomic file replacement and retain mode 0600 and protected
ownership. Removing a grant or producer key revokes access. Every decision reads
policy again after evidence verification; a change during that operation denies
the request. Nothing falls back to an earlier successful decision.

The policy's record pin is an explicit approval of those exact signed bytes.
Configure the producer independently of the broker and sandboxed candidate. A
compromised approved producer or policy owner can lie about observations; TLS and
signatures cannot establish the honesty of a trusted operator.

## Retained probe records

`QualificationRecord` contains schema version 1, kind `live-systemd-runsc-v1`,
source, reference, exact host, `DeploymentQualification`, `observed_at`, the ordered
`probes` list, and `signature`. The observation time is when collection finished.
The capability expiry must be finite and current. Fixture qualification IDs and
fixture evidence kinds are rejected.

The signature is Ed25519 over the UTF-8 domain prefix
`cortex-sandbox-qualification-record-v1\n` followed by canonical JSON of the record
with `Signature` set to nil. The prefix ends with an actual newline. The complete
signed record is then hashed for the policy pin. Producer tooling must perform
these steps only after the approved live probes finish for the named deployment.

Each probe contains `name`, `package`, `test` and an `output` file pin. Include all
13 groups from the [broker protocol](../cortex-sandboxd/README.md), exactly once
and in protocol order. `test` must name a live test beginning with `TestLive`.
Retain `go test -json` output for that package and test, including its subtests.
Run one selected top-level test per retained output so unrelated tests cannot
supply a passing package result. The producer signs the association between each
required group and the approved test that exercises it.

Every output must be a protected mode-0600 file of at most 256 KiB. The authority
verifies its hash and parses the event stream. It requires package start, the
named test's run and pass, successful completion of every started subtest, and
package pass. Every event needs a timestamp within the evidence window and no
later than the signed collection time. The oldest event bounds decision expiry.
Failures, skips, duplicate JSON fields, truncated output, unrelated
tests and text that merely prints `PASS` deny qualification. Oversized logs must
be addressed in probe output design, not silently truncated.

Keep the signed record and output files available for the full decision window.
The authority checks their bytes twice around the current-policy check and
returns only a reference and digest, never private logs. Deletion, replacement,
unsafe links or changed permissions deny subsequent requests. Restart reads the
same retained bytes; it never reruns probes.

The response echoes the client's fresh nonce and binds the exact host and
deployment. Expiry is the earliest of the decision TTL, policy expiry, evidence
freshness limit of the oldest probe event and signed capability expiry. The broker independently validates
that response and rechecks its local host and configuration.

## Local checks and deployment boundary

The local suite exercises a real TLS client/authority exchange using synthetic
signed records, restart, revocation, changed trust configuration, protocol bounds,
failed and incomplete probe output, signature tampering and retained-file changes.
It also checks the shared TLS listener's cancellation and joined shutdown.

```sh
GOWORK=off go test -race -count=1 -timeout=10m ./...
```

Linux qualification remains deferred. Provision production keys and exact grants,
run the selected deployment's live probes, retain and approve their signed output,
then test broker admission, revocation and recovery against this installed service.
The existing `TestLiveIsolationQualification` covers only part of the required
matrix; its success alone must never populate all 13 groups.
