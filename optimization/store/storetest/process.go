package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// ProcessFactory opens an existing database with a trusted test clock.
type ProcessFactory func(*testing.T, string, func() time.Time) store.Store

type processClaim struct {
	Database     string
	Result       string
	Scope        cortex.Scope
	ExperimentID id.ID
	Now          time.Time
}

// LeaseTakeoverAcrossProcesses checks fencing through a fresh test process.
// database runs only in the parent; factory opens the supplied database in either process.
func LeaseTakeoverAcrossProcesses(t *testing.T, database func(*testing.T) string, factory ProcessFactory) {
	t.Helper()
	// A fresh test process has no access to the parent's Go locks or maps.
	if fixturePath := os.Getenv("CORTEX_OPTIMIZER_PROCESS_CLAIM"); fixturePath != "" {
		encoded, err := os.ReadFile(fixturePath) //nolint:gosec // Only the parent test process supplies this fixture path.
		if err != nil {
			t.Fatal(err)
		}
		var request processClaim
		if err = json.Unmarshal(encoded, &request); err != nil {
			t.Fatal(err)
		}
		s := factory(t, request.Database, func() time.Time { return request.Now })
		ctx := cortex.WithScope(context.Background(), request.Scope)
		var lease optimization.WorkLease
		if err = s.Transact(ctx, func(tx store.Tx) error {
			var claimErr error
			lease, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: request.ExperimentID, Owner: "child", Duration: 30 * time.Second})
			return claimErr
		}); err != nil {
			t.Fatal(err)
		}
		encoded, err = json.Marshal(lease)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(request.Result, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := database(t)
	s := factory(t, path, func() time.Time { return now })
	bundle := Fixture(t, "process")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	var old optimization.WorkLease
	if err := s.Transact(ctx, func(tx store.Tx) error {
		if err := tx.CreateExperiment(bundle); err != nil {
			return err
		}
		if err := tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); err != nil {
			return err
		}
		var err error
		old, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: "parent", Duration: 30 * time.Second})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request := processClaim{Database: path, Result: filepath.Join(dir, "lease.json"), Scope: bundle.Experiment.Spec.Scope, ExperimentID: bundle.Experiment.ID, Now: now.Add(31 * time.Second)}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(dir, "claim.json")
	if err = os.WriteFile(fixturePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(deadline, executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$") //nolint:gosec // Relaunch this test executable with an exact, escaped test name.
	command.Env = append(os.Environ(), "CORTEX_OPTIMIZER_PROCESS_CLAIM="+fixturePath)
	if output, commandErr := command.CombinedOutput(); commandErr != nil {
		t.Fatalf("child: %v\n%s", commandErr, output)
	}
	encoded, err = os.ReadFile(request.Result)
	if err != nil {
		t.Fatal(err)
	}
	var replacement optimization.WorkLease
	if err = json.Unmarshal(encoded, &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Generation != old.Generation+1 || replacement.Owner != "child" {
		t.Fatalf("invalid takeover: %+v", replacement)
	}
	if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RenewLease(old) }); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("old process renewed: %v", err)
	}
	got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil || got.Lease != replacement {
		t.Fatalf("old connection has stale state: %+v, %v", got.Lease, err)
	}
}
