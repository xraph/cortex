package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

func testPreparationSnapshot(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	provider, ok := s.(store.PreparationSnapshotter)
	if !ok {
		t.Skip("optional preparation snapshot capability unavailable")
	}
	ctx, cancel := context.WithTimeout(cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "atomic"}, {Key: "project", Value: "one"}}}), 30*time.Second)
	defer cancel()
	scope := cortex.ScopeFromContext(ctx)
	parent := cortex.Scope{Levels: scope.Levels[:1]}
	parentCtx := cortex.WithScope(ctx, parent)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "atomic", SystemPrompt: "before"}
	p := &persona.Persona{ID: id.NewPersonaID(), Name: "persona", Identity: "before"}
	sk := &skill.Skill{ID: id.NewSkillID(), Name: "skill", Description: "before"}
	tr := &trait.Trait{ID: id.NewTraitID(), Name: "trait", Description: "before"}
	overlay := &prompt.Overlay{ID: id.NewOverlayID(), AgentID: ag.ID, Model: "before"}
	for _, err := range []error{s.Create(ctx, ag), s.CreatePersona(ctx, p), s.CreateSkill(ctx, sk), s.CreateTrait(ctx, tr), s.CreateOverlay(parentCtx, overlay)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	config := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "atomic-team", Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: ag.Name}}, Settings: orchestration.Settings{Model: "before"}}
	if err := s.CreateOrchestration(ctx, config); err != nil {
		t.Fatal(err)
	}
	var expired store.PreparationSources
	receipt, err := provider.CapturePreparation(ctx, func(view store.PreparationSources) error {
		expired = view
		teamView, supports := view.(store.OrchestrationPreparationSources)
		if !supports {
			return fmt.Errorf("orchestration snapshot reads unavailable")
		}
		originalConfig, readErr := teamView.GetOrchestration(ctx, config.ID)
		if readErr != nil {
			return readErr
		}
		if originalConfig.Settings.Model != "before" {
			return fmt.Errorf("unexpected initial orchestration")
		}
		// Establish the snapshot before a second connection commits every edit.
		before, readErr := view.Get(ctx, ag.ID)
		if readErr != nil {
			return readErr
		}
		if before.SystemPrompt != "before" {
			return fmt.Errorf("unexpected initial agent")
		}
		ag.SystemPrompt = "after"
		p.Identity = "after"
		sk.Description = "after"
		tr.Description = "after"
		overlay.Model = "after"
		config.Settings.Model = "after"
		for _, writeErr := range []error{s.UpdateOrchestration(ctx, config), s.Update(ctx, ag), s.UpdatePersona(ctx, p), s.UpdateSkill(ctx, sk), s.UpdateTrait(ctx, tr), s.UpdateOverlay(parentCtx, overlay), s.CreateOverlay(ctx, &prompt.Overlay{ID: id.NewOverlayID(), AgentID: ag.ID, Model: "new"})} {
			if writeErr != nil {
				return writeErr
			}
		}
		gotConfig, readErr := teamView.GetOrchestration(ctx, config.ID)
		if readErr != nil {
			return readErr
		}
		namedAgent, readErr := teamView.GetByName(ctx, ag.Name)
		if readErr != nil {
			return readErr
		}
		if gotConfig.Settings.Model != "before" || namedAgent.SystemPrompt != "before" {
			return fmt.Errorf("snapshot mixed orchestration or named agent versions")
		}
		gotAgent, readErr := view.Get(ctx, ag.ID)
		if readErr != nil {
			return readErr
		}
		gotPersona, readErr := view.GetPersonaByName(ctx, p.Name)
		if readErr != nil {
			return readErr
		}
		gotSkill, readErr := view.GetSkillByName(ctx, sk.Name)
		if readErr != nil {
			return readErr
		}
		gotTrait, readErr := view.GetTraitByName(ctx, tr.Name)
		if readErr != nil {
			return readErr
		}
		gotOverlay, readErr := view.GetOverlayForAgentAt(ctx, ag.ID, parent)
		if readErr != nil {
			return readErr
		}
		if gotAgent.SystemPrompt != "before" || gotPersona.Identity != "before" || gotSkill.Description != "before" || gotTrait.Description != "before" || gotOverlay.Model != "before" {
			return fmt.Errorf("snapshot mixed committed source versions")
		}
		if _, readErr = view.GetOverlayForAgentAt(ctx, ag.ID, scope); !errors.Is(readErr, cortex.ErrOverlayNotFound) {
			return fmt.Errorf("snapshot observed new overlay: %w", readErr)
		}
		if _, readErr = teamView.GetOrchestration(parentCtx, config.ID); !errors.Is(readErr, cortex.ErrInvalidScope) {
			return fmt.Errorf("orchestration widened capture scope: %w", readErr)
		}
		if _, readErr = teamView.GetByName(parentCtx, ag.Name); !errors.Is(readErr, cortex.ErrInvalidScope) {
			return fmt.Errorf("named agent widened capture scope: %w", readErr)
		}
		if _, readErr = view.Get(parentCtx, ag.ID); !errors.Is(readErr, cortex.ErrInvalidScope) {
			return fmt.Errorf("view widened capture scope: %w", readErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || receipt.ID == "" || receipt.Scope != scope.Canonical() || receipt.Provider == "" || receipt.Isolation == "" {
		t.Fatalf("invalid receipt: %+v", receipt)
	}
	if _, err = expired.Get(ctx, ag.ID); !errors.Is(err, cortex.ErrPreparationUnavailable) {
		t.Fatalf("view survived callback: %v", err)
	}
	expiredTeam, teamOK := expired.(store.OrchestrationPreparationSources)
	if !teamOK {
		t.Fatal("missing orchestration preparation view")
	}
	if _, err = expiredTeam.GetByName(ctx, ag.Name); !errors.Is(err, cortex.ErrPreparationUnavailable) {
		t.Fatalf("named agent view survived callback: %v", err)
	}
	if _, err = expiredTeam.GetOrchestration(ctx, config.ID); !errors.Is(err, cortex.ErrPreparationUnavailable) {
		t.Fatalf("orchestration view survived callback: %v", err)
	}
	afterConfig, err := s.GetOrchestration(ctx, config.ID)
	if err != nil || afterConfig.Settings.Model != "after" {
		t.Fatalf("orchestration writer did not commit: %v %v", afterConfig, err)
	}
	after, err := s.Get(ctx, ag.ID)
	if err != nil || after.SystemPrompt != "after" {
		t.Fatalf("writer did not commit: %v %v", after, err)
	}
	t.Run("current prepared sources", func(t *testing.T) {
		ag.PersonaRef, ag.InlineSkills, ag.InlineTraits = p.Name, []string{sk.Name}, []string{tr.Name}
		if updateErr := s.Update(ctx, ag); updateErr != nil {
			t.Fatal(updateErr)
		}
		e, engineErr := engine.New(engine.WithStore(s), engine.WithAtomicPreparation())
		if engineErr != nil {
			t.Fatal(engineErr)
		}
		prepared, prepareErr := e.PrepareAgent(ctx, ag.ID, nil)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		checked, checkErr := e.CheckPreparedSources(ctx, prepared)
		if checkErr != nil || checked == nil || checked.PreparedDigest != prepared.Digest() || checked.SourceCount != 6 || checked.Snapshot.Scope != scope.Canonical() {
			t.Fatalf("current source check failed: %+v %v", checked, checkErr)
		}
		overlay.Model = "changed ancestor"
		if updateErr := s.UpdateOverlay(parentCtx, overlay); updateErr != nil {
			t.Fatal(updateErr)
		}
		if checked, checkErr = e.CheckPreparedSources(ctx, prepared); checked != nil || !errors.Is(checkErr, cortex.ErrPreparationConflict) {
			t.Fatalf("changed ancestor accepted: %+v %v", checked, checkErr)
		}
	})
	t.Run("prepared orchestration", func(t *testing.T) {
		runtime, buildErr := engine.New(engine.WithStore(s))
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		prepared, prepareErr := runtime.PrepareOrchestration(ctx, config.ID)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		view, inspectErr := prepared.Inspect()
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		child, inspectErr := view.Participants[ag.Name].Inspect()
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if len(view.Participants) != 1 || view.Config.ID != config.ID || view.Snapshot.Provider != receipt.Provider || child.Provenance.SourceSnapshot != view.Snapshot {
			t.Fatalf("team source capture incomplete: %+v", view)
		}
	})
	sentinel := errors.New("collection rejected")
	receipt, err = provider.CapturePreparation(ctx, func(store.PreparationSources) error { return sentinel })
	if receipt != nil || !errors.Is(err, sentinel) {
		t.Fatalf("failed collection returned receipt: %v %v", receipt, err)
	}
	receipt, err = provider.CapturePreparation(context.Background(), func(store.PreparationSources) error { t.Fatal("unscoped callback ran"); return nil })
	if receipt != nil || !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("unscoped capture: %v %v", receipt, err)
	}
	cancelled, stop := context.WithCancel(ctx)
	receipt, err = provider.CapturePreparation(cancelled, func(view store.PreparationSources) error {
		if _, readErr := view.Get(cancelled, ag.ID); readErr != nil {
			return readErr
		}
		stop()
		return nil
	})
	stop()
	if receipt != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled collection returned receipt: %v %v", receipt, err)
	}
}
