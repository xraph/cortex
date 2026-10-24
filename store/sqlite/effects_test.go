package sqlite

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/assurance"
)

func sqliteEffectRequest() execution.EffectRequest {
	return execution.EffectRequest{SchemaVersion: 1, Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "effect-recovery"}}}, RunID: id.NewAgentRunID(), OperationKey: "publication-1", AttemptID: id.New(execution.PrefixEffectAttempt), ArgsDigest: evidence.Digest([]byte("payload")), PrincipalRef: "host", AuthorityRef: "grant-1", Deadline: time.Now().UTC().Add(time.Minute), Requirement: execution.EffectRequirement{Tool: "write", Revision: "1", Class: execution.EffectNonIdempotent, ReconcileRevision: "probe-1", Retry: execution.RetryPolicy{MaxAttempts: 1}}}
}

var errEffectAckLost = errors.New("effect write acknowledgement lost")

type lostEffectAckDriver struct {
	assuranceDriver
	kind string
}

func (d lostEffectAckDriver) Insert(ctx context.Context, r assurance.Record) error {
	if err := d.assuranceDriver.Insert(ctx, r); err != nil {
		return err
	}
	if r.Kind == d.kind {
		return errEffectAckLost
	}
	return nil
}
func (d lostEffectAckDriver) TransactRunCalls(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.Driver) error) error {
	err := d.assuranceDriver.TransactRunCalls(ctx, runID, callback)
	if err == nil && d.kind == "effect_request" {
		return errEffectAckLost
	}
	return err
}

func TestEffectLostReservationAcknowledgementNeverGrantsDispatch(t *testing.T) {
	s := newTestStore(t)
	r := sqliteEffectRequest()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	s.assuranceStore = assurance.New(lostEffectAckDriver{assuranceDriver: assuranceDriver{store: s}, kind: "effect_request"})
	created, err := s.BeginEffect(ctx, r)
	require.ErrorIs(t, err, errEffectAckLost)
	require.False(t, created)
	s.assuranceStore = assurance.New(assuranceDriver{store: s})
	created, err = s.BeginEffect(ctx, r)
	require.NoError(t, err)
	require.False(t, created)
	got, err := s.GetEffect(ctx, r.OperationKey)
	require.NoError(t, err)
	require.Nil(t, got.Receipt)
}

type fileEffectFixture struct {
	path        string
	waitForKill bool
	calls       int
}

func (f *fileEffectFixture) Execute(ctx context.Context, r execution.EffectRequest, _ []byte) (execution.EffectReceipt, error) {
	f.calls++
	_, digest, err := r.Freeze()
	if err != nil {
		return execution.EffectReceipt{}, err
	}
	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return execution.EffectReceipt{}, err
	}
	_, writeErr := fmt.Fprintln(file, digest)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return execution.EffectReceipt{}, err
	}
	if f.waitForKill {
		fmt.Println("effect-written")
		<-ctx.Done()
		return execution.EffectReceipt{}, ctx.Err()
	}
	return fileEffectReceipt(r), nil
}
func (f *fileEffectFixture) Reconcile(_ context.Context, r execution.EffectRequest) (execution.EffectStatus, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return execution.EffectStatus{Outcome: execution.EffectUnknown}, nil
	}
	_, digest, err := r.Freeze()
	if err != nil || string(data) != digest+"\n" {
		return execution.EffectStatus{Outcome: execution.EffectUnknown}, nil
	}
	receipt := fileEffectReceipt(r)
	return execution.EffectStatus{Outcome: execution.EffectApplied, Receipt: &receipt}, nil
}
func fileEffectReceipt(r execution.EffectRequest) execution.EffectReceipt {
	_, digest, _ := r.Freeze()
	return execution.EffectReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.EffectApplied, ExternalID: "fixture-file", ExternalVersion: "1", EvidenceRef: "fixture-fsynced-write"}
}
func sqliteEffectExecutor(t *testing.T, s execution.EffectStore, f *fileEffectFixture, r execution.EffectRequest) *execution.EffectExecutor {
	t.Helper()
	e, err := execution.NewEffectExecutor(execution.EffectExecutorConfig{Store: s, Adapters: []execution.EffectRegistration{{Requirement: r.Requirement, Adapter: f}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, MaxConcurrent: 1})
	require.NoError(t, err)
	return e
}

func TestEffectProcessCrashHelper(t *testing.T) {
	dir := os.Getenv("CORTEX_EFFECT_CRASH_FIXTURE")
	if dir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, "request.json"))
	require.NoError(t, err)
	var r execution.EffectRequest
	require.NoError(t, json.Unmarshal(data, &r))
	s := openTestStore(t, filepath.Join(dir, "effects.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	f := &fileEffectFixture{path: filepath.Join(dir, "external.log"), waitForKill: true}
	e := sqliteEffectExecutor(t, s, f, r)
	_, err = e.Execute(cortex.WithScope(context.Background(), r.Scope), r, []byte("payload"))
	t.Fatalf("expected process kill before completion: %v", err)
}
func TestEffectProcessCrashReconcilesWithoutSecondMutation(t *testing.T) {
	dir := t.TempDir()
	r := sqliteEffectRequest()
	data, err := json.Marshal(r)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "request.json"), data, 0600))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEffectProcessCrashHelper$")
	command.Env = append(os.Environ(), "CORTEX_EFFECT_CRASH_FIXTURE="+dir)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	line, readErr := bufio.NewReader(output).ReadString('\n')
	require.NoError(t, readErr)
	require.Equal(t, "effect-written\n", line)
	require.NoError(t, command.Process.Kill())
	require.Error(t, command.Wait())
	require.NoError(t, ctx.Err())
	s := openTestStore(t, filepath.Join(dir, "effects.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	scoped := cortex.WithScope(ctx, r.Scope)
	pending, err := s.GetEffect(scoped, r.OperationKey)
	require.NoError(t, err)
	require.Nil(t, pending.Receipt)
	f := &fileEffectFixture{path: filepath.Join(dir, "external.log")}
	e := sqliteEffectExecutor(t, s, f, r)
	page, err := e.RecoverPage(scoped, execution.EffectListRequest{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, execution.EffectRecoveryTerminal, page.Items[0].Status)
	require.Equal(t, execution.EffectApplied, page.Items[0].Outcome)
	require.Empty(t, page.NextAfter)
	receipt := fileEffectReceipt(r)
	_, err = e.Execute(scoped, r, []byte("payload"))
	require.NoError(t, err)
	require.Zero(t, f.calls)
	data, err = os.ReadFile(f.path)
	require.NoError(t, err)
	require.Equal(t, 1, bytes.Count(data, []byte("\n")))
	stored, err := s.GetEffect(scoped, r.OperationKey)
	require.NoError(t, err)
	require.Equal(t, &receipt, stored.Receipt)
}
