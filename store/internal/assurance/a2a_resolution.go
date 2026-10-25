package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const kindAskResolution = "a2a_ask_resolution"

// AskResolutionTransaction holds an exact-scope ask lock throughout the callback.
type AskResolutionTransaction interface {
	Driver
	ReadPendingAsk(context.Context, string) (*a2a.PendingAsk, error)
	ClaimResolvedAsk(context.Context, string, time.Time) error
}

// AskResolutionDriver serializes claims and immutable outcome insertion.
// Callbacks contain only storage work and may be replayed by MongoDB.
type AskResolutionDriver interface {
	ReadPendingAsk(context.Context, string) (*a2a.PendingAsk, error)
	TransactAskResolution(context.Context, string, func(context.Context, AskResolutionTransaction) error) error
}

func validAskToken(token string) bool {
	return token != "" && len(token) <= 1024 && utf8.ValidString(token)
}
func resolutionPartition(ctx context.Context) string { return "a2a:" + a2a.RoutingOrchestration(ctx) }
func resolutionKey(ctx context.Context, token string) string {
	return resolutionPartition(ctx) + ":" + evidence.Digest([]byte(token))
}

func askBinding(ask *a2a.PendingAsk) ([]byte, error) {
	snapshot := *ask
	snapshot.Entity, snapshot.ClaimedAt = cortex.Entity{}, nil
	return json.Marshal(snapshot)
}

func validResolutionAsk(ctx context.Context, ask *a2a.PendingAsk) error {
	if ask == nil || !validAskToken(ask.ReplyWith) || ask.MessageID.Prefix() != id.PrefixMessage || ask.ConversationID.Prefix() != id.PrefixConversation || ask.AskerRunID.Prefix() != id.PrefixAgentRun || ask.AskerAgent == "" || ask.ToolCallID == "" || ask.Expected.Agent == "" || ask.OrchestrationID != a2a.RoutingOrchestration(ctx) {
		return a2a.ErrAskResolutionConflict
	}
	for _, value := range []string{ask.AskerAgent, ask.ToolCallID, ask.Expected.Agent, ask.Expected.Node} {
		if !utf8.ValidString(value) {
			return a2a.ErrAskResolutionConflict
		}
	}
	return require(ctx, ask.Scope)
}

func resolutionPayload(ask *a2a.PendingAsk, reply a2a.AskReply) ([]byte, error) {
	if len(reply.Replies) == 0 || len(reply.Replies) > 256 {
		return nil, a2a.ErrAskResolutionConflict
	}
	for _, item := range reply.Replies {
		if !a2a.Performative(item.Performative).AnswersAsk() || item.ConversationID != ask.ConversationID.String() || item.Sender == "" || !utf8.ValidString(item.Content) || !utf8.ValidString(item.Sender) {
			return nil, a2a.ErrAskResolutionConflict
		}
	}
	payload, err := json.Marshal(reply)
	if err != nil || len(payload) > 4<<20 {
		return nil, a2a.ErrAskResolutionConflict
	}
	return payload, nil
}

// ReadPendingAsk reads the retained ask, including an already claimed ask.
func (s *Repository) ReadPendingAsk(ctx context.Context, token string) (*a2a.PendingAsk, error) {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	if !validAskToken(token) {
		return nil, a2a.ErrAskResolutionConflict
	}
	driver, ok := s.driver.(AskResolutionDriver)
	if !ok {
		return nil, a2a.ErrAskResolutionUnavailable
	}
	ask, err := driver.ReadPendingAsk(ctx, token)
	if err != nil {
		return nil, err
	}
	if checkErr := validResolutionAsk(ctx, ask); checkErr != nil {
		return nil, checkErr
	}
	if ask.ReplyWith != token {
		return nil, a2a.ErrAskResolutionConflict
	}
	return ask, nil
}

