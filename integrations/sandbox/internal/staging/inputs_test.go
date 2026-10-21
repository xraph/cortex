//go:build linux || darwin

package staging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

type opener func(context.Context, evidence.Ref) (io.ReadCloser, error)

func (f opener) Open(ctx context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	return f(ctx, ref)
}

type policy func(context.Context, evidence.Action, evidence.Ref) error

func (f policy) AuthorizeEvidence(ctx context.Context, action evidence.Action, ref evidence.Ref) error {
	return f(ctx, action, ref)
}

func fixture(t *testing.T) (context.Context, Config, sandbox.Job) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, MediaType: "text/plain", Version: "1", Producer: "capture", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("verified input")})
	if err != nil {
		t.Fatal(err)
	}
	j, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "staging", Scope: scope, Principal: "alice", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/bin/check"}, Inputs: []sandbox.Input{{Path: "/inputs/nested/data", Ref: a.Ref}}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 500, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 8 << 20, WallTime: time.Minute, OutputBytes: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Root: root, OwnerUID: uint32(os.Geteuid()), PerInputBytes: 1024, TotalBytes: 2048, Source: opener(func(context.Context, evidence.Ref) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(a.Data)), nil
	}), Policy: policy(func(context.Context, evidence.Action, evidence.Ref) error { return nil }), AuthorizeJob: func(context.Context, sandbox.Job) error { return nil }}
	return cortex.WithScope(t.Context(), scope), cfg, j
}

func TestStageVerifiedInputsAndRetry(t *testing.T) {
	ctx, cfg, j := fixture(t)
	opens := 0
	original := cfg.Source
	cfg.Source = opener(func(ctx context.Context, ref evidence.Ref) (io.ReadCloser, error) {
		opens++
		return original.Open(ctx, ref)
	})
	first, err := Stage(ctx, cfg, j)
	if err != nil {
		t.Fatal(err)
	}
	name := first.Paths[j.Inputs[0].Path]
	data, err := os.ReadFile(name)
	if err != nil || string(data) != "verified input" {
		t.Fatalf("staged data=%q %v", data, err)
	}
	info, err := os.Stat(name)
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("staged permissions: %v %v", info, err)
	}
	if first.JobDigest != j.Digest || filepath.Dir(name) != first.Directory {
		t.Fatalf("unbound input set: %+v", first)
	}
	first.Paths[j.Inputs[0].Path] = "mutated"
	retried, err := Stage(ctx, cfg, j)
	if err != nil || retried.Paths[j.Inputs[0].Path] != name || opens != 2 {
		t.Fatalf("retry=%+v opens=%d err=%v", retried, opens, err)
	}
	cfg.Policy = policy(func(context.Context, evidence.Action, evidence.Ref) error { return evidence.ErrDenied })
	if _, err = Stage(ctx, cfg, j); !errors.Is(err, evidence.ErrDenied) {
		t.Fatalf("revoked retry: %v", err)
	}
}

func TestStageRefusesWrongScopeAndOversizeBeforeReading(t *testing.T) {
	ctx, cfg, j := fixture(t)
	cfg.Source = opener(func(context.Context, evidence.Ref) (io.ReadCloser, error) {
		t.Fatal("invalid admission opened evidence")
		return nil, nil
	})
	wrong := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "other"}}})
	if _, err := Stage(wrong, cfg, j); !errors.Is(err, evidence.ErrDenied) {
		t.Fatalf("wrong scope: %v", err)
	}
	cfg.TotalBytes = 1
	if _, err := Stage(ctx, cfg, j); !errors.Is(err, evidence.ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	cfg.TotalBytes = j.Inputs[0].Ref.Bytes + 1
	j.Inputs = append(j.Inputs, sandbox.Input{Path: "/inputs/second", Ref: j.Inputs[0].Ref})
	var sealErr error
	j, sealErr = sandbox.Seal(j)
	if sealErr != nil {
		t.Fatal(sealErr)
	}
	if _, err := Stage(ctx, cfg, j); !errors.Is(err, evidence.ErrTooLarge) {
		t.Fatalf("aggregate input bound: %v", err)
	}
	entries, err := os.ReadDir(cfg.Root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid admission changed root: %v %v", entries, err)
	}
}

func TestStagingRequiresHostPrincipalAuthorization(t *testing.T) {
	ctx, cfg, j := fixture(t)
	cfg.AuthorizeJob = func(ctx context.Context, job sandbox.Job) error {
		actor, ok := cortex.PrincipalFromContext(ctx).(string)
		if !ok || actor != job.Principal {
			return evidence.ErrDenied
		}
		return nil
	}
	if _, err := Stage(ctx, cfg, j); !errors.Is(err, evidence.ErrDenied) {
		t.Fatalf("missing principal accepted: %v", err)
	}
	if _, err := Stage(cortex.WithPrincipal(ctx, "alice"), cfg, j); err != nil {
		t.Fatal(err)
	}
}

func TestRetryCannotReuseDeletedSourceEvidence(t *testing.T) {
	ctx, cfg, j := fixture(t)
	if _, err := Stage(ctx, cfg, j); err != nil {
		t.Fatal(err)
	}
	cfg.Source = opener(func(context.Context, evidence.Ref) (io.ReadCloser, error) { return nil, evidence.ErrDeleted })
	if set, err := Stage(ctx, cfg, j); !errors.Is(err, evidence.ErrDeleted) || set != nil {
		t.Fatalf("deleted evidence reused: %+v %v", set, err)
	}
}

