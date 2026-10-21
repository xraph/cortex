package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex/sandbox"
)

var errAcceptanceExpired = errors.New("cortex: sandbox acceptance expired")

func acceptanceDeadline(receipt sandbox.Receipt) time.Time {
	deadline := receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
	if receipt.Capabilities.QualifiedUntil.Before(deadline) {
		deadline = receipt.Capabilities.QualifiedUntil
	}
	return deadline
}

func checkAcceptanceTime(receipt sandbox.Receipt, now time.Time) error {
	if now.IsZero() || now.Before(receipt.AcceptedAt) {
		return fmt.Errorf("cortex: invalid sandbox acceptance clock")
	}
	if !now.Before(acceptanceDeadline(receipt)) {
		return errors.Join(errAcceptanceExpired, context.DeadlineExceeded)
	}
	return nil
}
