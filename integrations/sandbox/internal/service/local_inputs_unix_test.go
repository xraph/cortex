//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/sandbox"
)

func installInput(t *testing.T, root string, artifact evidence.Artifact) string {
	t.Helper()
	binding, err := artifact.Ref.BindingDigest()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, binding)
	if err = os.WriteFile(name, artifact.Data, 0o400); err != nil {
		t.Fatal(err)
	}
	return name
}

func localInputsFixture(t *testing.T) (*LocalInputs, sandbox.Job, evidence.Artifact, string) {
	t.Helper()
	_, controller, claim, _, input, _, _ := workerEvidenceFixture(t)
	record, err := controller.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	name := installInput(t, root, input)
	policy, _ := evidencePolicyFixture(t, record.Receipt.Job)
	source, err := newLocalInputs(root, 1<<20, 1, policy.AuthorizeInput, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	return source, record.Receipt.Job, input, name
}

func TestLocalInputsSnapshotCapacityAndEmptyContent(t *testing.T) {
	source, job, input, name := localInputsFixture(t)
	ctx := evidenceContext(t.Context(), job)
	reader, err := source.Open(ctx, input.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err = os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, []byte("changed"), 0o400); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(body, input.Data) {
		t.Fatal("snapshot changed", err)
	}
	waiting, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if other, openErr := source.Open(waiting, input.Ref); !errors.Is(openErr, context.DeadlineExceeded) || other != nil {
		t.Fatal("unbounded readers", openErr)
	}
	reader.Close()
	reader.Close()
	if _, err = reader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed reader", err)
	}
	if other, openErr := source.Open(ctx, input.Ref); openErr == nil || other != nil {
		t.Fatal("replaced content accepted", openErr)
	}
	if err = os.Remove(name); err != nil {
		t.Fatal(err)
	}
	installInput(t, source.root, input)
	again, err := source.Open(ctx, input.Ref)
	if err != nil {
		t.Fatal("failed read retained slot", err)
	}
	again.Close()

	input, err = evidence.Seal(evidence.Artifact{Ref: input.Ref})
	if err != nil {
		t.Fatal(err)
	}
	job.Inputs[0].Ref = input.Ref
	job, err = sandbox.Seal(job)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := evidencePolicyFixture(t, job)
	source.policy = policy.AuthorizeInput
	installInput(t, source.root, input)
	empty, err := source.Open(evidenceContext(t.Context(), job), input.Ref)
	if err != nil {
		t.Fatal("empty input", err)
	}
	defer empty.Close()
	body, err = io.ReadAll(empty)
	if err != nil || len(body) != 0 {
		t.Fatal("empty bytes", err)
	}
}

func TestLocalInputsRejectUnsafeFilesAndReplacedDirectory(t *testing.T) {
	for _, mode := range []string{"missing", "symlink", "hardlink", "fifo", "directory", "writable", "owner", "root-mode", "root-replaced", "parent-alias", "digest", "length", "too-large"} {
		t.Run(mode, func(t *testing.T) {
			source, job, input, name := localInputsFixture(t)
			var err error
			switch mode {
			case "missing", "symlink", "fifo", "directory":
				if err = os.Remove(name); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "symlink":
					err = os.Symlink("/etc/passwd", name)
				case "fifo":
					err = unix.Mkfifo(name, 0o400)
				case "directory":
					err = os.Mkdir(name, 0o700)
				}
			case "hardlink":
				err = os.Link(name, name+"-alias")
			case "writable":
				err = os.Chmod(name, 0o600)
			case "owner":
				source.owner++
			case "root-mode":
				err = os.Chmod(source.root, 0o755)
			case "root-replaced":
				if err = os.Rename(source.root, source.root+"-old"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(source.root + "-old") })
				if err = os.Mkdir(source.root, 0o700); err == nil {
					installInput(t, source.root, input)
				}
			case "parent-alias":
				alias := source.root + "-alias"
				err = os.Symlink(source.root, alias)
				t.Cleanup(func() { os.Remove(alias) })
				source.root = alias
			case "digest":
				if err = os.Remove(name); err == nil {
					err = os.WriteFile(name, bytes.Repeat([]byte("x"), len(input.Data)), 0o400)
				}
			case "length":
				if err = os.Remove(name); err == nil {
					err = os.WriteFile(name, []byte("short"), 0o400)
				}
			case "too-large":
				source.maximum = 1
			}
			if err != nil {
				t.Fatal(err)
			}
			if reader, openErr := source.Open(evidenceContext(t.Context(), job), input.Ref); openErr == nil || reader != nil {
				t.Fatal("unsafe input returned", openErr)
			}
		})
	}
}

