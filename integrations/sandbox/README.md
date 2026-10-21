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

`Runtime.Start` receives the signed original receipt. Verify it with the pinned
broker public key and retain its acceptance time, runtime binding and job digest.
The execution deadline is `AcceptedAt + Job.Limits.WallTime`. Expired accepted
retries enter cancellation before dispatch. The optional `Config.Now` is a trusted
host clock, and defaults to `time.Now`.

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

Build deployed brokers and workers with a patched Go toolchain. The module's
language minimum does not establish a secure compiler or standard library.

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

The contained driver binds the worker and payload to one job service. Its fixed
parent is `/system.slice/cortex-sandbox-worker@<job-id>.service`, with the worker
in `supervisor` and the OCI payload in `payload`. The service adapter must select
`system.slice` explicitly, delegate the required controllers and install the
worker subgroup. The driver checks actual membership before each runsc command
and rejects an OCI bundle targeting the older, separate hierarchy.

The Linux reaper helper writes `cgroup.kill` and waits up to ten seconds for the
whole hierarchy to become empty. It verifies procfs/cgroup-v2 and root ownership,
refuses linked control files and cannot target its own service. An absent group
is reported separately from an empty group. Neither observation fences a queued
service launch, so neither automatically authorizes `Reaped`. The external
adapter must durably prevent later starts before recording cleanup. Unsupported
hosts and failed or cancelled observations return errors without cleanup proof.

This code is not wired into broker admission. You still need claim and staging
integration, external worker supervision and a reaper that can
stop the whole workload if either the broker or worker dies. The internal driver
does not provide those guarantees, and the runtime remains unqualified.

## Durable worker ownership

The internal worker journal enrolls the exact signed receipt before issuing a
single worker claim. Its database is bound to the broker name and public key.
A retry cannot change the receipt, recover a lost claim token or claim a completed
job again. Terminal records count toward the configured capacity and are never
automatically evicted. Keep this journal across restarts.

Claimed jobs have no automatic lease takeover. Cancellation and recovery fence
late worker results and move unresolved execution to reaping. You must stop the
external worker and every sandbox descendant before recording `Reaped` at the
expected revision. Missing runtime state is insufficient. The terminal state is
failed or cancelled, with no invented successful output. Normal completion also
requires the caller to prove cleanup before persisting the result.

One daemon owns the journal's exclusive SQLite connection. Workers must use that
daemon's authenticated control path; they cannot open the database concurrently.
The broker and worker share a private-file opener that rejects file symlinks,
hard links, special files, unsafe permissions and another application's journal.
The host owner must prevent directory and file substitution while it is open.

The journal does not launch processes or enforce current user grants. The
subprocess test establishes claim durability after process exit; it does not
establish that an orphan sandbox was killed.

## Service recovery

The internal service reaper connects directly to `/run/systemd/private` on Linux
and checks that the Unix peer is root PID 1 before authenticating. It does not
use an environment-selected bus, a shell or a fallback process killer. Connection
setup is bounded to five seconds; each recovery operation is capped at thirty.

Recovery permanently masks the exact job instance, reloads systemd and replaces
queued starts with a stop job. It drains the cgroup before waiting for service
quiescence, then drains again to catch descendants created by stop helpers.
Success requires a persistent mask, a masked nontransient loaded unit or explicit
unit absence, no pending manager job, and empty or absent containment. The reaper
checks its own membership before asking systemd to stop anything.

