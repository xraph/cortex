package verification_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestContractSchemaOneDigestFixture(t *testing.T) {
	c, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.MustParseWithPrefix("vcontract_00000000000000000000000001", verification.PrefixContract), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}, Revision: "1", Objective: "check", Criteria: []verification.Criterion{{ID: "test", Kind: verification.KindTest, Required: true, Verifier: "runner", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "fixture:1"}, MaxAge: time.Minute}}})
	require.NoError(t, err)
	require.Equal(t, "e6eaa973dfa4597924cf3aff766bc37a6c71cfb0e4b5294f25a3229f436bf4a9", c.Digest)
}
