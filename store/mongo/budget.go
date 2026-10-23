package mongo

import (
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex/execution"
)

// BSON dates retain milliseconds. Keep the exact policy deadline alongside the
// native date so a frozen budget survives run and continuation round trips.
func exactBudgetDeadline(deadline time.Time) string {
	if deadline.IsZero() {
		return ""
	}
	return deadline.UTC().Format(time.RFC3339Nano)
}

func restoreBudgetDeadline(budget execution.Budget, exact string) (execution.Budget, error) {
	if exact == "" {
		return budget, nil // Historical documents retain their stored precision.
	}
	deadline, err := time.Parse(time.RFC3339Nano, exact)
	if err != nil {
		return execution.Budget{}, fmt.Errorf("cortex/mongo: decode exact budget deadline: %w", err)
	}
	if deadline.IsZero() || !deadline.Truncate(time.Millisecond).Equal(budget.Deadline.Truncate(time.Millisecond)) {
		return execution.Budget{}, errors.New("cortex/mongo: exact budget deadline differs from stored date")
	}
	budget.Deadline = deadline
	return budget, nil
}
