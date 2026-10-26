package mongo

import (
	"context"
	"regexp"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/assurance"
)

func (d assuranceDriver) EraseLessonPayloads(ctx context.Context, identifier id.ID) error {
	if !d.resultTransaction || identifier.IsNil() || identifier.Prefix() != "lesson" {
		return assurance.ErrExists
	}
	filter, err := assuranceFilter(ctx)
	if err != nil {
		return err
	}
	filter["kind"] = bson.M{"$in": assurance.LessonPayloadKinds()}
	filter["id"] = bson.M{"$regex": "^" + regexp.QuoteMeta(identifier.String()+"/")}
	_, err = d.store.mdb.Collection(colAssurance).UpdateMany(ctx, filter, bson.M{"$set": bson.M{"body": "{}", "deleted": true}})
	return err
}

func (d assuranceDriver) FenceLessonRevision(ctx context.Context, key, digest string) error {
	if !d.resultTransaction {
		return assurance.ErrExists
	}
	filter, err := assuranceFilter(ctx)
	if err != nil {
		return err
	}
	filter["kind"], filter["id"], filter["digest"], filter["deleted"] = "lesson", key, digest, false
	result, err := d.store.mdb.Collection(colAssurance).UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"retention_fence": int64(1)}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return assurance.ErrExists
	}
	return nil
}
