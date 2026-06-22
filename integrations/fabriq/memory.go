package fabriqbrain

import (
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/registry"
)

// MemorySpec returns the fabriq registry spec for the learning-loop memory
// entity: a dynamic entity with a `content` text column (the embedding target)
// and a `meta` JSON column (structured run fields), with `content` vector-indexed.
//
// Register it before opening fabriq, e.g.:
//
//	reg.MustRegister(fabriqbrain.MemorySpec("agent_memory"))
//
// The physical table is NOT auto-created by fabriq.Open — provision it via a
// migration or postgres.Adapter.EnsureDynamic before serving writes.
func MemorySpec(entity string) registry.EntitySpec {
	return registry.EntitySpec{
		Name: entity,
		Kind: registry.KindAggregate,
		Schema: &registry.DynamicSchema{
			Table: entity, // table name == entity name (ddl-validated by fabriq)
			Columns: []registry.DynamicColumn{
				{Name: "content", Type: registry.ColText, NotNull: true},
				{Name: "meta", Type: registry.ColJSON},
			},
		},
		Embed: &registry.EmbedSpec{Fields: []string{"content"}},
	}
}

// MemoryWritePolicy returns the deny-by-default write allowlist the learning-loop
// plugin needs: create on the memory entity. Pass it via WithWritePolicy.
func MemoryWritePolicy(entity string) agent.WritePolicy {
	return agent.WritePolicy{
		Allow: map[string][]command.Op{entity: {command.OpCreate}},
	}
}
