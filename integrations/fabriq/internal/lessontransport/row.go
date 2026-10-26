package lessontransport

import (
	"encoding/json"
	"time"

	"github.com/xraph/cortex/learning"
)

// Row is the existing typed Fabriq lesson projection.
type Row struct {
	ID            string                 `json:"id,omitempty"`
	Tenant        string                 `json:"tenant_id,omitempty"`
	Version       int64                  `json:"version,omitempty"`
	ScopeKey      string                 `json:"scope_key"`
	Domain        string                 `json:"domain"`
	HarnessSchema int                    `json:"harness_schema"`
	EvidenceClass learning.EvidenceClass `json:"evidence_class"`
	LocalRevision int64                  `json:"local_revision"`
	Digest        string                 `json:"payload_digest"`
	ExperimentID  string                 `json:"experiment_id"`
	Content       string                 `json:"content"`
	Active        bool                   `json:"active"`
	CreatedAt     time.Time              `json:"created_at"`
	ExpiresAt     *time.Time             `json:"expires_at"`
	Lesson        learning.Lesson        `json:"lesson"`
}

// For projects a durable publication without changing its payload.
func For(p learning.Publication) Row {
	row := Row{ScopeKey: p.Payload.Scope.Canonical(), Domain: p.Payload.Domain, HarnessSchema: p.Payload.HarnessSchema, EvidenceClass: p.Payload.EvidenceClass, LocalRevision: p.Revision, Digest: p.PayloadDigest, ExperimentID: p.SourceID.String(), Content: p.Payload.Statement, Active: p.Operation == "publish", CreatedAt: p.Payload.CreatedAt, Lesson: p.Payload}
	if !p.Payload.ExpiresAt.IsZero() {
		expiry := p.Payload.ExpiresAt
		row.ExpiresAt = &expiry
	}
	return row
}

// Decode handles SQL JSON values without re-encoding them as base64.
func Decode(raw map[string]any) (Row, error) {
	// SQL drivers may expose JSON columns as bytes; preserve JSON rather than
	// encoding those bytes as a base64 string.
	if data, ok := raw["lesson"].([]byte); ok {
		raw["lesson"] = json.RawMessage(data)
	}
	var row Row
	encoded, err := json.Marshal(raw)
	if err != nil {
		return row, learning.ErrIneligible
	}
	if err = json.Unmarshal(encoded, &row); err != nil {
		return row, learning.ErrIneligible
	}
	return row, nil
}

// Matches binds the entire remote projection to local durable state.
func (row Row) Matches(p learning.Publication, tenantID string) bool {
	digest, err := learning.Digest(row.Lesson)
	expected := For(p)
	sameExpiry := row.ExpiresAt == nil && expected.ExpiresAt == nil
	if row.ExpiresAt != nil && expected.ExpiresAt != nil {
		sameExpiry = row.ExpiresAt.Truncate(time.Microsecond).Equal(expected.ExpiresAt.Truncate(time.Microsecond))
	}
	return err == nil && digest == p.PayloadDigest && row.Digest == digest && row.ID == p.AggregateID && row.Tenant == tenantID && row.Version > 0 && row.ScopeKey == expected.ScopeKey && row.Domain == expected.Domain && row.HarnessSchema == expected.HarnessSchema && row.EvidenceClass == expected.EvidenceClass && row.LocalRevision == expected.LocalRevision && row.ExperimentID == expected.ExperimentID && row.Content == expected.Content && row.Active == expected.Active && row.CreatedAt.Truncate(time.Microsecond).Equal(expected.CreatedAt.Truncate(time.Microsecond)) && sameExpiry
}
