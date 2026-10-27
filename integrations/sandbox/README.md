# Cortex sandbox broker

You can embed this broker behind an authenticated HTTPS server. It accepts sealed
`sandbox.Job` requests, writes a signed receipt to SQLite before dispatch and
keeps job identity across retries and restarts. The root `sandbox` package carries
only the contracts. This module owns HTTP, durable state and export handling.

The runtime adapter, supervised worker, installed broker command and separate
[evidence authority](cmd/cortex-sandbox-authority/README.md) are implemented. The
authority verifies signed retained probe output and current policy over mTLS.
No qualified Linux worker ships here. Unit tests use explicit fixtures and cannot
establish live isolation or resource enforcement. Linux worker selection and
qualification remain deferred by request.

The implementation sections below retain some earlier delivery notes. The current
entry points are `cmd/cortex-sandboxd`, `cmd/cortex-sandbox-worker`,
`cmd/cortex-sandbox-input` and `cmd/cortex-sandbox-authority`. Their command guides
and the current assured-execution ledger supersede earlier statements that a
daemon, runtime adapter or authority service is missing.

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
attestation. The installed adapter performs this binding; live qualification
remains a separate deployment check.

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
deadline. Worker deadline supervision and the outer service ceiling remain
required. Installed assembly also wires independent service discovery and
retained workspace and credential cleanup.

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
`Serve` takes ownership of that listener, closes it on shutdown and leaves the
socket path for the host's cleanup. Keep the directory stable and outside guest
mounts.

You can use `ServeAt` to let the daemon create its socket in an existing private
directory. It acquires an exclusive lock on `<socket>.lock`, checks any existing
endpoint against its durable ownership record, then creates the socket with mode
`0600`. Shutdown joins the transport
handlers before removing the socket through the retained directory descriptor.
Removal requires the same device, inode, owner and socket type. A replacement is
left untouched and cleanup reports an error. Normal shutdown permits a later
daemon to bind the path again.

Keep the lock file in place. Removing or replacing it while a daemon runs would
break exclusive ownership. Before serving, the daemon synchronizes a bounded
canonical ownership record and its directory. The record binds the endpoint path,
device, inode, owner/group, mode, link count and nanosecond change timestamp.

After a crash, recovery requires that exact metadata and a kernel connection
refusal. A live listener, a timeout, a changed socket or a missing/corrupt record
prevents removal. Recovery rechecks identity after the liveness probe and unlinks
through the retained directory descriptor. A crash before ownership persistence
can leave an unrecorded socket requiring operator inspection. This intentionally
blocks startup rather than deleting an unknown endpoint. Executable daemon wiring
and live runtime qualification remain open.

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

The worker executable and installed daemon assembly connect this startup
component to image delivery, export and retained-file cleanup. Production provider
configuration and standalone daemon installation remain open. Session evidence
adapters connect input staging
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
not qualify systemd, runsc or a production worker. Installed provider
configuration and live qualification remain open.

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
operation. Standalone daemon installation, production repository integration
and live qualification remain open. The Linux staging fixture uses real image/input verification, bundle writes
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

Pass `WorkerContent{Evidence: access}` as the optional third argument to
`NewWorkerControl` to enable private evidence transfer. It must use the same
controller. `ReadEvidenceAt`
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

`worker.Run` obtains image, input and artifact capabilities from the bootstrapped
session. Local staging guards verify the signed job and image bindings; current
source and repository grants are enforced by the daemon at each content operation.
Registry and repository credentials stay in the daemon. Installed provider
configuration and live qualification remain open.

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

To enable image transfer, pass `WorkerContent{Images: access}` to
`NewWorkerControl`, alongside `Evidence` when you need both. Each configured
service must share the controller. Omitted capabilities reject requests before
payload admission.

`ReadImageAt` requests one pinned digest with a caller byte ceiling. It uses the
same private endpoint, bounded transfer slots and 64 KiB chunks as evidence reads.
Both ends verify SHA256. The final receipt binds the observed size, digest, job
and claim to the request. A corrupt, incomplete, expired or unacknowledged stream
returns an error, even if some bytes reached your destination. No automatic retry
occurs. Image requests accept at most two KiB of canonical metadata and cannot
supply a source URL.

`WorkerSession.ImageSource` binds image reads to an active execution, exact scope,
current deployment checks and the configured compressed-byte ceiling. It uses the
same reader lifecycle as evidence: early Close cancels and joins the transfer,
while clean EOF requires both the transport receipt and a final authority check.
A report or another content operation cannot overlap an active reader.

The worker pipeline takes its image source from this session and checks the signed
job's image pin before staging. Installed provider configuration remains open.
These transport tests do not qualify a live systemd/runsc deployment.

## Worker executable

Build the worker from this module for your Linux architecture:

```sh
GOWORK=off GOOS=linux GOARCH=arm64 go build -o cortex-sandbox-worker ./cmd/cortex-sandbox-worker
```

Use `GOARCH=amd64` for an amd64 host. Pin the resulting executable in the worker
configuration before rendering the service template. The command accepts only
that template's `--job <id> --config <absolute-path>` invocation and obtains its
credential directory from systemd's `CREDENTIALS_DIRECTORY` environment variable.
It authenticates through `BootstrapWorker`, then calls `worker.Run` once.

