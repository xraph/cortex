//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
	"github.com/xraph/cortex/sandbox"
)

// LocalInputs serves root-owned input files named by the full Ref.BindingDigest.
// Bytes are verified against the job's exact reference and current source policy.
// This read-only source cannot upload captures or erase evidence tombstones.
type LocalInputs struct {
	root      string
	maximum   int64
	owner     uint32
	policy    CapturePolicy
	slots     chan struct{}
	directory os.FileInfo
	now       func() time.Time
}

// NewLocalInputs requires a private mode-0700 directory and a current input
// policy. Files are mode-0400 single-link regular files. maximum is at most one
// MiB and readers is 1..16; each open reader retains a slot until Close. Provision
// exact reference bindings through the trusted host before submission. Use a
// responsive local filesystem and prevent directory/file changes during Open.
func NewLocalInputs(root string, maximum int64, readers int, policy CapturePolicy) (*LocalInputs, error) {
	return newLocalInputs(root, maximum, readers, policy, 0)
}

func validLocalInputLimits(maximum int64, readers int) bool {
	return maximum > 0 && maximum <= 1<<20 && readers >= 1 && readers <= 16
}

func newLocalInputs(root string, maximum int64, readers int, policy CapturePolicy, owner uint32) (*LocalInputs, error) {
	if !profilePath(root) || !validLocalInputLimits(maximum, readers) || policy == nil {
		return nil, fmt.Errorf("cortex: invalid local input source")
	}
	source := &LocalInputs{root: root, maximum: maximum, owner: owner, policy: policy, slots: make(chan struct{}, readers), now: time.Now}
	info, err := source.checkDirectory()
	if err != nil {
		return nil, err
	}
	source.directory = info
	return source, nil
}

func (s *LocalInputs) checkDirectory() (os.FileInfo, error) {
	directory, err := hostfs.OpenDirectory(s.root, s.owner)
	if err != nil {
		return nil, err
	}
	info, statErr := directory.Stat()
	if operationErr := errors.Join(statErr, directory.Close()); operationErr != nil {
		return nil, operationErr
	}
	if info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || (s.directory != nil && !os.SameFile(s.directory, info)) {
		return nil, fmt.Errorf("cortex: unsafe or replaced input directory")
	}
	return info, nil
}

// Open permits only a declared input in the service's private validated job
// context. It checks current policy before and after file access and returns no
// reader on failure. WorkerEvidence additionally rechecks live worker authority
// and source grants around the complete transfer before acknowledging its bytes.
func (s *LocalInputs) Open(ctx context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	if s == nil || s.slots == nil || s.policy == nil || s.now == nil {
		return nil, evidence.ErrDenied
	}
	ref = detachedCaptureRef(ref)
	if err := s.authorize(ctx, ref); err != nil {
		return nil, err
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	retained := false
	defer func() {
		if !retained {
			<-s.slots
		}
	}()
	// A queued read must not retain permission from before its capacity wait.
	if err := s.authorize(ctx, ref); err != nil {
		return nil, err
	}
	if _, err := s.checkDirectory(); err != nil {
		return nil, err
	}
	binding, err := ref.BindingDigest()
	if err != nil {
		return nil, err
	}
	body, err := hostfs.ReadPin(ctx, hostfs.Pin{Path: filepath.Join(s.root, binding), SHA256: ref.SHA256, Bytes: ref.Bytes, Mode: 0o400}, s.owner)
	if err != nil {
		return nil, err
	}
	if _, err = s.checkDirectory(); err != nil {
		return nil, err
	}
	if policyErr := s.authorize(ctx, ref); policyErr != nil {
		return nil, policyErr
	}
	retained = true
	return verifiedio.Snapshot(ctx, body, func() { <-s.slots }), nil
}

func (s *LocalInputs) authorize(ctx context.Context, ref evidence.Ref) error {
	authority, err := boundEvidenceAuthority(ctx, ref)
	if err != nil {
		return err
	}
	now := s.now()
	if authority.cleanup || !declaredInput(authority.job, ref) || ref.Bytes > s.maximum || now.Before(ref.CreatedAt) || !now.Before(ref.RetainUntil) {
		return evidence.ErrDenied
	}
	job, err := sandbox.Seal(authority.job)
	if err != nil {
		return err
	}
	if policyErr := s.policy(ctx, job, evidence.ActionRead, detachedCaptureRef(ref)); policyErr != nil {
		return policyErr
	}
	now = s.now()
	if now.Before(ref.CreatedAt) || !now.Before(ref.RetainUntil) {
		return evidence.ErrDenied
	}
	return ctx.Err()
}
