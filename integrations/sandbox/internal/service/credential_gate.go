package service

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// Fixed lanes bound coordination memory independently of lifetime job count.
// Collisions serialize unrelated jobs; they never weaken per-job exclusion.
type credentialGate struct{ lanes [32]chan struct{} }

func newCredentialGate() *credentialGate {
	gate := &credentialGate{}
	for i := range gate.lanes {
		gate.lanes[i] = make(chan struct{}, 1)
	}
	return gate
}

func (g *credentialGate) acquire(ctx context.Context, key id.ID) (func(), error) {
	if key.IsNil() || key.Prefix() != sandbox.PrefixJob {
		return nil, fmt.Errorf("cortex: invalid credential coordination identity")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hash := fnv.New32a()
	if _, err := hash.Write([]byte(key.String())); err != nil {
		return nil, err
	}
	lane := g.lanes[hash.Sum32()%uint32(len(g.lanes))]
	select {
	case lane <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-lane }
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (g *credentialGate) prepare(store *workerstore.Store, write func(context.Context, LaunchProfile, workerstore.Claim) error) func(context.Context, LaunchProfile, workerstore.Claim) error {
	return func(ctx context.Context, profile LaunchProfile, claim workerstore.Claim) error {
		release, err := g.acquire(ctx, claim.Record.Receipt.Job.ID)
		if err != nil {
			return err
		}
		defer release()
		current, err := store.VerifyClaim(ctx, claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision)
		if err != nil {
			return err
		}
		claim.Record = current
		return write(ctx, profile, claim)
	}
}

func (g *credentialGate) cleanup(remove func(context.Context, id.ID) error) func(context.Context, id.ID) error {
	return func(ctx context.Context, key id.ID) error {
		release, err := g.acquire(ctx, key)
		if err != nil {
			return err
		}
		defer release()
		return remove(ctx, key)
	}
}