Use an installed worker template. Transient units, aliases, runtime-only masks,
stale loaded configuration and malformed state cannot supply recovery proof.
The mask and state checks use the documented
[systemd manager API](https://www.freedesktop.org/software/systemd/man/latest/org.freedesktop.systemd1.html).
The host must reserve this service namespace, retain masks and journal identities,
and prevent payload migration outside the fixed hierarchy. No operation here
unmasks a service or deletes a completed identity.

`Reap` moves claimed work to durable `reaping` before touching the service and
records its result only at that revision after cleanup succeeds. For cancellation,
call the journal's `Cancel` first. A concurrent cancellation, lost final database
write or failed service operation leaves unresolved work available for retry.
Tests cover these paths with the real SQLite journal and a simulated manager.

The service reaper is implemented, but live systemd/runsc qualification is still
pending. Template installation and pinning, launch admission, daemon control,
resource ceilings, periodic orphan discovery and broker wiring remain open.
The current low-level runsc qualification probes do not qualify this service path.

## Evidence input staging

The internal staging service copies evidence into private directories keyed by
exact scope and job ID. You provide the authenticated context, a job authorizer,
an evidence policy and a source compatible with `evidence.Repository.Open`.
The job authorizer must bind the caller's host identity to the sealed principal.
Scope and initial grant checks must pass before any files are created.
Input paths cannot overlap: `/inputs/a` and `/inputs/a/b` cannot both
name regular-file mounts.

Set per-input and total byte limits explicitly. Their upper bounds are one GiB
and four GiB respectively. Copies use bounded buffers and must match the exact
declared length and SHA256. Cancellation closes the source reader; the provider
must honor context and unblock reads when closed. Failed copies remove only the
files created by that attempt.

The completed record is published after file synchronization and another grant
check. Returned files have mode `0444` inside a `0700` directory so the worker can
bind them read-only for the guest. The trusted host owner must keep them unchanged
while they are used. Retries check the stored bytes, current grants, retention
and the authoritative source again. A deleted source cannot be replaced by an
older local copy. Extra files, unsafe permissions, links and conflicting job
digests fail the check.

An interrupted directory without a completed record returns
`ErrSandboxInputsIncomplete`. It is preserved for the authorized reaper rather
than overwritten. This code does not claim jobs, resolve images, grant execution
or replace the authorization check immediately before dispatch. Its production
worker wiring remains pending.

Required live probes remain open: descendant cancellation, resource exhaustion,
DNS and redirect denial, metadata/private-host denial, restart during execution,
image/runtime pin validation and protected grading. `compatibility.json` records
that state without claiming a tested runtime version. Run these probes on the
chosen Linux worker before issuing a qualification attestation or enabling the
adapter in an installed host.

## Image staging

The internal image service builds a fresh root filesystem from an approved OCI
manifest digest. You supply a content source and an authorizer that binds current
image access to the authenticated caller. The source receives digests only.
Descriptor URLs, embedded data, image indexes and foreign layers are rejected.

The supported profile is Linux on the worker's `amd64` or `arm64` architecture,
without additional platform variants or features. Layers may use tar or gzip.
The service verifies the manifest and config, each compressed layer and every
uncompressed DiffID before extraction. It rejects duplicate metadata fields,
ambiguous paths, duplicate archive entries, device nodes, FIFOs, extended
attributes and setuid/setgid permissions. Containerd's pinned OCI extractor
handles hard links, guest symlinks and whiteouts within the staged rootfs.

Set aggregate compressed and expanded byte limits, an entry budget and a
timeout. Compressed bytes include metadata; expanded bytes include tar headers
and padding. The entry budget charges each header and its path/link components,
including repeated parents, to bound implicit directory creation. Maximums are
4 GiB compressed, 8 GiB expanded, 100,000 entry/component units, 128 layers and
ten minutes. Provision space for both the extracted tree and temporary layer
copies, plus filesystem metadata. The external worker still needs a disk quota.

You retain ownership of the private staging directory. Extraction keeps the
worker's UID/GID, and image process defaults never change the sealed job's
command, environment, identity or mounts. Do not follow guest symlinks from the
host. Keep the tree unchanged until the workload is destroyed.

Failed attempts remove their own temporary directory and report cleanup errors.
Retries build a fresh copy and recheck authorization. This is not a durable image
cache: a crashed worker may leave an orphan for the external reaper. Broker
dispatch, durable claims and live qualification remain open.

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
