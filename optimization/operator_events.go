package optimization

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type operatorEventCursor struct {
	Scope        string `json:"scope"`
	ExperimentID id.ID  `json:"experiment_id"`
	After        int64  `json:"after"`
}

// ListEvents binds reconnect cursors to the exact scope and experiment.
func (o *Optimizer) ListEvents(ctx context.Context, experimentID id.ID, query PageQuery) (ResourcePage[Event], error) {
	var empty ResourcePage[Event]
	if _, err := o.readExperiment(ctx, experimentID); err != nil {
		return empty, err
	}
	query, err := operatorPage(query)
	if err != nil {
		return empty, err
	}
	scope := sha256.Sum256([]byte(cortex.ScopeFromContext(ctx).Canonical()))
	cursor := operatorEventCursor{Scope: hex.EncodeToString(scope[:]), ExperimentID: experimentID}
	if query.Cursor != "" {
		if len(query.Cursor) > 1024 {
			return empty, ErrInvalidSpec
		}
		data, decodeErr := base64.RawURLEncoding.Strict().DecodeString(query.Cursor)
		if decodeErr != nil {
			return empty, ErrInvalidSpec
		}
		var received operatorEventCursor
		if decodeErr = json.Unmarshal(data, &received); decodeErr != nil || received.Scope != cursor.Scope || received.ExperimentID != experimentID || received.After < 1 {
			return empty, ErrInvalidSpec
		}
		canonical, encodeErr := json.Marshal(received)
		if encodeErr != nil || base64.RawURLEncoding.EncodeToString(canonical) != query.Cursor {
			return empty, ErrInvalidSpec
		}
		cursor = received
	}
	page, err := o.config.Repository.ListEvents(copyScopeContext(ctx), experimentID, cursor.After, query.Limit)
	if err != nil {
		return empty, err
	}
	if page.NextCursor != "" {
		after, parseErr := strconv.ParseInt(page.NextCursor, 10, 64)
		if parseErr != nil || after <= cursor.After {
			return empty, ErrIncompleteEvidence
		}
		cursor.After = after
		data, encodeErr := json.Marshal(cursor)
		if encodeErr != nil {
			return empty, encodeErr
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return resourcePage(page), nil
}
