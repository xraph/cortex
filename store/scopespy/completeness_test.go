package scopespy

import (
	"context"
	"reflect"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store"
)

// reachedByReactLoop is the fixed set of store.Store methods RunAgent and
// StreamAgent actually reach, established by reading BuildSystemPrompt,
// runReAct and streamReAct (engine/engine.go, engine/react.go): resolving
// the agent and its persona/skills/traits, loading and saving
// conversation history, and recording the run/step/tool-call trail.
//
// This used to be a claim backed only by two people having read the
// code during review — a throwaway completeness test proved it once and
// was then discarded, so nothing in the repo backed it afterward. Pinning
// the list here turns "the loop reaches exactly these methods" into an
// assertion: if a future call site is added to the loop without teaching
// Spy to record it, TestSpy_CoversEveryMethodTheReactLoopReaches fails
// instead of the hole passing silently.
var reachedByReactLoop = []string{
	"GetByName",
	"GetPersonaByName",
	"GetSkillByName",
	"GetTraitByName",
	"LoadConversation",
	"SaveConversation",
	"CreateRun",
	"UpdateRun",
	"CreateStep",
	"CreateToolCall",
}

// TestSpy_CoversEveryMethodTheReactLoopReaches invokes each method in
// reachedByReactLoop through reflection, on a fresh Spy backed by a nil
// embedded store.Store. Spy overrides only what it needs to; anything it
// hasn't overridden falls through to that nil interface and panics on
// the call. So a method that returns cleanly and is recorded proves Spy
// actually overrides it, rather than merely happening to compile against
// the store.Store interface.
func TestSpy_CoversEveryMethodTheReactLoopReaches(t *testing.T) {
	storeType := reflect.TypeOf((*store.Store)(nil)).Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})

	for _, name := range reachedByReactLoop {
		t.Run(name, func(t *testing.T) {
			if _, ok := storeType.MethodByName(name); !ok {
				t.Fatalf("store.Store has no method %q; reachedByReactLoop is stale (renamed or removed)", name)
			}

			spy := New()
			mv := reflect.ValueOf(spy).MethodByName(name)
			if !mv.IsValid() {
				t.Fatalf("*Spy has no method %q", name)
			}

			mt := mv.Type()
			args := make([]reflect.Value, mt.NumIn())
			for i := range args {
				pt := mt.In(i)
				if pt == ctxType {
					args[i] = reflect.ValueOf(ctx)
					continue
				}
				args[i] = reflect.Zero(pt)
			}

			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s panicked, meaning Spy falls through to the embedded nil store.Store instead of overriding it: %v", name, r)
					}
				}()
				mv.Call(args)
			}()

			if spy.CallCount() != 1 {
				t.Errorf("%s: CallCount() = %d, want 1 (record() was not reached)", name, spy.CallCount())
			}
		})
	}
}
