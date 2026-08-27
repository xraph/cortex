package a2aremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/skill"
)

func testCardOptions() CardOptions {
	return CardOptions{
		BaseURL:  "https://cortex.example/a2a",
		Version:  "1.0.0",
		Provider: AgentProvider{Organization: "acme", URL: "https://acme.example"},
	}
}

func TestBuildCardDeclaresTheAgentAsATenant(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "db-expert", Description: "knows the database"}, nil, testCardOptions())

	if card.Name != "db-expert" {
		t.Errorf("name = %q", card.Name)
	}
	if card.Description == "" {
		t.Error("description is required by the schema and is what a human reads first")
	}
	if len(card.SupportedInterfaces) == 0 {
		t.Fatal("a card with no interface tells a client nothing about how to reach it")
	}
	iface := card.SupportedInterfaces[0]
	if iface.ProtocolBinding != BindingJSONRPC {
		t.Errorf("protocolBinding = %q, want JSONRPC", iface.ProtocolBinding)
	}
	// tenant is how one endpoint serves many agents, and it is the
	// protocol's own mechanism rather than a cortex convention.
	if iface.Tenant != "db-expert" {
		t.Errorf("tenant = %q, want the agent name", iface.Tenant)
	}
	if iface.URL != "https://cortex.example/a2a" {
		t.Errorf("url = %q", iface.URL)
	}
}

func TestCardDeclaresTheFIPAExtensionAsOptional(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "a"}, nil, testCardOptions())

	var found *AgentExtension
	for i := range card.Capabilities.Extensions {
		if card.Capabilities.Extensions[i].URI == FIPAExtensionURI {
			found = &card.Capabilities.Extensions[i]
		}
	}
	if found == nil {
		t.Fatal("the card must declare the extension its messages use")
	}
	// Required would refuse conversations we can hold perfectly well: a
	// peer that ignores the extension still gets valid A2A and reads the
	// text.
	if found.Required {
		t.Fatal("the FIPA extension must be optional")
	}
	if found.Description == "" {
		t.Error("an extension nobody can look up needs a description in the card")
	}
}

func TestCardDeclaresWhatIsNotSupported(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "a"}, nil, testCardOptions())

	if card.Capabilities.PushNotifications {
		t.Error("push notifications are not implemented and must not be advertised")
	}
	if card.Capabilities.ExtendedAgentCard {
		t.Error("the extended card is not implemented and must not be advertised")
	}
	if card.Capabilities.Streaming {
		t.Error("streaming is not implemented yet and must not be advertised")
	}
}

func TestCardSkillsComeFromTheAgentsSkills(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "a"}, []*skill.Skill{
		{Name: "sql-review", Description: "reviews SQL migrations"},
	}, testCardOptions())
	if len(card.Skills) != 1 || card.Skills[0].Name != "sql-review" {
		t.Fatalf("skills = %+v", card.Skills)
	}
	if card.Skills[0].ID == "" || card.Skills[0].Description == "" {
		t.Errorf("id and description are required by the schema: %+v", card.Skills[0])
	}

	// An agent with no skills still needs one entry: skills is REQUIRED
	// in the schema, and an empty list makes the agent look useless.
	bare := BuildCard(&agent.Config{Name: "a", Description: "does things"}, nil, testCardOptions())
	if len(bare.Skills) != 1 {
		t.Fatalf("a skill-less agent needs one synthesised skill, got %+v", bare.Skills)
	}
}

func TestCardModesAreText(t *testing.T) {
	card := BuildCard(&agent.Config{Name: "a"}, nil, testCardOptions())
	// Only text is understood, and the card says so rather than letting a
	// peer discover it by having its attachment refused.
	if len(card.DefaultInputModes) != 1 || card.DefaultInputModes[0] != "text/plain" {
		t.Errorf("defaultInputModes = %+v", card.DefaultInputModes)
	}
	if len(card.DefaultOutputModes) != 1 || card.DefaultOutputModes[0] != "text/plain" {
		t.Errorf("defaultOutputModes = %+v", card.DefaultOutputModes)
	}
}

func TestCardWireNames(t *testing.T) {
	b, err := json.Marshal(BuildCard(&agent.Config{Name: "a"}, nil, testCardOptions()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"name", "description", "supportedInterfaces", "version", "capabilities", "defaultInputModes", "defaultOutputModes", "skills"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing wire field %q in %s", key, b)
		}
	}
}

func TestFetchCardRoundTrip(t *testing.T) {
	want := BuildCard(&agent.Config{Name: "db-expert", Description: "knows the database"}, nil, testCardOptions())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The path a 1.0 client asks for. 0.x used /.well-known/agent.json
		// and nothing looks there now.
		if r.URL.Path != WellKnownCardPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	got, err := FetchCard(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchCard: %v", err)
	}
	if got.Name != want.Name || len(got.SupportedInterfaces) != len(want.SupportedInterfaces) {
		t.Fatalf("fetched card does not match: %+v", got)
	}
}

func TestFetchCardRejectsANonCard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nothing":"useful"}`))
	}))
	defer srv.Close()

	if _, err := FetchCard(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("a document with no name and no interfaces is not a card")
	}
}
