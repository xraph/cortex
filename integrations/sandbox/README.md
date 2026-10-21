# Cortex sandbox broker

You can embed this broker behind an authenticated HTTPS server. It accepts sealed
`sandbox.Job` requests, writes a signed receipt to SQLite before dispatch and
keeps job identity across retries and restarts. The root `sandbox` package carries
only the contracts. This module owns HTTP, durable state and export handling.

The production runtime adapter and daemon are still pending. An internal runsc
worker driver and lifecycle supervisor are implemented, but no qualified worker
ships here. Unit tests use explicit fixture drivers and cannot establish live
isolation or resource enforcement. The Linux worker will be selected separately,
as agreed on October 18, 2026.

## Host integration

1. Create a private directory with mode `0700` for the SQLite journal. Keep both
   the journal and runtime state across restarts. One broker process owns it.
2. Provide `Config.Runtime`, a host image allowlist, an Ed25519 signing key and
   `Config.Authorize`. The authorizer rechecks current grants on every access.
   Only the runtime adapter can attest that a deployed worker passed its probes.
3. Mount `broker.Handler(authenticate)` on an HTTPS server. Map verified client
   certificates or host credentials to an exact `Identity`. Do not copy scope or
   principal from request bodies into authentication context.
4. Give clients the broker's public key through trusted configuration. Create
   `NewClient` with your authenticated HTTP transport. Redirects are rejected.
5. Call `Reconcile` at startup and on a bounded timer, using the host clock. It
   completes pending cancellations and cancels expired recorded jobs. It never
   starts new work. Keep calling it when an individual runtime call fails.

Set server read/header/write/idle timeouts in the embedding host. The client has
its own timeout, and HTTP bodies have fixed size limits. The default admission
limits are 16 active jobs and 10,000 retained records. You can lower them through
`Config`; a full journal denies new work until the host manages retention. There
is no automatic deletion of job identity history.

The caller must retry the same sealed job after an uncertain submission. A new
ID changes the digest and conflicts with an existing request key. `Inspect`
reconciles execution status; `Collect` returns a terminal result. Revoked callers
cannot inspect, cancel or collect. The trusted supervisor can still reap their
expired workloads without exposing output.

## Runtime obligations

`Runtime.Start` must preserve durable idempotency, including terminal tombstones.
It must never rerun a completed ID when a previous response was lost. `Cancel`
must stop every descendant and retain terminal status. A broker receipt is not
permission to fall back to host execution when the runtime is unavailable.

Schema one accepts a digest-pinned image, argv, read-only evidence inputs under
`/inputs`, a small locale environment allowlist and finite CPU, memory, process,
scratch, wall-time and output limits. It has no host-mount, privileged-mode,
capability or credential fields. Network access is denied. Output is limited to
one MiB, and an executable's stdout cannot supply a trusted grading verdict.

