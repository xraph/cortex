package workerstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/acceptance"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/sandbox"
)

const maxCaptureRecordBytes = 64 << 10

const captureTableSQL = "CREATE TABLE worker_captures (job_id TEXT NOT NULL, artifact_id TEXT NOT NULL, state TEXT NOT NULL, revision INTEGER NOT NULL, body BLOB NOT NULL, PRIMARY KEY(job_id,artifact_id))"

// CaptureState describes repository handling, never a grading verdict.
type CaptureState string

const (
	// CaptureIntent preserves the exact binding before an upload may begin.
	CaptureIntent CaptureState = "intent"
	// CaptureVerified records the worker's authenticated readback confirmation.
	CaptureVerified CaptureState = "verified"
	// CaptureDiscarded retains a binding after authorized repository cleanup.
	CaptureDiscarded CaptureState = "discarded"
)

// CaptureRecord survives worker exit and job finalization. Its revision is
// independent of execution ownership; capture never renews or fences a claim.
// Intent does not assert whether the repository accepted an upload.
type CaptureRecord struct {
	SchemaVersion int          `json:"schema_version"`
	JobID         id.ID        `json:"job_id"`
	JobDigest     string       `json:"job_digest"`
	ClaimRevision uint64       `json:"claim_revision"`
	Ref           evidence.Ref `json:"ref"`
	State         CaptureState `json:"state"`
	Revision      uint64       `json:"revision"`
	RecordedAt    time.Time    `json:"recorded_at"`
	VerifiedAt    time.Time    `json:"verified_at,omitempty"`
	DiscardedAt   time.Time    `json:"discarded_at,omitempty"`
	DiscardReason string       `json:"discard_reason,omitempty"`
}

// RecordCapture commits an immutable upload intent under the current claim.
// The daemon must authenticate the worker and current grants before calling.
// An identical retry returns the existing record; changed metadata conflicts.
func (s *Store) RecordCapture(ctx context.Context, key id.ID, token string, claimRevision uint64, ref evidence.Ref) (CaptureRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, operation, finish, err := s.captureClaim(ctx, key, token, claimRevision)
	if err != nil {
		return CaptureRecord{}, err
	}
	defer finish()
	if validationErr := job.Receipt.Job.ValidateArtifacts([]evidence.Ref{ref}, false); validationErr != nil {
		return CaptureRecord{}, validationErr
	}
	if !s.cfg.Now().Before(ref.RetainUntil) {
		return CaptureRecord{}, ErrConflict
	}
	previous, err := s.capture(operation, job, ref.ID)
	if err == nil {
		if !sameCaptureRef(previous.Ref, ref) || previous.State == CaptureDiscarded {
			return CaptureRecord{}, ErrConflict
		}
		return previous, nil
	}
	if !errors.Is(err, broker.ErrNotFound) {
		return CaptureRecord{}, err
	}
	now := s.cfg.Now().UTC()
	record := CaptureRecord{SchemaVersion: 1, JobID: key, JobDigest: job.Receipt.Job.Digest, ClaimRevision: claimRevision, Ref: ref, State: CaptureIntent, Revision: 1, RecordedAt: now}
	if validationErr := validateCapture(record, job); validationErr != nil {
		return CaptureRecord{}, validationErr
	}
	if !now.Before(ref.RetainUntil) {
		return CaptureRecord{}, ErrConflict
	}
	body, err := encodeCapture(record)
	if err != nil {
		return CaptureRecord{}, err
	}
	if _, err = s.journal.Conn.ExecContext(operation, "INSERT INTO worker_captures(job_id,artifact_id,state,revision,body) VALUES(?,?,?,?,?)", key.String(), ref.ID.String(), record.State, record.Revision, body); err != nil {
		return CaptureRecord{}, err
	}
	return s.capture(operation, job, ref.ID)
}

// ConfirmCapture records readback only for the exact retained upload intent.
// Call after repository readback and current evidence authorization succeed.
// It does not prove correctness or grant another upload/execution attempt.
func (s *Store) ConfirmCapture(ctx context.Context, key id.ID, token string, claimRevision uint64, ref evidence.Ref) (CaptureRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, operation, finish, err := s.captureClaim(ctx, key, token, claimRevision)
	if err != nil {
		return CaptureRecord{}, err
	}
	defer finish()
	record, err := s.capture(operation, job, ref.ID)
	if err != nil {
		return CaptureRecord{}, err
	}
	if !sameCaptureRef(record.Ref, ref) || record.State == CaptureDiscarded {
		return CaptureRecord{}, ErrConflict
	}
	if !s.cfg.Now().Before(ref.RetainUntil) {
		return CaptureRecord{}, ErrConflict
	}
	if record.State == CaptureVerified {
		return record, nil
	}
	record.State = CaptureVerified
	record.VerifiedAt = s.cfg.Now().UTC()
	return s.saveCapture(operation, job, record)
}

