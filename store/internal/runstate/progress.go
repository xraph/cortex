package runstate

import (
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex/suspension"
)

// progressRecord is the stored body of a run's progress snapshot. The run,
// owner, scope and time live in columns of their own. The continuation is
// encoded with encoding/json, the encoder cortex_suspensions.continuation
// uses, so Prepared travels as a base64 string that a JSONB column cannot
// reformat.
type progressRecord struct {
	Continuation suspension.Continuation     `json:"continuation"`
	Started      []suspension.PendingCall    `json:"started,omitempty"`
	Done         []suspension.ProgressResult `json:"done,omitempty"`
	Ending       *suspension.Ending          `json:"ending,omitempty"`
}

// EncodeProgress returns the SQL column value for a progress snapshot.
func EncodeProgress(p *suspension.Progress) (string, error) {
	data, err := json.Marshal(progressRecord{Continuation: p.Cont, Started: p.Started, Done: p.Done, Ending: p.Ending})
	if err != nil {
		return "", fmt.Errorf("encode run progress: %w", err)
	}
	return string(data), nil
}

// DecodeProgress reverses EncodeProgress into p.
func DecodeProgress(stored string, p *suspension.Progress) error {
	var record progressRecord
	if err := json.Unmarshal([]byte(stored), &record); err != nil {
		return fmt.Errorf("decode run progress: %w", err)
	}
	p.Cont, p.Started, p.Done, p.Ending = record.Continuation, record.Started, record.Done, record.Ending
	return nil
}
