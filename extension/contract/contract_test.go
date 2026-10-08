package contract

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"
	"path/filepath"
	"testing"
)

func openStore(t *testing.T, path string) *sqlitestore.Store {
	t.Helper()
	drv := sqlitedriver.New()
	if err := drv.Open(context.Background(), path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatal(err)
	}
	s := sqlitestore.New(db)
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func principal(tenant string) dc.Principal {
	return dc.Principal{User: &dashauth.UserInfo{Subject: "operator", Scopes: []string{"cortex.read", "cortex.manage", "cortex.run", "cortex.approve"}}, Claims: map[string]any{"tenant_id": tenant, "app_id": "app-a"}}
}
func testService(t *testing.T, opts ...engine.Option) (*dispatcher.Dispatcher, *service, *sqlitestore.Store) {
	t.Helper()
	st := openStore(t, filepath.Join(t.TempDir(), "cortex.db"))
	e, err := engine.New(append([]engine.Option{engine.WithStore(st)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{Engine: e, Audit: func(context.Context, AuditEvent) error { return nil }}
	d := dispatcher.New(nil)
	if err = Register(d, dc.NewRegistry(), dc.NewWardenRegistry(), deps); err != nil {
		t.Fatal(err)
	}
	return d, &service{deps: deps}, st
}
func dispatch(t *testing.T, d *dispatcher.Dispatcher, p dc.Principal, intent string, body any) (json.RawMessage, error) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := d.Dispatch(context.Background(), dc.Request{Contributor: ContributorName, Intent: intent, IntentVersion: 1, Kind: dc.KindCommand, Payload: b}, p)
	return data, err
}
func TestClaimsNeverFallBackWhenPresentAndInvalid(t *testing.T) {
	fallback := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "default"}}}
	for _, key := range []string{"tenant_id", "app_id", "cortex_scope"} {
		for _, value := range []any{"", nil, 42, map[string]any{}} {
			t.Run(key+"/"+stringMustJSON(value), func(t *testing.T) {
				_, err := scopeFromClaims(dc.Principal{Claims: map[string]any{key: value}}, fallback)
				if !errors.Is(err, dc.ErrPermissionDenied) {
					t.Fatalf("got %v", err)
				}
			})
		}
	}
	scope, err := scopeFromClaims(dc.Principal{}, fallback)
	if err != nil || scope.Canonical() != fallback.Canonical() {
		t.Fatalf("absent fallback %v %v", scope, err)
	}
	_, err = scopeFromClaims(dc.Principal{Claims: map[string]any{"cortex_scope": cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}}, "tenant_id": "b"}}, fallback)
	if !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatal(err)
	}
}
func stringMustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func TestCRUDIsolationPatchAndAudit(t *testing.T) {
	d, s, st := testService(t)
	a, b := principal("tenant-a"), principal("tenant-b")
	ctx, err := s.context(context.Background(), a, "manage")
	if err != nil {
		t.Fatal(err)
	}
	ag := &agent.Config{ID: id.NewAgentID(), Name: "reviewer", Description: "keep", Enabled: true, Tools: []string{"read"}, Sections: []prompt.Section{{ID: "role", Body: "first", Order: 1}}}
	if err = s.deps.Engine.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	for _, intent := range []string{"agents.detail", "agents.update", "agents.delete"} {
		_, err = dispatch(t, d, b, intent, map[string]any{"id": ag.ID.String(), "patch": map[string]any{"description": "foreign"}})
		if !errors.Is(err, dc.ErrNotFound) {
			t.Fatalf("%s: %v", intent, err)
		}
	}
	_, err = dispatch(t, d, a, "agents.update", map[string]any{"id": ag.ID.String(), "patch": map[string]any{"tools": []string{}, "enabled": false, "sections": []prompt.Section{{ID: "role", Body: "second", Order: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "keep" || got.Enabled || len(got.Tools) != 0 || got.SystemPrompt != "second" || got.Scope.Canonical() != ag.Scope.Canonical() {
		t.Fatalf("patch lost fields: %+v", got)
	}
	s.deps.Audit = nil
	if err = s.write(ctx, a, "test", "", func() error { t.Fatal("write ran without audit"); return nil }); !errors.Is(err, dc.ErrUnavailable) {
		t.Fatal(err)
	}
	_, err = dispatch(t, d, dc.Principal{}, "agents.list", struct{}{})
	if !errors.Is(err, dc.ErrUnauthenticated) {
		t.Fatal(err)
	}
	reader := a
	reader.User = &dashauth.UserInfo{Subject: "reader", Scopes: []string{"cortex.read"}}
	_, err = dispatch(t, d, reader, "agents.delete", IDInput{ag.ID.String()})
	if !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatal(err)
	}
}
func TestPopulatedPersonaAndReferencedDelete(t *testing.T) {
	d, s, st := testService(t)
	p := principal("tenant-a")
	ctx, err := s.context(context.Background(), p, "manage")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := dispatch(t, d, p, "skills.create", map[string]any{"data": map[string]any{"name": "analysis", "tools": []map[string]any{{"tool_name": "read", "mastery": "expert", "guidance": "Read only"}}, "knowledge": []map[string]any{{"source": "manual", "inject_mode": "prompt", "priority": 2}}}})
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if err = json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	raw, err = dispatch(t, d, p, "personas.create", map[string]any{"data": map[string]any{"name": "operator", "identity": "Review the source", "skills": []map[string]any{{"skill_name": "analysis", "proficiency": "expert"}}, "communication_style": map[string]any{"tone": "direct", "formality": 0.4}, "perception": map[string]any{"detail_orientation": 0.8}}})
	if err != nil {
		t.Fatal(err)
	}
	var per persona.Persona
	if err = json.Unmarshal(raw, &per); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPersona(ctx, per.ID)
	if err != nil || len(got.Skills) != 1 || got.Perception.DetailOrientation != 0.8 || got.CommunicationStyle.Tone != "direct" {
		t.Fatalf("nested values: %+v %v", got, err)
	}
	_, err = dispatch(t, d, p, "skills.delete", map[string]any{"id": created["id"]})
	if !errors.Is(err, dc.ErrConflict) {
		t.Fatal(err)
	}
}
func TestRestartKeepsIdentityAndNestedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	st := openStore(t, path)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}})
	p := &persona.Persona{ID: id.NewPersonaID(), Name: "saved", Identity: "persisted", Skills: []persona.SkillAssignment{{SkillName: "analysis", Proficiency: "expert"}}}
	if err := st.CreatePersona(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, path)
	got, err := reopened.GetPersona(ctx, p.ID)
	if err != nil || got.ID != p.ID || len(got.Skills) != 1 || got.Skills[0].Proficiency != "expert" {
		t.Fatalf("restart: %+v %v", got, err)
	}
}
func TestEmptyScopeAndPaginationAreRefused(t *testing.T) {
	d, _, st := testService(t)
	if _, err := st.List(context.Background(), &agent.ListFilter{}); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("empty scope store behavior: %v", err)
	}
	for _, in := range []ListInput{{Limit: -1}, {Limit: 101}, {Offset: -1}} {
		_, err := dispatch(t, d, principal("a"), "agents.list", in)
		if !errors.Is(err, dc.ErrBadRequest) {
			t.Fatalf("pagination: %v", err)
		}
	}
}