func (s *Store) captureClaim(ctx context.Context, key id.ID, token string, revision uint64) (Record, context.Context, context.CancelFunc, error) {
	job, err := s.load(ctx, key)
	if err != nil {
		return Record{}, nil, nil, err
	}
	if job.Phase != Claimed || job.CancelRequested || job.Revision != revision || !matchesToken(job, token) {
		return Record{}, nil, nil, ErrConflict
	}
	operation, finish, err := s.reportContext(ctx, job.Receipt)
	return job, operation, finish, err
}

// Captures loads at most sixteen detached bindings, including tombstones and
// records for terminal jobs. This is an internal recovery read, not a user API.
func (s *Store) Captures(ctx context.Context, key id.ID) ([]CaptureRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	rows, err := s.journal.Conn.QueryContext(ctx, "SELECT artifact_id,state,revision,CASE WHEN length(body)<=65536 THEN body ELSE NULL END FROM worker_captures WHERE job_id=? ORDER BY artifact_id LIMIT 17", key.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CaptureRecord
	for rows.Next() {
		var name string
		var state CaptureState
		var revision uint64
		var body []byte
		if scanErr := rows.Scan(&name, &state, &revision, &body); scanErr != nil {
			return nil, scanErr
		}
		keyID, parseErr := id.ParseWithPrefix(name, evidence.PrefixArtifact)
		if parseErr != nil {
			return nil, ErrCorrupt
		}
		record, decodeErr := decodeCapture(body, job, keyID, state, revision)
		if decodeErr != nil {
			return nil, decodeErr
		}
		result = append(result, record)
	}
	if len(result) > len(job.Receipt.Job.Outputs) {
		return nil, ErrCorrupt
	}
	return result, rows.Err()
}

func (s *Store) capture(ctx context.Context, job Record, artifact id.ID) (CaptureRecord, error) {
	if artifact.IsNil() || artifact.Prefix() != evidence.PrefixArtifact {
		return CaptureRecord{}, broker.ErrNotFound
	}
	var body []byte
	var state CaptureState
	var revision uint64
	err := s.journal.Conn.QueryRowContext(ctx, "SELECT state,revision,CASE WHEN length(body)<=65536 THEN body ELSE NULL END FROM worker_captures WHERE job_id=? AND artifact_id=?", job.Receipt.Job.ID.String(), artifact.String()).Scan(&state, &revision, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return CaptureRecord{}, broker.ErrNotFound
	}
	if err != nil {
		return CaptureRecord{}, err
	}
	return decodeCapture(body, job, artifact, state, revision)
}

func decodeCapture(body []byte, job Record, artifact id.ID, state CaptureState, revision uint64) (CaptureRecord, error) {
	var record CaptureRecord
	if len(body) > maxCaptureRecordBytes || json.Unmarshal(body, &record) != nil {
		return CaptureRecord{}, ErrCorrupt
	}
	canonical, err := encodeCapture(record)
	if err != nil || !bytes.Equal(canonical, body) || record.Ref.ID != artifact || record.State != state || record.Revision != revision {
		return CaptureRecord{}, ErrCorrupt
	}
	if err = validateCapture(record, job); err != nil {
		return CaptureRecord{}, errors.Join(ErrCorrupt, err)
	}
	return record, nil
}

func encodeCapture(record CaptureRecord) ([]byte, error) {
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(body) > maxCaptureRecordBytes {
		return nil, fmt.Errorf("cortex: capture record exceeds storage bound")
	}
	return body, nil
}

func validateCapture(record CaptureRecord, job Record) error {
	deadline := acceptance.Deadline(job.Receipt)
	if record.SchemaVersion != 1 || record.JobID != job.Receipt.Job.ID || record.JobDigest != job.Receipt.Job.Digest || record.ClaimRevision < 2 || record.ClaimRevision > job.Revision || record.RecordedAt.Before(job.Receipt.AcceptedAt) || !record.RecordedAt.Before(deadline) || record.Ref.CreatedAt.Before(job.Receipt.AcceptedAt) || record.Ref.CreatedAt.After(record.RecordedAt) || !record.RecordedAt.Before(record.Ref.RetainUntil) {
		return ErrCorrupt
	}
	if job.Phase == Accepted || job.TokenHash == "" {
		return ErrCorrupt
	}
	if err := job.Receipt.Job.ValidateArtifacts([]evidence.Ref{record.Ref}, false); err != nil {
		return err
	}
	if err := capture.ValidateRetention(record.Ref.RetainUntil.Sub(record.Ref.CreatedAt)); err != nil {
		return err
	}
	verified := !record.VerifiedAt.IsZero()
	if verified && (record.VerifiedAt.Before(record.RecordedAt) || !record.VerifiedAt.Before(deadline) || !record.VerifiedAt.Before(record.Ref.RetainUntil)) {
		return ErrCorrupt
	}
	switch record.State {
	case CaptureIntent:
		if record.Revision != 1 || verified || !record.DiscardedAt.IsZero() || record.DiscardReason != "" {
			return ErrCorrupt
		}
	case CaptureVerified:
		if record.Revision != 2 || !verified || !record.DiscardedAt.IsZero() || record.DiscardReason != "" {
			return ErrCorrupt
		}
	case CaptureDiscarded:
		expected := uint64(2)
		if verified {
			expected = 3
		}
		if job.Phase != Terminal || record.Revision != expected || record.DiscardedAt.Before(record.RecordedAt) || record.DiscardedAt.Before(record.VerifiedAt) || !discardReason(record.DiscardReason) {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	return nil
}

func sameCaptureRef(a, b evidence.Ref) bool {
	first, err := a.BindingDigest()
	if err != nil {
		return false
	}
	second, err := b.BindingDigest()
	return err == nil && first == second
}

func (s *Store) saveCapture(ctx context.Context, job Record, record CaptureRecord) (CaptureRecord, error) {
	previous := record.Revision
	record.Revision++
	if err := validateCapture(record, job); err != nil {
		return CaptureRecord{}, err
	}
	body, err := encodeCapture(record)
	if err != nil {
		return CaptureRecord{}, err
	}
	result, err := s.journal.Conn.ExecContext(ctx, "UPDATE worker_captures SET state=?,revision=?,body=? WHERE job_id=? AND artifact_id=? AND revision=?", record.State, record.Revision, body, record.JobID.String(), record.Ref.ID.String(), previous)
	if err != nil {
		return CaptureRecord{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return CaptureRecord{}, err
	}
	if count != 1 {
		return CaptureRecord{}, ErrConflict
	}
	return s.capture(ctx, job, record.Ref.ID)
}

// DiscardCapture records a trusted recovery worker's proof of authorized durable
// repository cleanup. It requires independently finalized whole-service cleanup
// (a terminal job), not merely a missing process or expired execution deadline.
// Callers must prove a repository tombstone or authoritative absence separately.
func (s *Store) DiscardCapture(ctx context.Context, key, artifact id.ID, expected uint64, reason string) (CaptureRecord, error) {
	if !discardReason(reason) {
		return CaptureRecord{}, ErrConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, err := s.load(ctx, key)
	if err != nil {
		return CaptureRecord{}, err
	}
	if job.Phase != Terminal {
		return CaptureRecord{}, ErrConflict
	}
	record, err := s.capture(ctx, job, artifact)
	if err != nil {
		return CaptureRecord{}, err
	}
	if record.State == CaptureDiscarded {
		if expected != record.Revision-1 || record.DiscardReason != reason {
			return CaptureRecord{}, ErrConflict
		}
		return record, nil
	}
	if record.Revision != expected {
		return CaptureRecord{}, ErrConflict
	}
	record.State, record.DiscardedAt, record.DiscardReason = CaptureDiscarded, s.cfg.Now().UTC(), reason
	return s.saveCapture(ctx, job, record)
}

func discardReason(reason string) bool {
	return strings.TrimSpace(reason) != "" && len(reason) <= 512 && utf8.ValidString(reason) && !strings.ContainsRune(reason, 0)
}

// CaptureJobs pages recovery candidates even after execution becomes terminal.
// Load each job and its Captures before acting. Every non-discarded binding stays
// indexed until authorized cleanup, including previously verified artifacts.
func (s *Store) CaptureJobs(ctx context.Context, after string, limit int) ([]id.ID, error) {
	if limit < 1 || limit > 256 {
		return nil, fmt.Errorf("cortex: invalid capture recovery page size")
	}
	if after != "" {
		if _, err := id.ParseWithPrefix(after, sandbox.PrefixJob); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.journal.Conn.QueryContext(ctx, "SELECT DISTINCT job_id FROM worker_captures WHERE job_id>? AND state<>? ORDER BY job_id LIMIT ?", after, CaptureDiscarded, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []id.ID
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, scanErr
		}
		key, parseErr := id.ParseWithPrefix(name, sandbox.PrefixJob)
		if parseErr != nil {
			return nil, ErrCorrupt
		}
		result = append(result, key)
	}
	return result, rows.Err()
}

func (s *Store) confirmedCaptures(ctx context.Context, job Record, result sandbox.Result) error {
	for _, ref := range result.Artifacts {
		record, err := s.capture(ctx, job, ref.ID)
		if err != nil {
			return err
		}
		if record.State != CaptureVerified || !sameCaptureRef(record.Ref, ref) || !s.cfg.Now().Before(ref.RetainUntil) {
			return ErrConflict
		}
	}
	return nil
}