func TestStageDigestMismatchAndRevocationRemoveOnlyOwnFiles(t *testing.T) {
	for _, mode := range []string{"digest", "oversized stream", "revoked after read", "close failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cfg, j := fixture(t)
			closed := false
			cfg.Source = opener(func(context.Context, evidence.Ref) (io.ReadCloser, error) {
				body := []byte("verified input")
				if mode == "digest" {
					body = []byte("modified input")
				}
				if mode == "oversized stream" {
					body = append(body, '!')
				}
				return &closingReader{Reader: bytes.NewReader(body), close: func() error {
					closed = true
					if mode == "close failure" {
						return io.ErrUnexpectedEOF
					}
					return nil
				}}, nil
			})
			cfg.Policy = policy(func(context.Context, evidence.Action, evidence.Ref) error {
				if mode == "revoked after read" && closed {
					return evidence.ErrDenied
				}
				return nil
			})
			if set, err := Stage(ctx, cfg, j); err == nil || set != nil {
				t.Fatalf("unsafe staging succeeded: %+v %v", set, err)
			}
			entries, err := os.ReadDir(cfg.Root)
			if err != nil || len(entries) != 0 || !closed {
				t.Fatalf("failed staging leaked: %v closed=%v %v", entries, closed, err)
			}
		})
	}
}

type closingReader struct {
	io.Reader
	close func() error
}

func (r *closingReader) Close() error { return r.close() }

func TestStageRejectsTamperingAndConflictingIdentity(t *testing.T) {
	ctx, cfg, j := fixture(t)
	set, err := Stage(ctx, cfg, j)
	if err != nil {
		t.Fatal(err)
	}
	changed := j
	changed.Principal = "other"
	changed, err = sandbox.Seal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Stage(ctx, cfg, changed); !errors.Is(err, cortex.ErrSandboxInputsConflict) {
		t.Fatalf("conflicting identity: %v", err)
	}
	name := set.Paths[j.Inputs[0].Path]
	if err = os.Chmod(name, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, []byte("modified input"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(name, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err = Stage(ctx, cfg, j); !errors.Is(err, evidence.ErrDigestMismatch) {
		t.Fatalf("tampered retry: %v", err)
	}
}

func TestStagePreservesUnfinishedDirectory(t *testing.T) {
	ctx, cfg, j := fixture(t)
	set, err := Stage(ctx, cfg, j)
	if err != nil {
		t.Fatal(err)
	}
	dir := set.Directory
	if err = os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "unrecognized")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(ctx, cfg, j); !errors.Is(err, cortex.ErrSandboxInputsIncomplete) {
		t.Fatalf("unfinished staging: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve" {
		t.Fatal("retry deleted unfinished state")
	}
}

func TestStagingClosesBlockedSourceOnCancellation(t *testing.T) {
	base, cfg, j := fixture(t)
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	started := make(chan struct{})
	cfg.Source = opener(func(context.Context, evidence.Ref) (io.ReadCloser, error) {
		return &signalledReader{ReadCloser: reader, started: started}, nil
	})
	go func() { <-started; cancel() }()
	if _, err := Stage(ctx, cfg, j); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled input stream: %v", err)
	}
	entries, err := os.ReadDir(cfg.Root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled staging leaked: %v %v", entries, err)
	}
}

type signalledReader struct {
	io.ReadCloser
	started chan struct{}
}

func (r *signalledReader) Read(data []byte) (int, error) {
	close(r.started)
	return r.ReadCloser.Read(data)
}

func TestStagingRefusesExpiredEvidenceAndSymlinkedRetry(t *testing.T) {
	ctx, cfg, j := fixture(t)
	set, err := Stage(ctx, cfg, j)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Now = func() time.Time { return j.Inputs[0].Ref.RetainUntil }
	if _, err = Stage(ctx, cfg, j); !errors.Is(err, evidence.ErrDenied) {
		t.Fatalf("expired evidence retry: %v", err)
	}
	cfg.Now = nil
	file := set.Paths[j.Inputs[0].Path]
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("/etc/passwd", file); err != nil {
		t.Fatal(err)
	}
	if _, err = Stage(ctx, cfg, j); err == nil {
		t.Fatal("symlinked evidence accepted")
	}
}

func TestStagingScopesIdenticalJobIDs(t *testing.T) {
	ctx, cfg, j := fixture(t)
	first, err := Stage(ctx, cfg, j)
	if err != nil {
		t.Fatal(err)
	}
	other, err := sandbox.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	other.Scope.Levels[0].Value = "two"
	other.Inputs[0].Ref.Scope.Levels[0].Value = "two"
	other, err = sandbox.Seal(other)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Stage(cortex.WithScope(ctx, other.Scope), cfg, other)
	if err != nil {
		t.Fatal(err)
	}
	if first.Directory == second.Directory {
		t.Fatal("different scopes share staging files")
	}
}

func TestPolicyCallbacksCannotMutateStagedIdentity(t *testing.T) {
	ctx, cfg, j := fixture(t)
	cfg.AuthorizeJob = func(_ context.Context, request sandbox.Job) error {
		request.Scope.Levels[0].Value = "changed"
		request.Inputs[0].Ref.Scope.Levels[0].Value = "changed"
		return nil
	}
	cfg.Policy = policy(func(_ context.Context, _ evidence.Action, ref evidence.Ref) error {
		ref.Scope.Levels[0].Value = "changed"
		return nil
	})
	cfg.Source = opener(func(_ context.Context, ref evidence.Ref) (io.ReadCloser, error) {
		ref.Scope.Levels[0].Value = "changed"
		return io.NopCloser(strings.NewReader("verified input")), nil
	})
	if _, err := Stage(ctx, cfg, j); err != nil {
		t.Fatal(err)
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("caller job changed: %v", err)
	}
}
