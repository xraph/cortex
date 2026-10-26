package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/planning"
)

const (
	kindArtifactBoard          = "artifact_board"
	kindArtifactBoardIdentity  = "artifact_board_identity"
	kindArtifactBoardCandidate = "artifact_board_candidate"
	kindArtifactBoardCount     = "artifact_board_count"
)

var _ coordination.ArtifactBoardStore = (*Repository)(nil)

func artifactBoardKey(r coordination.ArtifactBoardRead) string {
	return r.PlanID.String() + ":" + evidence.Digest([]byte(r.Resource.Resource+"\x00"+r.Resource.Revision))
}
func artifactBoardRead(r coordination.ArtifactBoardRequest) coordination.ArtifactBoardRead {
	return coordination.ArtifactBoardRead{Scope: r.Scope, PlanID: r.PlanID, Resource: r.Resource, Limit: coordination.MaxBoardTransitions}
}
func decodeArtifactBoard(ctx context.Context, row Record) (coordination.ArtifactBoardEntry, error) {
	var entry coordination.ArtifactBoardEntry
	if err := validateRecord(ctx, row); err != nil {
		return entry, err
	}
	if len(row.Body) > 128<<10 || json.Unmarshal([]byte(row.Body), &entry) != nil || entry.Validate() != nil || row.Digest != entry.Digest || row.ScopeCanon != entry.Request.Scope.Canonical() {
		return coordination.ArtifactBoardEntry{}, planning.ErrInvalid
	}
	return entry, nil
}

