package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

type layout uint8

const (
	workspaceLayout layout = iota
	credentialLayout
)

// NewWorkspaces binds an existing private root-owned workspace directory on Linux.
// Fence must durably prevent starts and confirm complete service/cgroup drain.
// It must honor cancellation; execution permission alone is not cleanup proof.
func NewWorkspaces(root string, fence func(context.Context, id.ID) error) (*Recovery, error) {
	return productionRecovery(root, fence, workspaceLayout)
}

// Discover returns a sorted bounded snapshot of canonical retained job IDs.
// Invalid entries or overflow fail the entire snapshot before any cleanup.
func (r *Recovery) Discover(ctx context.Context, maximum int) ([]id.ID, error) {
	if maximum < 1 || maximum > 10000 {
		return nil, fmt.Errorf("cortex: invalid workspace discovery limit")
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := operation.Err(); err != nil {
		return nil, err
	}
	return r.discover(operation, maximum)
}

// Reclaim fences the service before inspecting its retained directory. Only
// the selected layout on the original root filesystem may be removed. Partial
// setup and already absent directories are valid; uncertainty requires retry.
func (r *Recovery) Reclaim(ctx context.Context, key id.ID) error {
	if key.IsNil() || key.Prefix() != sandbox.PrefixJob {
		return fmt.Errorf("cortex: invalid workspace cleanup identity")
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := operation.Err(); err != nil {
		return err
	}
	if err := r.fence(operation, key); err != nil {
		return err
	}
	if err := operation.Err(); err != nil {
		return err
	}
	return r.reclaim(operation, key)
}

// NewCredentials binds the private launch directory. Cleanup removes only the
// fixed claim file or interrupted temporary file after service fencing.
func NewCredentials(root string, fence func(context.Context, id.ID) error) (*Recovery, error) {
	return productionRecovery(root, fence, credentialLayout)
}
