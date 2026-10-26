//go:build linux || darwin

package grading

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/sandbox"
	"github.com/xraph/cortex/verification"
)

type evaluationJournal struct {
	directory exportDirectory
	owner     uint32
}

func newEvaluationJournal(path string, owner uint32) (evaluationJournal, error) {
	directory, err := pinExportDirectory(path, owner)
	return evaluationJournal{directory: directory, owner: owner}, err
}
func (j evaluationJournal) read(ctx context.Context, name string, target any) error {
	if err := j.directory.check(j.owner); err != nil {
		return err
	}
	body, err := hostfs.ReadOwned(ctx, filepath.Join(j.directory.path, name), j.owner, 0o400, 1<<20)
	if err != nil {
		return err
	}
	if decodeErr := strictJSON(body, target); decodeErr != nil {
		return decodeErr
	}
	return j.directory.check(j.owner)
}
func (j evaluationJournal) write(ctx context.Context, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 1<<20 {
		return verification.ErrInvalid
	}
	check := func() error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return j.directory.check(j.owner)
	}
	if checkErr := check(); checkErr != nil {
		return checkErr
	}
	pin := hostfs.Pin{Path: filepath.Join(j.directory.path, name), SHA256: evidence.Digest(data), Bytes: int64(len(data)), Mode: 0o400}
	return hostfs.PublishPin(ctx, pin, j.owner, data, check)
}
func (j evaluationJournal) lock() (*os.File, error) {
	if err := j.directory.check(j.owner); err != nil {
		return nil, err
	}
	file, err := hostfs.OpenPrivateState(filepath.Join(j.directory.path, "service.lock"), j.owner, true)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(verification.ErrConflict, file.Close())
	}
	return file, nil
}

type journalRunner struct {
	sandbox.Runner
	journal evaluationJournal
}

func (r *journalRunner) Submit(ctx context.Context, job sandbox.Job) (sandbox.Receipt, error) {
	if job.Validate() != nil {
		return sandbox.Receipt{}, verification.ErrInvalid
	}
	name := job.ID.String() + ".intent"
	var retained sandbox.Job
	err := r.journal.read(ctx, name, &retained)
	if err == nil {
		if retained.Validate() != nil || retained.Digest != job.Digest {
			return sandbox.Receipt{}, verification.ErrConflict
		}
		state, inspectErr := r.Inspect(ctx, job.ID)
		if inspectErr != nil {
			return sandbox.Receipt{}, errors.Join(verification.ErrCallUncertain, inspectErr)
		}
		return state.Receipt, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return sandbox.Receipt{}, err
	}
	// A consumed intent is never sent again, even if the process died before
	// Submit reached the broker. Recovery requires the original broker receipt.
	if err = r.journal.write(ctx, name, job); err != nil {
		return sandbox.Receipt{}, errors.Join(verification.ErrCallUncertain, err)
	}
	return r.Runner.Submit(ctx, job)
}

// Cancellation belongs to the phase service's original attempt and never creates
// a replacement identity. The ordinary verifier already bounds cleanup time.
func (r *journalRunner) Cancel(ctx context.Context, key id.ID) error {
	return r.Runner.Cancel(ctx, key)
}
func readEvaluationArtifact(reader io.ReadCloser, ref evidence.Ref) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, ref.Bytes+1))
	err = errors.Join(err, reader.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.Bytes || evidence.Digest(data) != ref.SHA256 {
		return nil, evidence.ErrDigestMismatch
	}
	return data, nil
}