// SealAskResolution preserves the exact selected payload with the ask claim.
func (s *Repository) SealAskResolution(ctx context.Context, input *a2a.PendingAsk, reply a2a.AskReply) (a2a.AskResolution, error) {
	var result a2a.AskResolution
	if err := validResolutionAsk(ctx, input); err != nil {
		return result, err
	}
	binding, err := askBinding(input)
	if err != nil || len(binding) > 64<<10 {
		return result, a2a.ErrAskResolutionConflict
	}
	var ask a2a.PendingAsk
	if decodeErr := json.Unmarshal(binding, &ask); decodeErr != nil {
		return result, decodeErr
	}
	payload, err := resolutionPayload(&ask, reply)
	if err != nil {
		return result, err
	}
	driver, ok := s.driver.(AskResolutionDriver)
	if !ok {
		return result, a2a.ErrAskResolutionUnavailable
	}
	bindingDigest, outcomeDigest := evidence.Digest(binding), evidence.Digest(payload)
	err = driver.TransactAskResolution(ctx, ask.ReplyWith, func(tc context.Context, tx AskResolutionTransaction) error {
		repo := &Repository{driver: tx, clock: s.clock}
		prior, priorErr := repo.GetAskResolution(tc, ask.ReplyWith)
		if priorErr == nil {
			if prior.BindingDigest != bindingDigest {
				return a2a.ErrAskResolutionConflict
			}
			if prior.OutcomeDigest != outcomeDigest {
				return a2a.ErrAskAlreadyClaimed
			}
			result = prior
			return tc.Err()
		}
		if !errors.Is(priorErr, a2a.ErrAskResolutionNotFound) {
			return priorErr
		}
		current, readErr := tx.ReadPendingAsk(tc, ask.ReplyWith)
		if readErr != nil {
			return readErr
		}
		if checkErr := validResolutionAsk(tc, current); checkErr != nil {
			return checkErr
		}
		currentBinding, encodeErr := askBinding(current)
		if encodeErr != nil || evidence.Digest(currentBinding) != bindingDigest {
			return a2a.ErrAskResolutionConflict
		}
		if current.ClaimedAt != nil {
			return a2a.ErrAskAlreadyClaimed
		}
		at := s.clock.Now()
		if claimErr := tx.ClaimResolvedAsk(tc, ask.ReplyWith, at); claimErr != nil {
			return claimErr
		}
		retained := ask
		retained.ClaimedAt = &at
		result = a2a.AskResolution{SchemaVersion: 1, ResolutionID: resolutionKey(tc, ask.ReplyWith), Ask: retained, Payload: string(payload), BindingDigest: bindingDigest, OutcomeDigest: outcomeDigest, SealedAt: at}
		data, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			return encodeErr
		}
		if len(data) > 6<<20 {
			return a2a.ErrAskResolutionConflict
		}
		row, recordErr := record(kindAskResolution, result.ResolutionID, evidence.Digest(data), resolutionPartition(tc), ask.Scope, result)
		if recordErr != nil {
			return recordErr
		}
		if insertErr := repo.insert(tc, row); insertErr != nil {
			return insertErr
		}
		result.Fresh = true
		return tc.Err()
	})
	if err != nil {
		return a2a.AskResolution{}, err
	}
	return result, nil
}

func decodeAskResolution(ctx context.Context, row Record) (a2a.AskResolution, error) {
	var result a2a.AskResolution
	if err := validateRecord(ctx, row); err != nil {
		return result, err
	}
	if row.Deleted || row.Kind != kindAskResolution || row.RunID != resolutionPartition(ctx) || len(row.Body) > 6<<20 || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &result) != nil {
		return a2a.AskResolution{}, a2a.ErrAskResolutionConflict
	}
	if result.SchemaVersion != 1 || validResolutionAsk(ctx, &result.Ask) != nil || result.ResolutionID != row.ID || result.ResolutionID != resolutionKey(ctx, result.Ask.ReplyWith) || result.SealedAt.IsZero() || result.Ask.ClaimedAt == nil || !result.Ask.ClaimedAt.Equal(result.SealedAt) {
		return a2a.AskResolution{}, a2a.ErrAskResolutionConflict
	}
	binding, err := askBinding(&result.Ask)
	if err != nil || result.BindingDigest != evidence.Digest(binding) || result.OutcomeDigest != evidence.Digest([]byte(result.Payload)) {
		return a2a.AskResolution{}, a2a.ErrAskResolutionConflict
	}
	var reply a2a.AskReply
	if err = json.Unmarshal([]byte(result.Payload), &reply); err != nil {
		return a2a.AskResolution{}, a2a.ErrAskResolutionConflict
	}
	if _, err = resolutionPayload(&result.Ask, reply); err != nil {
		return a2a.AskResolution{}, err
	}
	result.Fresh = false
	return result, nil
}

// GetAskResolution returns retained evidence, never authority to resume again.
func (s *Repository) GetAskResolution(ctx context.Context, token string) (a2a.AskResolution, error) {
	if !validAskToken(token) {
		return a2a.AskResolution{}, a2a.ErrAskResolutionConflict
	}
	row, err := s.get(ctx, Query{Kind: kindAskResolution, ID: resolutionKey(ctx, token), RunID: resolutionPartition(ctx)})
	if errors.Is(err, ErrAbsent) {
		return a2a.AskResolution{}, a2a.ErrAskResolutionNotFound
	}
	if err != nil {
		return a2a.AskResolution{}, err
	}
	resolution, err := decodeAskResolution(ctx, row)
	if err != nil {
		return a2a.AskResolution{}, err
	}
	if resolution.Ask.ReplyWith != token {
		return a2a.AskResolution{}, a2a.ErrAskResolutionConflict
	}
	return resolution, nil
}

// ListAskResolutions pages immutable outcomes within one exact scope and partition.
func (s *Repository) ListAskResolutions(ctx context.Context, after string, limit int) (a2a.AskResolutionPage, error) {
	var page a2a.AskResolutionPage
	if _, err := cortex.RequireScope(ctx); err != nil {
		return page, err
	}
	prefix := resolutionPartition(ctx) + ":"
	if after != "" && (!strings.HasPrefix(after, prefix) || !evidence.ValidDigest(strings.TrimPrefix(after, prefix))) {
		return page, a2a.ErrAskResolutionConflict
	}
	if limit == 0 {
		limit = 64
	}
	if limit < 1 || limit > 256 {
		return page, a2a.ErrAskResolutionConflict
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindAskResolution, RunID: resolutionPartition(ctx), After: after, Limit: limit + 1})
	if err != nil {
		return page, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	for _, row := range rows {
		resolution, decodeErr := decodeAskResolution(ctx, row)
		if decodeErr != nil {
			return a2a.AskResolutionPage{}, decodeErr
		}
		page.Resolutions = append(page.Resolutions, resolution)
	}
	if more {
		page.Next = rows[len(rows)-1].ID
	}
	return page, ctx.Err()
}
