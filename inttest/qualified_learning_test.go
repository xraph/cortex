//go:build integration

package inttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/tenant"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	cortexagent "github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	fabriqbrain "github.com/xraph/cortex/integrations/fabriq"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/store/storetest"
	"github.com/xraph/cortex/verification"
)

type qualifiedAuthority struct{ denied bool }

func (a *qualifiedAuthority) AuthorizeLesson(context.Context, learning.Action, learning.Resource) error {
	if a.denied {
		return learning.ErrDenied
	}
	return nil
}
func (a *qualifiedAuthority) AuthorizeResultContent(context.Context, verification.ResultContentRequest) error {
	if a.denied {
		return learning.ErrDenied
	}
	return nil
}
func (*qualifiedAuthority) WithCommitFence(ctx context.Context, binding verification.Binding, next func(verification.Binding) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return next(binding)
}
func (*qualifiedAuthority) Now() time.Time { return time.Now().UTC() }

type qualifiedLostReply struct {
	*fabriqbrain.LessonToolkit
	lose, outage bool
	afterRecall  func()
}

func (s *qualifiedLostReply) Remember(ctx context.Context, req agent.RememberRequest) (command.Result, error) {
	if s.outage {
		return command.Result{}, learning.ErrUnavailable
	}
	result, err := s.LessonToolkit.Remember(ctx, req)
	if err == nil && s.lose {
		s.lose = false
		return command.Result{}, errors.New("lost response")
	}
	return result, err
}
func (s *qualifiedLostReply) Recall(ctx context.Context, req agent.RecallRequest) (agent.ContextPack, error) {
	pack, err := s.LessonToolkit.Recall(ctx, req)
	if s.afterRecall != nil {
		s.afterRecall()
	}
	return pack, err
}
func openQualifiedStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	driver := sqlitedriver.New()
	if err := driver.Open(context.Background(), path+"?_pragma=journal_mode(WAL)"); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(driver)
	if err != nil {
		t.Fatal(err)
	}
	s := sqlite.New(db)
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type qualifiedRecallClient struct {
	query    string
	runtime  bool
	calls    int
	observed string
}

func (c *qualifiedRecallClient) Complete(_ context.Context, request *llm.Request) (*llm.Response, error) {
	c.calls++
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	c.observed += string(body)
	if c.runtime && c.calls == 1 {
		arguments, marshalErr := json.Marshal(map[string]string{"query": c.query})
		if marshalErr != nil {
			return nil, marshalErr
		}
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "recall", Name: "knowledge_search", Arguments: string(arguments)}}, FinishReason: "tool_calls"}, nil
	}
	return &llm.Response{Content: "done", FinishReason: "stop"}, nil
}

func (*qualifiedRecallClient) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("stream not used")
}

