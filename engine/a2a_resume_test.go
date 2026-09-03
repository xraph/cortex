package engine

import (
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/suspension"
)

// The three resume sources have three different authorities, and the
// table is the whole contract: a public caller may answer an external
// tool and nothing else.
func TestCheckResumeAuthority(t *testing.T) {
	cases := []struct {
		name   string
		reason suspension.SuspendReason
		source resumeSource
		want   error
	}{
		{"public answers an external tool", suspension.ReasonExternalTool, resumeSourcePublic, nil},
		{"public may not answer an approval", suspension.ReasonApproval, resumeSourcePublic, cortex.ErrRequiresApproval},
		{"a checkpoint may answer an approval", suspension.ReasonApproval, resumeSourceApproval, nil},

		// A run waiting on a peer is not the caller's to answer. Letting a
		// host do it would forge a message the peer never sent, and the
		// correlation ledger that decides a reply is genuine would become
		// decoration.
		{"public may not answer an agent reply", suspension.ReasonAgentReply, resumeSourcePublic, ErrNotAgentReplyResumable},
		{"a checkpoint may not answer an agent reply", suspension.ReasonAgentReply, resumeSourceApproval, ErrNotAgentReplyResumable},
		{"the bus may answer an agent reply", suspension.ReasonAgentReply, resumeSourceAgentReply, nil},

		// The bus holds a ledger row for an agent-reply pause and nothing
		// else, so it has no business answering the other two.
		{"the bus may not answer an approval", suspension.ReasonApproval, resumeSourceAgentReply, cortex.ErrRequiresApproval},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkResumeAuthority(tc.reason, tc.source)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAgentReplyReasonIsItsOwnThing(t *testing.T) {
	// External says the CALLER executes the call and reports back.
	// Agent-reply says cortex is waiting on a peer. Sharing one value
	// would tell a host to go execute something on the bus's behalf.
	if suspension.ReasonAgentReply == suspension.ReasonExternalTool {
		t.Fatal("agent-reply must not collapse into the external-tool reason")
	}
	if suspension.ReasonAgentReply == "" {
		t.Fatal("the reason needs a stored value")
	}
}
