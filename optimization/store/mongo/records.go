package mongo

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/internal/scoped"
	"github.com/xraph/cortex/optimization/store/internal/state"
)

const chunkBytes = 4 << 20

type scopeFields struct {
	Hash  string `bson:"scope_hash"`
	Canon string `bson:"scope_canon"`
	L0    string `bson:"scope_l0"`
	L1    string `bson:"scope_l1"`
	L2    string `bson:"scope_l2"`
}

func fields(scope scoped.Index) scopeFields {
	return scopeFields{Hash: scope.Hash, Canon: scope.Canon, L0: scope.Levels[0], L1: scope.Levels[1], L2: scope.Levels[2]}
}

func scopeFilter(scope scoped.Index) bson.D {
	return bson.D{{Key: "scope_hash", Value: scope.Hash}, {Key: "scope_canon", Value: scope.Canon}, {Key: "scope_l0", Value: scope.Levels[0]}, {Key: "scope_l1", Value: scope.Levels[1]}, {Key: "scope_l2", Value: scope.Levels[2]}}
}

type recordDocument struct {
	Scope    scopeFields `bson:",inline"`
	ID       string      `bson:"_id"`
	RecordID string      `bson:"record_id"`
	Schema   int         `bson:"record_schema"`
	Kind     string      `bson:"kind"`
	Parent   string      `bson:"parent_key"`
	Key      string      `bson:"item_key"`
	Digest   string      `bson:"digest"`
	Size     int64       `bson:"body_size"`
	Chunks   int64       `bson:"chunks"`
}

type chunkDocument struct {
	Scope    scopeFields `bson:",inline"`
	ID       string      `bson:"_id"`
	RecordID string      `bson:"record_id"`
	Digest   string      `bson:"digest"`
	Index    int64       `bson:"part"`
	Data     []byte      `bson:"data"`
}

func (b *backend) Load(ctx context.Context, key string) ([]state.Record, error) {
	scope, err := scoped.New(ctx, key)
	if err != nil {
		return nil, err
	}
	var records []state.Record
	err = b.transact(ctx, func(session context.Context) error {
		var loadErr error
		records, loadErr = b.load(session, scope)
		return loadErr
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func documents[T any](ctx context.Context, collection *mongo.Collection, filter bson.D) (result []T, resultErr error) {
	cursor, err := collection.Find(ctx, filter)
	if err != nil {
		return nil, databaseError("find records", err)
	}
	defer func() { resultErr = errors.Join(resultErr, databaseError("close cursor", cursor.Close(ctx))) }()
	for cursor.Next(ctx) {
		var value T
		if err = cursor.Decode(&value); err != nil {
			return nil, databaseError("decode record", err)
		}
		result = append(result, value)
	}
	return result, databaseError("read cursor", cursor.Err())
}

func (b *backend) load(ctx context.Context, scope scoped.Index) ([]state.Record, error) {
	headers, err := documents[recordDocument](ctx, b.mongo.Collection(recordCollection), scopeFilter(scope))
	if err != nil {
		return nil, err
	}
	chunks, err := documents[chunkDocument](ctx, b.mongo.Collection(chunkCollection), scopeFilter(scope))
	if err != nil {
		return nil, err
	}
	byID := make(map[string]recordDocument, len(headers))
	for _, header := range headers {
		if header.ID != scope.Hash+"/"+header.RecordID || header.Scope != fields(scope) || header.Size <= 0 || header.Chunks != (header.Size-1)/chunkBytes+1 {
			return nil, corruptRecords()
		}
		if _, exists := byID[header.RecordID]; exists {
			return nil, corruptRecords()
		}
		byID[header.RecordID] = header
	}
	parts := make(map[string][]chunkDocument, len(headers))
	for _, chunk := range chunks {
		header, exists := byID[chunk.RecordID]
		if !exists || chunk.Scope != fields(scope) || chunk.Digest != header.Digest || chunk.Index < 0 || chunk.Index >= header.Chunks || chunk.ID != chunkID(header.ID, header.Digest, chunk.Index) || len(chunk.Data) > chunkBytes || len(chunk.Data) == 0 {
			return nil, corruptRecords()
		}
		parts[chunk.RecordID] = append(parts[chunk.RecordID], chunk)
	}
	records := make([]state.Record, 0, len(headers))
	for _, header := range headers {
		segments := parts[header.RecordID]
		if int64(len(segments)) != header.Chunks {
			return nil, corruptRecords()
		}
		slices.SortFunc(segments, func(a, b chunkDocument) int {
			if a.Index < b.Index {
				return -1
			}
			if a.Index > b.Index {
				return 1
			}
			return 0
		})
		var body []byte
		for index, segment := range segments {
			if segment.Index != int64(index) || index < len(segments)-1 && len(segment.Data) != chunkBytes {
				return nil, corruptRecords()
			}
			body = append(body, segment.Data...)
		}
		if int64(len(body)) != header.Size {
			return nil, corruptRecords()
		}
		records = append(records, state.Record{Schema: header.Schema, Scope: header.Scope.Canon, Kind: header.Kind, Parent: header.Parent, Key: header.Key, ID: header.RecordID, Digest: header.Digest, Body: body})
	}
	return records, nil
}

func corruptRecords() error {
	return fmt.Errorf("%w: mongo record chunks are inconsistent", optimization.ErrUnavailable)
}

func chunkID(recordID, digest string, index int64) string {
	return recordID + "/" + digest + "/" + strconv.FormatInt(index, 10)
}

func (b *backend) Update(ctx context.Context, key string, update func([]state.Record) ([]state.Mutation, error)) error {
	scope, err := scoped.New(ctx, key)
	if err != nil {
		return err
	}
	return b.transact(ctx, func(session context.Context) error {
		var fence struct {
			Canon string `bson:"scope_canon"`
		}
		err := b.mongo.Collection(scopeCollection).FindOneAndUpdate(session,
			bson.D{{Key: "_id", Value: scope.Hash}},
			bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: "scope_canon", Value: scope.Canon}}}, {Key: "$set", Value: bson.D{{Key: "fence", Value: rand.Text()}}}},
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&fence)
		if err != nil {
			return databaseError("fence scope", err)
		}
		if fence.Canon != scope.Canon {
			return fmt.Errorf("%w: stored scope digest collision", optimization.ErrUnavailable)
		}
		records, err := b.load(session, scope)
		if err != nil {
			return err
		}
		changes, err := update(records)
		if err != nil {
			return err
		}
		for _, change := range changes {
			if applyErr := b.apply(session, scope, change); applyErr != nil {
				return applyErr
			}
		}
		return nil
	})
}

