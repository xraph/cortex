# Sandbox broker command

Build from the sandbox module and install the binary on your chosen Linux worker:

```sh
GOWORK=off go build -trimpath -o cortex-sandboxd ./cmd/cortex-sandboxd
cortex-sandboxd --config /etc/cortex/broker.json --qualification-config /etc/cortex/qualification.json
```

Run it as root. The command accepts exactly those four arguments in that order.
Both paths must be absolute, clean and distinct. SIGINT and SIGTERM cancel the
owned daemon, which joins its workers and recovery services before releasing
storage. A failed or cancelled run exits with status 1 and a fixed diagnostic;
configuration paths, credential contents and authority responses stay out of
process output. Other operating systems reject startup.

You need the installed worker, broker configuration, policies, credentials and
storage described in the [sandbox README](../../README.md). The command calls
`service.RunInstalledDaemon` and gives it an online qualification verifier. The
verifier opens no public listener. Recovery precedes public admission, and the
daemon still checks its installed deployment before and after each observation.

## Qualification authority

You must provide the authority service. This repository implements its client
protocol, not the service that collects, retains and verifies live probe evidence.
No authority is deployed by this command. Linux worker selection and live
systemd/runsc qualification remain deferred, so passing these client tests does
not qualify an installation or close S2.4.

Install `service.QualificationClientConfig` as canonical JSON in a root-owned,
single-link mode-0600 file no larger than one MiB. Marshal the Go type to preserve
field order and exact JSON encoding. It contains these fields:

| Field | Value |
| --- | --- |
| `schema_version` | `1` |
| `endpoint` | Dedicated HTTPS endpoint ending exactly in `/v1/qualification`, without user information, query or fragment |
| `source` | Expected authority identity, at most 512 bytes |
| `server_spki` | SHA256 of the server leaf certificate's DER SubjectPublicKeyInfo, as lowercase hex |
| `certificate`, `tls_key`, `server_cas` | Distinct `hostfs.Pin` objects for the client certificate, PKCS8 client key and trusted CA PEM files |
| `timeout_ns` | One to ten seconds per complete observation |
| `max_evidence_age_ns` | One minute to 24 hours, set to your approved evidence freshness window |
| `readers` | One to 32 concurrent observations; excess requests fail immediately |

Each pin includes an absolute `path`, exact `sha256` and `bytes`, and numeric
`mode: 384` (0600). The credential files must be root-owned and single-link too.
Their maximum sizes are 192 KiB for the client certificate chain, 24 KiB for its
private key and 256 KiB for the CA bundle. The loader rejects duplicate CA
certificates, malformed PEM, unknown configuration fields and repeated fields.
Keep this authority client identity separate from worker credentials.

TLS 1.3 verifies the authority's hostname, CA chain and pinned public key. The
client presents its own certificate. Your authority must authenticate that client
and authorize it for the requested source, host and deployment before consulting
its evidence. Client-supplied identity fields are lookup constraints, never proof
that probes passed.

The transport ignores environment proxies, follows no redirects, disables
compression and uses a fresh TLS handshake for every observation. It does not
cache successful decisions or retry failed requests. Response headers are capped
at 8 KiB and bodies at 64 KiB. The request context covers trust checks, host
observation and HTTP I/O. A stalled local filesystem call still depends on the
kernel returning, as with the other protected file loaders.

## Wire protocol

Send one canonical JSON `QualificationRequest` with `POST /v1/qualification` and
`Content-Type: application/json`. The fields, in order, are `schema_version`,
`source`, `nonce`, `host` and `deployment`. The nonce contains 32 random bytes
encoded as 64 lowercase hex characters. It changes on every call.

`host` contains `machine_id`, `boot_id`, `kernel_release`, `kernel_version` and
`architecture`. The first is the SHA256 of the exact `/etc/machine-id` bytes.
The client requires that file to be root-owned, single-link, mode 0644 or 0444, and to
contain a 32-character lowercase hex ID with optional surrounding whitespace.
`boot_id` comes from `/proc/sys/kernel/random/boot_id`; the kernel fields come
from `uname`. These values must describe the host running the client. The client
reads them again after receiving the decision and rejects a change.

`deployment` uses the existing `DeploymentIdentity` JSON names: `Broker`,
`ProfileDigest` and `RuntimeDigest`. The installed attestor supplies those values.
The profile digest covers the worker configuration, executable, service template
and outer limits. The runtime digest covers its complete pinned inventory.

Return HTTP 200 with `Content-Type: application/json` and canonical JSON matching
`QualificationResponse`. The field order is:

```text
schema_version, source, nonce, host, status, checked_at, valid_until,
qualification, evidence
```

The response must echo schema version 1, the configured source, nonce and exact
host. `status` must be `qualified`. Any other status, including `revoked`, denies
admission. Unknown fields, duplicate fields, trailing data and partial responses
also deny admission. Use the Go structs to produce canonical JSON without a
trailing newline.

`qualification` contains `Deployment` and `Capabilities`, with the existing Go
JSON names. `Deployment` must equal the request. `Capabilities` must validate
against the sandbox contract, use its exact runtime digest, and expire no later
than `valid_until`.

`evidence` contains these fields in order:

| Field | Authority obligation |
| --- | --- |
| `kind` | Exactly `live-systemd-runsc-v1`; fixture, simulated and partial evidence are ineligible |
| `reference` | Current retained evidence reference, at most 512 bytes |
| `sha256` | SHA256 of that retained record |
| `observed_at` | When the complete live evidence was collected for this deployment and host boot |
| `controls_digest` | Exact requested profile digest whose effective controls the authority verified |
| `checks` | Every passing probe group listed below, exactly once and in this order |

```text
artifact-export
broker-worker-restart
cpu-memory-process-limits
credential-isolation
deployed-image-pins
descendant-cleanup
effective-service-controls
host-file-denial
network-dns-redirect-denial
protected-grading
read-only-root-inputs
scratch-limit
wall-output-supervision
```

The authority must verify retained live evidence for all groups, including the
installed service path, deployed image pins, effective controls and protected
grading. It must check current revocation and source availability after receiving
the challenge, and set `checked_at` to that decision time. Echoing request fields
or listing these names without examining evidence violates the authority
contract. The client authenticates the decision and checks its bindings; it does
not fetch the retained logs or independently prove that the authority is honest.

A decision can differ from local time by at most five seconds for `checked_at`.
That tolerance never extends expiry. Evidence cannot be future-dated or older
than `max_evidence_age_ns`, and `valid_until` cannot extend past `observed_at` plus
that window. The client checks capability expiry again after its local checks.
Keep the authority and broker clocks synchronized.

Every observation rereads the protected authority configuration and verifies all
credential pins before and after the request. Changing trust settings or files
closes admission until you restart with the approved configuration. Existing
qualification callbacks still run for launches, admission and recovery, so a
revoked or unavailable authority cannot be replaced by an earlier successful
response. The broker's normal recovery and shutdown rules still apply.

## Local validation

The tests use an authenticated local TLS authority with explicit synthetic
responses. They cover protocol bindings, revoked decisions, fixture and partial
evidence, stale observations, changed protected files, TLS pin mismatch,
argument validation, cancellation, timeouts and concurrent request bounds.
They exercise the client contract. Run live acceptance separately on the chosen
Linux installation before your authority issues usable qualification.
