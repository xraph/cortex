package fabriqbrain

import (
	"context"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	c := defaultConfig()
	if c.budget != 4096 {
		t.Fatalf("default budget = %d, want 4096", c.budget)
	}
	if c.memoryEntity != "agent_memory" {
		t.Fatalf("default memoryEntity = %q, want %q", c.memoryEntity, "agent_memory")
	}
	if c.tenant == nil {
		t.Fatalf("default tenant mapper is nil; want identity")
	}
	ctx := context.Background()
	if c.tenant(ctx) != ctx {
		t.Fatalf("default tenant mapper is not identity")
	}
}

func TestOptionsApply(t *testing.T) {
	c := defaultConfig()
	for _, o := range []Option{
		WithEntities("doc", "note"),
		WithBudget(1000),
		WithMemoryEntity("mem"),
	} {
		o(&c)
	}
	if len(c.entities) != 2 || c.entities[0] != "doc" {
		t.Fatalf("entities = %v, want [doc note]", c.entities)
	}
	if c.budget != 1000 {
		t.Fatalf("budget = %d, want 1000", c.budget)
	}
	if c.memoryEntity != "mem" {
		t.Fatalf("memoryEntity = %q, want mem", c.memoryEntity)
	}
}