The intended profile uses gVisor's `runsc` with external networking disabled.
The [gVisor networking guide](https://gvisor.dev/docs/user_guide/networking/)
describes `--network=none`; the
[OCI guide](https://gvisor.dev/docs/user_guide/quick_start/oci/) describes runtime
bundles. The `oci.Spec` builder encodes namespaces, read-only mounts, empty
capabilities, non-root identity, cgroups and scratch limits using OCI runtime-spec
v1.3.0. The adapter must enforce that configuration. Neither the configuration
checks nor the broker tests qualify a running worker.

After execution has stopped, `ReadExport` can read bounded regular files through
no-follow descriptors on Linux and macOS. It rejects traversal, symlinks, hard
links and special files. It does not expand archives. The host still needs to
verify content digests, authorize destinations and approve export separately.

## Validation

The `deployment` package verifies a dedicated runsc installation against a
trusted `Manifest`. Pin every executable, including the sidecars under
`gvisor-bin/`, by path, SHA256, byte count and mode (`493` for `0755`, or `365`
for `0555`). Include the containerd shim if you install it. The canonical
inventory digest also binds the architecture (`amd64` or `arm64`). It does not
depend on the order of entries.

You supply that inventory from an approved release. Computing the expected
hashes from an unknown installation would only approve whatever is already
there. Current releases use sidecars, as described in the
[gVisor installation guide](https://gvisor.dev/docs/user_guide/install/).

`deployment.Verify` checks ownership, permissions and content through no-follow
file descriptors. Missing or extra files, unexpected directories, links and
special files fail the check. The installation must contain only the pinned
executables and their directories. Keep manifests and runtime state elsewhere.
Use a canonical absolute installation path with no symlinked ancestors. The
root-run worker supplies owner UID zero; tests may use their own UID.

The trusted owner must keep those files unchanged while the worker uses them.
Recheck before dispatch and stop admission during upgrades. This check does not
freeze files against their owner, execute the runtime or issue a qualification.
The production adapter must bind this complete inventory digest to its worker
attestation. Its implementation remains pending.

```sh
GOWORK=off go test -race -count=1 ./...
GOWORK=off go build ./...
```

## Worker supervision

The internal supervisor bounds stdout and stderr together, applies the accepted
receipt's deadline and the job's wall limit, and cleans up after preparation,
startup, wait errors, cancellation and normal exit. Cleanup has its own bounded
context, so cancelling the caller does not cancel the reap attempt. A cleanup
failure returns no collectible terminal result.

The runsc driver checks the complete deployment pin, private host directories
and job binding. Before creating the sandbox, it compares `config.json` against
the deny-all OCI specification rebuilt from the sealed job and staged paths.
It passes control arguments directly to the approved executable with a minimal
environment. Guest output uses separate pipes from the bounded JSON wait
response, whose job identity and exit status are checked.

Forced deletion does not prove cleanup after a lost creation acknowledgement:
runsc also succeeds when its state record is absent. That case remains unresolved
until the external reaper proves the worker's containment has stopped. No
terminal result is returned merely because forced deletion succeeded.

This code is not wired into broker admission. You still need durable job claims,
verified image/input staging, external worker supervision and a reaper that can
stop the whole workload if either the broker or worker dies. The internal driver
does not provide those guarantees, and the runtime remains unqualified.

Required live probes remain open: descendant cancellation, resource exhaustion,
DNS and redirect denial, metadata/private-host denial, restart during execution,
image/runtime pin validation and protected grading. `compatibility.json` records
that state without claiming a tested runtime version. Run these probes on the
chosen Linux worker before issuing a qualification attestation or enabling the
adapter in an installed host.

## Linux probe suite

The `sandbox_qualification` build tag adds host-side probes. They fail when the
worker, cgroup v2, root access or pinned binaries are missing. There are no skips.
Run them only on the dedicated Linux worker selected for qualification.

Build the trusted fixture from this checkout, then supply the approved runsc
binary and its release checksum. Use a root shell on that worker:

```sh
CORTEX_PROBE_BUILD_DIR="$(mktemp -d)"
CGO_ENABLED=0 GOWORK=off go build -trimpath \
  -o "$CORTEX_PROBE_BUILD_DIR/probe" ./testdata/isolation-probe
export CORTEX_QUALIFICATION_RUNSC=/opt/cortex/runsc-release/runsc
export CORTEX_QUALIFICATION_RUNSC_SHA256="approved-runtime-sha256"
export CORTEX_QUALIFICATION_RUNTIME_MANIFEST=/etc/cortex/runsc-release.json
export CORTEX_QUALIFICATION_PROBE="$CORTEX_PROBE_BUILD_DIR/probe"
export CORTEX_QUALIFICATION_PROBE_SHA256="$(sha256sum "$CORTEX_QUALIFICATION_PROBE" | cut -d ' ' -f 1)"
GOWORK=off go test -v -tags=sandbox_qualification \
  -run '^TestLiveIsolationQualification$' -count=1 -timeout=5m .
```

The suite creates disposable OCI bundles containing that fixture. It checks
read-only root/input mounts, denied host-file access, scratch exhaustion, denied
TCP/DNS access to local private-address fixtures, effective cgroup limits,
descendant termination and reopening runtime state. Supervisor probes cover
normal exit, combined output overflow and a deadline with live descendants;
they require the job cgroup to be empty or removed after teardown.
The redirect fixture must
receive no connection at all. It does not contact public services or a real
cloud metadata endpoint.

The probe gate requires the trusted JSON deployment manifest before invoking
runsc, and verifies its whole directory with owner UID zero. A single binary
checksum is insufficient when sidecars are installed. The manifest has
`schema_version: 1`, `architecture` and `files`; each file has `path`, `sha256`,
`bytes` and numeric Unix `mode`. No manifest or runtime release is selected for
you by this repository.

Record the runtime version, deployment digest, checksums, host kernel,
architecture and Go version
with the test log. The fixture's binary digest is a test identity, not a
qualification of a production OCI image. CI compiles these probes and their
fixture; it does not execute them or turn `compatibility.json` into a qualified
manifest. Broker/worker crash recovery, external wall/output supervision, memory/CPU exhaustion,
guest process exhaustion, deployed image pins and protected grading still need
live acceptance checks.
