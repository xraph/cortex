// Package acceptance shares the signed execution time boundary.
package acceptance

import (
	"time"

	"github.com/xraph/cortex/sandbox"
)

// Deadline returns the earlier job or deployment qualification deadline.
func Deadline(receipt sandbox.Receipt) time.Time {
	deadline := receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
	if receipt.Capabilities.QualifiedUntil.Before(deadline) {
		return receipt.Capabilities.QualifiedUntil
	}
	return deadline
}
