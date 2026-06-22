package fabriqbrain

import (
	"testing"

	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/registry"
)

func TestMemorySpec_DynamicContentMetaEmbedded(t *testing.T) {
	spec := MemorySpec("agent_memory")
	if spec.Name != "agent_memory" {
		t.Fatalf("Name = %q", spec.Name)
	}
	if spec.Kind != registry.KindAggregate {
		t.Fatalf("Kind = %v, want KindAggregate", spec.Kind)
	}
	if spec.Schema == nil {
		t.Fatalf("Schema is nil; want a dynamic schema")
	}
	cols := map[string]registry.ColumnType{}
	for _, c := range spec.Schema.Columns {
		cols[c.Name] = c.Type
	}
	if cols["content"] != registry.ColText {
		t.Fatalf("content column type = %v, want ColText", cols["content"])
	}
	if cols["meta"] != registry.ColJSON {
		t.Fatalf("meta column type = %v, want ColJSON", cols["meta"])
	}
	if spec.Embed == nil || len(spec.Embed.Fields) != 1 || spec.Embed.Fields[0] != "content" {
		t.Fatalf("Embed = %+v, want Fields [content]", spec.Embed)
	}
}

func TestMemoryWritePolicy_AllowsCreateOnEntity(t *testing.T) {
	p := MemoryWritePolicy("agent_memory")
	ops := p.Allow["agent_memory"]
	if len(ops) != 1 || ops[0] != command.OpCreate {
		t.Fatalf("Allow[agent_memory] = %v, want [OpCreate]", ops)
	}
}
