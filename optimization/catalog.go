package optimization

import (
	"context"
	"encoding/base64"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// CatalogKind names one class of host-approved creation choices.
type CatalogKind string

const (
	CatalogAgent       CatalogKind = "agent"
	CatalogEvaluation  CatalogKind = "evaluation"
	CatalogController  CatalogKind = "controller"
	CatalogEnvironment CatalogKind = "environment"
)

// CatalogChoice contains display metadata and an immutable reference. Prompt
// bodies, evaluation cases, credentials and executable policy stay with the host.
type CatalogChoice struct {
	Scope       cortex.Scope `json:"scope"`
	Kind        CatalogKind  `json:"kind"`
	Ref         ContentRef   `json:"ref"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
}

// CatalogQuery selects a bounded catalog page. An empty Kind includes all kinds.
type CatalogQuery struct {
	PageQuery
	Kind CatalogKind `json:"kind,omitempty"`
}

// CatalogTarget binds a source read to the current exact scope and caller.
// Cursor belongs to the source; the operator service wraps it for transport.
type CatalogTarget struct {
	Scope  cortex.Scope `json:"scope"`
	Actor  IdentityRef  `json:"actor"`
	Kind   CatalogKind  `json:"kind,omitempty"`
	Limit  int          `json:"limit"`
	Cursor string       `json:"cursor,omitempty"`
}

// CatalogResult identifies the source request that produced its page.
type CatalogResult struct {
	Target CatalogTarget       `json:"target"`
	Page   Page[CatalogChoice] `json:"page"`
}

// CatalogProvider lists approved references under current source permissions.
// List must filter unauthorized choices and return source failures as errors.
// AuthorizeChoice rechecks mapped source access, including Sentinel permissions.
// Both methods must honor context cancellation. Cursors must retain stable source
// revisions; changed registrations must not silently continue an older page.
type CatalogProvider interface {
	List(context.Context, CatalogTarget) (CatalogResult, error)
	AuthorizeChoice(context.Context, CatalogChoice) error
}

type catalogCursor struct {
	Version int    `json:"version"`
	Binding string `json:"binding"`
	Source  string `json:"source"`
}

func validCatalogKind(kind CatalogKind) bool {
	return kind == CatalogAgent || kind == CatalogEvaluation || kind == CatalogController || kind == CatalogEnvironment
}
func validateCatalogRef(kind CatalogKind, ref ContentRef) error {
	if !validCatalogKind(kind) || ref.Validate() != nil || len(ref.Ref) > 1024 || len(ref.Revision) > 256 {
		return ErrInvalidSpec
	}
	switch kind {
	case CatalogAgent:
		if _, err := id.ParseAgentID(ref.Ref); err != nil {
			return ErrInvalidSpec
		}
	case CatalogController:
		if _, err := ParseID(ref.Ref, PrefixVersion); err != nil {
			return ErrInvalidSpec
		}
	}
	return nil
}
func (choice CatalogChoice) validate() error {
	if _, err := ScopeKey(choice.Scope); err != nil {
		return err
	}
	if validateCatalogRef(choice.Kind, choice.Ref) != nil || !ValidOpaqueKey(choice.Name) || len(choice.Name) > 256 || len(choice.Description) > 2048 || !utf8.ValidString(choice.Description) {
		return ErrInvalidSpec
	}
	return nil
}
func catalogChoiceKey(choice CatalogChoice) string {
	// A revision may name only one digest, even if its display metadata differs.
	return catalogDigest(choice.Scope.Canonical(), string(choice.Kind), choice.Ref.Ref, choice.Ref.Revision)
}
func catalogBinding(target CatalogTarget) string {
	return catalogDigest(target.Scope.Canonical(), target.Actor.Subject, string(target.Kind))
}
func catalogDigest(parts ...string) string {
	// Length framing keeps separators inside external references unambiguous.
	var encoded string
	for _, part := range parts {
		encoded += strconv.Itoa(len(part)) + ":" + part
	}
	return digestBytes([]byte(encoded))
}
func decodeCatalogCursor(raw, binding string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > 8192 {
		return "", ErrInvalidSpec
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return "", ErrInvalidSpec
	}
	var value catalogCursor
	if decodeErr := canonical.Decode(data, &value); decodeErr != nil || value.Version != 1 || value.Binding != binding || value.Source == "" || len(value.Source) > 4096 {
		return "", ErrInvalidSpec
	}
	encoded, err := canonical.Marshal(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(encoded) != raw {
		return "", ErrInvalidSpec
	}
	return value.Source, nil
}

// ListCatalog returns authorized metadata without preparing or executing work.
// Missing configuration is unavailable; it is never an empty successful catalog.
func (o *Optimizer) ListCatalog(ctx context.Context, query CatalogQuery) (ResourcePage[CatalogChoice], error) {
	var empty ResourcePage[CatalogChoice]
	if err := o.authorizeRead(ctx, Resource{Kind: "catalog"}); err != nil {
		return empty, err
	}
	actor, err := o.caller(ctx)
	if err != nil {
		return empty, err
	}
	normalized, err := operatorPage(query.PageQuery)
	if err != nil || (query.Kind != "" && !validCatalogKind(query.Kind)) {
		return empty, ErrInvalidSpec
	}
	target := CatalogTarget{Scope: copyScope(cortex.ScopeFromContext(ctx)), Actor: actor, Kind: query.Kind, Limit: normalized.Limit}
	target.Cursor, err = decodeCatalogCursor(query.Cursor, catalogBinding(target))
	if err != nil {
		return empty, err
	}
	if o.config.Catalog == nil {
		return empty, ErrUnavailable
	}
	sourceTarget, err := copyModelJSON(&target, 64<<10)
	if err != nil {
		return empty, err
	}
	sourceCtx, cancel := context.WithTimeout(copyScopeContext(ctx), 5*time.Second)
	defer cancel()
	response, err := o.config.Catalog.List(sourceCtx, *sourceTarget)
	if err != nil {
		return empty, err
	}
	detached, err := copyModelJSON(&response, 1<<20)
	if err != nil {
		return empty, ErrIncompleteEvidence
	}
	if !reflect.DeepEqual(detached.Target, target) || len(detached.Page.Items) > target.Limit || len(detached.Page.NextCursor) > 4096 || (target.Cursor != "" && detached.Page.NextCursor == target.Cursor) {
		return empty, ErrIncompleteEvidence
	}
	seen := map[string]bool{}
	for _, choice := range detached.Page.Items {
		if choice.validate() != nil || choice.Scope.Canonical() != target.Scope.Canonical() || (query.Kind != "" && choice.Kind != query.Kind) {
			return empty, ErrIncompleteEvidence
		}
		key := catalogChoiceKey(choice)
		if seen[key] {
			return empty, ErrIncompleteEvidence
		}
		seen[key] = true
		// The host source owns external identities that do not fit Cortex's TypeIDs.
		authorizationInput, copyErr := copyModelJSON(&choice, 64<<10)
		if copyErr != nil {
			return empty, ErrIncompleteEvidence
		}
		if grantErr := o.config.Catalog.AuthorizeChoice(copyScopeContext(sourceCtx), *authorizationInput); grantErr != nil {
			return empty, grantErr
		}
	}
	if grantErr := o.authorizeRead(ctx, Resource{Kind: "catalog"}); grantErr != nil {
		return empty, grantErr
	}
	current, err := o.caller(ctx)
	if err != nil {
		return empty, err
	}
	if current != actor || cortex.ScopeFromContext(ctx).Canonical() != target.Scope.Canonical() {
		return empty, ErrPolicyDenied
	}
	if contextErr := sourceCtx.Err(); contextErr != nil {
		return empty, contextErr
	}
	page := detached.Page
	if page.NextCursor != "" {
		cursorData, encodeErr := canonical.Marshal(catalogCursor{Version: 1, Binding: catalogBinding(target), Source: page.NextCursor})
		if encodeErr != nil {
			return empty, encodeErr
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(cursorData)
		if len(page.NextCursor) > 8192 {
			return empty, ErrIncompleteEvidence
		}
	}
	return resourcePage(page), nil
}
