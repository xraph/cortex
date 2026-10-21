//go:build linux || darwin

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/deployment"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/oci"
	"github.com/xraph/cortex/sandbox"
)

// RunscConfig comes only from the trusted staging service. Bundle must already
// contain the exact deny-all OCI spec for JobDigest, a verified image and verified
// evidence inputs. The bundle and state root are private host-owned directories.
// No path or deployment manifest may come from a workload's arguments.
type RunscConfig struct {
	Binary           string
	StateRoot        string
	Bundle           string
	Rootfs           string
	Inputs           map[string]string
	JobDigest        string
	Manifest         deployment.Manifest
	DeploymentDigest string
}

// NewRunsc checks the Linux/root boundary and deployment pin before constructing
// a driver. The caller still owns bundle integrity, qualification admission,
// durable job claims and external process supervision. No program is executed
// here. This driver must never be used as a host-execution fallback.
func NewRunsc(ctx context.Context, cfg RunscConfig) (Driver, error) {
	return newRunsc(ctx, cfg, id.ID{})
}

// NewContainedRunsc requires the worker's exact job service membership and puts
// the payload below that service. It rechecks membership before every control
// command. The service manager's start fence, kill policy, limits and live
// qualification still belong to the external adapter. Use NewRunsc only for
// independently supervised low-level probes or another proven containment host.
func NewContainedRunsc(ctx context.Context, cfg RunscConfig, key id.ID) (Driver, error) {
	if _, err := containment.ForJob(key); err != nil {
		return nil, err
	}
	if err := containment.VerifyCurrent(ctx, key); err != nil {
		return nil, err
	}
	return newRunsc(ctx, cfg, key)
}

func newRunsc(ctx context.Context, cfg RunscConfig, serviceID id.ID) (Driver, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, fmt.Errorf("cortex: sandbox supervision requires a root-run Linux worker")
	}
	if !evidence.ValidDigest(cfg.JobDigest) || !evidence.ValidDigest(cfg.DeploymentDigest) || filepath.Base(cfg.Binary) != "runsc" {
		return nil, fmt.Errorf("cortex: incomplete runsc supervision configuration")
	}
	for _, name := range []string{cfg.Binary, cfg.StateRoot, cfg.Bundle} {
		if !filepath.IsAbs(name) || filepath.Clean(name) != name {
			return nil, fmt.Errorf("cortex: runsc supervision requires absolute clean paths")
		}
	}
	digest, err := deployment.Verify(ctx, filepath.Dir(cfg.Binary), cfg.Manifest, 0)
	if err != nil {
		return nil, err
	}
	if digest != cfg.DeploymentDigest {
		return nil, fmt.Errorf("cortex: runsc deployment pin mismatch")
	}
	for _, name := range []string{cfg.StateRoot, cfg.Bundle} {
		directory, openErr := hostfs.OpenDirectory(name, 0)
		if openErr != nil {
			return nil, openErr
		}
		info, statErr := directory.Stat()
		if operationErr := errors.Join(statErr, directory.Close()); operationErr != nil {
			return nil, operationErr
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("cortex: runsc state and bundle directories must be private")
		}
	}
	cfg.Inputs = maps.Clone(cfg.Inputs)
	return &runscDriver{cfg: cfg, serviceID: serviceID, checkMembership: containment.VerifyCurrent, invoke: func(cmd *exec.Cmd) error { return cmd.Run() }}, nil
}

type runscDriver struct {
	cfg                RunscConfig
	jobID              id.ID
	invoke             func(*exec.Cmd) error
	readers            []*os.File
	joined             chan struct{}
	pumpFailures       chan error
	prepared           bool
	createAttempted    bool
	createAcknowledged bool
	serviceID          id.ID
	checkMembership    func(context.Context, id.ID) error
}

func (d *runscDriver) Prepare(ctx context.Context, job sandbox.Job, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := job.Validate(); err != nil {
		return err
	}
	if d.prepared || job.Digest != d.cfg.JobDigest {
		return fmt.Errorf("cortex: runsc job binding mismatch or repeated prepare")
	}
	if !d.serviceID.IsNil() && job.ID != d.serviceID {
		return fmt.Errorf("cortex: runsc service identity mismatch")
	}
	if err := d.verifyMembership(ctx); err != nil {
		return err
	}
	if err := d.verifyBundle(job); err != nil {
		return err
	}
	d.prepared = true
	d.jobID = job.ID
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		outRead.Close()
		outWrite.Close()
		return err
	}
	d.readers = []*os.File{outRead, errRead}
	d.pumpFailures = make(chan error, 2)
	d.joined = make(chan struct{})
	var copies sync.WaitGroup
	for i, writer := range []io.Writer{stdout, stderr} {
		reader := d.readers[i]
		copies.Go(func() {
			_, copyErr := io.Copy(writer, reader)
			if copyErr != nil && !errors.Is(copyErr, ErrOutputLimit) {
				d.pumpFailures <- copyErr
			}
		})
	}
	go func() { copies.Wait(); close(d.joined) }()
	control, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := d.command(control, "create", "--bundle", d.cfg.Bundle, d.jobID.String())
	// *os.File lets runsc's sandbox inherit the descriptors after create exits.
	// exec's implicit io.Writer pipes would close when the create command exits.
	cmd.Stdout = outWrite
	cmd.Stderr = errWrite
	d.createAttempted = true
	err = d.executeCommand(control, cmd)
	d.createAcknowledged = err == nil
	return errors.Join(err, outWrite.Close(), errWrite.Close())
}

