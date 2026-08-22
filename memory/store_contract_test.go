package memory_test

import (
	"reflect"
	"testing"

	"github.com/xraph/cortex/memory"
)

// TestStore_ConversationMethodsTakeNoTenant is a compile-shape guard. The
// tenantID parameter was the one that received "" at every call site in
// the react loop, so this test fails if anyone reintroduces a bare string
// parameter alongside the context.
func TestStore_ConversationMethodsTakeNoTenant(t *testing.T) {
	typ := reflect.TypeOf((*memory.Store)(nil)).Elem()

	wantArgs := map[string]int{
		"SaveConversation":  3, // ctx, agentID, messages
		"LoadConversation":  3, // ctx, agentID, limit
		"ClearConversation": 2, // ctx, agentID
		"SaveSummary":       3, // ctx, agentID, summary
		"LoadSummaries":     2, // ctx, agentID
	}

	for name, want := range wantArgs {
		m, ok := typ.MethodByName(name)
		if !ok {
			t.Errorf("memory.Store is missing %s", name)
			continue
		}
		if got := m.Type.NumIn(); got != want {
			t.Errorf("%s takes %d args, want %d (a stray tenant string?)", name, got, want)
		}
	}
}
