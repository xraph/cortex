//go:build linux || darwin

package grading

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

type fileExportAuth func(context.Context, verification.ArtifactExportRequest) error

func (f fileExportAuth) AuthorizeArtifactExport(ctx context.Context, r verification.ArtifactExportRequest) error {
	return f(ctx, r)
}
func exportNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func exportWantError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected rejection")
	}
}
func fileExportFixture(t *testing.T) (FileExportConfig, verification.ArtifactExportRequest, []byte) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	exportNoError(t, err)
	journal, target := filepath.Join(root, "journal"), filepath.Join(root, "target")
	exportNoError(t, os.Mkdir(journal, 0o700))
	exportNoError(t, os.Mkdir(target, 0o700))
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}
	now := time.Now().UTC()
	data := []byte("approved artifact")
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", Producer: "host", MediaType: "application/octet-stream", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: data})
	exportNoError(t, err)
	request := verification.ArtifactExportRequest{SchemaVersion: 1, ID: id.New(verification.PrefixExport), Result: verification.ResultContentRequest{SchemaVersion: 1, RunID: id.NewAgentRunID(), ResultID: id.New(verification.PrefixResult)}, Artifact: artifact.Ref, Actor: "operator", ApprovalRef: "approval:1", Destination: "review", DestinationRevision: "1"}
	cfg := FileExportConfig{Owner: uint32(os.Geteuid()), JournalDirectory: journal, Destinations: []FileExportDestination{{Name: "review", Revision: "1", Directory: target}}, Command: verification.ArtifactExporterConfig{Authorizer: fileExportAuth(func(context.Context, verification.ArtifactExportRequest) error { return nil })}}
	return cfg, request, data
}
func TestFileExportPersistsIdenticalRetriesAndRejectsChangedRequest(t *testing.T) {
	cfg, r, data := fileExportFixture(t)
	sink, err := newFileExportSink(cfg)
	exportNoError(t, err)
	ctx := cortex.WithScope(t.Context(), r.Artifact.Scope)
	first, err := sink.ExportArtifact(ctx, r, data)
	exportNoError(t, err)
	exportNoError(t, first.Validate(r))
	sink, err = newFileExportSink(cfg)
	exportNoError(t, err)
	second, err := sink.ExportArtifact(ctx, r, data)
	exportNoError(t, err)
	if first != second {
		t.Fatal("retry changed receipt")
	}
	saved, err := os.ReadFile(filepath.Join(cfg.Destinations[0].Directory, r.ID.String()))
	exportNoError(t, err)
	if string(saved) != string(data) {
		t.Fatal("wrong exported bytes")
	}
	r.ApprovalRef = "approval:other"
	_, err = sink.ExportArtifact(ctx, r, data)
	exportWantError(t, err)
}
func TestFileExportRejectsTraversalLinksAndReplacement(t *testing.T) {
	for _, mode := range []string{"target traversal", "symlink", "hardlink", "existing bytes", "directory", "changed content", "wrong scope", "replaced target"} {
		t.Run(mode, func(t *testing.T) {
			cfg, r, data := fileExportFixture(t)
			sink, err := newFileExportSink(cfg)
			exportNoError(t, err)
			path := filepath.Join(cfg.Destinations[0].Directory, r.ID.String())
			ctx := cortex.WithScope(t.Context(), r.Artifact.Scope)
			switch mode {
			case "target traversal":
				r.Destination = "../target"
			case "symlink", "hardlink":
				source := filepath.Join(cfg.JournalDirectory, "source")
				exportNoError(t, os.WriteFile(source, data, 0o400))
				if mode == "symlink" {
					exportNoError(t, os.Symlink(source, path))
				} else {
					exportNoError(t, os.Link(source, path))
				}
			case "existing bytes":
				exportNoError(t, os.WriteFile(path, []byte("other"), 0o400))
			case "directory":
				exportNoError(t, os.Mkdir(path, 0o700))
			case "changed content":
				data = []byte("unapproved content")
			case "wrong scope":
				ctx = cortex.WithScope(t.Context(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			case "replaced target":
				exportNoError(t, os.Rename(cfg.Destinations[0].Directory, cfg.Destinations[0].Directory+".old"))
				exportNoError(t, os.Mkdir(cfg.Destinations[0].Directory, 0o700))
			}
			_, err = sink.ExportArtifact(ctx, r, data)
			exportWantError(t, err)
		})
	}
}
func TestFileExportConcurrentIdenticalPublication(t *testing.T) {
	cfg, r, data := fileExportFixture(t)
	sink, err := newFileExportSink(cfg)
	exportNoError(t, err)
	ctx := cortex.WithScope(t.Context(), r.Artifact.Scope)
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Go(func() {
			receipt, writeErr := sink.ExportArtifact(ctx, r, data)
			if writeErr == nil {
				writeErr = receipt.Validate(r)
			}
			results <- writeErr
		})
	}
	wg.Wait()
	close(results)
	for writeErr := range results {
		exportNoError(t, writeErr)
	}
}

func TestFileExportReservationSurvivesInterruptedDelivery(t *testing.T) {
	cfg, r, data := fileExportFixture(t)
	checks := 0
	cfg.Command.Authorizer = fileExportAuth(func(context.Context, verification.ArtifactExportRequest) error {
		checks++
		if checks == 3 {
			return verification.ErrResultAccessDenied
		}
		return nil
	})
	sink, err := newFileExportSink(cfg)
	exportNoError(t, err)
	ctx := cortex.WithScope(t.Context(), r.Artifact.Scope)
	_, err = sink.ExportArtifact(ctx, r, data)
	exportWantError(t, err)
	if _, statErr := os.Stat(filepath.Join(cfg.JournalDirectory, r.ID.String()+".json")); statErr != nil {
		t.Fatal("missing durable reservation", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.Destinations[0].Directory, r.ID.String())); !os.IsNotExist(statErr) {
		t.Fatal("artifact published after revocation", statErr)
	}
	cfg.Command.Authorizer = fileExportAuth(func(context.Context, verification.ArtifactExportRequest) error { return nil })
	restarted, err := newFileExportSink(cfg)
	exportNoError(t, err)
	receipt, err := restarted.ExportArtifact(ctx, r, data)
	exportNoError(t, err)
	exportNoError(t, receipt.Validate(r))
}

func TestFileExportOperationCannotMoveToAnotherDestination(t *testing.T) {
	cfg, r, data := fileExportFixture(t)
	other := filepath.Join(filepath.Dir(cfg.JournalDirectory), "other-target")
	exportNoError(t, os.Mkdir(other, 0o700))
	cfg.Destinations = append(cfg.Destinations, FileExportDestination{Name: "other", Revision: "1", Directory: other})
	sink, err := newFileExportSink(cfg)
	exportNoError(t, err)
	ctx := cortex.WithScope(t.Context(), r.Artifact.Scope)
	_, err = sink.ExportArtifact(ctx, r, data)
	exportNoError(t, err)
	r.Destination = "other"
	_, err = sink.ExportArtifact(ctx, r, data)
	exportWantError(t, err)
	entries, err := os.ReadDir(other)
	exportNoError(t, err)
	if len(entries) != 0 {
		t.Fatal("changed retry wrote to another target")
	}
}

type fileExportClock struct{ now time.Time }

func (c fileExportClock) Now() time.Time { return c.now }

type fileExportStore struct{ snapshot verification.ResultSnapshot }

func (s fileExportStore) GetCommittedResult(context.Context, id.ID) (verification.CommitReceipt, error) {
	return s.snapshot.Receipt, nil
}
func (s fileExportStore) ReadResultSnapshot(context.Context, verification.ResultReadRequest) (verification.ResultSnapshot, error) {
	return s.snapshot, nil
}

type fileExportRepository struct{ artifact evidence.Artifact }

func (s fileExportRepository) Put(context.Context, evidence.Artifact) (evidence.Ref, error) {
	return evidence.Ref{}, verification.ErrInvalid
}
func (s fileExportRepository) Delete(context.Context, evidence.Ref, string) error {
	return verification.ErrInvalid
}
func (s fileExportRepository) Open(context.Context, evidence.Ref) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.artifact.Data)), nil
}

