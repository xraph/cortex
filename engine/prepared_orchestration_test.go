package engine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

func TestPrepareOrchestrationPinsEveryParticipant(t *testing.T) {
	ctx := overlayCtx("prepared-team")
	s := newSQLiteStoreForTest(t)
	for _, name := range []string{"left", "right", "synthesizer"} {
		if err := s.Create(ctx, &agent.Config{ID: id.NewAgentID(), Name: name, SystemPrompt: "original " + name, Model: "original-model"}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "team", Strategy: orchestration.StrategyParallel, Participants: []orchestration.Participant{{AgentName: "left"}, {AgentName: "right"}}, Settings: orchestration.Settings{Aggregator: "synthesizer", Model: "team-model"}}
	if err := s.CreateOrchestration(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(s))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := e.PrepareOrchestration(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Participants) != 3 || view.Snapshot.ID == "" || view.Snapshot.Scope != cortex.ScopeFromContext(ctx).Canonical() {
		t.Fatalf("unbound team: %+v", view)
	}
	for name, child := range view.Participants {
		part, inspectErr := child.Inspect()
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if part.Config.Model != "team-model" || part.Provenance.SourceSnapshot != view.Snapshot {
			t.Fatalf("participant %s outside capture: %+v", name, part)
		}
	}
	encoded, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	checkMalformedTeamWires(t, encoded)
	var restored engine.PreparedOrchestration
	if err = json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Digest() != prepared.Digest() {
		t.Fatal("round trip changed digest")
	}
	var numericWire struct {
		Digest   string                     `json:"digest"`
		Snapshot map[string]json.RawMessage `json:"snapshot"`
	}
	if err = json.Unmarshal(encoded, &numericWire); err != nil {
		t.Fatal(err)
	}
	numericConfig := view.Config
	numericConfig.Metadata = map[string]any{"exact": int64(9007199254740993)}
	numericWire.Snapshot["config"], err = json.Marshal(numericConfig)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(numericWire.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	numericWire.Digest = hex.EncodeToString(sum[:])
	numericData, err := json.Marshal(numericWire)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(numericData, &restored); err != nil {
		t.Fatal(err)
	}
	numeric, err := restored.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(numeric.Config.Metadata)
	if err != nil || !bytes.Contains(metadata, []byte("9007199254740993")) {
		t.Fatalf("artifact metadata rounded: %s %v", metadata, err)
	}
	view.Config.Settings.Model = "mutated"
	delete(view.Participants, "left")
	cfg.Settings.Model = "new-model"
	if err = s.UpdateOrchestration(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if again.Config.Settings.Model != "team-model" || len(again.Participants) != 3 {
		t.Fatal("inspection or store edit mutated preparation")
	}
	var bad map[string]any
	if err = json.Unmarshal(encoded, &bad); err != nil {
		t.Fatal(err)
	}
	bad["digest"] = "wrong"
	encoded, err = json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &restored); !errors.Is(err, cortex.ErrInvalidPrepared) {
		t.Fatalf("tampered team accepted: %v", err)
	}
}

type changingTeamSources struct {
	store.OrchestrationPreparationSources
	change func()
}

func (s changingTeamSources) GetByName(ctx context.Context, name string) (*agent.Config, error) {
	s.change()
	return s.OrchestrationPreparationSources.GetByName(ctx, name)
}

func TestPrepareOrchestrationUsesOneSnapshot(t *testing.T) {
	ctx := overlayCtx("atomic-team")
	s := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "team.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	left := &agent.Config{ID: id.NewAgentID(), Name: "left", SystemPrompt: "before"}
	right := &agent.Config{ID: id.NewAgentID(), Name: "right", SystemPrompt: "before"}
	for _, ag := range []*agent.Config{left, right} {
		if err := s.Create(ctx, ag); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "team", Strategy: orchestration.StrategyParallel, Participants: []orchestration.Participant{{AgentName: left.Name}, {AgentName: right.Name}}}
	if err := s.CreateOrchestration(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	captures, changes := 0, 0
	wrapped := controlledSnapshotStore{Store: s, capture: func(c context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
		captures++
		return s.CapturePreparation(c, func(sources store.PreparationSources) error {
			return collect(changingTeamSources{OrchestrationPreparationSources: sources.(store.OrchestrationPreparationSources), change: func() {
				if changes > 0 {
					return
				}
				changes++
				right.SystemPrompt = "after"
				cfg.Strategy = orchestration.StrategySequential
				if err := s.Update(ctx, right); err != nil {
					t.Fatal(err)
				}
				if err := s.UpdateOrchestration(ctx, cfg); err != nil {
					t.Fatal(err)
				}
			}})
		})
	}}
	e, err := engine.New(engine.WithStore(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareOrchestration(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := p.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if captures != 1 || changes != 1 || view.Config.Strategy != orchestration.StrategyParallel {
		t.Fatalf("mixed or repeated capture: %+v %d %d", view, captures, changes)
	}
	child, err := view.Participants["right"].Inspect()
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range child.Sections {
		if strings.Contains(section.Body, "after") {
			t.Fatal("participant escaped snapshot")
		}
	}
	fresh, err := e.PrepareOrchestration(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := fresh.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if updated.Config.Strategy != orchestration.StrategySequential {
		t.Fatal("next capture missed committed changes")
	}
}

type independentTeamStore struct{ store.Store }

func TestPrepareOrchestrationFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing participant", "foreign scope", "no scope", "cancelled", "no snapshot", "no team view", "invalid receipt", "invalid strategy"} {
		t.Run(mode, func(t *testing.T) {
			ctx := overlayCtx("team-boundary", "project")
			s := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
			if err := s.Create(ctx, ag); err != nil {
				t.Fatal(err)
			}
			cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "team", Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: ag.Name}}}
			if mode == "missing participant" {
				cfg.Participants[0].AgentName = "absent"
			}
			if mode == "invalid strategy" {
				cfg.Strategy = "unknown"
			}
			if err := s.CreateOrchestration(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			var selected store.Store = s
			switch mode {
			case "foreign scope":
				ctx = overlayCtx("team-boundary")
			case "no scope":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "no snapshot":
				selected = independentTeamStore{Store: s}
			case "no team view", "invalid receipt":
				selected = controlledSnapshotStore{Store: s, capture: func(c context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
					receipt, captureErr := s.CapturePreparation(c, func(sources store.PreparationSources) error {
						if mode == "no team view" {
							return collect(struct{ store.PreparationSources }{sources})
						}
						return collect(sources)
					})
					if mode == "invalid receipt" && receipt != nil {
						receipt.Scope = "other"
					}
					return receipt, captureErr
				}}
			}
			e, err := engine.New(engine.WithStore(selected))
			if err != nil {
				t.Fatal(err)
			}
			p, err := e.PrepareOrchestration(ctx, cfg.ID)
			if err == nil || p != nil {
				t.Fatalf("unsafe preparation accepted: %+v %v", p, err)
			}
		})
	}
}

func checkMalformedTeamWires(t *testing.T, encoded []byte) {
	t.Helper()
	for _, mode := range []string{"version", "missing participant", "extra participant", "foreign scope", "foreign receipt", "model changed", "oversized config"} {
		t.Run(mode, func(t *testing.T) {
			var wire struct {
				Digest   string                     `json:"digest"`
				Snapshot map[string]json.RawMessage `json:"snapshot"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "version":
				wire.Snapshot["schema_version"] = json.RawMessage("2")
			case "missing participant", "extra participant":
				var participants map[string]json.RawMessage
				if err := json.Unmarshal(wire.Snapshot["participants"], &participants); err != nil {
					t.Fatal(err)
				}
				if mode == "missing participant" {
					delete(participants, "left")
				} else {
					participants["extra"] = participants["left"]
				}
				data, err := json.Marshal(participants)
				if err != nil {
					t.Fatal(err)
				}
				wire.Snapshot["participants"] = data
			case "foreign scope", "model changed", "oversized config":
				var config orchestration.Config
				if err := json.Unmarshal(wire.Snapshot["config"], &config); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "foreign scope":
					config.Scope.Levels[0].Value = "other"
				case "oversized config":
					config.Metadata = map[string]any{"large": strings.Repeat("x", 1<<20)}
				default:
					config.Settings.Model = "other"
				}
				data, err := json.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				wire.Snapshot["config"] = data
			case "foreign receipt":
				var receipt run.SourceSnapshot
				if err := json.Unmarshal(wire.Snapshot["snapshot"], &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.ID = "different-capture"
				data, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				wire.Snapshot["snapshot"] = data
			}
			payload, err := json.Marshal(wire.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(payload)
			wire.Digest = hex.EncodeToString(digest[:])
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var artifact engine.PreparedOrchestration
			if err = json.Unmarshal(data, &artifact); !errors.Is(err, cortex.ErrInvalidPrepared) {
				t.Fatalf("invalid artifact accepted: %v", err)
			}
		})
	}
}
