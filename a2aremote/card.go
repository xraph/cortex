package a2aremote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/skill"
)

// WellKnownCardPath is where a 1.0 client looks for an agent card. The
// 0.x path was /.well-known/agent.json, and serving that instead is how
// an agent ends up invisible to every current client.
const WellKnownCardPath = "/.well-known/agent-card.json"

// The protocol binding names, which are an open set with three official
// members.
const (
	BindingJSONRPC = "JSONRPC"
	BindingGRPC    = "GRPC"
	BindingREST    = "HTTP+JSON"
)

// maxCardBytes caps a fetched card. A peer that answers the card path
// with something enormous should not be able to spend our memory.
const maxCardBytes = 1 << 20

// AgentProvider is who runs an agent.
type AgentProvider struct {
	URL          string `json:"url"`
	Organization string `json:"organization"`
}

// AgentInterface is one way to reach an agent.
//
// Tenant is the protocol's routing identifier for serving many agents
// behind one endpoint, and it is what makes cortex's many-agents model
// expressible without inventing anything: the tenant IS the agent name.
type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	Tenant          string `json:"tenant,omitempty"`
}

// AgentExtension declares an extension an agent understands.
type AgentExtension struct {
	URI         string         `json:"uri"`
	Description string         `json:"description,omitempty"`
	Required    bool           `json:"required,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
}

// AgentCapabilities says what an agent can do beyond the base protocol.
type AgentCapabilities struct {
	Streaming         bool             `json:"streaming,omitempty"`
	PushNotifications bool             `json:"pushNotifications,omitempty"`
	Extensions        []AgentExtension `json:"extensions,omitempty"`
	ExtendedAgentCard bool             `json:"extendedAgentCard,omitempty"`
}

// AgentSkill is one thing an agent can do.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
}

// AgentCard is what an agent publishes about itself.
//
// Everything here is public to anyone who can reach the endpoint, which
// is why exposing an agent is opt-in rather than automatic: an agent's
// description and skill list are disclosure.
type AgentCard struct {
	Name                string            `json:"name"`
	Description         string            `json:"description"`
	SupportedInterfaces []AgentInterface  `json:"supportedInterfaces"`
	Provider            *AgentProvider    `json:"provider,omitempty"`
	Version             string            `json:"version"`
	DocumentationURL    string            `json:"documentationUrl,omitempty"`
	Capabilities        AgentCapabilities `json:"capabilities"`
	DefaultInputModes   []string          `json:"defaultInputModes"`
	DefaultOutputModes  []string          `json:"defaultOutputModes"`
	Skills              []AgentSkill      `json:"skills"`
}

// CardOptions is the host-supplied half of a card: the things cortex
// cannot know about itself, like the URL it is reachable at.
type CardOptions struct {
	BaseURL          string
	Version          string
	Provider         AgentProvider
	DocumentationURL string
	// Bindings names which bindings the card advertises. Empty means
	// JSON-RPC only, which is what this module serves on its own.
	//
	// Advertise a binding only when you actually serve it. A card is a
	// promise, and a client that picks GRPC because the card offered it
	// has no way to recover when nothing answers.
	Bindings []string
	// URLs gives a binding its own address, for the ones that do not
	// share the HTTP endpoint. gRPC in particular is a host:port rather
	// than a URL path, so BaseURL cannot describe it. A binding with no
	// entry here uses BaseURL.
	URLs map[string]string
}

// BuildCard renders one cortex agent as an A2A agent card.
func BuildCard(a *agent.Config, skills []*skill.Skill, opts CardOptions) AgentCard {
	version := opts.Version
	if version == "" {
		version = "1.0.0"
	}
	bindings := opts.Bindings
	if len(bindings) == 0 {
		bindings = []string{BindingJSONRPC}
	}

	interfaces := make([]AgentInterface, 0, len(bindings))
	for _, b := range bindings {
		url := opts.BaseURL
		if explicit, ok := opts.URLs[b]; ok {
			url = explicit
		}
		interfaces = append(interfaces, AgentInterface{
			URL:             url,
			ProtocolBinding: b,
			// The tenant is the agent's name, which is how one endpoint
			// serves many agents. It is the protocol's own mechanism
			// rather than a cortex convention.
			Tenant: a.Name,
		})
	}

	card := AgentCard{
		Name:                a.Name,
		Description:         describe(a),
		SupportedInterfaces: interfaces,
		Version:             version,
		DocumentationURL:    opts.DocumentationURL,
		Capabilities: AgentCapabilities{
			// Everything cortex does not implement is declared false
			// rather than left out, so a peer reads a refusal here
			// instead of discovering it mid-conversation.
			Streaming:         false,
			PushNotifications: false,
			ExtendedAgentCard: false,
			Extensions: []AgentExtension{{
				URI: FIPAExtensionURI,
				Description: "FIPA-ACL speech acts carried in message metadata. " +
					"Optional: a client that ignores it still receives valid A2A messages.",
				Required: false,
			}},
		},
		// Only text is understood. Saying so here beats letting a peer
		// find out by having its attachment refused.
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             cardSkills(a, skills),
	}
	if opts.Provider.Organization != "" {
		p := opts.Provider
		card.Provider = &p
	}
	return card
}

func describe(a *agent.Config) string {
	if a.Description != "" {
		return a.Description
	}
	return "A cortex agent named " + a.Name + "."
}

// cardSkills maps cortex skills onto card skills, synthesising one when
// the agent has none. skills is REQUIRED in the schema, and an empty
// list reads as an agent that can do nothing.
func cardSkills(a *agent.Config, skills []*skill.Skill) []AgentSkill {
	if len(skills) == 0 {
		return []AgentSkill{{
			ID:          a.Name,
			Name:        a.Name,
			Description: describe(a),
			Tags:        []string{"cortex"},
		}}
	}
	out := make([]AgentSkill, 0, len(skills))
	for _, s := range skills {
		desc := s.Description
		if desc == "" {
			desc = s.Name
		}
		out = append(out, AgentSkill{
			ID:          s.Name,
			Name:        s.Name,
			Description: desc,
			Tags:        []string{"cortex"},
		})
	}
	return out
}

// FetchCard reads a peer's agent card from its well-known path.
func FetchCard(ctx context.Context, c *http.Client, baseURL string) (AgentCard, error) {
	if c == nil {
		c = http.DefaultClient
	}
	url := strings.TrimSuffix(baseURL, "/") + WellKnownCardPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return AgentCard{}, fmt.Errorf("build card request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return AgentCard{}, fmt.Errorf("fetch agent card: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return AgentCard{}, fmt.Errorf("fetch agent card: %s returned %s", url, resp.Status)
	}

	var card AgentCard
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCardBytes)).Decode(&card); err != nil {
		return AgentCard{}, fmt.Errorf("decode agent card: %w", err)
	}
	// A card with no name and nowhere to reach it is not a card, and
	// treating it as one means failing later with a stranger error.
	if card.Name == "" || len(card.SupportedInterfaces) == 0 {
		return AgentCard{}, fmt.Errorf("%s served a document that is not an agent card", url)
	}
	return card, nil
}
