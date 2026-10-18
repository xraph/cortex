package mongo

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/preparation"
)

var _ store.PreparationSnapshotter = (*Store)(nil)

// CapturePreparation collects sources under explicit snapshot read concern.
// MongoDB must support transactions, such as a replica set or sharded cluster.
func (s *Store) CapturePreparation(ctx context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if collect == nil {
		return nil, cortex.ErrPreparationUnavailable
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	session, err := s.mdb.Client().StartSession()
	if err != nil {
		return nil, fmt.Errorf("cortex/mongo: start preparation session: %w: %w", cortex.ErrPreparationUnavailable, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		session.EndSession(cleanup)
	}()
	if err = session.StartTransaction(options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority())); err != nil {
		return nil, fmt.Errorf("cortex/mongo: begin preparation: %w: %w", cortex.ErrPreparationUnavailable, err)
	}
	view := preparation.New(ctx, s, func(c context.Context) context.Context { return mongo.NewSessionContext(c, session) })
	defer view.Close()
	if collectErr := collect(view); collectErr != nil {
		return nil, collectErr
	}
	view.Close()
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err = session.CommitTransaction(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: commit preparation: %w: %w", cortex.ErrPreparationUnavailable, err)
	}
	return &run.SourceSnapshot{ID: rand.Text(), Provider: "mongo", Isolation: "snapshot", Scope: scope.Canonical()}, nil
}