SIGTERM and interrupt cancel the existing operation. Failed startup, cancellation
or an uncertain report returns exit code 1 without replaying execution. Exit code
0 means the coordinator returned successfully; it does not prove result
finalization or that the evaluated task passed. No errors or captured content are
printed by the command. Broker state remains the inspection path.

The non-Linux executable exits with code 1. Installing the pinned binary,
configuration and service template still requires a host deployment. Daemon
assembly and live runtime qualification remain open.

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

### Broker runtime adapter

Use `service.NewRuntime(controller, attest)` to connect the broker to the worker
controller. The attestation callback must check the installed worker, configuration
and runtime against host-owned live qualification evidence. Missing qualification
refuses execution. The adapter does not run qualification probes for you.

Start rechecks the current runtime digest, profile, qualification identity, limits
and qualification deadline before the launcher verifies the signed acceptance.
A shorter qualification window cannot extend an existing job beyond current
controls. The original acceptance and claim still govern retries.

Inspection returns output only from a finalized journal record. Claimed work and
pending cleanup report running with no proposal bytes; an unclaimed acceptance
returns pending. Cancellation records intent before reaping. For a missing worker
record, it masks and stops that service identity before reporting absence, so a
late launch cannot cross completed cancellation. Cleanup errors remain unresolved.

All four runtime operations join the controller lifetime and use a 30-second
operation bound. Keep the adapter private to the authenticated broker and its
supervisor. Recovery can inspect and cancel after grants expire; the broker still
checks caller identity and current grants before exposing results.

Tests cover broker restart, uncertain starts, signed result collection, foreign
principal denial, missing-record cancellation, proposal withholding, journal
failure and shutdown. These use controlled service-manager fixtures. Installed
daemon assembly, provider configuration and live systemd/runsc qualification
remain open.

### Daemon lifecycle

`service.NewDaemon` joins prepared controller and content services with the broker
journal. Supply an unused controller and its worker control transport, a private
socket path, current deployment attestation and broker configuration. The signing
key must be valid and match the identity pinned in the worker journal. The daemon
supplies the runtime adapter and refuses a separate runtime override.

`Run` acquires the broker journal and socket before starting recovery. It waits
for controller recovery and private transport readiness, reconciles retained broker
state, then calls your public server. Use the existing TLS-only `Broker.Handler`
with host authentication. Your server must honor its context, close its listener
and join requests before returning. Do not retain the broker after that call.

Any child exit cancels the shared lifetime immediately. Reconciliation errors
also stop the daemon. Shutdown joins public serving, worker transport and
controller recovery before releasing the socket and broker journal. Errors from
cleanup remain in the returned error. You still own the worker journal, reaper
and providers; close them after `Run` returns. A daemon cannot be restarted in
place.

An unclaimed worker acceptance remains pending during reconciliation. That state
does not report execution or completed cancellation. A pending error joined with
another runtime failure still fails reconciliation.

The lifecycle tests use real journals and sockets with controlled service-manager
behavior. Linux kernel fixtures also exercise the production endpoint and private
transport, including ambient-root denial. Host installation, provider assembly,
public TLS server configuration, orphan discovery and live systemd/runsc
qualification remain required.

Runtime absence is a positive observation. Return bare `ErrNotFound` only after
confirming that identity is absent and cannot execute later. The broker preserves
wrapped or joined errors, including `ErrNotFound` joined with a cleanup failure.
Those errors cannot produce a successful cancellation or a retained result.
The HTTP adapter also preserves that distinction: wrapped or joined absence and
pending errors return 503, rather than 404 or 202, so remote clients cannot lose
the accompanying runtime failure.

### Bounded public TLS serving

Use `sandboxbroker.NewServer` for the daemon's public callback. Supply a current
certificate and matching software key, an authenticator and explicit `ServerLimits`.
Supported keys are ECDSA P-256/P-384/P-521, Ed25519 and two-prime RSA from 2048 to
8192 bits. Certificate chains, metadata and keys have size bounds. The server
copies this material and any client CA pool; it does not retain your mutable key
or parsed leaf certificate.

TLS 1.3 and HTTP/1.1 are required. Session tickets and HTTP multiplexing are disabled.
A non-nil client CA pool requires a verified client certificate before your
application authenticator runs. Without that pool, the authenticator must validate
host-issued credentials over TLS. Identity and current grants still belong to the
host. Static certificates are replaced by starting a new server lifetime.

Set `Connections` from 1 to 256 and `HeaderBytes` from 1024 to 32768. Header timeout
is 10ms to 10s; request timeout is 50ms to 30s and cannot be shorter than the header
timeout. Idle timeout is 10ms to 30s. Header parsing uses net/http's bounded header
budget. The broker's existing request and result size checks still apply.

Connections and active broker handlers have separate admission limits. An expired
request closes its connection, but its work slot stays occupied until the handler
returns. Saturated work admission returns 503 before authentication or dispatch.
This also bounds work when a trusted callback takes time to finish cancellation.

Create the listener inside the daemon callback, then pass it to `Serve`:

```go
err = daemon.Run(ctx, func(lifetime context.Context, instance *sandboxbroker.Broker) error {
    listener, err := (&net.ListenConfig{}).Listen(lifetime, "tcp", bindAddress)
    if err != nil {
        return err
    }
    return publicServer.Serve(lifetime, listener, instance)
})
```

