package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

func TestPreparedSourceCheckDetectsCurrentChanges(t *testing.T) {
	for _, change := range []string{"none", "agent", "persona", "skill", "trait", "overlay_added", "overlay_removed"} {
		t.Run(change, func(t *testing.T) {
			ctx := overlayCtx("compatibility")
			s := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", PersonaRef: "identity", InlineSkills: []string{"research"}, InlineTraits: []string{"careful"}}
			p := &persona.Persona{ID: id.NewPersonaID(), Name: "identity", Identity: "original"}
			sk := &skill.Skill{ID: id.NewSkillID(), Name: "research", SystemPromptFragment: "original"}
			tr := &trait.Trait{ID: id.NewTraitID(), Name: "careful", Description: "original"}
			o := &prompt.Overlay{ID: id.NewOverlayID(), AgentID: ag.ID}
			for _, err := range []error{s.Create(ctx, ag), s.CreatePersona(ctx, p), s.CreateSkill(ctx, sk), s.CreateTrait(ctx, tr)} {
				if err != nil {
					t.Fatal(err)
				}
			}
			if change == "overlay_removed" {
				if err := s.CreateOverlay(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			e, err := engine.New(engine.WithStore(s))
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			// A derived artifact keeps its source identity while changing its prompt.
			derived, err := prepared.Derive(engine.PreparedEdits{Temperature: new(0.25)})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "agent":
				ag.SystemPrompt = "changed"
				err = s.Update(ctx, ag)
			case "persona":
				p.Identity = "changed"
				err = s.UpdatePersona(ctx, p)
			case "skill":
				sk.SystemPromptFragment = "changed"
				err = s.UpdateSkill(ctx, sk)
			case "trait":
				tr.Description = "changed"
				err = s.UpdateTrait(ctx, tr)
			case "overlay_added":
				err = s.CreateOverlay(ctx, o)
			case "overlay_removed":
				err = s.DeleteOverlay(ctx, o.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			checked, err := e.CheckPreparedSources(ctx, derived)
			if change != "none" {
				if checked != nil || !errors.Is(err, cortex.ErrPreparationConflict) {
					t.Fatalf("changed source accepted: %+v %v", checked, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if checked.PreparedDigest != derived.Digest() || checked.SourceCount != 5 || checked.Snapshot.ID == "" || checked.Snapshot.Scope != cortex.ScopeFromContext(ctx).Canonical() {
				t.Fatalf("missing source check binding: %+v", checked)
			}
		})
	}
}

func TestPreparedSourceCheckUsesOneSnapshot(t *testing.T) {
	ctx := overlayCtx("compatibility", "project")
	s := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sources.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", PersonaRef: "identity"}
	p := &persona.Persona{ID: id.NewPersonaID(), Name: "identity", Identity: "original"}
	for _, err := range []error{s.Create(ctx, ag), s.CreatePersona(ctx, p)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	preparer, err := engine.New(engine.WithStore(s))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := preparer.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	wrapped := controlledSnapshotStore{Store: s, capture: func(c context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
		return s.CapturePreparation(c, func(sources store.PreparationSources) error {
			return collect(&changingSnapshotSources{PreparationSources: sources, change: func() {
				if changed {
					return
				}
				changed = true
				p.Identity = "changed during capture"
				if updateErr := s.UpdatePersona(ctx, p); updateErr != nil {
					t.Fatal(updateErr)
				}
			}})
		})
	}}
	checker, err := engine.New(engine.WithStore(wrapped), engine.WithPublicationGate(snapshotPublicationGate{check: func() { t.Fatal("source inspection called scanner") }}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := checker.CheckPreparedSources(ctx, prepared)
	if err != nil || result == nil || !changed {
		t.Fatalf("mixed source snapshots: %+v %v", result, err)
	}
	if result, err = checker.CheckPreparedSources(ctx, prepared); result != nil || !errors.Is(err, cortex.ErrPreparationConflict) {
		t.Fatalf("next snapshot missed committed change: %+v %v", result, err)
	}
}

func TestPreparedSourceCheckRejectsIncompleteManifest(t *testing.T) {
	ctx := overlayCtx("compatibility")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
	e, _, _ := newConfigEngine(t, ctx, ag)
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"empty", "missing_agent", "missing_overlay", "unknown_kind", "invalid_digest", "conflicting_duplicate"} {
		t.Run(mode, func(t *testing.T) {
			var wire struct {
				Digest   string         `json:"digest"`
				Snapshot map[string]any `json:"snapshot"`
			}
			if decodeErr := json.Unmarshal(encoded, &wire); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			provenance := wire.Snapshot["provenance"].(map[string]any)
			reads := provenance["source_reads"].([]any)
			switch mode {
			case "empty":
				reads = nil
			case "missing_agent":
				reads = reads[1:]
			case "missing_overlay":
				reads = reads[:1]
			case "unknown_kind":
				reads = append(reads, map[string]any{"name": "unknown:value", "digest": reads[0].(map[string]any)["digest"]})
			case "invalid_digest":
				reads[0].(map[string]any)["digest"] = "invalid"
			case "conflicting_duplicate":
				reads = append(reads, map[string]any{"name": reads[0].(map[string]any)["name"], "digest": reads[1].(map[string]any)["digest"]})
			}
			provenance["source_reads"] = reads
			payload, encodeErr := json.Marshal(wire.Snapshot)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			digest := sha256.Sum256(payload)
			wire.Digest = hex.EncodeToString(digest[:])
			data, encodeErr := json.Marshal(wire)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			var changed engine.PreparedAgent
			if decodeErr := json.Unmarshal(data, &changed); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if result, checkErr := e.CheckPreparedSources(ctx, &changed); result != nil || checkErr == nil {
				t.Fatalf("invalid manifest accepted: %+v %v", result, checkErr)
			}
		})
	}
}

func TestPreparedSourceCheckFailsClosed(t *testing.T) {
	ctx := overlayCtx("compatibility")
	s := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(s))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"unsupported", "no_callback", "no_receipt", "duplicate_callback", "wrong_scope", "commit_failure", "ignored_read_error", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			checkCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var installed store.Store = controlledSnapshotStore{Store: s, capture: func(c context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
				receipt := &run.SourceSnapshot{ID: "capture", Provider: "fixture", Isolation: "fixture", Scope: cortex.ScopeFromContext(c).Canonical()}
				if mode == "no_callback" {
					return receipt, nil
				}
				if mode == "ignored_read_error" {
					_ = collect(nil)
					return receipt, nil
				}
				if readErr := collect(s); readErr != nil {
					return nil, readErr
				}
				switch mode {
				case "no_receipt":
					return nil, nil
				case "duplicate_callback":
					_ = collect(s)
				case "wrong_scope":
					receipt.Scope = "elsewhere"
				case "commit_failure":
					return nil, errors.New("commit failed")
				case "cancelled":
					cancel()
				}
				return receipt, nil
			}}
			if mode == "unsupported" {
				installed = noSnapshotStore{s}
			}
			checker, newErr := engine.New(engine.WithStore(installed))
			if newErr != nil {
				t.Fatal(newErr)
			}
			result, checkErr := checker.CheckPreparedSources(checkCtx, p)
			if result != nil || checkErr == nil {
				t.Fatalf("invalid capture accepted: %+v %v", result, checkErr)
			}
		})
	}
	for _, c := range []context.Context{context.Background(), overlayCtx("foreign")} {
		if result, checkErr := e.CheckPreparedSources(c, p); result != nil || checkErr == nil {
			t.Fatalf("scope mismatch accepted: %+v %v", result, checkErr)
		}
	}
}
