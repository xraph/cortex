package mongo

import (
	"context"
	"encoding/json"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store/internal/assurance"
)

const colAssurance = "cortex_assurance_records"

type assuranceDriver struct {
	store             *Store
	resultTransaction bool
}

// Keep Mongo scope overflow in the same document form used by other collections.
type assuranceDocument struct {
	assurance.Record `bson:",inline"`
	ScopeExtra       bson.M `bson:"scope_extra"`
}

func (d assuranceDriver) Insert(ctx context.Context, row assurance.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := d.store.mdb.Collection(colAssurance).InsertOne(ctx, assuranceDocument{Record: row, ScopeExtra: bson.M{}})
	if mongo.IsDuplicateKeyError(err) {
		return assurance.ErrExists
	}
	return err
}
func assuranceFilter(ctx context.Context) (bson.M, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	columns := assurance.Columns(scope)
	return bson.M{"scope_canon": scope.Canonical(), "scope_l0": columns[0], "scope_l1": columns[1], "scope_l2": columns[2]}, nil
}
func (d assuranceDriver) Select(ctx context.Context, query assurance.Query) ([]assurance.Record, error) {
	filter, err := assuranceFilter(ctx)
	if err != nil {
		return nil, err
	}
	filter["kind"] = query.Kind
	direction, after := 1, "$gt"
	if query.Reverse {
		direction, after = -1, "$lt"
	}
	if query.ID != "" {
		filter["id"] = query.ID
	}
	if query.Digest != "" {
		filter["digest"] = query.Digest
	}
	if query.RunID != "" {
		filter["run_id"] = query.RunID
	}
	if query.After != "" {
		filter["id"] = bson.M{after: query.After}
	}
	cursor, err := d.store.mdb.Collection(colAssurance).Find(ctx, filter, options.Find().SetLimit(int64(query.Limit)).SetSort(bson.D{{Key: "id", Value: direction}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var documents []assuranceDocument
	if checkErr := cursor.All(ctx, &documents); checkErr != nil {
		return nil, checkErr
	}
	records := make([]assurance.Record, 0, len(documents))
	for _, document := range documents {
		extra, marshalErr := json.Marshal(document.ScopeExtra)
		if marshalErr != nil {
			return nil, marshalErr
		}
		document.Record.ScopeExtra = string(extra)
		if d.resultTransaction && query.Kind == assurance.KindEvidence {
			// A real write makes concurrent tombstoning conflict with this transaction.
			// Snapshot reads alone would allow a revoked artifact to be committed.
			fence, fenceErr := assuranceFilter(ctx)
			if fenceErr != nil {
				return nil, fenceErr
			}
			fence["kind"], fence["id"], fence["body"] = assurance.KindEvidence, document.ID, document.Body
			updated, updateErr := d.store.mdb.Collection(colAssurance).UpdateOne(ctx, fence, bson.M{"$inc": bson.M{"publication_fence": int64(1)}})
			if updateErr != nil {
				return nil, updateErr
			}
			if updated.MatchedCount != 1 {
				return nil, assurance.ErrExists
			}
		}
		records = append(records, document.Record)
	}
	return records, nil
}
func (d assuranceDriver) ReplaceEvidence(ctx context.Context, row assurance.Record, expectedBody string) error {
	filter, err := assuranceFilter(ctx)
	if err != nil {
		return err
	}
	filter["kind"] = assurance.KindEvidence
	filter["id"] = row.ID
	filter["body"] = expectedBody
	filter["deleted"] = false
	result, err := d.store.mdb.Collection(colAssurance).UpdateOne(ctx, filter, bson.M{"$set": bson.M{"body": row.Body, "deleted": true}})
	if err != nil {
		return err
	}
	if result.ModifiedCount != 1 {
		return assurance.ErrExists
	}
	return nil
}
