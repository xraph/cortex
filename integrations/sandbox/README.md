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

## Worker deployment admission

The internal launch profile renders one installed worker template. You supply
approved worker and configuration hashes, exact sizes and private state paths.
The worker executable must have mode `0755`, its configuration `0600`, the
template `0644` and the state directory `0700`. Verification rejects links,
extra hard links, changed bytes, unsafe owners and writable ancestors. Reuse the
approved pins from your deployment process; do not calculate expected hashes
from the installation being inspected.

The template delegates CPU, memory and PID controls to the worker subgroup and
sets whole-service memory, swap, task, CPU and wall-time ceilings. It disables
automatic restart, kills the whole service on stop, suppresses stdout/stderr
logging and loads the job claim through a fixed credential name. Paths cannot
inject unit directives or substitutions. Executable, configuration and template
files must remain outside the worker's writable state directory.

`CheckLaunch` verifies those bytes, loads the job instance without starting it,
and checks the effective manager properties. Overrides, alternate roots or
mounts, additional commands or credentials, stale configuration, pending jobs,
aliases and changed resource settings fail admission. This matters because
[systemd can combine unit files with overrides](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html).
Missing required properties fail closed, including worker subgroup delegation.

This preflight feeds the internal launcher described below. The profile's
wall-time value is only an outer service ceiling. Template installation, the
worker executable/configuration contract, daemon/broker wiring and live manager
tests remain pending. Preflight alone installs nothing and starts no service.

## One-time launch handoff

The internal launcher rechecks current host authorization, enrolls the exact
signed receipt and checks the installed deployment before consuming a durable
claim. Only that claim's winner can prepare credentials and issue `StartUnit`.
Concurrent requests and retries acknowledge existing claimed or completed work
without another start attempt. Reaping work remains pending.

Credentials bind the receipt, claim revision, token and launch-profile digest.
Publication creates a new private job directory, synchronizes a `0400` file and
its parent directories, and refuses to overwrite an existing identity. The
credential is capped at one MiB. Keep it outside guest mounts and logs. Failed
or interrupted publication is retained for fenced cleanup; it is never used to
reconstruct a lost claim or retry execution.

Before dispatch, the launcher repeats deployment and grant checks, verifies the
current journal token/revision and enforces both acceptance-deadline and runtime
qualification expiry. The start request cannot replace a queued stop. A lost
acknowledgement, cancellation or other failure after claiming triggers bounded
recovery with an independent context. Failed recovery retains `reaping` state.

The worker credential reader requires the exact service subgroup, private file
metadata, canonical JSON, the broker signature and matching job/profile identity.
A copied credential cannot establish current authority by itself: use the
journal's `VerifyClaim` through authenticated daemon control and recheck grants
before guest execution. Cancellation, completion, reaping and expiration
invalidate that current-ownership check.

These launch and handoff components are implemented and fixture-tested. The
worker CLI, daemon startup wiring, cleanup of
retained credential files, full deadline supervision and broker activation are
still pending. No live systemd/runsc launch is qualified by these tests.

## Recovery scans

Use the internal `service.RecoveryScanner` with a separate host authorizer that
checks current grants for the receipt's signed scope and principal. It runs
without the original client connection. Authorization outages stop the job and
remain visible as scan errors.

Keep admission closed while `Recover` visits every pending journal page. It
cancels unclaimed acceptances and fences old claims, preserving any cancellation
already recorded. A cleanup failure does not prevent later pages from running.
Open admission only after a complete pass returns no error.

During operation, call `Sweep` with its returned `Next` cursor until `Done`, then
begin another pass with an empty cursor. Each page contains at most 256 entries.
Continue across individual failures so later jobs get checked; the next pass
retries unresolved entries. `Done` reports the end of the index, not success.
Check the returned error too.

Sweeps recheck original deadlines, qualification expiry and current grants, then
cancel and reap work that no longer qualifies. The scanner reloads each full
record before acting. A known service identity still gets a stop attempt when
its record cannot be read or cancellation cannot be saved, but those failures
cannot produce a terminal result.

