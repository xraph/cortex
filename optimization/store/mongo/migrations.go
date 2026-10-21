package mongo

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/xraph/cortex/optimization"
)

type schemaDocument struct {
	ID      string `bson:"_id"`
	Version int    `bson:"version"`
}

func (b *backend) Migrate(ctx context.Context) error {
	// A real operation in a snapshot transaction proves support. Merely
	// starting a session succeeds even against a standalone server.
	if err := b.transact(ctx, func(session context.Context) error {
		var schema schemaDocument
		err := b.mongo.Collection(schemaCollection).FindOne(session, bson.D{{Key: "_id", Value: "schema"}}).Decode(&schema)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
		if err != nil {
			return databaseError("probe transactions", err)
		}
		return checkSchema(schema)
	}); err != nil {
		return err
	}
	schemas := b.mongo.Database().Collection(schemaCollection, options.Collection().SetReadPreference(readpref.Primary()).SetWriteConcern(writeconcern.Majority()))
	if _, err := schemas.UpdateOne(ctx, bson.D{{Key: "_id", Value: "schema"}}, bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: "version", Value: 1}}}}, options.UpdateOne().SetUpsert(true)); err != nil {
		return databaseError("initialize schema", err)
	}
	var schema schemaDocument
	if err := schemas.FindOne(ctx, bson.D{{Key: "_id", Value: "schema"}}).Decode(&schema); err != nil {
		return databaseError("read schema", err)
	}
	if err := checkSchema(schema); err != nil {
		return err
	}
	// Create collections before accepting transactions. Implicit collection
	// creation inside the first scope fence leaves competing transactions with
	// separate catalog snapshots rather than one shared lock document.
	for _, collection := range []string{scopeCollection, recordCollection, chunkCollection} {
		if err := b.mongo.Database().CreateCollection(ctx, collection); err != nil {
			var commandError mongo.CommandError
			if !errors.As(err, &commandError) || commandError.Code != 48 {
				return databaseError("create collection", err)
			}
		}
	}
	for _, collection := range []string{recordCollection, chunkCollection} {
		keys := bson.D{{Key: "scope_hash", Value: 1}, {Key: "record_id", Value: 1}}
		if collection == recordCollection {
			keys = bson.D{{Key: "scope_hash", Value: 1}, {Key: "kind", Value: 1}, {Key: "parent_key", Value: 1}}
		}
		if _, err := b.mongo.Collection(collection).Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys, Options: options.Index().SetName("scope_records")}); err != nil {
			return databaseError("create index", err)
		}
	}
	return nil
}

func checkSchema(schema schemaDocument) error {
	if schema.ID != "schema" || schema.Version != 1 {
		return fmt.Errorf("%w: unsupported optimization schema", optimization.ErrUnavailable)
	}
	return nil
}
