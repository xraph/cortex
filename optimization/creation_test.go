package optimization_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestCreationReceiptRejectsMalformedSpecDigest(t *testing.T) {
	bundle := storetest.Fixture(t, "creation-receipt")
	spec := bundle.Experiment.Spec
	receipt := opt.CreationReceipt{
		Key: "operator/" + strings.Repeat("a", 64), Scope: spec.Scope,
		Actor: spec.Requester, ExperimentID: bundle.Experiment.ID, CreatedAt: time.Now(),
		Selection: opt.CreationSelection{
			Agent:       opt.ContentRef{Ref: spec.AgentID.String(), Revision: "1", Digest: strings.Repeat("a", 64)},
			Evaluation:  spec.Evaluation,
			Controller:  opt.ContentRef{Ref: spec.Controller.ID.String(), Revision: "1", Digest: spec.Controller.Digest},
			Environment: spec.Tasks[0].EnvironmentSnapshot,
		},
	}
	for _, digest := range []string{"bad", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		receipt.SpecDigest = digest
		if _, err := opt.SealCreationReceipt(receipt); !errors.Is(err, opt.ErrInvalidSpec) {
			t.Fatalf("accepted malformed specification digest %q: %v", digest, err)
		}
	}
	for _, digest := range []string{"", strings.Repeat("a", 64)} {
		receipt.SpecDigest = digest
		if _, err := opt.SealCreationReceipt(receipt); err != nil {
			t.Fatalf("valid construction or committed receipt: %v", err)
		}
	}
}