`Serve` owns that listener for its first valid call. Cancellation or a listener
failure closes connections, cancels requests and joins active broker handlers
before returning. Callbacks must honor cancellation. The daemon can then recover
workers and release the broker journal. Reusing a server returns `ErrConflict`
without taking ownership of another listener.

Loopback TLS tests cover authentication, mTLS, connection saturation, unfinished
handler admission, request deadlines, slow bodies, oversized headers, certificate
copying and listener failure. A daemon fixture submits a signed job through this
server and verifies joined shutdown. Host certificate issuance, current credential
authorization, installed provider configuration and live sandbox qualification
remain required.

### Installation-bound qualification

Use `service.NewDeploymentAttestation` when assembling the installed daemon on
Linux. It loads the protected worker configuration and pins its profile digest,
runtime inventory digest and broker identity for that lifetime. Construction
checks installed files; it does not grant qualification.

Each observation reloads and verifies the installation before calling your
`QualificationVerifier`, then checks it again before returning capabilities.
Changing the configuration, worker binary, service template, runtime inventory
or protected directory permissions fails the observation. Reassemble after an
approved installation change. An old qualification cannot silently follow it.

Your verifier receives the expected `DeploymentIdentity` and returns a
`DeploymentQualification` bound to that same identity. It must verify retained
live probe evidence against the current host, kernel and effective controls,
including revocation and expiry. The attestor checks the returned identity,
runtime digest and capability validity. It cannot establish the authenticity of
evidence supplied by a dishonest verifier. Fixture results never qualify a host.

Observations have a 30-second deadline, preserve shorter caller deadlines and do
not cache qualification. The verifier must honor cancellation and support
concurrent calls. Launch and worker admission still perform their own checks.
`sandbox.Capabilities.Validate` shares shape and expiry checks with job admission;
validation alone grants no execution authority.

Tests cover installation changes before and during verification, mismatched
identities, revoked or expired qualification, malformed capabilities, concurrent
observations and cancellation. A Linux fixture exercises protected configuration
loading through the production constructor. Host evidence-provider assembly and
live Linux qualification remain open.

### Installed host assembly

`service.AssembleDaemon(ctx, HostConfig)` joins the installed configuration,
worker journal, service manager, source providers, capture recovery and broker.
It requires root on Linux. Supply an already opened worker journal and reaper;
you retain ownership of those resources and the providers until `Run` returns.
Assembly starts no service and does not invoke qualification.

The installed configuration supplies the control socket, launch profile, input
size ceiling and compressed-image ceiling. Launch and attestation use the same
verified configuration snapshot. Broker name, signing key and worker journal
must agree with the installed identity. Configure at least one private report
slot so workers can transfer content and report completion.

Supply public-operation authorization through `Broker.Authorize`, connected-caller
launch checks through `AuthorizeLaunch`, and signed-subject grant checks through
`Revalidate`. Recovery loads the original receipt from the worker journal and
requires its exact job digest. It then checks current grants and qualification
against that receipt, including the accepted runtime, profile, qualification ID,
execution deadline and resource limits. Worker access uses this same recovery
check. Policy callbacks receive detached job values.

Capture recovery is required even when new captures are disabled, so evidence
from an earlier daemon remains recoverable. Its journal is supplied by assembly;
an explicitly supplied journal must match. `CapturePolicy` must be present exactly
when the installed `CaptureRetention` is nonzero. Worker uploads and independent
cleanup use the repository configured in `Captures`. Cleanup authorization must
remain independent of the expired execution grant.

Pass the resulting daemon to the bounded TLS serving pattern above. The assembler
accepts trusted host providers; it does not provision certificates, credentials,
provider configuration or a live qualification-evidence service. A standalone installed
broker command and live systemd/runsc qualification remain open.

Root Linux fixtures cover lifecycle, missing services, identity mismatches,
capture configuration, changed grants, changed qualification and mismatched jobs.
CI runs these fixtures with the race detector alongside private worker transport
checks. These are controlled service-manager tests, not live runtime qualification.

### Orphan discovery

Reserve the entire `cortex-sandbox-worker@*.service` namespace for one worker
journal. Installed assembly requires `DiscoveryLimit` (1 through 10000) and a
manager that can enumerate that namespace independently of journal records.
Do not share it across brokers with separate journals.