func (b *backend) apply(ctx context.Context, scope scoped.Index, change state.Mutation) error {
	record := change.Record
	if record.Scope != scope.Canon {
		return optimization.ErrPolicyDenied
	}
	if len(record.Body) == 0 {
		return optimization.ErrInvalidSpec
	}
	header := recordDocument{Scope: fields(scope), ID: scope.Hash + "/" + record.ID, RecordID: record.ID, Schema: record.Schema, Kind: record.Kind, Parent: record.Parent, Key: record.Key, Digest: record.Digest, Size: int64(len(record.Body)), Chunks: int64((len(record.Body)-1)/chunkBytes + 1)}
	for index := int64(0); index < header.Chunks; index++ {
		start := int(index) * chunkBytes
		end := min(start+chunkBytes, len(record.Body))
		chunk := chunkDocument{Scope: fields(scope), ID: chunkID(header.ID, header.Digest, index), RecordID: record.ID, Digest: record.Digest, Index: index, Data: record.Body[start:end]}
		if _, err := b.mongo.Collection(chunkCollection).InsertOne(ctx, chunk); err != nil {
			return databaseError("insert record chunk", err)
		}
	}
	if change.ExpectedDigest == "" {
		_, err := b.mongo.Collection(recordCollection).InsertOne(ctx, header)
		return databaseError("insert record", err)
	}
	filter := append(scopeFilter(scope), bson.E{Key: "_id", Value: header.ID}, bson.E{Key: "record_id", Value: record.ID}, bson.E{Key: "kind", Value: record.Kind}, bson.E{Key: "parent_key", Value: record.Parent}, bson.E{Key: "item_key", Value: record.Key}, bson.E{Key: "digest", Value: change.ExpectedDigest})
	result, err := b.mongo.Collection(recordCollection).ReplaceOne(ctx, filter, header)
	if err != nil {
		return databaseError("replace record", err)
	}
	if result.MatchedCount != 1 {
		return optimization.ErrConflict
	}
	oldChunks := append(scopeFilter(scope), bson.E{Key: "record_id", Value: record.ID}, bson.E{Key: "digest", Value: change.ExpectedDigest})
	deleted, err := b.mongo.Collection(chunkCollection).DeleteMany(ctx, oldChunks)
	if err != nil {
		return databaseError("remove superseded chunks", err)
	}
	if deleted.DeletedCount == 0 {
		return corruptRecords()
	}
	return nil
}
