package optimization

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// DiscoveryRef freezes one earlier task's private reflection content.
type DiscoveryRef struct {
	ExperimentID id.ID  `json:"experiment_id"`
	Digest       string `json:"digest"`
}

// DiscoveryLessons retains provisional learning from one controller-arm task.
// It is private to the comparison and cannot enter the published lesson library.
type DiscoveryLessons struct {
	ExperimentID       id.ID         `json:"experiment_id"`
	ParentExperimentID id.ID         `json:"parent_experiment_id"`
	Controller         VersionRef    `json:"controller"`
	Arm                int           `json:"arm"`
	Sequence           int           `json:"sequence"`
	Domain             string        `json:"domain"`
	Lessons            []LessonDraft `json:"lessons"`
	Digest             string        `json:"digest"`
	CreatedAt          time.Time     `json:"created_at"`
}

// DiscoveryLessonsRequest records one completed discovery reflection. An empty
// non-nil Lessons slice records that the controller produced no reusable lesson.
type DiscoveryLessonsRequest struct {
	ExperimentID id.ID         `json:"experiment_id"`
	Lease        WorkLease     `json:"lease"`
	Domain       string        `json:"domain"`
	Lessons      []LessonDraft `json:"lessons"`
}

// Validate checks bounded provisional content before storage resolves its evidence.
func (request DiscoveryLessonsRequest) Validate() error {
	if _, err := ParseID(request.ExperimentID.String(), PrefixExperiment); err != nil {
		return err
	}
	if !ValidOpaqueKey(request.Domain) || len(request.Domain) > 256 || request.Lessons == nil || len(request.Lessons) > 64 {
		return ErrInvalidSpec
	}
	for _, draft := range request.Lessons {
		if draft.Domain != request.Domain {
			return ErrInvalidSpec
		}
		if err := validateLessonContent(draft.Domain, draft.Statement, draft.Applicability, draft.Contraindications); err != nil {
			return err
		}
		if len(draft.EvidenceIDs) == 0 || len(draft.EvidenceIDs) > 500 {
			return ErrIncompleteEvidence
		}
		seen := map[id.ID]bool{}
		for _, trialID := range draft.EvidenceIDs {
			if _, err := ParseID(trialID.String(), PrefixTrial); err != nil {
				return err
			}
			if seen[trialID] {
				return ErrIncompleteEvidence
			}
			seen[trialID] = true
		}
	}
	encoded, err := canonical.Marshal(request.Lessons)
	if err != nil {
		return err
	}
	if len(encoded) > 1<<20 {
		return ErrInvalidSpec
	}
	return nil
}

// DiscoveryLessonsDigest binds content to its source controller, task and timestamp.
func DiscoveryLessonsDigest(value DiscoveryLessons) (string, error) {
	value.Digest = ""
	data, err := canonical.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
