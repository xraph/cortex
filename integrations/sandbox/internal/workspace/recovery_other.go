//go:build !linux

package workspace

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/id"
)

// Recovery is unavailable outside Linux. NewRecovery always returns an error.
type Recovery struct {
	fence func(context.Context, id.ID) error
}

func productionRecovery(string, func(context.Context, id.ID) error) (*Recovery, error) {
	return nil, fmt.Errorf("cortex: workspace recovery requires root on Linux")
}
func (*Recovery) discover(context.Context, int) ([]id.ID, error) {
	return nil, fmt.Errorf("cortex: workspace recovery requires root on Linux")
}
func (*Recovery) reclaim(context.Context, id.ID) error {
	return fmt.Errorf("cortex: workspace recovery requires root on Linux")
}