type fileExportFence struct{}

func (fileExportFence) WithArtifactExportFence(_ context.Context, _ verification.ArtifactExportRequest, b verification.Binding, next func(verification.Binding) error) error {
	return next(b)
}

type fileExportScreen struct{ allow bool }

func (s fileExportScreen) ScreenArtifactExport(_ context.Context, r verification.ArtifactExportScreenRequest) (verification.ArtifactExportScreenResult, error) {
	return verification.ArtifactExportScreenResult{Approved: s.allow, SHA256: evidence.Digest(r.Data)}, nil
}

func TestFileExporterCommandChecksApprovalAndContentBeforeFiles(t *testing.T) {
	for _, mode := range []string{"allowed", "denied", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			cfg, r, data := fileExportFixture(t)
			now := r.Artifact.CreatedAt
			contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: r.Artifact.Scope, Revision: "1", Objective: "approved export fixture", Criteria: []verification.Criterion{{ID: "test", Kind: verification.KindTest, Required: true, Verifier: "test", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
			exportNoError(t, err)
			binding := verification.Binding{Scope: r.Artifact.Scope, RunID: r.Result.RunID, ContractDigest: contract.Digest, Candidate: r.Artifact, Context: verification.ContextSnapshot{PrincipalRef: "operator", AuthorityRevision: "1", PreparedDigest: evidence.Digest([]byte("prepared")), PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
			report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: binding, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: []verification.Outcome{{CriterionID: "test", Verifier: "test", VerifierRevision: "1", Status: verification.StatusPassed}}})
			exportNoError(t, err)
			commit := verification.CommitRequest{SchemaVersion: 1, ResultID: r.Result.ResultID, ExpectedRunRevision: 1, ContractID: contract.ID, ReportIDs: []id.ID{report.ID}, Current: binding}
			_, commitDigest, err := commit.Freeze()
			exportNoError(t, err)
			bindingDigest, err := binding.Digest()
			exportNoError(t, err)
			receipt, err := verification.SealCommitReceipt(verification.CommitReceipt{SchemaVersion: 1, Request: commit, RequestDigest: commitDigest, RunRevision: 2, ReportDigests: []string{report.Digest}, ObservedAt: now, Outbox: verification.CommitOutbox{EventID: id.New(verification.PrefixCommitEvent), Type: "result_committed", RunID: r.Result.RunID, ResultID: r.Result.ResultID, BindingDigest: bindingDigest}})
			exportNoError(t, err)
			cfg.Command.Store = fileExportStore{snapshot: verification.ResultSnapshot{SchemaVersion: 1, Receipt: receipt, Contract: contract, Reports: []verification.Report{report}, ObservedAt: now, Output: string(data)}}
			cfg.Command.Repository = fileExportRepository{artifact: evidence.Artifact{Ref: r.Artifact, Data: data}}
			cfg.Command.Clock = fileExportClock{now: now}
			cfg.Command.MaxConcurrent = 1
			cfg.Command.Fence = fileExportFence{}
			cfg.Command.Screener = fileExportScreen{allow: mode != "blocked"}
			if mode == "denied" {
				cfg.Command.Authorizer = fileExportAuth(func(context.Context, verification.ArtifactExportRequest) error {
					return verification.ErrResultAccessDenied
				})
			}
			exporter, err := NewFileExporter(cfg)
			exportNoError(t, err)
			result, err := exporter.Export(cortex.WithScope(t.Context(), r.Artifact.Scope), r)
			if mode == "allowed" {
				exportNoError(t, err)
				exportNoError(t, result.Validate(r))
				saved, readErr := os.ReadFile(filepath.Join(cfg.Destinations[0].Directory, r.ID.String()))
				exportNoError(t, readErr)
				if !bytes.Equal(data, saved) {
					t.Fatal("wrong exported content")
				}
			} else {
				exportWantError(t, err)
				for _, directory := range []string{cfg.JournalDirectory, cfg.Destinations[0].Directory} {
					entries, readErr := os.ReadDir(directory)
					exportNoError(t, readErr)
					if len(entries) != 0 {
						t.Fatal("denied export changed files")
					}
				}
			}
		})
	}
}
