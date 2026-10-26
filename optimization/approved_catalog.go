package optimization

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// ApprovedCatalogConfig registers immutable choices with current host access.
// Authorize checks each source's grants using the supplied context. Prepare is
// optional for read-only catalogs; it freezes host policy and executable content
// for an authorized selection and must not run trials or start background work.
type ApprovedCatalogConfig struct {
	Choices   []CatalogChoice
	Authorize func(context.Context, CatalogChoice) error
	Prepare   func(context.Context, CreationTarget) (ExperimentBundle, error)
}

// ApprovedCatalog shares a detached registration between listing and creation.
// Rebuild it to publish new registrations. Pagination detects changed visible
// revisions instead of mixing old and new catalog content across pages.
type ApprovedCatalog struct {
	choices   map[string]CatalogChoice
	keys      []string
	authorize func(context.Context, CatalogChoice) error
	prepare   func(context.Context, CreationTarget) (ExperimentBundle, error)
}

// NewApprovedCatalog validates and freezes up to ten thousand host choices.
func NewApprovedCatalog(config ApprovedCatalogConfig) (*ApprovedCatalog, error) {
	if config.Authorize == nil || len(config.Choices) > 10000 {
		return nil, ErrInvalidSpec
	}
	choices, err := copyModelJSON(&struct{ Choices []CatalogChoice }{config.Choices}, 16<<20)
	if err != nil {
		return nil, err
	}
	result := &ApprovedCatalog{choices: map[string]CatalogChoice{}, authorize: config.Authorize, prepare: config.Prepare}
	for _, choice := range choices.Choices {
		if validationErr := choice.validate(); validationErr != nil {
			return nil, validationErr
		}
		key := catalogChoiceKey(choice)
		if _, found := result.choices[key]; found {
			return nil, ErrInvalidSpec
		}
		result.choices[key] = choice
		result.keys = append(result.keys, key)
	}
	slices.Sort(result.keys)
	return result, nil
}

// AuthorizeChoice requires an exact registered revision in the context scope.
func (c *ApprovedCatalog) AuthorizeChoice(ctx context.Context, choice CatalogChoice) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	if choice.Scope.Canonical() != scope.Canonical() {
		return ErrPolicyDenied
	}
	registered, ok := c.choices[catalogChoiceKey(choice)]
	if !ok || !reflect.DeepEqual(registered, choice) {
		return ErrPolicyDenied
	}
	detached, err := copyModelJSON(&registered, 64<<10)
	if err != nil {
		return err
	}
	return c.authorize(copyScopeContext(ctx), *detached)
}

type approvedCatalogCursor struct {
	Revision string `json:"revision"`
	After    string `json:"after"`
}

// List reads currently authorized registrations. A source outage is an error,
// while an ordinary source permission denial removes that choice from discovery.
func (c *ApprovedCatalog) List(ctx context.Context, target CatalogTarget) (CatalogResult, error) {
	var empty CatalogResult
	if cortex.ScopeFromContext(ctx).IsZero() {
		return empty, cortex.ErrNoScope
	}
	if target.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || !ValidOpaqueKey(target.Actor.Subject) {
		return empty, ErrPolicyDenied
	}
	if target.Limit < 1 || target.Limit > 100 || (target.Kind != "" && !validCatalogKind(target.Kind)) {
		return empty, ErrInvalidSpec
	}
	rows := []CatalogChoice{}
	for _, key := range c.keys {
		choice := c.choices[key]
		if choice.Scope.Canonical() != target.Scope.Canonical() || (target.Kind != "" && choice.Kind != target.Kind) {
			continue
		}
		if err := c.AuthorizeChoice(ctx, choice); err != nil {
			if errors.Is(err, ErrPolicyDenied) {
				continue
			}
			return empty, err
		}
		rows = append(rows, choice)
	}
	encoded, err := canonical.Marshal(rows)
	if err != nil {
		return empty, err
	}
	cursor := approvedCatalogCursor{Revision: digestBytes(encoded)}
	if target.Cursor != "" {
		if len(target.Cursor) > 512 {
			return empty, ErrInvalidSpec
		}
		data, decodeErr := base64.RawURLEncoding.Strict().DecodeString(target.Cursor)
		var previous approvedCatalogCursor
		if decodeErr != nil || canonical.Decode(data, &previous) != nil || !validDigest(previous.Revision) || !validDigest(previous.After) {
			return empty, ErrInvalidSpec
		}
		canonicalData, encodeErr := canonical.Marshal(previous)
		if encodeErr != nil || base64.RawURLEncoding.EncodeToString(canonicalData) != target.Cursor {
			return empty, ErrInvalidSpec
		}
		if previous.Revision != cursor.Revision {
			return empty, ErrConflict
		}
		cursor = previous
	}
	result := CatalogResult{Target: target, Page: Page[CatalogChoice]{Items: []CatalogChoice{}}}
	for _, choice := range rows {
		key := catalogChoiceKey(choice)
		if cursor.After != "" && strings.Compare(key, cursor.After) <= 0 {
			continue
		}
		if len(result.Page.Items) == target.Limit {
			data, encodeErr := canonical.Marshal(cursor)
			if encodeErr != nil {
				return empty, encodeErr
			}
			result.Page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
			break
		}
		result.Page.Items = append(result.Page.Items, choice)
		cursor.After = key
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return empty, contextErr
	}
	detached, err := copyModelJSON(&result, 1<<20)
	if err != nil {
		return empty, err
	}
	return *detached, nil
}

// Authorize checks all selected revisions, including mapped evaluation access.
func (c *ApprovedCatalog) Authorize(ctx context.Context, selection CreationSelection) error {
	if err := selection.validate(); err != nil {
		return err
	}
	for _, item := range []struct {
		kind CatalogKind
		ref  ContentRef
	}{{CatalogAgent, selection.Agent}, {CatalogEvaluation, selection.Evaluation}, {CatalogController, selection.Controller}, {CatalogEnvironment, selection.Environment}} {
		key := catalogChoiceKey(CatalogChoice{Scope: cortex.ScopeFromContext(ctx), Kind: item.kind, Ref: item.ref})
		choice, found := c.choices[key]
		if !found || choice.Ref != item.ref {
			return ErrPolicyDenied
		}
		if err := c.AuthorizeChoice(ctx, choice); err != nil {
			return err
		}
	}
	return nil
}

// Prepare delegates freezing only after authorizing registered source revisions.
func (c *ApprovedCatalog) Prepare(ctx context.Context, target CreationTarget) (ExperimentBundle, error) {
	var empty ExperimentBundle
	if target.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return empty, ErrPolicyDenied
	}
	if err := c.Authorize(ctx, target.Selection); err != nil {
		return empty, err
	}
	if c.prepare == nil {
		return empty, ErrUnavailable
	}
	detached, err := copyModelJSON(&target, 64<<10)
	if err != nil {
		return empty, err
	}
	result, err := c.prepare(copyScopeContext(ctx), *detached)
	if err != nil {
		return empty, err
	}
	if grantErr := c.Authorize(ctx, target.Selection); grantErr != nil {
		return empty, grantErr
	}
	return result, nil
}
