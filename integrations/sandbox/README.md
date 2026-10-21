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
failed or cancelled, with no invented successful output.

Normal completion uses `Propose` followed by `Finalized`. A proposal binds the
report digest and receipt time to the current claim revision, removes execution
authority and enters `finalizing`. It has no collectible result. The external
finalizer must stop the whole service and recheck current grants before committing
at that exact revision. Cancellation discards the proposal and advances the
revision, so an older finalizer cannot overwrite it. Identical report retries
return existing state; changed reports are refused.

The journal upgrades its format marker to version 3 in a transaction after
checking the broker key and capture-table constraints. Ownership records retain
format 2; version 1 ownership records move to format 2 on their next transition.
Existing claims and terminal identities are retained. Migration grants no new
execution. Older binaries refuse the upgraded journal rather than ignore pending
artifact captures.

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
wall-time value is only an outer service ceiling. Template installation,
worker execution assembly, daemon/broker wiring and live manager
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
already recorded. Pending reports require whole-service cleanup and current
authorization before finalization. A cleanup failure does not prevent later pages from running.
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
visible, and unresolved journal entries remain available to the next controller.
Host callbacks must honor
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
bootstrap is described below; staging and runtime integration remain pending.

`Controller.ProposeWorkerResult` applies the same live peer, claim, deployment
and grant checks before persisting a report. Its acknowledgement contains the
job, original claim revision and report digest. It cannot be used as a completed
result. The controller remains responsible for the call through the journal
write, so shutdown joins report submission before recovery.

Startup and periodic scans retain valid reports across failed cleanup or a lost
final write. Before committing, they independently fence and stop the service,
then recheck grants and the original acceptance and qualification deadlines.
Revocation or expiry discards the report and follows cancellation cleanup;
provider failures remain visible. A report does not extend either deadline.
Failed cleanup leaves the report pending for another recovery attempt.

## Private worker control

Use the internal `service.WorkerControl` to connect workers to the controller's
authorization and report receipt methods. After controller recovery, pass it a
Unix listener in a root-owned `0700` directory with a root-owned `0600` socket.
It takes ownership of the listener, closes it on shutdown and leaves the socket
path for the host's fenced cleanup. Keep that directory stable and outside guest mounts. Socket
installation, restart cleanup and executable daemon wiring remain pending.

Set `ControlLimits` explicitly: one to 64 accepted connections, with a total
exchange timeout between 10ms and 30 seconds. An authorization connection carries
one length-prefixed canonical JSON claim, bounded to 2 KiB, followed by a write-side
close. Unknown or duplicate fields, trailing bytes, oversized frames and stalled
reads are refused. Grant callbacks must honor their contexts so shutdown can
join them.

For authorization, the server opens a kernel peer handle and calls
`Controller.AuthorizeWorker`.
It returns a denial, authorization or activation-pending response. Authorization
and pending responses bind the exact request digest. Pending requires the
approved live worker and current claim/grants, with a validated outstanding
service job. Provider availability failures do not permit activation retries.
Internal errors and tokens are never copied into replies. This endpoint cannot
launch, cancel or complete a job, and it does not retry an exchange.

The worker calls `service.AuthorizeAt` with its verified credential's claim and
the configured private socket path. The client checks the endpoint and the
daemon's root credentials before sending the token, then checks the bounded
reply, its request binding and the connection's end. Repeat authorization after
staging, immediately before execution; success does not extend the signed
deadline or replace independent cleanup proof.

Enable report submission with `ControlLimits.Reports`: one to four concurrent
uploads, strictly fewer than `Connections`. Zero keeps report submission
disabled. An upload cannot consume the reserved authorization capacity. When
report capacity is full, the server returns a request-bound pending response
before acquiring another peer handle or receiving the report body.

Call `service.ReportAt` with the same claim and the bounded execution result.
Its version 2 header declares the report's exact encoded length and digest in
at most 2 KiB. The server authenticates the live worker and current authority
before acknowledging readiness. It holds that process handle while receiving
the canonical report, capped at 2 MiB, and shortens the exchange to the original
signed deadline. It then calls `Controller.ProposeWorkerResult`, which rechecks
authority before persistence. Version 1 authorization clients remain compatible.

