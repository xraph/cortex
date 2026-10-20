package sandboxbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex/sandbox"
)

// Reconcile is a trusted supervisor operation, never an HTTP route. Call it at
// startup and periodically with the host clock. It reaps expired recorded jobs
// even after the submitter's grant is revoked, but never starts new execution.
// Runtime state and the journal must both survive broker restarts.
func (b *Broker) Reconcile(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("cortex: missing sandbox supervisor time")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cursor := ""
	var failures []error
	for {
		records, err := b.pending(ctx, cursor)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		if len(records) == 0 {
			break
		}
		for i := range records {
			rec := &records[i]
			j := rec.Status.Receipt.Job
			cursor = j.ID.String()
			if operationErr := bindingCheck(rec, b.PublicKey()); operationErr != nil {
				return operationErr
			}
			expired := !now.Before(rec.Status.Receipt.AcceptedAt.Add(j.Limits.WallTime))
			if expired || rec.Cancelling {
				rec.Cancelling = true
				if operationErr := b.save(ctx, rec); operationErr != nil {
					return operationErr
				}
				if operationErr := b.stop(ctx, rec); operationErr != nil {
					failures = append(failures, operationErr)
					continue
				}
			}
			result, inspectErr := b.observe(ctx, rec)
			if result == nil && inspectErr == nil {
				continue
			}
			if inspectErr != nil {
				failures = append(failures, inspectErr)
				continue
			}
			if operationErr := validateResult(*result, j); operationErr != nil {
				failures = append(failures, operationErr)
				continue
			}
			rec.Status.State = result.State
			if result.State != sandbox.StateRunning {
				if operationErr := b.signResult(result); operationErr != nil {
					return operationErr
				}
				rec.Result = result
			}
			if operationErr := b.save(ctx, rec); operationErr != nil {
				return operationErr
			}
		}
	}
	return errors.Join(failures...)
}

func (b *Broker) pending(ctx context.Context, after string) ([]record, error) {
	rows, err := b.conn.QueryContext(ctx, "SELECT body FROM jobs WHERE id>? AND json_extract(body,'$.result') IS NULL ORDER BY id LIMIT 256", after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]record, 0, 256)
	for rows.Next() {
		var body []byte
		if operationErr := rows.Scan(&body); operationErr != nil {
			return nil, operationErr
		}
		var rec record
		if operationErr := json.Unmarshal(body, &rec); operationErr != nil {
			return nil, operationErr
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}