Discovery queries systemd's `ListUnitsByPatterns` with all states, including
inactive services and queued jobs. It rejects aliases, malformed names,
duplicates and oversized snapshots. The systemd method has no pagination;
`DiscoveryLimit` bounds accepted rows after D-Bus decoding, not transport memory.
You can inspect the method in the [systemd D-Bus interface documentation](https://www.freedesktop.org/software/systemd/man/257/org.freedesktop.systemd1.html).

Services missing from the journal or already terminal go through persistent
fencing and whole-service drain checks. A journal read failure also triggers
cleanup, but the failure remains unresolved even if cleanup succeeds. Recovery
does not manufacture a receipt or change a recorded result. Known nonterminal
jobs stay with normal execution recovery.

Startup recovery gates admission. Periodic discovery uses its own page cursor;
an error closes admission and triggers shutdown recovery. Each pass revisits
services inserted before the previous cursor. This discovery runs inside the
daemon, so you still need installed restart supervision and external resource
limits when the daemon itself is unavailable.

### Retained workspace recovery

Create a root-owned `0700` directory at `StateDirectory/workspaces` before
assembling the daemon. Assembly pins its inode and unique Linux mount identity.
Workspace discovery has its own bounded cursor and uses `DiscoveryLimit` and
`PageSize`. It includes directories whose services are no longer loaded.

The scanner shares orphan recovery's journal rules: leave nonterminal jobs with
execution recovery, reclaim missing or terminal jobs, and retain journal errors
even when cleanup succeeds. Before removing a workspace it persistently fences
the service and confirms the complete cgroup drain. Cleanup authority does not
depend on an expired execution grant.

Only empty `stage` and `scratch` mountpoints and their job directory are removed.
These are the host directories beneath the worker's private tmpfs mounts. Cleanup
does not traverse payload files, unmount filesystems or delete unexpected content.
Symlinks, changed ownership or permissions, replacement roots, mounted job
directories and mounted children leave recovery unresolved. Same-filesystem bind
mounts also fail the unique mount identity check.

Partial setup and already absent directories are safe to retry. Startup recovery
must finish before admission; periodic failures close admission and trigger
shutdown recovery. The host must prevent concurrent privileged filesystem
changes. Launch credentials use the separate fixed file layout described below.


### Launch-credential recovery

Create a root-owned `0700` directory at `StateDirectory/launch` before assembly.
The daemon pins this root and scans it independently of loaded services. Missing
or terminal journal records are eligible for cleanup; live claims remain with
execution recovery. Journal errors still trigger containment cleanup and remain
unresolved afterward.

The cleanup layout accepts at most one file: `claim.tmp` or `claim.json`. It must
be a regular, single-link file owned by root, mode `0400`, at most one MiB, and on
the original filesystem and mount. Empty or malformed content can come from an
interrupted write, so cleanup neither parses nor logs it. Unknown entries,
multiple files, aliases, changed permissions and mounts stop cleanup. Directory
changes are synced before success is returned. These are unlink guarantees, not
physical media erasure.

A shared gate coordinates credential publication and removal for each job.
Cleanup waits for an in-flight publisher; a later publisher must verify current
journal ownership while holding the same gate. Cancelled or terminal claims
cannot recreate a file after cleanup. Gate storage is fixed, waits honor caller
cancellation, and a failed operation releases its slot. Use the controller for
all launches and keep privileged writes outside these directories.

Installed assembly requires credential recovery alongside workspace and service
recovery. Each has its own periodic cursor, and an unresolved scan closes
admission. Startup and shutdown run all three through the same journal eligibility
rules. A replacement controller must retain the launcher's matching credential
gate; it cannot silently discard or replace that coordination boundary.

### Installed local image content

Within the sandbox module, use `image.NewLocalSource(root, maximum, readers)`
as `HostConfig.Images` to serve preinstalled content. Provision the root,
`blobs` and `blobs/sha256` directories with root ownership and mode 0700.
Each blob must be a root-owned, single-link regular file with mode 0400.
The filename is its lowercase SHA256 without the `sha256:` prefix, following
the [OCI blob layout](https://github.com/opencontainers/image-spec/blob/v1.1.1/image-layout.md#blobs).
The source resolves only explicit digests. It does not resolve index tags,
fetch missing content or accept registry URLs.

Set a per-blob maximum of at most 64 MiB and 1 through 32 simultaneous readers.
Their combined reserved snapshot capacity cannot exceed 256 MiB. Budget daemon
memory for those snapshots plus its other services. Capacity waits honor the
request context, and every returned reader must be closed, including after EOF.
Closing releases its bytes and slot; repeated or concurrent closes are safe.
Choose another `image.Source` when your approved image has larger blobs.

The source checks directory identity before and after loading each blob, verifies
file metadata through no-follow descriptors and hashes the complete content
before returning a snapshot. Worker reads then use memory, so a later host file
replacement cannot change their bytes. Keep provisioning separate from active
reads and use a responsive local filesystem. Cancellation is checked between
file calls, but cannot forcibly interrupt a stalled kernel filesystem call.

You still supply `ImagePolicy`. `WorkerImages` checks current authorization,
the signed job's manifest and membership of each requested blob; image staging
also verifies hashes and layer DiffIDs. Local content is neither execution
authority nor runtime qualification. Installed provider configuration,
credential provisioning and live Linux qualification remain separate work.

### Installed daemon credentials

Use `service.ReadDaemonCredentials` with the public key returned by
`ReadWorkerConfig`, protected `DaemonCredentialFiles`, your authenticator and
explicit `ServerLimits`. It returns the receipt-signing key and a prepared
`sandboxbroker.Server`. Pass the signing key into `HostConfig.Broker.SigningKey`
and use the server's `Serve` method in the daemon callback shown above.
Loading credentials does not open a listener or qualify a worker.

Each credential pin specifies an absolute path, SHA256, byte length and mode
0600. Files must belong to root and have one link. The loader uses the same
no-follow path and hash checks as deployment verification. Keep these pins and
private keys in daemon configuration, outside worker configuration and launch
credentials. The receipt key must match the verified worker's public key.

| File | Encoding | Maximum |
|---|---|---|
| Receipt-signing key | One PKCS8 Ed25519 `PRIVATE KEY` block | 4 KiB |
| Server certificate chain | 1 through 8 `CERTIFICATE` blocks | 192 KiB |
| Server private key | One PKCS8 `PRIVATE KEY` block | 24 KiB |
| Client CA bundle | 1 through 32 distinct `CERTIFICATE` blocks | 256 KiB |

The loader rejects extra text, malformed or nested PEM blocks, PEM headers and
trailing DER values after a private key.
Client trust entries must be CA certificates with certificate-signing usage.
TLS key support, certificate validity, key matching and decoded size limits come
from `NewServer`; the loader does not maintain another TLS validation policy.
Any failure returns no credentials or server.

Client certificates are required for this installed transport. A verified TLS
chain establishes transport identity; your authenticator must still map it to
the current principal and scope and apply revocation policy on each request.
The broker authorizer governs execution grants separately. Loading a CA bundle
does not grant its clients permission to execute jobs.

TLS rotation requires a new server lifetime. Receipt-key changes also require
matching worker configuration and journal identity. Certificate issuance,
credential installation, host identity policy and standalone broker configuration
remain host responsibilities; this loader only reads an approved installation.

### Current host grants

`service.NewHostPolicy(ctx, path)` loads a root-owned, single-link mode-0600
policy file. Encode `HostPolicyDocument` with `json.Marshal`; the loader requires
canonical JSON without extra fields or trailing whitespace and limits the file
to one MiB. Keep its parent protected. Publish updates by atomic replacement.

The document has a finite validity interval and at most 512 subjects. Each
`HostSubjectGrant` binds an exact principal and ordered scope to a validity
interval, 1 through 256 manifest digests and a `sandbox.Limits` ceiling.
The grant interval must fit inside the document's interval. All resource limits
must be explicit. An ancestor scope does not grant access to descendant scopes.
The broker's static image allowlist remains an independent requirement.

A subject can map up to 16 client-certificate fingerprints. Use the lowercase
SHA256 of the leaf certificate's DER bytes, as produced by
`evidence.Digest(certificate.Raw)`. Fingerprints must be unique across the whole
document, and each principal/scope pair can appear once. Authentication requires
TLS 1.3, a completed handshake and a verified chain of at most 16 certificates.
Certificate validity is checked again on every request, including requests on
an existing connection. Names and headers do not select an identity or scope.

Wire the policy methods into the installed services:

| Configuration | Method |
|---|---|
| `ReadDaemonCredentials` authenticator | `policy.Authenticate` |
| `HostConfig.Broker.Authorize` | `policy.Authorize` |
| `HostConfig.AuthorizeLaunch` | `policy.Revalidate` |
| `HostConfig.Revalidate` | `policy.Revalidate` |
| `HostConfig.ImagePolicy` | `policy.AuthorizeImage` |

Every method rereads the protected file. A missing, malformed or expired document
denies access without falling back to earlier grants. Empty `Subjects` revokes
all policy access. Removing a fingerprint revokes that transport identity on its
next authentication check while retaining the subject's separate execution
grant. Remove the subject, expire it or reduce its limits to revoke execution.
The existing recovery scanner then fences work that no longer has a valid grant.
The active request remains subject to its own current authorization checks.

Evidence access, capture cleanup and runtime qualification still require their
own policies. This host policy grants bounded sandbox execution and image access;
it does not grant artifact access or publication. Supply those services when
assembling the broker. Standalone configuration and installation remain open.

### Evidence repository binding

Use `service.NewEvidenceRepository` to connect the daemon to an existing Cortex
`evidence.Storage` and its `evidence.TombstoneReader`. Both dependencies must use
the same durable repository. The adapter reuses the root evidence repository for
exact scope, immutable writes, byte ceilings and content verification. Configure
`MaxBytes` explicitly, at most one GiB and within the backend's limit. The shared
SQLite, PostgreSQL and MongoDB evidence stores currently cap artifacts at one MiB.

Supply current `Inputs` and `Captures` policies, plus an independent `Cleanup`
policy. Nil `Captures` disables capture reads and writes. Cleanup must authorize
the context's deletion actor under the original signed job subject, including
after execution grants expire. It covers deletion and tombstone audit together.

Pass this adapter as `HostConfig.Inputs` and as both `Repository` and `Audit` in
`HostConfig.Captures`. Keep the corresponding worker and recovery policies in
place. `WorkerEvidence` supplies the private validated job context after checking
the live worker, claim and capture intent. `CaptureRecovery` supplies a separate
cleanup context. A caller with only scope, principal or a deletion actor receives
no repository authority.

Worker contexts can read exact declared inputs and read or write declared
captures. They cannot delete evidence or inspect tombstones. Cleanup contexts can
delete and audit declared captures, but cannot access content or delete inputs.
Tombstone reads recheck policy after storage access and verify the complete
reference binding before returning proof. Cleanup preserves the store's durable
fence against late uploads, including captures whose bytes never arrived.

SQLite integration fixtures exercise worker input, intent, upload, readback and
confirmation, plus cleanup of stored and absent captures. They also cover revoked
policy, cancellation, altered authority and mismatched audit records. Database
provisioning, installed source grants and live runtime qualification remain open.

### Installed evidence grants

`service.NewEvidencePolicy(ctx, path)` loads an `EvidencePolicyDocument` from a
root-owned, single-link mode-0600 file. Encode it with `json.Marshal`, keep the
parent directory protected and publish changes by atomic replacement. The loader
requires canonical JSON and caps the file at one MiB. Every operation rereads it;
a missing, invalid or expired file denies access without using an older snapshot.

The document has separate `Subjects` and `Cleanup` lists, each limited to 512
entries. Every grant has a finite validity interval inside the document's
interval. Principal and actor names are nonempty UTF-8 strings of at most 512
bytes, without NUL. Scope matching is exact, including level order and depth.
Duplicate subject/scope pairs or cleanup actor/subject/scope triples are invalid.

For input access, place each approved `Ref.BindingDigest()` in the subject's
`InputBindings`, up to 256 unique digests. This approves the complete reference,
including its content hash, provenance, sensitivity and retention. The reference
must also be a signed job input. Reads reject future creation times and expired
retention. Changing any reference field requires a new approval.

A subject's optional `Captures` grant allows declared output reads and writes.
Set explicit `MaxBytes` (1 through 1,048,576) and `MaxRetention` (positive, at most
24 hours, encoded as nanoseconds). These ceilings apply to each output and its
full creation-to-retention interval. The job's signed output declarations and
resource budget still apply. Nil `Captures` revokes capture access.

Cleanup grants identify an actor, the job's original principal and its exact
scope. They authorize deletion and tombstone audit only. Removing a subject's
input or capture grant does not revoke cleanup, and expired evidence remains
eligible for cleanup. Keep cleanup grants current until recovery has finished;
an expired cleanup grant leaves the capture unresolved.

Wire one policy into both service and repository checks:

| Configuration | Method |
|---|---|
| `HostConfig.InputPolicy`, `EvidenceRepositoryConfig.Inputs` | `policy.AuthorizeInput` |
| `HostConfig.CapturePolicy`, `EvidenceRepositoryConfig.Captures` | `policy.AuthorizeCapture` |
| `HostConfig.Captures.Authorize`, `EvidenceRepositoryConfig.Cleanup` | `policy.AuthorizeCleanup` |

Leave `HostConfig.CapturePolicy` nil when the deployment disables capture.
Capture intent and confirmation use the same validated job context as content
transfer. The repository adds its own checks; current worker claims and terminal
recovery state remain the controller's responsibility. These grants cannot publish
artifacts, qualify a runtime or authorize sandbox execution.

The SQLite fixture exercises installed input grants, intent, upload, readback and
confirmation, then revokes content access and cleans the abandoned capture through
its independent grant. File, policy and revocation tests run on macOS and Linux.
You still need to provision the database and policy files, install the broker and
qualify its live systemd/runsc environment.

### Protected evidence database

The internal `evidencedb.Open(ctx, path)` factory initializes a dedicated SQLite
store using Cortex's existing schema and evidence persistence. Production opening
requires root. Provide a canonical absolute path inside an existing root-owned
mode-0700 directory. The database, its `.lock` ownership file and any SQLite WAL,
shared-memory or rollback-journal files must be single-link mode-0600 regular
files. Aliases and unsafe metadata are rejected.

A new database receives an evidence application ID and format version before
Grove opens it or enables WAL. Existing databases must carry that identity;
ordinary Cortex databases, worker/broker journals and future format versions are
not adopted. Startup checks SQLite integrity and runs the root store's migrations.
File names are encoded as SQLite URI paths, so query characters remain part of
the file name. Startup does not repair rejected files or delete their sidecars.

The separate ownership lock spans initialization and the open store's lifetime.
SQLite also uses exclusive locking, one pooled connection, full synchronous
writes and a five-second busy timeout. Close waits for active evidence operations,
closes SQLite and then releases ownership. Do not replace these files while the
store is open. Use local storage that supports SQLite locking and synchronous
writes, and keep the database and its recovery files together.

The factory exposes evidence storage, tombstone audit and Close. It grants no
artifact access. Pass it through `service.NewEvidenceRepository` and the installed
evidence policy; the private worker and recovery boundaries still apply. Tests
verify content and tombstones across clean restarts and process exits with a
committed WAL, plus late-upload fences and exclusion of other SQLite processes.

`RunInstalledDaemon` below owns this database through the daemon lifetime.
Directory/policy provisioning, input ingestion and live systemd/runsc
qualification remain host responsibilities. This factory does not install or
start the broker.

### Installed broker configuration and startup

Use `service.RunInstalledDaemon(ctx, configPath, verifier)` inside the sandbox
module to run a configured Linux broker as root. You must supply a trusted
`QualificationVerifier` that checks current deployment evidence. The runner
rejects a missing verifier before opening storage. The service package remains
internal; a standalone command and a concrete live verifier are still pending.

Write `service.BrokerConfig` as canonical JSON in a root-owned, single-link
mode-0600 file, at most one MiB. `ReadBrokerConfig` validates that document and
the worker installation without opening databases, reading private keys or
starting services. The returned SHA256 identifies the exact broker settings;
the worker keeps its own deployment identity.

| Setting | Required value |
| --- | --- |
| `schema_version`, `worker_config` | Version 1 and an absolute installed worker configuration path |
| `credentials` | Pinned receipt key, server certificate, TLS key and client CA files |
| `host_policy`, `evidence_policy` | Distinct protected policy files, evaluated on each authorization check |
| `listen` | Canonical literal IP and nonzero port, such as `127.0.0.1:8443` or `[::1]:8443` |
| `storage` | Distinct `broker_journal`, `worker_journal` and `evidence` files directly inside the worker state directory |
| `images`, `image_root` | One to 256 unique immutable digests and the private local content tree |
| `image_limits` | Explicit `max_bytes` and `readers` within local-source and worker staging ceilings |
| `input_root`, `input_readers` | Separate private input directory and one to 16 concurrent snapshots |
| `evidence_bytes` | One byte to one MiB, within the worker's per-input ceiling |
| `max_jobs`, `max_active` | One to 100,000 retained jobs; one to 256 active jobs, no more than retained capacity |
| `cleanup_actor` | Principal for independent evidence cleanup grants |
| `public_limits`, `control` | Explicit existing server and control limits, with at least one private report slot |
| `interval_ns`, `page_size`, `discovery_limit` | 10 ms to one second; one to 256 items; one to 10,000 discovered workers |

Marshal the Go types to produce canonical field names and duration values.
The document contains credential pins, not key bytes. The loader reserves all
database sidecar and lock names, worker state subdirectories, configuration,
credentials, policies, worker executable, service template and control socket.
Image and input storage must be separate from each other, runtime and worker state storage. Unsafe
paths, overlapping roles and implicit limits fail startup.

The runner loads current policies, pinned credentials and local image/input storage,
opens the evidence and worker databases, connects to systemd and assembles the
daemon. It rechecks broker configuration bytes and worker profile identity before
starting recovery. Once private control and recovery are ready, it requests fresh
qualification and opens the public mTLS listener. Startup failure or cancellation
joins the daemon and closes owned resources, including a partially opened service
or listener. Cleanup errors are returned with the original failure.

Provision directories, policy grants, credentials and input evidence before you
start the broker. Host and evidence grants remain current during its lifetime;
other configuration changes require a restart. Keep the installed files protected
while it runs. The runner's bounded preparation phase does not replace your
verifier's cancellation handling or the daemon's shutdown supervision.

Linux tests use real protected files, SQLite databases, sockets and mTLS with a
controlled service manager and fixture qualification. They check authenticated
serving, revocation on a reused connection, changed startup files, failed
qualification, partial service/listener failures and released database ownership.
Those fixtures do not qualify a live systemd/runsc installation.

### Installed input files

Provision inputs in the directory configured by `input_root`. It must be
root-owned with mode 0700 and separate from images, runtime, state and installed
configuration. Each file contains the raw artifact bytes and is named by
`ref.BindingDigest()`, the lowercase SHA256 of the full evidence reference.
Use mode 0400 and a single hard link. The reference must also appear in the
signed job and the current evidence policy's input bindings.

The full binding includes scope, source, version, sensitivity and retention.
Identical bytes under another reference need a separate installed file and
grant. `SourceURI` is metadata; the source never fetches it. You can provision
new bindings while the broker runs without opening its exclusive capture
database. Use the provisioning command below and keep the original references
with their source records.

`service.NewLocalInputs` verifies ownership, exact mode, link count, size and
content digest before returning a memory snapshot. The configured ceiling is at
most one MiB per input; empty inputs still require a real protected file and the
empty-content digest. Symlinks, devices, changed directories and mismatched bytes
fail without returning a reader. The source reuses the protected file reader;
images and inputs share the same cancellation-aware snapshot reader.

Each open reader holds one capacity slot until Close. There are at most 16
slots, and waiting respects cancellation. Current grants are checked before the
wait, after acquiring capacity and after reading. Retention is checked around
policy calls. Worker transfer adds live claim and source-policy checks before
its final acknowledgement, so bytes remain provisional until that succeeds.

Revoke a grant to stop subsequent authorized transfers. Existing memory
snapshots cannot be erased from another reader, and trusted host provisioning
owns physical file deletion and retention. Captured outputs continue to use the
evidence database, including its durable deletion and delayed-upload fences;
this source exposes no write or delete operation.

### Provisioning an input

Build `./cmd/cortex-sandbox-input` from the sandbox module with `GOWORK=off`
and install the approved binary through your host deployment. Run it as root
on Linux with these arguments in this order:

```sh
cortex-sandbox-input \
  --config /etc/cortex/broker.json \
  --job /etc/cortex/jobs/task.json \
  --input /inputs/data \
  --file /etc/cortex/sources/data.bin
```

`--job` is the protected canonical encoding of a host-sealed `sandbox.Job`.
`--input` selects one of its declared guest paths. The broker and job files must
be root-owned, single-link mode-0600 files; the source must also have mode 0600
and match the selected reference's exact size and digest. The input directory
must already exist with mode 0700. Current evidence policy must grant that exact
input binding to the job's principal and scope.

The command verifies configuration, the job, source bytes and current grants,
then publishes a mode-0400 file under the reference binding digest. It syncs the
file before an exclusive atomic rename and syncs the directory afterward.
Configuration, job and policy are rechecked before publication and before a
successful receipt. It opens no broker database, listener or worker service and
does not qualify or execute the job.

An identical retry verifies the installed file. Conflicting contents or unsafe
metadata fail without replacement. A failed call after rename may still have
installed complete bytes; retry the same request to resolve that outcome. A
process exit before rename can leave a private `.cortex-publish-*.tmp` file.
Those names cannot be read as inputs. Remove orphan staging files only after
confirming that their provisioning process has stopped; this command does not
delete another writer's files or run a background cleanup service.

Success writes a JSON receipt with the job ID, job digest, reference binding,
content digest and byte count. Failure exits nonzero with a generic diagnostic;
source bytes, policy bodies and internal errors are not printed. Cancellation
and preparation have a 30-second bound between local filesystem operations.
As with input reads, use responsive local storage because a stalled kernel call
cannot be forcibly cancelled.

Linux fixtures execute the built command, verify identical retries and preserve
installed data after a conflicting request. Publication tests cover concurrent
writers, policy changes, unsafe metadata and process exit before and after commit.
Host installation and live systemd/runsc qualification remain separate work.


## Protected test evaluation

Use `verification.NewSandboxTestVerifier` to connect a registered evaluation
manifest to separate candidate and grader jobs. Register the verifier with
`verification.RegisterTests` and supply it to your verification host through
that existing typed registration.

You provide the sealed manifest registry, reserved job plans, runner, evidence
repository, current binding authorizer, clock and external supervisor. Set
`MaxConcurrent` between 1 and 64 and `PollInterval` between 10 milliseconds and
one second. Overflow returns `cortex.ErrVerificationUnavailable` immediately.
Every evaluation has a deadline of at most five minutes, shortened by the
request and caller deadlines. Your ports must honor cancellation.

Pin the broker name and Ed25519 public key with
`grading.NewAuthenticity(name, publicKey)`. This adapter reuses the broker's
receipt and result signature verification. Configure the identity independently
of responses; accepting a key from a workload would let that workload sign its
own evidence.

Reserve job IDs, keys and output IDs durably before you register a plan. The
candidate gets only `/inputs/candidate`. The grader template gets `/inputs/tests`,
`/inputs/fixtures` and `/inputs/result-schema`. After authenticating the completed
candidate job and digest-checking its captured output, the verifier adds that
output at `/inputs/candidate-export` and seals the final grader job. Both plans
require an empty environment, one declared output and the sandbox's deny-all
network profile. Mounted inputs follow the runner's read-only input contract.

The supervisor receives job identities and captured references. It receives no
stdout, stderr, production credentials or effect tools through this interface.
Its assessment must match the manifest, supervisor revision and both job digests;
unknown, duplicate or malformed checks are rejected, while a missing required
check produces an inconclusive result. Tests that import candidate code need
independent corroboration because the candidate can manipulate their process.

Your supervisor must retain authenticated observations and reconcile retries
against the same manifest and job digests. The verifier does not implement that
supervisor or attach its new artifacts to the verification report's captured
binding. You also own total evaluation cost accounting and durable phase recovery.
An uncertain submission keeps its reserved identity and returns `ErrCallUncertain`.
The verifier attempts bounded cancellation; failed cancellation remains uncertain.
Do not allocate replacement identities to hide that outcome.

Fixture tests cover tampered exports, forged stdout, revoked authority, signature
failure, missing checks, timeout and bounded admission. They do not qualify
runsc isolation or an installed supervisor. Live Linux qualification and installed export wiring remain required before
deployment. The separate local export command is described below.


## Approved local artifact export

Use `grading.NewFileExporter` to construct the separate
`verification.ArtifactExporter` host command on Linux or macOS. It accepts
`FileExportConfig` with your root command ports, an owner UID, one journal
directory and registered `FileExportDestination` values. Leave `Command.Sink`
unset; the factory installs the protected file adapter.

Each destination maps a logical name and revision to a canonical absolute
directory. Provision the journal and destination directories with mode 0700,
owned by the configured UID, under protected ancestors. They must be distinct
and cannot contain one another. Construction verifies and remembers directory
identities. It opens no listener and creates no directories or files.

Your command configuration supplies committed-result storage, an authorized
evidence repository, current export authorization, an export fence, a content
screener, a clock and bounded admission. The authorizer must authenticate the
request's actor and check the exact approval, artifact and destination. The fence
covers relevant grant, approval, policy, evidence-deletion and run-revision
writers through delivery. Configure a binary-capable screener for binary media.
There is no permissive default or conversion through a text scanner.

Reserve the export operation ID durably before calling `Export`. The local
adapter first publishes `<operation-id>.json` in the private journal, binding the
full request. It then publishes the artifact as `<operation-id>` in the selected
directory. Both files are mode 0400, synced and installed with exclusive rename.
The shared host filesystem helper rejects traversal, symlinks, hard links and
special files. Archives remain opaque bytes.

An identical retry verifies the reservation and artifact. A changed approval,
actor, artifact or destination conflicts with the original reservation. The
journal is shared across all registered destinations, so the same operation
cannot move to a second destination. Keep that journal stable across restarts
and destination changes. Retain operation history and provision filesystem quotas;
the command bounds individual artifacts and concurrency, not total retained disk.

An interrupted attempt can leave a reservation without an artifact. A partial
file is never the destination. Retry the same request while its approval and
verification remain current; failed calls after publication remain uncertain.
For expired or revoked requests, use an authorized host audit process to inspect
retained state before issuing any new operation. A crash may leave a private
`.cortex-publish-*.tmp` file. Remove it only after confirming its publisher has
stopped, following the input-provisioning cleanup rules above.

Fixture tests cover the composed command, denied approval, blocked screening,
interruption between reservation and delivery, restart, conflicting retries,
concurrent publication, directory replacement and unsafe file types. These tests
do not qualify a production approval service, screener, external deployment
sink or installed API/dashboard export flow.
