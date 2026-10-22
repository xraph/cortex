package sandboxbroker_test

import (
	"context"
	"testing"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func TestReaperCancelsExpiredWorkAfterRestart(t *testing.T) {
	b, _, cfg, ctx := setup(t)
	j := job(t)
	if _, err := b.Submit(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := broker.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err = next.Reconcile(context.Background(), time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := next.Collect(ctx, j.ID)
	if err != nil || result.State != sandbox.StateCancelled {
		t.Fatalf("%+v %v", result, err)
	}
}
