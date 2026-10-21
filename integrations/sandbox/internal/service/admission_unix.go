//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func verifyProfile(ctx context.Context, p LaunchProfile, owner uint32) error {
	body, err := p.Template()
	if err != nil {
		return err
	}
	for _, pin := range []hostfs.Pin{p.Worker, p.Config, {Path: p.TemplatePath, SHA256: evidence.Digest(body), Bytes: int64(len(body)), Mode: 0o644}} {
		if operationErr := hostfs.VerifyPin(ctx, pin, owner); operationErr != nil {
			return operationErr
		}
	}
	directory, err := hostfs.OpenDirectory(p.StateDirectory, owner)
	if err != nil {
		return err
	}
	info, statErr := directory.Stat()
	closeErr := directory.Close()
	if statErr != nil || closeErr != nil {
		return errors.Join(statErr, closeErr)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("cortex: worker state directory must be private")
	}
	return ctx.Err()
}

// CheckLaunch verifies approved host bytes and systemd's effective configuration
// without granting a claim or starting execution. Recheck immediately before
// dispatch. Only the host may supply profiles; callers must also authenticate
// the receipt, enforce current grants and retain one-time launch ownership.
func (r *Reaper) CheckLaunch(ctx context.Context, p LaunchProfile, key id.ID) error {
	if runtime.GOOS != "linux" {
		return errUnsupported
	}
	if r.check == nil {
		return fmt.Errorf("cortex: missing containment guard")
	}
	if err := r.check(ctx, key); err != nil {
		return err
	}
	reader, ok := r.manager.(interface {
		Loaded(context.Context, string) (map[string]any, error)
	})
	if !ok {
		return fmt.Errorf("cortex: service manager cannot inspect launch configuration")
	}
	return checkLaunch(ctx, p, key, 0, reader.Loaded)
}

func checkLaunch(ctx context.Context, p LaunchProfile, key id.ID, owner uint32, load func(context.Context, string) (map[string]any, error)) error {
	layout, err := containment.ForJob(key)
	if err != nil {
		return err
	}
	if operationErr := verifyProfile(ctx, p, owner); operationErr != nil {
		return operationErr
	}
	properties, err := load(ctx, layout.Unit)
	if err != nil {
		return err
	}
	if operationErr := checkProperties(p, key, properties); operationErr != nil {
		return operationErr
	}
	return ctx.Err()
}