`service.Controller` joins a launcher and scanner that share the same journal
and reaper. Run it for the daemon's lifetime and submit exclusively through its
`Launch` method. Admission stays closed until startup recovery succeeds. The
controller then schedules pages every 10ms to one second, as configured. A scan
error closes admission and stops that controller; resolve the error before
constructing another instance. `Status` exposes readiness and the retained error
to the daemon.

Cancelling `Run` closes admission and cancels in-flight launch and worker
authorization contexts. The controller joins those calls before using an
independent 30-second context to recover remaining claims. Cleanup errors stay
visible, and unresolved journal
entries remain available to the next controller. Host callbacks must honor
their contexts. A stopped controller cannot run again.

The executable daemon still needs to construct and run these components. A slow
cleanup can delay a later page, so scans cannot enforce an exact execution
deadline. Worker deadline supervision, the outer service ceiling, discovery of
services absent from the journal and cleanup of retained files remain required.

## Worker identity and authorization

The internal `workerpeer.Open` captures a connected Unix stream peer with
`SO_PEERPIDFD`. It also requires root socket credentials, current root process
credentials, matching procfs PID numbering and the exact job's `supervisor`
subgroup. The handle is checked for exit around the bounded procfs reads.
Close it when the connection ends. A numeric PID supplied in a request cannot
replace that handle, and unsupported hosts are refused.

This requires Linux's peer-pidfd socket option. The
[kernel implementation](https://github.com/torvalds/linux/blob/v6.6/net/core/sock.c)
obtains the handle from the socket's recorded peer; the
[pidfd documentation](https://man7.org/linux/man-pages/man2/pidfd_open.2.html)
describes observing process exit through that handle. The adapter does not use
`pidfd_open` on a numeric peer PID as a fallback.

Pass the live peer and private `WorkerClaim` to `Controller.AuthorizeWorker`.
The controller checks its readiness, the profile digest, current journal token
and revision, current grants and the effective running service. The service's
main PID and execution record must match the peer, and its pinned files and
resource settings must match the approved profile. Grant callbacks receive the
verified job scope. The controller checks peer liveness after the last grant
lookup and checks the journal again before returning a detached record.

Authorization creates no claim, starts no process and commits no result. Repeat
it after staging, immediately before guest execution. It never extends the
original deadline. Keep claim tokens out of URLs, logs and guest mounts.

The `sandbox_peer_kernel` test tag runs real Unix-socket and child-process
checks on Linux as root. CI compiles a race-enabled test binary and runs those
checks. They cover process capture, exit detection and rejection of a root
process outside the job service, including descriptor cleanup. They do not
start systemd jobs or qualify the running-service acceptance path. Worker
startup and result-finalization wiring remain pending.

## Private worker control

Use the internal `service.WorkerControl` to connect workers to the controller's
authorization checks. After controller recovery, pass it a Unix listener in a
root-owned `0700` directory with a root-owned `0600` socket. It takes ownership of
the listener, closes it on shutdown and leaves the socket path for the host's
fenced cleanup. Keep that directory stable and outside guest mounts. Socket
installation, restart cleanup and executable daemon wiring remain pending.

Set `ControlLimits` explicitly: one to 64 accepted connections, with a total
exchange timeout between 10ms and 30 seconds. Each connection carries one
length-prefixed canonical JSON claim, bounded to 2 KiB, followed by a write-side
close. Unknown or duplicate fields, trailing bytes, oversized frames and stalled
reads are refused. Grant callbacks must honor their contexts so shutdown can
join them.

The server opens a kernel peer handle and calls `Controller.AuthorizeWorker`.
It returns only a denial or an authorization bound to the exact request digest.
Internal errors and tokens are never copied into replies. This endpoint cannot
launch, cancel or complete a job, and it does not retry an exchange.

The worker calls `service.AuthorizeAt` with its verified credential's claim and
the configured private socket path. The client checks the endpoint and the
daemon's root credentials before sending the token, then checks the bounded
reply, its request binding and the connection's end. Repeat authorization after
staging, immediately before execution; success does not extend the signed
deadline or replace independent cleanup proof.

Tests cover concurrency ceilings, timeouts, malformed requests and replies,
current claim cancellation, unsafe socket paths and shutdown. The tagged Linux
tests also exercise daemon socket authentication and the public server's refusal
of an unrelated root process. These checks do not qualify an installed worker,
systemd service or runsc deployment.

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