The final acknowledgement binds the job, claim revision and report digest. It
confirms receipt only; the scanner still must prove cleanup before collection.
`ErrPending` permits a report-only retry with the same claim and original
deadline. Other lost or uncertain acknowledgements require broker inspection.
Never rerun the job to recover a reply. The client performs no automatic retry.

Tests cover concurrency ceilings, timeouts, malformed requests and replies,
current claim cancellation, unsafe socket paths and shutdown. Report tests cover
upload limits, readiness and receipt tampering, revocation during upload, a lost
acknowledgement, maximum output and peer-handle cleanup. The tagged Linux
tests also exercise daemon socket authentication and the public server's refusal
of an unrelated root process. These checks do not qualify an installed worker,
systemd service or runsc deployment.

## Worker configuration and bootstrap

Install `service.WorkerConfig` as canonical JSON from `json.Marshal`, without
indentation or a trailing newline, in a protected `0600` file capped at one MiB.
You supply the broker name and public key, private control socket,
`WorkerService` settings and complete `WorkerRuntime` manifest. Keep approved
runtime and worker pins in your deployment inputs. The loader does not infer
them from installed binaries or grant qualification from matching hashes.

The configuration contains no hash of itself. To prepare the launch profile,
serialize the settings once and use the exact byte count and SHA-256 as its
`Config` pin. Install the corresponding template, then call
`service.ReadWorkerConfig` to verify the files and obtain the complete profile
and digest for the launcher. Any change to the settings requires a new matching
profile and credential. The host must keep these files stable during use.

Call `service.BootstrapWorker` from the worker with the configured path,
systemd's credential directory and the job ID. It requires root on Linux in the
exact worker subgroup, checks the protected configuration and installed files,
verifies the signed credential, matches its runtime pin and asks the private
daemon for current authority. Bootstrap is bounded to 30 seconds. An explicit,
bound activation-pending response permits retries every 50ms for at most ten
seconds, also bounded by the earlier signed execution or qualification deadline.
Denial, connection loss, provider failure and cancellation stop startup.

The returned `WorkerSession` keeps the launch token private. `Job` and
`Configuration` return detached snapshots. Carry `Deadline` through staging and
supervision, then call `Authorize` immediately before execution. That call checks
the installed files, current subgroup and broker authority again, with no
activation retry. No call extends the original deadline.

Use `WorkerSession.Report` for the bounded execution result. It binds the result
to the session's job and checks the returned receipt. After the first send, only
an identical report may be retried following an explicit capacity refusal.
Further execution authorization is refused, including while a report is waiting
for upload capacity. A successful or uncertain report cannot be sent again from
that session; inspect the broker to resolve a lost acknowledgement.

This is an internal startup component. Worker CLI assembly, approved image
delivery, production broker/export wiring, daemon setup and retained-file cleanup
still need implementation. Session evidence adapters now connect input staging
and output capture to the private daemon. Tests cover configuration and credential
substitution, activation races, deadline/cancellation handling,
snapshot isolation and report retry rules. Linux root fixtures exercise the real
private-daemon client with an explicit service-membership seam and reject ambient
root through the public bootstrap function. They do not qualify an installed
systemd worker or runsc deployment.

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

## Bounded worker storage

The internal Linux workspace owns separate staging and scratch filesystems.
`WorkerConfig.workspace` pins staging bytes, staging and scratch inode ceilings,
and a worker memory reserve. Bootstrap refuses a signed job unless staging,
scratch, guest memory and that reserve fit the outer service memory limit.
These are ceilings. They do not guarantee that a workload will fit.

Workspace setup requires the exact worker subgroup, current authorization and
an existing root-owned `0700` parent directory. Each job gets a new directory;
retained directories are refused. The service template requires `PrivateMounts`.
Setup also checks the actual mount namespace against PID 1 before making its
mounts recursively private. It never changes a running Go thread's namespace.