func TestLocalInputsRequireCurrentDeclaredJobAuthority(t *testing.T) {
	for _, mode := range []string{"ambient", "scope", "principal", "cleanup", "altered-ref", "invalid-job", "revoked", "revoked-after-read", "cancelled", "cancelled-after-read", "expired", "future", "expired-after-read"} {
		t.Run(mode, func(t *testing.T) {
			source, job, input, _ := localInputsFixture(t)
			ctx := evidenceContext(t.Context(), job)
			switch mode {
			case "ambient":
				ctx = cortex.WithScope(t.Context(), job.Scope)
			case "scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{})
			case "principal":
				ctx = cortex.WithPrincipal(ctx, "other")
			case "cleanup":
				ctx = cleanupEvidenceContext(t.Context(), job, "service:cleanup")
			case "altered-ref":
				input.Ref.Version = "substituted"
			case "invalid-job":
				job.Principal = "tampered"
				ctx = evidenceContext(ctx, job)
			case "revoked":
				source.policy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return evidence.ErrDenied }
			case "revoked-after-read":
				calls := 0
				source.policy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error {
					calls++
					if calls > 2 {
						return evidence.ErrDenied
					}
					return nil
				}
			case "cancelled", "cancelled-after-read":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				if mode == "cancelled" {
					cancel()
				} else {
					calls := 0
					source.policy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error {
						calls++
						if calls == 3 {
							cancel()
						}
						return nil
					}
				}
			case "expired-after-read":
				calls := 0
				source.policy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error {
					calls++
					if calls == 3 {
						source.now = func() time.Time { return input.Ref.RetainUntil }
					}
					return nil
				}
			case "expired":
				source.now = func() time.Time { return input.Ref.RetainUntil }
			case "future":
				source.now = func() time.Time { return input.Ref.CreatedAt.Add(-time.Second) }
			}
			if reader, err := source.Open(ctx, input.Ref); err == nil || reader != nil {
				t.Fatal("authority bypass", err)
			}
		})
	}
}

func TestLocalInputsIntegrateWithCurrentWorkerPolicy(t *testing.T) {
	access, controller, claim, peer, input, _, _ := workerEvidenceFixture(t)
	record, err := controller.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	policy, doc := evidencePolicyFixture(t, record.Receipt.Job)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	installInput(t, root, input)
	source, err := newLocalInputs(root, 1<<20, 2, policy.AuthorizeInput, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	access.inputs, access.inputPolicy = source, policy.AuthorizeInput
	var body bytes.Buffer
	if err = access.read(t.Context(), peer, claim, input.Ref, false, &body); err != nil || !bytes.Equal(body.Bytes(), input.Data) {
		t.Fatal("worker input transfer", err)
	}
	doc.Subjects = nil
	writeInstalledPolicy(t, policy.path, doc)
	body.Reset()
	if err = access.read(t.Context(), peer, claim, input.Ref, false, &body); !errors.Is(err, evidence.ErrDenied) || body.Len() != 0 {
		t.Fatal("revoked source readable", err)
	}
}

func TestLocalInputsDetachPolicyArgumentsAndRejectInvalidSetup(t *testing.T) {
	source, job, input, _ := localInputsFixture(t)
	source.policy = func(_ context.Context, copyJob sandbox.Job, _ evidence.Action, copyRef evidence.Ref) error {
		copyJob.Inputs[0].Ref.Scope.Levels[0].Value = "changed"
		copyRef.Scope.Levels[0].Value = "changed"
		return nil
	}
	for range 2 {
		reader, err := source.Open(evidenceContext(t.Context(), job), input.Ref)
		if err != nil {
			t.Fatal("policy mutated authority", err)
		}
		reader.Close()
	}
	for _, limits := range [][2]int64{{0, 1}, {1<<20 + 1, 1}, {1, 0}, {1, 17}} {
		if other, err := newLocalInputs(source.root, limits[0], int(limits[1]), source.policy, source.owner); err == nil || other != nil {
			t.Fatal("invalid limits accepted", limits)
		}
	}
	if other, err := newLocalInputs(source.root, 1, 1, nil, source.owner); err == nil || other != nil {
		t.Fatal("missing policy accepted")
	}
	if other, err := newLocalInputs(source.root+"/", 1, 1, source.policy, source.owner); err == nil || other != nil {
		t.Fatal("alias accepted")
	}
	for _, empty := range []*LocalInputs{nil, {}} {
		if reader, err := empty.Open(t.Context(), input.Ref); err == nil || reader != nil {
			t.Fatal("empty source accepted")
		}
	}
}

func TestLocalInputsRequireProvisioningForExactReference(t *testing.T) {
	source, job, input, _ := localInputsFixture(t)
	// Identical content under another version needs its own installed binding.
	input.Ref.Version = "source:2"
	job.Inputs[0].Ref = input.Ref
	var err error
	job, err = sandbox.Seal(job)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := evidencePolicyFixture(t, job)
	source.policy = policy.AuthorizeInput
	ctx := evidenceContext(t.Context(), job)
	if reader, openErr := source.Open(ctx, input.Ref); openErr == nil || reader != nil {
		t.Fatal("reused differently bound content", openErr)
	}
	installInput(t, source.root, input)
	reader, err := source.Open(ctx, input.Ref)
	if err != nil {
		t.Fatal("new binding unavailable", err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(body, input.Data) {
		t.Fatal("new binding content", err)
	}
}
