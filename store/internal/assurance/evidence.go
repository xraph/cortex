package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex/evidence"
)

var _ evidence.Storage = (*Repository)(nil)
var _ evidence.TombstoneReader = (*Repository)(nil)

// SaveEvidence persists bounded, immutable content. The host must separately
// authorize sensitivity and source access through evidence.Repository.
func (s *Repository) SaveEvidence(ctx context.Context, a evidence.Artifact) error {
	if err := require(ctx, a.Ref.Scope); err != nil {
		return err
	}
	if len(a.Data) > MaxEvidenceBytes {
		return evidence.ErrTooLarge
	}
	if err := a.Validate(); err != nil {
		return err
	}
	digest, err := a.Ref.BindingDigest()
	if err != nil {
		return err
	}
	row, err := record(KindEvidence, a.Ref.ID.String(), digest, "", a.Ref.Scope, a)
	if err != nil {
		return err
	}
	if len(row.Body) > 2*MaxEvidenceBytes {
		return evidence.ErrTooLarge
	}
	err = s.insert(ctx, row)
	if errors.Is(err, ErrExists) {
		existing, readErr := s.get(ctx, Query{Kind: KindEvidence, ID: row.ID})
		if readErr != nil {
			return readErr
		}
		if existing.Deleted {
			return evidence.ErrDeleted
		}
		return evidence.ErrConflict
	}
	return err
}

func (s *Repository) evidenceRecord(ctx context.Context, ref evidence.Ref) (Record, error) {
	if err := require(ctx, ref.Scope); err != nil {
		return Record{}, err
	}
	if err := ref.Validate(); err != nil {
		return Record{}, err
	}
	row, err := s.get(ctx, Query{Kind: KindEvidence, ID: ref.ID.String()})
	if errors.Is(err, ErrAbsent) {
		return Record{}, evidence.ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	digest, err := ref.BindingDigest()
	if err != nil {
		return Record{}, err
	}
	if digest != row.Digest {
		return Record{}, evidence.ErrDigestMismatch
	}
	return row, nil
}

// LoadEvidence validates persisted metadata and bytes on every read.
func (s *Repository) LoadEvidence(ctx context.Context, ref evidence.Ref) (evidence.Artifact, error) {
	row, err := s.evidenceRecord(ctx, ref)
	if err != nil {
		return evidence.Artifact{}, err
	}
	if row.Deleted {
		return evidence.Artifact{}, evidence.ErrDeleted
	}
	if len(row.Body) > 2*MaxEvidenceBytes {
		return evidence.Artifact{}, evidence.ErrTooLarge
	}
	var artifact evidence.Artifact
	if checkErr := json.Unmarshal([]byte(row.Body), &artifact); checkErr != nil {
		return evidence.Artifact{}, checkErr
	}
	if validateErr := artifact.Validate(); validateErr != nil {
		return evidence.Artifact{}, validateErr
	}
	digest, err := artifact.Ref.BindingDigest()
	if err != nil {
		return evidence.Artifact{}, err
	}
	if digest != row.Digest {
		return evidence.Artifact{}, evidence.ErrDigestMismatch
	}
	if len(artifact.Data) > MaxEvidenceBytes {
		return evidence.Artifact{}, evidence.ErrTooLarge
	}
	return artifact, nil
}

// TombstoneEvidence atomically removes bytes and preserves the first revocation.
// A retry with a different actor or reason conflicts instead of rewriting audit.
func (s *Repository) TombstoneEvidence(ctx context.Context, ref evidence.Ref, reason string) error {
	row, err := s.evidenceRecord(ctx, ref)
	if err != nil {
		return err
	}
	actor := evidence.DeletionActor(ctx)
	if !utf8.ValidString(actor) || !utf8.ValidString(reason) || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(actor) > 4096 || len(reason) > 4096 {
		return evidence.ErrInvalid
	}
	if row.Deleted {
		previous, readErr := s.GetEvidenceTombstone(ctx, ref)
		if readErr != nil {
			return readErr
		}
		if previous.Actor == actor && previous.Reason == reason {
			return nil
		}
		return evidence.ErrConflict
	}
	tombstone := evidence.Tombstone{Ref: ref, Actor: actor, Reason: reason, DeletedAt: time.Now().UTC()}
	data, err := json.Marshal(tombstone)
	if err != nil {
		return err
	}
	previousBody := row.Body
	row.Body = string(data)
	row.Deleted = true
	if err = s.driver.ReplaceEvidence(ctx, row, previousBody); errors.Is(err, ErrExists) {
		previous, readErr := s.GetEvidenceTombstone(ctx, ref)
		if readErr != nil {
			return readErr
		}
		if previous.Actor == actor && previous.Reason == reason {
			return nil
		}
		return evidence.ErrConflict
	}
	return err
}

// GetEvidenceTombstone reads audit metadata under exact scope. The caller must
// independently enforce audit permissions; it never returns removed bytes.
func (s *Repository) GetEvidenceTombstone(ctx context.Context, ref evidence.Ref) (evidence.Tombstone, error) {
	row, err := s.evidenceRecord(ctx, ref)
	if err != nil {
		return evidence.Tombstone{}, err
	}
	if !row.Deleted {
		return evidence.Tombstone{}, evidence.ErrNotFound
	}
	var tombstone evidence.Tombstone
	if checkErr := json.Unmarshal([]byte(row.Body), &tombstone); checkErr != nil {
		return evidence.Tombstone{}, checkErr
	}
	digest, err := tombstone.Ref.BindingDigest()
	if err != nil {
		return evidence.Tombstone{}, err
	}
	if digest != row.Digest || tombstone.DeletedAt.IsZero() || strings.TrimSpace(tombstone.Actor) == "" || strings.TrimSpace(tombstone.Reason) == "" {
		return evidence.Tombstone{}, evidence.ErrInvalid
	}
	return tombstone, nil
}