Both filesystems require Linux's descriptor-based mount API, unique mount IDs
and tmpfs `noswap`. Missing kernel features fail setup. There is no disk-backed
fallback. Staging is private; scratch is writable by the guest with `nodev`,
`nosuid` and `noexec`. Scratch capacity rounds down to native pages so it cannot
exceed the signed limit. The [kernel tmpfs documentation](https://www.kernel.org/doc/html/v6.12/filesystems/tmpfs.html)
explains its byte, inode and swap controls.

`Check` repeats authority, containment, directory identity, mount identity,
capacity, flags and propagation checks within the original deadline. Cleanup
uses an independent context and normal unmounts. Busy mounts remain available
for cleanup retry, while replaced mounts or directories are left untouched.
Only the owned empty mountpoints and job directory are removed. A failed setup
can return an unusable workspace with unresolved cleanup; its caller must retry
cleanup or exit the worker for independent service recovery.

`NewWorkspaceRunsc` binds the runtime to that workspace's job, bundle, runtime
state, image directory and input directory. `WorkspaceSpec` mounts the retained
scratch filesystem at `/tmp` with private propagation and the same guest
restrictions. Runtime commands recheck storage identity; execution commands also
recheck current grants. Forced runtime cleanup uses `VerifyStorage`, which grants
no execution authority and does not depend on an unexpired execution deadline.
Keep the workspace open through runtime cleanup and any authorized export.

The `sandbox_mount_kernel` tests create a child mount namespace before the Go
runtime starts. They exercise actual byte and inode exhaustion, replacement
refusal, revocation, cancellation, busy cleanup and mount removal on process
exit. Run them as root only in a disposable Linux environment. These tests do
not qualify systemd, runsc or a production worker. Worker execution and export
assembly, retained-file recovery and live qualification remain open.

## Worker execution coordinator

The internal Linux worker coordinator joins an authenticated `WorkerSession`,
bounded workspace, verified image/input staging, OCI bundle writer and contained
runtime. `BeginExecution` consumes one attempt before current-authority checks;
a failed entry cannot be retried through the same session. Reauthorization and
reporting remain available within that attempt.

Image and input staging limits come from the pinned worker configuration and
share validation with the staging implementations. You supply authenticated
source services and policies that receive a detached copy of the verified job,
including its principal and scope. The coordinator repeats current broker
checks around staging and before execution. It writes the canonical bundle
exclusively, so an existing configuration cannot be overwritten.

Runtime cleanup precedes workspace cleanup. Unresolved runtime cleanup leaves
storage for independent service recovery and withholds the report. Preparation
failures receive a bounded, generic reason only after workspace cleanup and a
fresh authority check. A source error cannot change the cleanup sequence.
Report submission retries only explicit backpressure, with the same payload and
original deadline. An uncertain acknowledgement requires broker inspection.
The worker process must exit after the coordinator returns.

After runtime cleanup, the coordinator captures declared files before removing
the workspace. You must supply an immutable evidence repository, current output
policy and a pinned `capture_retention_ns`
between zero (exclusive) and 24 hours. The worker session supplies the
authenticated capture journal through its private daemon connection.
Zero disables capture. A job requesting capture is refused before execution if
these services are unavailable. Authorized release remains a separate host
operation. Source adapters, executable worker/daemon installation, broker runtime
activation, retained-file recovery, production repository integration and live qualification remain
open. The Linux staging fixture uses real image/input verification, bundle writes
and artifact capture with an explicit test executor and workspace. It does not
attest to runtime isolation.

## Declared artifact capture

Each signed `outputs` entry carries a host-allocated evidence ID, guest path
under `/tmp/outputs/`, media type and positive `max_bytes`. The job and host grant
must both provide `limits.artifact_bytes`; an omitted budget permits no capture.
The current bounds are 16 files, one MiB per file and 16 MiB total. Duplicate
identities, reused input identities, overlapping paths and traversal are refused.
Jobs without capture retain their previous JSON representation.

The reader opens every path component without following links and accepts only
regular files with one hard link. Archives remain opaque bytes. Captures always
use restricted sensitivity, job-bound provenance and the host retention window.
The output policy receives a detached signed job and must check current principal,
source grants and retention policy. The worker verifies each returned reference,
reopens its content and checks the exact length and digest before reporting it.
Media type describes expected content; it does not validate the bytes or make
HTML safe to render.

Every declared file is required for an exited result, including nonzero exit
codes. Missing or invalid capture produces a failed result with a generic reason;
previously verified captures may remain attached. Failed or interrupted execution
does not attempt capture. The broker rejects undeclared, substituted or duplicate
references. Captured output remains untrusted input for protected grading.

Before upload, `capture.Journal.Record` must commit the exact reference, including
digest, timestamps and provenance, to the daemon. After repository readback,
`Confirm` must commit its confirmation. The worker checks current read permission
again before returning the reference. Missing journal services refuse capture
before execution. An uncertain journal acknowledgement stops capture, just as an
uncertain repository acknowledgement does. Neither operation retries itself.

The daemon's `workerstore.RecordCapture` and `ConfirmCapture` bind these operations
to the current token, job and claim revision. Capture revisions are separate from
execution revisions, so recording an intent cannot renew or invalidate the
worker's claim. Records are limited to 64 KiB and must match signed declarations.
`Propose` and `Finalized` accept only matching confirmed artifacts whose retention
has not expired. Confirmation records verified readback, not task correctness.

`CaptureJobs` pages up to 256 recovery job IDs, including terminal jobs. `Captures`
loads their bounded records. A recovery worker can distinguish an uncertain
upload from confirmed content using the retained reference. After independent
whole-service cleanup and authorized repository cleanup, `DiscardCapture` retains
a revision-bound tombstone. The caller must prove a durable repository tombstone
or authoritative absence; a missing process or denied read does not suffice.

The worker-control adapter sends `capture-intent` and `capture-confirm` through
the existing authenticated upload handshake. Each reference is bounded to 32 KiB;
captures share the report capacity ceiling and preserve the authorization reserve.
Receipts bind the exact job, claim revision, artifact, reference and capture state.
A report receipt cannot acknowledge capture, or vice versa.

Supply a `service.CapturePolicy` when constructing the controller to enable
capture. Omission denies it. The policy receives the signed job principal,
detached reference and scoped context for put or read authorization. The controller
rechecks the peer, current grants, service configuration and claim after policy
approval, then commits under the original deadline before acknowledging.

`WorkerSession.CaptureJournal` binds the pipeline to this transport. It permits
one intent and one confirmation attempt per declared output after execution starts,
serializes them with reporting, and rechecks authority after the response. Even
explicit upload backpressure consumes the capture attempt. An uncertain response
requires daemon inspection; it cannot trigger another upload. Failed capture still
permits a failed job report. Workers never open the daemon's database.

Production repository adapters remain required before deployment.
Tests connect the private transport to real journal persistence with an explicit
worker-identity fixture. Linux fixtures exercise daemon socket authentication and
the session exchange with a service-membership seam. Artifact capture
does not authorize publication, deployment or an external destination write.

## Artifact recovery

Configure `service.CaptureRecovery` with the worker journal, evidence repository,
tombstone reader and an independent cleanup actor and policy. Pass it through
`RecoveryConfig.Captures`. Enabling a controller capture policy requires recovery
bound to the same journal. Existing pending captures also keep startup closed
when recovery services are missing.

The controller recovers execution first, then captures, before admitting work.
Periodic scans keep separate bounded cursors for execution and artifact recovery.
Each capture page visits at most 256 jobs and each job has a 30-second cleanup
budget. A failed artifact does not prevent checking later artifacts or pages.
An unresolved scan stops admission and remains available for the next recovery.

Recovery leaves nonterminal jobs alone. It retains exact artifacts referenced by
the terminal result until their retention deadline. Other captures are abandoned,
including uploads whose confirmation or result was lost. Recovery never turns
those bytes into a successful result. Both abandoned and expired captures require
current deletion and audit authority, followed by an exact durable tombstone,
before the journal records disposal. A denied read or momentary absence is not
proof of cleanup.

Deletion must reserve the identity even when content has not arrived yet. The
shared SQLite, PostgreSQL and Mongo evidence adapter now creates such tombstones
for absent identities. Concurrent or delayed saves cannot resurrect those bytes.
An identical deletion preserves its original actor, reason and time; a different
binding, actor or reason conflicts. External repositories must provide the same
barrier, including for uploads already in flight. A backend that only reports
that an object is missing cannot satisfy this contract.

After a lost deletion acknowledgement, the next scan reads the retained tombstone
instead of issuing another deletion. Recovery rechecks authority after reading
that proof. The repository keeps its original revocation audit, while the worker
journal retains the capture binding and disposal reason. Production repository
permissions, installed daemon lifecycle and live runtime qualification still need
end-to-end verification.

## Worker evidence access

`service.WorkerEvidence` supplies the daemon-side input, upload and readback
boundary. Configure an input source, current read policy and a byte ceiling of
at most one GiB. Input references must exactly match the signed job. Output
access uses the capture policy and the same repository as artifact recovery;
it cannot choose a different destination. Output writes require a matching
unconfirmed capture intent, while readback accepts the retained intent or
confirmation for that claim.

Each operation verifies the live worker, service configuration, claim and current
grants. Policies receive detached signed identity and reference values. A policy
callback cannot change the retained job or upload binding. The service rechecks
capture state after policy evaluation, so a confirmation during that callback
prevents a later upload from starting.

Reads use bounded copying with exact length and SHA256 checks. The source is
closed on cancellation, read failure or completion, and close errors prevent
success. Current authority is checked after opening and again after copying.
The caller must unblock destination writes when the context is cancelled.
Transferred bytes remain provisional until the call succeeds. Uploads are limited
to one MiB, verify the returned reference and perform no provider retry after an
uncertain response. A returned upload reference still requires repository readback
and capture confirmation before a result can use it.

Pass the evidence service as the optional third argument to `NewWorkerControl`
to enable private byte transfer. It must use the same controller. `ReadEvidenceAt`
reads signed inputs or output readback; `PutArtifactAt` uploads an output. These
operations share the report/capture capacity limit and preserve the reserved
authorization connection. Without the service, evidence requests fail before
payload admission.

Requests authenticate through the private endpoint before sending canonical JSON.
References are bounded to 32 KiB and uploads to two MiB of encoded data, with a
one MiB content ceiling. Downloads use chunks of at most 64 KiB and independently
verify exact length and SHA256 at both ends. Completion requires an explicit end
marker, a receipt bound to the request, job, claim and full artifact reference,
and EOF. Partial data, ignored write failures, provider errors, peer-close errors
and cancellation cannot acknowledge success. The transport performs no retry.

`WorkerSession.InputSource` and `ArtifactRepository` connect staging and capture
to this transport. They require the signed scope and reference, an active execution
attempt, and current deployment and job authority. Output upload requires the
acknowledged capture intent and consumes one attempt before contacting the daemon.
Even explicit backpressure cannot replay that upload. Readback requires successful
upload, and a session with evidence access cannot confirm capture before verified
readback completes.

Readers stream through a bounded pipe. EOF follows the final receipt and authority
check; partial bytes and late failures remain failures. Closing early cancels and
joins the transfer. Reports and other session operations cannot overlap an active
transfer. The original signed deadline remains in force.

`worker.Run` always obtains evidence capabilities from the bootstrapped session.
Its `Sources` argument now supplies only the approved image source and image
policy. Local staging guards verify signed bindings; current repository grants
are enforced by the daemon at the content operation. Approved image delivery,
installed provider configuration and live qualification remain open.

## Approved image access

`service.WorkerImages` is the daemon-side image content boundary. Configure an
approved digest-addressed source, current image policy and an aggregate compressed
budget of at most four GiB. Each read authenticates the worker claim and reloads
the exact manifest pinned by its signed job. The policy receives detached job
identity and scope. Workers cannot select registry URLs, tags or unrelated blobs.

The broker verifies the manifest digest before parsing it. Staging and broker
access share `image.ParseManifest`, which bounds metadata to one MiB, permits up
to 128 layers, checks the total declared size, and rejects external URLs, embedded
content, unsupported descriptors and conflicting declarations for one digest.
Only the manifest, its configuration and its directly declared layers may be read.
Platform, DiffID and archive validation remain in image staging.

Blob reads verify exact size and SHA256, close sources once on cancellation, and
preserve read/close errors. Current grants are checked before opening, after
opening and after copying. A successful size receipt follows the final authority
check; received bytes remain provisional until then. No source failure retries.
The caller owns destination cancellation and must unblock its writes.

This boundary still needs private image transport, a worker image-source adapter
and installed provider configuration. It does not establish live containment or
permission to execute an image.

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