func assertInstalledLessonProvenance(ctx context.Context, t *testing.T, local *sqlite.Store, provider *fabriqbrain.LessonProvider, publication learning.Publication) {
	t.Helper()
	for _, runtime := range []bool{false, true} {
		name := "prepared-recall"
		if runtime {
			name = "runtime-recall"
		}
		t.Run(name, func(t *testing.T) {
			config := &cortexagent.Config{ID: id.NewAgentID(), Name: name}
			if !runtime {
				config.InlineSkills = []string{name}
				if err := local.CreateSkill(ctx, &skill.Skill{Entity: cortex.NewEntity(), ID: id.NewSkillID(), Name: name, Knowledge: []skill.KnowledgeRef{{Source: publication.Payload.Statement}}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := local.Create(ctx, config); err != nil {
				t.Fatal(err)
			}
			client := &qualifiedRecallClient{query: publication.Payload.Statement, runtime: runtime}
			instance, err := engine.New(engine.WithStore(local), engine.WithKnowledge(provider), engine.WithLLM(client))
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.RunAgent(ctx, name, "apply the runbook", nil)
			if err != nil {
				t.Fatal(err)
			}
			current, err := local.GetRun(ctx, result.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := []run.Revision{{Name: "lesson:" + publication.LessonID.String(), Revision: strconv.FormatInt(publication.Revision, 10), Digest: publication.PayloadDigest}}
			if !slices.Equal(current.Provenance.RecalledLessons, want) || !strings.Contains(client.observed, publication.Payload.Statement) {
				t.Fatalf("installed recall provenance: %+v", current.Provenance.RecalledLessons)
			}
			if !errors.Is(learning.ValidateProvenance(current.Provenance), learning.ErrIneligible) {
				t.Fatal("recalled lesson became independent evidence")
			}
			assertInstalledRecallCannotPromote(ctx, t, local, current)
		})
	}
}

func assertInstalledRecallCannotPromote(ctx context.Context, t *testing.T, local *sqlite.Store, current *run.Run) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Second)
	scope := cortex.ScopeFromContext(ctx)
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "test-verifier", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte(current.Output)})
	require.NoError(t, err)
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "check retrieved run output", Criteria: []verification.Criterion{{ID: "test", Kind: verification.KindTest, Required: true, Verifier: "test", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	current.VerificationStatus = verification.StatusPending
	current.Provenance.AcceptanceRef = contract.ID.String()
	require.NoError(t, local.UpdateRun(ctx, current))
	// The binding deliberately omits recall. Acceptance must use the saved run.
	binding := verification.Binding{Scope: scope, RunID: current.ID, Candidate: artifact.Ref, ContractDigest: contract.Digest, Context: verification.ContextSnapshot{PrincipalRef: "operator", AuthorityRevision: "1", PreparedDigest: current.Provenance.PreparedDigest, PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: binding, CheckedAt: now, ExpiresAt: now.Add(time.Hour), Outcomes: []verification.Outcome{{CriterionID: "test", Verifier: "test", VerifierRevision: "1", Status: verification.StatusPassed}}})
	require.NoError(t, err)
	require.NoError(t, local.CreateContract(ctx, contract))
	require.NoError(t, local.SaveEvidence(ctx, artifact))
	require.NoError(t, local.SaveReport(ctx, report))
	commit, err := local.CommitVerifiedResult(ctx, verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: current.Revision, ContractID: contract.ID, ReportIDs: []id.ID{report.ID}, Current: binding})
	require.NoError(t, err)
	result := verification.ResultReadRequest{SchemaVersion: 1, RunID: current.ID, ResultID: commit.Request.ResultID, ReceiptDigest: commit.Digest}
	_, err = local.ReadResultSnapshot(ctx, result)
	require.NoError(t, err, "the source result is independently readable before the learning gate")
	proposal := learning.AcceptRequest{ID: id.New(learning.PrefixLesson), Domain: "reasoning", Result: result, ExpiresAt: now.Add(30 * time.Minute), Actor: "operator"}
	_, err = local.AcceptLesson(ctx, proposal)
	require.ErrorIs(t, err, learning.ErrIneligible)
	_, err = local.GetLesson(ctx, proposal.ID)
	require.ErrorIs(t, err, learning.ErrNotFound)
}

func TestQualifiedLearningInstalledFabriq(t *testing.T) {
	const entity = "qualified_lessons"
	fabric := openLearningFabric(t, fabriqbrain.LessonSpec(entity))
	path := filepath.Join(t.TempDir(), "lessons.db")
	local := openQualifiedStore(t, path)
	ctx, request := storetest.AcceptedLessonFixture(t, local)
	authority := &qualifiedAuthority{}
	makeGuard := func() *learning.Guard {
		guard, err := learning.NewGuard(learning.GuardConfig{Repository: local, Results: local, ContentAuthority: authority, Authority: authority, Fence: authority, Clock: authority})
		if err != nil {
			t.Fatal(err)
		}
		return guard
	}
	guard := makeGuard()
	publication, err := guard.Accept(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	toolkit, err := fabriqbrain.NewLessonToolkit(fabric, fabric.Registry(), hashEmbedder{}, entity)
	if err != nil {
		t.Fatal(err)
	}
	sink := &qualifiedLostReply{LessonToolkit: toolkit, lose: true}
	makeAdapter := func() *fabriqbrain.LessonAdapter {
		adapter, createErr := fabriqbrain.NewLessonStore(sink, qualifiedLegacyFabric{Fabric: fabric}, fabriqbrain.LessonConfig{Entity: entity, Repository: local, Guard: guard, Tenant: func(context.Context, cortex.Scope) (string, error) { return "acme", nil }})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return adapter
	}
	adapter := makeAdapter()
	if err = adapter.CheckReady(ctx); err != nil {
		t.Fatal(err)
	}
	publisher, err := learning.NewPublisher(local, adapter, guard, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publisher.WorkPage(ctx, ""); err != nil {
		t.Fatal(err)
	}
	delivered, err := local.GetLessonPublication(ctx, publication.LessonID, 1)
	if err != nil || delivered.Receipt == nil || delivered.Receipt.Version != 1 {
		t.Fatalf("delivery: %+v %v", delivered, err)
	}
	if err = local.Close(); err != nil {
		t.Fatal(err)
	}
	local = openQualifiedStore(t, path)
	guard = makeGuard()
	adapter = makeAdapter()
	if receipt, replayErr := adapter.Deliver(ctx, delivered); replayErr != nil || receipt.Version != 1 {
		t.Fatalf("replay: %+v %v", receipt, replayErr)
	}
	mapped, err := tenant.WithTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	indexer, err := agent.NewIndexer(fabric, fabric.Registry(), hashEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	if n, indexErr := indexer.Reindex(mapped, entity); indexErr != nil || n != 1 {
		t.Fatalf("index: %d %v", n, indexErr)
	}
	provider, err := fabriqbrain.NewLessonProvider(adapter, request.Domain)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := provider.Retrieve(ctx, publication.Payload.Statement, nil)
	if err != nil || len(chunks) != 1 || chunks[0].Metadata["evidence_class"] != "validated" {
		t.Fatalf("qualified retrieval: %+v %v", chunks, err)
	}
	assertInstalledLessonProvenance(ctx, t, local, provider, publication)
	for _, domain := range []string{"unrelated", "reasoning-other"} {
		page, queryErr := adapter.Recall(ctx, learning.Query{Scope: publication.Payload.Scope, Domain: domain, HarnessSchema: 1, Query: publication.Payload.Statement, Cutoff: time.Now().UTC(), Limit: 5})
		if queryErr != nil || len(page.Items) != 0 {
			t.Fatalf("domain isolation: %+v %v", page, queryErr)
		}
	}
	wrongScope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}}
	if _, queryErr := adapter.Recall(ctx, learning.Query{Scope: wrongScope, Domain: request.Domain, HarnessSchema: 1, Query: "x", Cutoff: time.Now().UTC(), Limit: 5}); !errors.Is(queryErr, learning.ErrInvalid) {
		t.Fatalf("scope override: %v", queryErr)
	}
	if _, queryErr := adapter.Recall(ctx, learning.Query{Scope: publication.Payload.Scope, Domain: request.Domain, HarnessSchema: 2, Query: "x", Cutoff: time.Now().UTC(), Limit: 5}); !errors.Is(queryErr, learning.ErrInvalid) {
		t.Fatalf("schema override: %v", queryErr)
	}
	authority.denied = true
	if _, err = provider.Retrieve(ctx, publication.Payload.Statement, nil); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("denied retrieval: %v", err)
	}
	authority.denied = false
	sink.afterRecall = func() { authority.denied = true }
	if _, err = provider.Retrieve(ctx, publication.Payload.Statement, nil); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("revoked grant during hydration: %v", err)
	}
	authority.denied = false
	sink.afterRecall = nil
	replacement := request
	replacement.ID = id.New(learning.PrefixLesson)
	replacement.Corrects = &learning.Ref{ID: publication.LessonID, Revision: 1, Digest: publication.PayloadDigest}
	replacement.Applicability = []string{"corrected applicability"}
	corrected, err := guard.Accept(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	sink.outage = true
	withdrawal, err := local.GetLessonPublication(ctx, publication.LessonID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.Deliver(ctx, withdrawal); !errors.Is(err, learning.ErrUnavailable) {
		t.Fatalf("correction withdrawal outage: %v", err)
	}
	chunks, err = provider.Retrieve(ctx, publication.Payload.Statement, nil)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("stale destination escaped local withdrawal: %+v %v", chunks, err)
	}
	sink.outage = false
	revoked, err := local.GetLessonPublication(ctx, publication.LessonID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, revokeErr := adapter.Deliver(ctx, revoked); revokeErr != nil || receipt.Version != 2 {
		t.Fatalf("withdraw retry: %+v %v", receipt, revokeErr)
	}
	if _, err = adapter.Deliver(ctx, delivered); !errors.Is(err, learning.ErrIneligible) {
		t.Fatalf("late publish: %v", err)
	}
	if _, err = adapter.Deliver(ctx, corrected); err != nil {
		t.Fatal(err)
	}
	if _, err = indexer.Reindex(mapped, entity); err != nil {
		t.Fatal(err)
	}
	chunks, err = provider.Retrieve(ctx, corrected.Payload.Statement, nil)
	if err != nil || len(chunks) != 1 || chunks[0].DocumentID != corrected.LessonID.String() {
		t.Fatalf("corrected recall: %+v %v", chunks, err)
	}
	deletion := learning.DeletionRequest{ID: corrected.LessonID, ExpectedRevision: 1, Actor: "retention-worker", Reason: "retention_expired"}
	deleted, err := adapter.Delete(ctx, deletion)
	if err != nil || !deleted.ActiveCopiesRemoved || deleted.HistoryErased {
		t.Fatalf("deletion: %+v %v", deleted, err)
	}
	if _, err = adapter.Delete(ctx, deletion); err != nil {
		t.Fatalf("deletion replay: %v", err)
	}
	if _, err = adapter.SweepDeleted(ctx, ""); err != nil {
		t.Fatalf("restartable deletion scan: %v", err)
	}
	chunks, err = provider.Retrieve(ctx, corrected.Payload.Statement, nil)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("deleted recall: %+v %v", chunks, err)
	}
}
