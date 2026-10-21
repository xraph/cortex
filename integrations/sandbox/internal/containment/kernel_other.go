//go:build !linux

package containment

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/id"
)

type kernelGroup struct{}

// VerifyCurrent rejects hosts without the required Linux containment controls.
func VerifyCurrent(context.Context, id.ID) error {
	return fmt.Errorf("cortex: Linux cgroup-v2 containment required")
}

// VerifyReaper rejects hosts without Linux containment controls.
func VerifyReaper(context.Context, id.ID) error {
	return fmt.Errorf("cortex: Linux cgroup-v2 containment required")
}
func (kernelGroup) Events(context.Context, id.ID) ([]byte, error) {
	return nil, fmt.Errorf("cortex: Linux cgroup-v2 containment required")
}
func (kernelGroup) Kill(context.Context, id.ID) error {
	return fmt.Errorf("cortex: Linux cgroup-v2 containment required")
}
