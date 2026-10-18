package storetest

import (
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func testScopeBoundary(t *testing.T, newStore func(*testing.T) store.Store) {
	t.Run("InvalidContextCannotFallBack", func(t *testing.T) {
		s := newStore(t)
		parent := ctxWithScope("workspace")
		for _, levels := range [][]cortex.Level{
			{{Key: "workspace", Value: ""}},
			{{Key: "workspace", Value: "a"}, {Key: "workspace", Value: "b"}},
			{{Key: "workspace", Value: "a/project=b"}},
			{{Key: "workspace", Value: "a"}, {Key: "project", Value: "b"}, {Key: "environment", Value: "c"}, {Key: "region", Value: "d"}},
		} {
			ctx := cortex.WithScope(parent, cortex.Scope{Levels: levels})
			if err := s.Create(ctx, &agent.Config{ID: id.NewAgentID(), Name: id.NewAgentID().String()}); err == nil {
				t.Error("invalid scope stored an agent")
			}
			if _, err := s.List(ctx, nil); err == nil {
				t.Error("invalid scope listed agents")
			}
		}
	})
	t.Run("ExactMutationDoesNotUseAncestorGrant", func(t *testing.T) {
		s := newStore(t)
		parent, child := ctxWithScope("workspace"), ctxWithScope("workspace", "project")
		ag := &agent.Config{ID: id.NewAgentID(), Name: "child", SystemPrompt: "original"}
		if err := s.Create(child, ag); err != nil {
			t.Fatal(err)
		}
		listed, err := s.List(parent, nil)
		if err != nil || len(listed) != 1 {
			t.Fatalf("prefix list changed: %v %v", listed, err)
		}
		ag.SystemPrompt = "changed"
		if updateErr := s.Update(parent, ag); updateErr == nil {
			t.Error("ancestor context updated child")
		}
		if deleteErr := s.Delete(parent, ag.ID); deleteErr == nil {
			t.Error("ancestor context deleted child")
		}
		got, err := s.Get(child, ag.ID)
		if err != nil || got.SystemPrompt != "original" {
			t.Fatalf("child changed: %v %v", got, err)
		}
		if err := s.Update(child, ag); err != nil {
			t.Fatal(err)
		}
	})
}
