// Package mongo persists optimization records in MongoDB snapshot transactions.
package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/internal/state"
)

const (
	recordCollection = "cortex_optimization_records"
	chunkCollection  = "cortex_optimization_chunks"
	scopeCollection  = "cortex_optimization_scopes"
	schemaCollection = "cortex_optimization_schema"
)

// Store uses the shared optimization rules with MongoDB transaction fencing.
type Store struct{ *state.Store }

// New binds an open Grove MongoDB connection. Migrate verifies transaction
// support before the host accepts work; standalone servers are unsupported.
func New(db *grove.DB) *Store {
	b := &backend{db: db, mongo: mongodriver.Unwrap(db)}
	return &Store{Store: state.NewBackend(b)}
}

type backend struct {
	db    *grove.DB
	mongo *mongodriver.MongoDB
}

func (b *backend) Ping(ctx context.Context) error { return b.db.Ping(ctx) }
func (b *backend) Close() error                   { return b.db.Close() }

func databaseError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var serverError mongo.ServerError
	if mongo.IsDuplicateKeyError(err) || errors.As(err, &serverError) && serverError.HasErrorLabel("TransientTransactionError") {
		return fmt.Errorf("%w: mongo %s: %w", optimization.ErrConflict, operation, err)
	}
	return fmt.Errorf("%w: mongo %s: %w", optimization.ErrUnavailable, operation, err)
}

func (b *backend) transact(ctx context.Context, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	session, err := b.mongo.Client().StartSession()
	if err != nil {
		return databaseError("start session", err)
	}
	committed := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if !committed {
			_ = session.AbortTransaction(cleanup) //nolint:errcheck // Preserve the operation error; cleanup is bounded and ends the session.
		}
		session.EndSession(cleanup)
	}()
	if err = session.StartTransaction(options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()).SetReadPreference(readpref.Primary())); err != nil {
		return databaseError("start transaction", err)
	}
	if callbackErr := fn(mongo.NewSessionContext(ctx, session)); callbackErr != nil {
		return callbackErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if err = session.CommitTransaction(ctx); err != nil {
		// An unacknowledged commit is not evidence that the mutation failed.
		// Let the caller reconcile persisted identities; never replay its callback.
		return fmt.Errorf("%w: mongo commit outcome unavailable: %w", optimization.ErrUnavailable, err)
	}
	committed = true
	return nil
}