// ReadArtifactBoard reads exact resource history. Callers authenticate current
// metadata access; these immutable references never authorize content export.
func (s *Repository) ReadArtifactBoard(ctx context.Context, request coordination.ArtifactBoardRead) ([]coordination.ArtifactBoardEntry, error) {
	req, _, err := request.Freeze()
	if err != nil {
		return nil, err
	}
	if scopeErr := require(ctx, req.Scope); scopeErr != nil {
		return nil, scopeErr
	}
	key := artifactBoardKey(req)
	after := ""
	if req.After > 0 {
		after = fmt.Sprintf("%s:%020d", key, req.After)
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindArtifactBoard, RunID: key, After: after, Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	result := make([]coordination.ArtifactBoardEntry, 0, len(rows))
	for _, row := range rows {
		entry, readErr := decodeArtifactBoard(ctx, row)
		if readErr != nil {
			return nil, readErr
		}
		if entry.Request.PlanID != req.PlanID || entry.Request.Resource != req.Resource || row.ID != fmt.Sprintf("%s:%020d", key, entry.Sequence) || entry.Sequence != req.After+int64(len(result))+1 {
			return nil, planning.ErrConflict
		}
		result = append(result, entry)
	}
	return result, nil
}

// WriteArtifactBoard rechecks accepted evidence and appends a candidate and head
// decision under the existing plan transaction. Conflicts retain both candidates.
func (s *Repository) WriteArtifactBoard(ctx context.Context, request coordination.ArtifactBoardRequest) (coordination.ArtifactBoardEntry, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.ArtifactBoardEntry{}, err
	}
	if scopeErr := require(ctx, req.Scope); scopeErr != nil {
		return coordination.ArtifactBoardEntry{}, scopeErr
	}
	var result coordination.ArtifactBoardEntry
	err = s.planTransaction(ctx, req.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.ArtifactBoardEntry{}
		cached, readErr := tx.get(tc, Query{Kind: kindArtifactBoardIdentity, ID: req.ID.String()})
		if readErr == nil {
			entry, decodeErr := decodeArtifactBoard(tc, cached)
			if decodeErr != nil {
				return decodeErr
			}
			if entry.RequestDigest != digest {
				return planning.ErrConflict
			}
			result = entry
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}

		candidateKey := artifactBoardKey(artifactBoardRead(req)) + ":" + req.AcceptanceID.String()
		if req.Resolve.IsNil() {
			if _, candidateErr := tx.get(tc, Query{Kind: kindArtifactBoardCandidate, ID: candidateKey}); candidateErr == nil {
				return planning.ErrConflict
			} else if !errors.Is(candidateErr, ErrAbsent) {
				return candidateErr
			}
		}
		p, readErr := tx.GetPlan(tc, req.PlanID)
		if readErr != nil {
			return readErr
		}
		if p.Digest != req.PlanDigest {
			return planning.ErrConflict
		}
		accepted, readErr := tx.GetTaskAcceptance(tc, req.AcceptanceID)
		if readErr != nil {
			return readErr
		}
		if accepted.Digest != req.AcceptanceDigest || accepted.Claim.PlanID != p.ID || accepted.Claim.PlanDigest != p.Digest {
			return planning.ErrConflict
		}
		current, readErr := tx.GetTaskClaim(tc, p.ID, accepted.Claim.TaskID)
		if readErr != nil {
			return readErr
		}
		if current.Digest != accepted.Claim.Digest {
			return planning.ErrConflict
		}
		found := false
		for _, task := range p.Tasks {
			if task.ID == current.TaskID {
				for _, resource := range task.Resources {
					if resource == req.Resource {
						found = true
					}
				}
			}
		}
		if !found {
			return planning.ErrInvalid
		}
		original, readErr := tx.claimRevision(tc, p.ID, current.TaskID, accepted.Request.Claim.Revision)
		if readErr != nil {
			return readErr
		}
		if coordination.ValidateClaimSuccessor(original, current) != nil {
			return planning.ErrConflict
		}
		acceptance := accepted.Request
		acceptance.Current = req.Current
		acceptance.Dependencies = req.Dependencies
		own, used, readErr := tx.loadTaskAcceptanceMaterial(tc, p, acceptance, original, 8<<20, accepted.ObservedAt)
		if readErr != nil {
			return readErr
		}
		dependencies, readErr := tx.acceptedPrerequisiteMaterials(tc, p, current.TaskID, req.Dependencies, (32<<20)-used)
		if readErr != nil {
			return readErr
		}
		materials := append([]taskAcceptanceMaterial{own}, dependencies...)
		if checkErr := validateTaskAcceptanceMaterials(p, current.TaskID, materials, tx.clock.Now()); checkErr != nil {
			return checkErr
		}
		history, readErr := tx.ReadArtifactBoard(tc, artifactBoardRead(req))
		if readErr != nil {
			return readErr
		}
		if len(history) >= coordination.MaxBoardTransitions {
			return planning.ErrConflict
		}
		all, readErr := tx.driver.Select(tc, Query{Kind: kindArtifactBoardCount, RunID: p.ID.String(), Limit: coordination.MaxPlanBoardTransitions})
		if readErr != nil {
			return readErr
		}
		if len(all) >= coordination.MaxPlanBoardTransitions {
			return planning.ErrConflict
		}
		var head coordination.ArtifactBoardEntry
		var selected *coordination.ArtifactBoardEntry
		resolved := false
		for i := range history {
			entry := history[i]
			if !entry.Conflict {
				head = entry
			}
			if entry.Request.ID == req.Resolve {
				selected = &history[i]
			}
			if !req.Resolve.IsNil() && entry.Request.Resolve == req.Resolve {
				resolved = true
			}
		}
		matches := req.ExpectedRevision == head.HeadRevision && req.ExpectedDigest == head.Digest
		conflict := !matches || (head.HeadRevision > 0 && (head.Owner != current.Worker || head.TaskID != current.TaskID))
		if !req.Resolve.IsNil() {
			if !matches || resolved || selected == nil || !selected.Conflict || selected.Request.AcceptanceID != req.AcceptanceID || selected.Request.AcceptanceDigest != req.AcceptanceDigest {
				return planning.ErrConflict
			}
			conflict = false
		}

		// A conflict reserves the slot needed for its eventual explicit resolution.
		// Resolution consumes that reservation instead of competing with new heads.
		cost := 1
		if conflict {
			cost = 2
		} else if !req.Resolve.IsNil() {
			cost = 0
		}
		resourceCost := 0
		for _, previous := range history {
			if previous.Conflict {
				resourceCost += 2
			} else if previous.Request.Resolve.IsNil() {
				resourceCost++
			}
		}
		planCost := 0
		for _, counter := range all {
			if checkErr := validateRecord(tc, counter); checkErr != nil {
				return checkErr
			}
			var retainedCost int
			if counter.Deleted || counter.Kind != kindArtifactBoardCount || counter.RunID != p.ID.String() || counter.Digest != evidence.Digest([]byte(counter.Body)) || len(counter.Body) > 8 || json.Unmarshal([]byte(counter.Body), &retainedCost) != nil || retainedCost < 0 || retainedCost > 2 {
				return planning.ErrInvalid
			}
			planCost += retainedCost
		}
		if resourceCost+cost > coordination.MaxBoardTransitions || planCost+cost > coordination.MaxPlanBoardTransitions {
			return planning.ErrConflict
		}
		revision := head.HeadRevision
		if !conflict {
			revision++
		}
		entry, sealErr := coordination.SealArtifactBoardEntry(coordination.ArtifactBoardEntry{SchemaVersion: 1, PlanRevision: p.Revision, Request: req, RequestDigest: digest, Sequence: int64(len(history)) + 1, HeadRevision: revision, PreviousDigest: head.Digest, Conflict: conflict, Owner: current.Worker, TaskID: current.TaskID, Artifact: req.Current.Candidate, At: tx.clock.Now()})
		if sealErr != nil {
			return sealErr
		}
		key := artifactBoardKey(artifactBoardRead(req))
		row, rowErr := record(kindArtifactBoard, fmt.Sprintf("%s:%020d", key, entry.Sequence), entry.Digest, key, p.Scope, entry)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, row); writeErr != nil {
			return writeErr
		}
		identity, rowErr := record(kindArtifactBoardIdentity, req.ID.String(), entry.Digest, p.ID.String(), p.Scope, entry)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, identity); writeErr != nil {
			return writeErr
		}

		if req.Resolve.IsNil() {
			candidate, candidateErr := record(kindArtifactBoardCandidate, candidateKey, entry.Digest, p.ID.String(), p.Scope, req.ID)
			if candidateErr != nil {
				return candidateErr
			}
			if writeErr := tx.driver.Insert(tc, candidate); writeErr != nil {
				return writeErr
			}
		}

		count, rowErr := record(kindArtifactBoardCount, req.ID.String(), evidence.Digest([]byte(fmt.Sprint(cost))), p.ID.String(), p.Scope, cost)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, count); writeErr != nil {
			return writeErr
		}
		if _, eventErr := tx.appendCoordinationEvent(tc, boardSourceEvent(entry)); eventErr != nil {
			return eventErr
		}
		if checkErr := validateTaskAcceptanceMaterials(p, current.TaskID, materials, tx.clock.Now()); checkErr != nil {
			return checkErr
		}
		result = entry
		return tc.Err()
	})
	if err != nil {
		return coordination.ArtifactBoardEntry{}, err
	}
	return result, nil
}
