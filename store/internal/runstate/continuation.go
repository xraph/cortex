package runstate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// readyRecord is the stored form of run.Readiness. Results travel as a
// base64 string, so a JSONB column cannot reformat the engine's bytes.
type readyRecord struct {
	Source       run.ReadySource `json:"source"`
	Results      []byte          `json:"results,omitempty"`
	CheckpointID id.CheckpointID `json:"checkpoint_id"`
	At           time.Time       `json:"at"`
}

// EncodeReady returns the SQL column value for a run's readiness. Nil is
// stored as NULL.
func EncodeReady(ready *run.Readiness) (*string, error) {
	if ready == nil {
		return nil, nil
	}
	data, err := json.Marshal(readyRecord{Source: ready.Source, Results: ready.Results, CheckpointID: ready.CheckpointID, At: ready.At.UTC()})
	if err != nil {
		return nil, fmt.Errorf("encode run readiness: %w", err)
	}
	encoded := string(data)
	return &encoded, nil
}

// DecodeReady reverses EncodeReady.
func DecodeReady(stored *string) (*run.Readiness, error) {
	if stored == nil || *stored == "" {
		return nil, nil
	}
	var record readyRecord
	if err := json.Unmarshal([]byte(*stored), &record); err != nil {
		return nil, fmt.Errorf("decode run readiness: %w", err)
	}
	ready := &run.Readiness{Source: record.Source, CheckpointID: record.CheckpointID, At: record.At}
	if record.Results != nil {
		ready.Results = json.RawMessage(record.Results)
	}
	return ready, nil
}

// LeaseColumns splits a run's lease into its owner and expiry columns. A
// zero lease, or none, is stored as no owner and a NULL expiry, which no
// lapsed listing ever matches.
func LeaseColumns(lease *run.Lease) (string, *time.Time) {
	if lease == nil {
		return "", nil
	}
	if lease.ExpiresAt.IsZero() {
		return lease.Owner, nil
	}
	at := lease.ExpiresAt.UTC()
	return lease.Owner, &at
}

// LeaseFromColumns reverses LeaseColumns.
func LeaseFromColumns(owner string, expiresAt *time.Time) *run.Lease {
	if owner == "" && expiresAt == nil {
		return nil
	}
	lease := &run.Lease{Owner: owner}
	if at := UTCTime(expiresAt); at != nil {
		lease.ExpiresAt = *at
	}
	return lease
}

// UTCTime returns a copy of t in UTC, or nil.
func UTCTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	at := t.UTC()
	return &at
}
