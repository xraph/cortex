package lessontransport

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/registry"
)

var entityPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// ValidEntity restricts dynamic schema names to bounded SQL identifiers.
func ValidEntity(value string) bool { return entityPattern.MatchString(value) }

// Spec describes the dynamic lesson entity. Provision it through a host
// migration or EnsureDynamic before writing. Only the statement is embedded.
func Spec(name string) registry.EntitySpec {
	return registry.EntitySpec{Name: name, Kind: registry.KindAggregate, Schema: &registry.DynamicSchema{Table: name, Columns: []registry.DynamicColumn{
		{Name: "scope_key", Type: registry.ColText, NotNull: true},
		{Name: "domain", Type: registry.ColText, NotNull: true},
		{Name: "harness_schema", Type: registry.ColInt, NotNull: true},
		{Name: "evidence_class", Type: registry.ColText, NotNull: true},
		{Name: "local_revision", Type: registry.ColInt, NotNull: true},
		{Name: "payload_digest", Type: registry.ColText, NotNull: true},
		{Name: "experiment_id", Type: registry.ColText, NotNull: true},
		{Name: "content", Type: registry.ColText, NotNull: true},
		{Name: "active", Type: registry.ColBool, NotNull: true},
		{Name: "created_at", Type: registry.ColTime, NotNull: true},
		{Name: "expires_at", Type: registry.ColTime},
		{Name: "lesson", Type: registry.ColJSON, NotNull: true},
	}, Indexes: []registry.DynamicIndex{
		{Name: indexName(name, "eligible"), Columns: []string{"tenant_id", "scope_key", "domain", "harness_schema", "active", "evidence_class", "expires_at"}},
		{Name: indexName(name, "source"), Columns: []string{"tenant_id", "scope_key", "experiment_id"}},
		{Name: indexName(name, "digest"), Columns: []string{"tenant_id", "scope_key", "payload_digest"}},
	}}, Embed: &registry.EmbedSpec{Fields: []string{"content"}}}
}

// WritePolicy permits creation and guarded updates for this entity only.
func WritePolicy(name string) agent.WritePolicy {
	return agent.WritePolicy{Allow: map[string][]command.Op{name: {command.OpCreate, command.OpUpdate}}}
}

func indexName(name, suffix string) string {
	digest := sha256.Sum256([]byte(name))
	return name[:min(len(name), 32)] + "_" + hex.EncodeToString(digest[:6]) + "_" + suffix
}