func (d *runscDriver) verifyBundle(job sandbox.Job) error {
	expected, err := oci.Spec(job, d.cfg.Rootfs, d.cfg.Inputs)
	if !d.serviceID.IsNil() {
		expected, err = oci.ContainedSpec(job, d.cfg.Rootfs, d.cfg.Inputs)
	}
	if err != nil {
		return err
	}
	body, err := hostfs.ReadRegular(d.cfg.Bundle, "config.json", 1<<20)
	if err != nil {
		return err
	}
	var actual specs.Spec
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&actual); decodeErr != nil {
		return decodeErr
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("cortex: trailing sandbox OCI specification")
	}
	want, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	got, err := json.Marshal(actual)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, got) {
		return fmt.Errorf("cortex: sandbox OCI specification does not match sealed job")
	}
	return nil
}

func (d *runscDriver) verifyMembership(ctx context.Context) error {
	if d.serviceID.IsNil() {
		return ctx.Err()
	}
	if d.checkMembership == nil {
		return fmt.Errorf("cortex: runsc containment checker missing")
	}
	return d.checkMembership(ctx, d.serviceID)
}

func (d *runscDriver) executeCommand(ctx context.Context, cmd *exec.Cmd) error {
	if err := d.verifyMembership(ctx); err != nil {
		return err
	}
	return d.invoke(cmd)
}

func (d *runscDriver) Start(ctx context.Context) error {
	if d.jobID.IsNil() {
		return fmt.Errorf("cortex: runsc job has not been prepared")
	}
	control, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := d.control(control, "start", d.jobID.String())
	return err
}

func (d *runscDriver) Wait(ctx context.Context) (int, error) {
	if d.jobID.IsNil() {
		return -1, fmt.Errorf("cortex: runsc job has not been prepared")
	}
	body, err := d.control(ctx, "wait", d.jobID.String())
	if err != nil {
		return -1, err
	}
	var result struct {
		ID         string `json:"id"`
		ExitStatus *int   `json:"exitStatus"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&result); decodeErr != nil {
		return -1, decodeErr
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return -1, fmt.Errorf("cortex: trailing runsc wait response")
	}
	if result.ID != d.jobID.String() || result.ExitStatus == nil || *result.ExitStatus < 0 || *result.ExitStatus > 255 {
		return -1, fmt.Errorf("cortex: invalid runsc wait response")
	}
	return *result.ExitStatus, nil
}

func (d *runscDriver) Cleanup(ctx context.Context) error {
	if d.jobID.IsNil() {
		return nil
	}
	_, err := d.control(ctx, "delete", "--force", d.jobID.String())
	// Forced delete succeeds for absent state. It cannot prove that a failed
	// create left no process outside that state record. The external service
	// manager must reap and attest the worker's containment before collection.
	if d.createAttempted && !d.createAcknowledged {
		err = errors.Join(err, fmt.Errorf("cortex: ambiguous runsc creation requires external reaping"))
	}
	if d.joined == nil {
		return err
	}
	if err == nil {
		select {
		case <-d.joined:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	for _, reader := range d.readers {
		err = errors.Join(err, reader.Close())
	}
	d.readers = nil
	<-d.joined
	close(d.pumpFailures)
	for pumpErr := range d.pumpFailures {
		err = errors.Join(err, pumpErr)
	}
	d.joined = nil
	return err
}

func (d *runscDriver) command(ctx context.Context, args ...string) *exec.Cmd {
	argv := append([]string{"--root=" + d.cfg.StateRoot, "--network=none"}, args...)
	cmd := exec.CommandContext(ctx, d.cfg.Binary, argv...) //nolint:gosec // Host-pinned runsc path and fixed control verbs; no shell or guest argv.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	cmd.Dir = d.cfg.StateRoot
	cmd.WaitDelay = time.Second
	return cmd
}

func (d *runscDriver) control(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	control, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	output := &capture{maximum: 64 << 10, cancel: cancel}
	cmd := d.command(control, args...)
	cmd.Stdout = &stream{capture: output}
	cmd.Stderr = &stream{capture: output, diagnostic: true}
	err := d.executeCommand(control, cmd)
	stdout, _, outputErr := output.finish()
	return stdout, errors.Join(err, outputErr, context.Cause(control))
}
