package planning

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskAuthorityIsDetachedAndCannotWidenGrants(t *testing.T) {
	p := fixturePlan(t)
	authority, err := TaskAuthority(p, p.Tasks[0].ID)
	require.NoError(t, err)
	require.True(t, authority.Within(authority))
	for _, mode := range []string{"unbounded", "budget", "resource", "duplicate", "invalid_parent"} {
		t.Run(mode, func(t *testing.T) {
			child, childErr := TaskAuthority(p, p.Tasks[0].ID)
			require.NoError(t, childErr)
			parent, parentErr := TaskAuthority(p, p.Tasks[0].ID)
			require.NoError(t, parentErr)
			switch mode {
			case "unbounded":
				child.Budget.MaxTokens = 0
			case "budget":
				child.Budget.MaxTokens++
			case "resource":
				child.Resources[0].Access = AccessWrite
			case "duplicate":
				child.Resources = append(child.Resources, child.Resources[0])
			case "invalid_parent":
				parent.Resources[0].Access = "invented"
			}
			require.False(t, child.Within(parent))
		})
	}
	authority.Resources[0].Revision = "changed"
	require.NoError(t, Validate(p, Limits{}))
}
