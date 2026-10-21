package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex/integrations/sandbox/internal/acceptance"
	"github.com/xraph/cortex/sandbox"
)

var errAcceptanceExpired = errors.New("cortex: sandbox acceptance expired")

func acceptanceDeadline(receipt sandbox.Receipt) time.Time {
	return acceptance.Deadline(receipt)
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
