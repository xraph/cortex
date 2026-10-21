package state

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

func TestRetriedTrialRetainsArtifactDependencies(t *testing.T) {
	now := time.Now().UTC()
	experiment := id.New(optimization.PrefixExperiment)
	ref := optimization.ArtifactRef{ExperimentID: experiment, Scope: cortex.ScopeFromContext(operationContext("retry-artifacts")), Digest: strings.Repeat("a", 64), Size: 1, MediaType: "text/plain", Kind: "trace", AccessPolicy: "restricted"}
	key, err := ref.BindingDigest()
	if err != nil {
		t.Fatal(err)
	}
	trial := optimization.Trial{ExperimentID: experiment, Attempts: []optimization.Attempt{{Execution: &optimization.ExecutionRecord{Artifacts: []optimization.ArtifactRef{ref}}}}}
	data := newScopeData()
	if err = checkTrialArtifacts(data, trial, now); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("retry discarded missing artifact dependency: %v", err)
	}
	if !trialUsesArtifact(trial, key) {
		t.Fatal("deletion cannot find prior attempt")
	}
	data.Artifacts = map[string]optimization.ArtifactRetention{key: {Ref: ref, Revision: 1, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}}
	if err = checkTrialArtifacts(data, trial, now); err != nil {
		t.Fatal(err)
	}
}
