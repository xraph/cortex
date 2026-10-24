package engine

import (
	"context"
	"sync"
)

// lifecycleGate keeps Start and Stop, including shutdown notification, in one
// order. A queued caller can abandon its wait without cancelling another caller.
// An uncontended Stop still signals services when its context is already done.
type lifecycleGate struct {
	once  sync.Once
	token chan struct{}
}

func (g *lifecycleGate) acquire(ctx context.Context) (func(), error) {
	g.once.Do(func() { g.token = make(chan struct{}, 1) })
	release := func() { <-g.token }
	select {
	case g.token <- struct{}{}:
		return release, nil
	default:
	}
	select {
	case g.token <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
