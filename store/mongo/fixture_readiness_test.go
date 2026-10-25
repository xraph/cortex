package mongo

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Replica-set initiation can finish before the election. Direct connections
// can read during that window, but migrations need a writable primary.
func requireMongoPrimary(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := waitMongoPrimary(ctx, func(probeCtx context.Context) (bool, error) {
		var hello struct {
			Writable bool `bson:"isWritablePrimary"`
		}
		probeErr := s.mdb.Database().RunCommand(probeCtx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
		return hello.Writable, probeErr
	})
	if err != nil {
		t.Fatalf("wait for writable mongodb primary: %v", err)
	}
}

func waitMongoPrimary(ctx context.Context, probe func(context.Context) (bool, error)) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, lastErr)
		}
		ready, err := probe(ctx)
		lastErr = err
		if contextErr := ctx.Err(); contextErr != nil {
			return errors.Join(contextErr, lastErr)
		}
		if err == nil && ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}
