//go:build linux || darwin

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/oci"
	"github.com/xraph/cortex/sandbox"
)

func TestContainedDriverRejectsPayloadOutsideWorkerService(t *testing.T) {
	j := job(t)
	d := fixtureRunsc(t, j)
	d.serviceID = j.ID
	d.checkMembership = func(context.Context, id.ID) error { return nil }
	d.invoke = func(*exec.Cmd) error { t.Fatal("escaped payload reached runsc"); return nil }
	if err := d.Prepare(t.Context(), j, io.Discard, io.Discard); err == nil {
		t.Fatal("legacy cgroup accepted for contained worker")
	}
}

func TestContainedDriverFencesCommandsAfterWorkerMoves(t *testing.T) {
	j := job(t)
	d := fixtureRunsc(t, j)
	spec, err := oci.ContainedSpec(j, d.cfg.Rootfs, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(d.cfg.Bundle, "config.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	d.serviceID = j.ID
	moved := false
	failure := errors.New("worker left service cgroup")
	d.checkMembership = func(_ context.Context, key id.ID) error {
		if key != j.ID {
			t.Fatal("wrong membership target")
		}
		if moved {
			return failure
		}
		return nil
	}
	var verbs []string
	d.invoke = func(cmd *exec.Cmd) error { verbs = append(verbs, cmd.Args[3]); moved = true; return nil }
	result, err := Run(t.Context(), d, j, time.Now().Add(time.Second))
	if !errors.Is(err, failure) || !errors.Is(err, ErrCleanup) || result.State != "" || strings.Join(verbs, ",") != "create" {
		t.Fatalf("moved worker still controlled runtime: %+v %v %v", result, verbs, err)
	}
}

func fixtureRunsc(t *testing.T, j sandbox.Job) *runscDriver {
	t.Helper()
	bundle := t.TempDir()
	rootfs := filepath.Join(bundle, "rootfs")
	spec, err := oci.Spec(j, rootfs, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(bundle, "config.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	return &runscDriver{cfg: RunscConfig{Binary: "/approved/runsc", StateRoot: "/private/runtime", Bundle: bundle, Rootfs: rootfs, JobDigest: j.Digest}}
}

func TestRunscControlSeparatesGuestStreamsFromWaitResponse(t *testing.T) {
	j := job(t)
	var calls []string
	d := fixtureRunsc(t, j)
	d.invoke = func(cmd *exec.Cmd) error {
		if cmd.Path != "/approved/runsc" || !reflect.DeepEqual(cmd.Env, []string{"PATH=/usr/bin:/bin", "LANG=C"}) || cmd.WaitDelay != time.Second {
			t.Fatalf("unsafe command: %+v", cmd)
		}
		if !reflect.DeepEqual(cmd.Args[1:3], []string{"--root=/private/runtime", "--network=none"}) {
			t.Fatalf("runtime flags: %v", cmd.Args)
		}
		verb := cmd.Args[3]
		calls = append(calls, verb)
		switch verb {
		case "create":
			if !reflect.DeepEqual(cmd.Args[4:], []string{"--bundle", d.cfg.Bundle, j.ID.String()}) {
				t.Fatalf("create arguments %v", cmd.Args)
			}
			if _, ok := cmd.Stdout.(*os.File); !ok {
				t.Fatal("create output does not survive command exit")
			}
			_, err := io.WriteString(cmd.Stdout, "hello")
			return err
		case "wait":
			_, err := fmt.Fprintf(cmd.Stdout, `{"id":%q,"exitStatus":7}`, j.ID.String())
			return err
		case "delete":
			if !reflect.DeepEqual(cmd.Args[4:], []string{"--force", j.ID.String()}) {
				t.Fatalf("cleanup arguments %v", cmd.Args)
			}
		}
		return nil
	}
	r, err := Run(t.Context(), d, j, time.Now().Add(time.Second))
	if err != nil || r.State != sandbox.StateExited || r.ExitCode != 7 || string(r.Stdout) != "hello" {
		t.Fatalf("result=%+v %v", r, err)
	}
	if strings.Join(calls, ",") != "create,start,wait,delete" {
		t.Fatalf("commands %v", calls)
	}
}

func TestRunscRejectsMalformedOrMismatchedWaitResponses(t *testing.T) {
	j := job(t)
	for _, body := range []string{`{}`, `{"id":"other","exitStatus":0}`, fmt.Sprintf(`{"id":%q}`, j.ID.String()), fmt.Sprintf(`{"id":%q,"exitStatus":256}`, j.ID.String()), fmt.Sprintf(`{"id":%q,"exitStatus":0} {}`, j.ID.String())} {
		t.Run(body, func(t *testing.T) {
			d := &runscDriver{cfg: RunscConfig{Binary: "/approved/runsc", StateRoot: "/private/runtime"}, jobID: j.ID, invoke: func(cmd *exec.Cmd) error { _, err := io.WriteString(cmd.Stdout, body); return err }}
			if _, err := d.Wait(t.Context()); err == nil {
				t.Fatal("invalid runtime response accepted")
			}
		})
	}
}

func TestRunscRejectsWrongJobBeforeExecuting(t *testing.T) {
	d := &runscDriver{cfg: RunscConfig{JobDigest: strings.Repeat("b", 64)}, invoke: func(*exec.Cmd) error { t.Fatal("unbound job reached executable"); return nil }}
	if err := d.Prepare(t.Context(), job(t), io.Discard, io.Discard); err == nil {
		t.Fatal("job binding ignored")
	}
	if err := d.Cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRunscWaitOutputIsBounded(t *testing.T) {
	j := job(t)
	d := &runscDriver{cfg: RunscConfig{Binary: "/approved/runsc", StateRoot: "/private/runtime"}, jobID: j.ID, invoke: func(cmd *exec.Cmd) error {
		_, err := io.WriteString(cmd.Stdout, strings.Repeat("a", 128<<10))
		return err
	}}
	if _, err := d.Wait(t.Context()); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("unbounded control response: %v", err)
	}
}

func TestRunscDeleteFailureCannotReturnCollectibleResult(t *testing.T) {
	j := job(t)
	failure := errors.New("runtime delete failed")
	d := fixtureRunsc(t, j)
	d.invoke = func(cmd *exec.Cmd) error {
		switch cmd.Args[3] {
		case "wait":
			_, err := fmt.Fprintf(cmd.Stdout, `{"id":%q,"exitStatus":0}`, j.ID.String())
			return err
		case "delete":
			return failure
		}
		return nil
	}
	r, err := Run(context.Background(), d, j, time.Now().Add(time.Second))
	if r.State != "" || !errors.Is(err, ErrCleanup) || !errors.Is(err, failure) {
		t.Fatalf("unsafe terminal result %+v %v", r, err)
	}
}

func TestRunscRejectsBundleForDifferentJobBeforeExecuting(t *testing.T) {
	j := job(t)
	d := fixtureRunsc(t, j)
	body, err := os.ReadFile(filepath.Join(d.cfg.Bundle, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.ReplaceAll(string(body), "/bin/check", "/bin/other"))
	if err = os.WriteFile(filepath.Join(d.cfg.Bundle, "config.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	d.invoke = func(*exec.Cmd) error { t.Fatal("substituted OCI bundle reached executable"); return nil }
	if err = d.Prepare(t.Context(), j, io.Discard, io.Discard); err == nil {
		t.Fatal("OCI bundle binding ignored")
	}
}

func TestRunscAmbiguousCreateNeedsExternalReaping(t *testing.T) {
	j := job(t)
	d := fixtureRunsc(t, j)
	failure := errors.New("create acknowledgement lost")
	d.invoke = func(cmd *exec.Cmd) error {
		if cmd.Args[3] == "create" {
			return failure
		}
		return nil // Forced delete also succeeds when runsc has no state record.
	}
	r, err := Run(t.Context(), d, j, time.Now().Add(time.Second))
	if !errors.Is(err, ErrCleanup) || r.State != "" {
		t.Fatalf("ambiguous create became collectible: %+v %v", r, err)
	}
}

func workspaceRunscFixture(t *testing.T, j sandbox.Job) *runscDriver {
	t.Helper()
	d := fixtureRunsc(t, j)
	d.serviceID = j.ID
	d.checkMembership = func(context.Context, id.ID) error { return nil }
	d.scratch = "/private/scratch"
	d.checkStorage = func(ctx context.Context) error { return ctx.Err() }
	d.checkExecution = func(ctx context.Context) error { return ctx.Err() }
	spec, err := oci.WorkspaceSpec(j, d.cfg.Rootfs, nil, d.scratch)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(d.cfg.Bundle, "config.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWorkspaceRunscRevocationStillPermitsCleanup(t *testing.T) {
	j := job(t)
	d := workspaceRunscFixture(t, j)
	revoked := errors.New("current grant revoked")
	active := true
	d.checkExecution = func(context.Context) error {
		if !active {
			return revoked
		}
		return nil
	}
	var verbs []string
	d.invoke = func(cmd *exec.Cmd) error { verbs = append(verbs, cmd.Args[3]); active = false; return nil }
	result, err := Run(t.Context(), d, j, time.Now().Add(time.Second))
	if !errors.Is(err, revoked) || errors.Is(err, ErrCleanup) || result.State == "" || strings.Join(verbs, ",") != "create,delete" {
		t.Fatal("revocation failed to fence execution or blocked cleanup", result, err, verbs)
	}
}

func TestWorkspaceRunscRefusesChangedStorageIncludingCleanup(t *testing.T) {
	j := job(t)
	d := workspaceRunscFixture(t, j)
	changed := errors.New("workspace replaced")
	valid := true
	d.checkStorage = func(context.Context) error {
		if !valid {
			return changed
		}
		return nil
	}
	var verbs []string
	d.invoke = func(cmd *exec.Cmd) error { verbs = append(verbs, cmd.Args[3]); valid = false; return nil }
	result, err := Run(t.Context(), d, j, time.Now().Add(time.Second))
	if !errors.Is(err, changed) || !errors.Is(err, ErrCleanup) || result.State != "" || strings.Join(verbs, ",") != "create" {
		t.Fatal("changed storage reached runtime or produced a result", result, err, verbs)
	}
}

func TestWorkspaceRunscRejectsMissingChecksAndChangedScratch(t *testing.T) {
	for _, mode := range []string{"storage", "execution", "source", "containment"} {
		t.Run(mode, func(t *testing.T) {
			j := job(t)
			d := workspaceRunscFixture(t, j)
			switch mode {
			case "storage":
				d.checkStorage = nil
			case "execution":
				d.checkExecution = nil
			case "source":
				d.scratch = "/another/scratch"
			case "containment":
				d.serviceID = id.ID{}
			}
			d.invoke = func(*exec.Cmd) error { t.Fatal("unbound workspace reached runtime"); return nil }
			if err := d.Prepare(t.Context(), j, io.Discard, io.Discard); err == nil {
				t.Fatal("unbound workspace accepted")
			}
		})
	}
}

func TestRunscConstructorFailureReturnsNoDriver(t *testing.T) {
	if driver, err := NewRunsc(t.Context(), RunscConfig{}); err == nil || driver != nil {
		t.Fatal("invalid constructor returned a driver", err)
	}
}
