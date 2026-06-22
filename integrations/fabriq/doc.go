// Package fabriqbrain makes fabriq a plug-n-play living brain for cortex agents.
//
// It adapts fabriq's core/agent toolkit to three cortex seams:
//
//   - knowledge.Provider — multi-channel recall (vector + full-text + graph,
//     RRF-fused, distillation-aware) exposed as the engine's knowledge_search.
//   - rich tools — fabriq's graph_traverse, remember, map/digest/resolve
//     registered as cortex tools (engine.WithTool).
//   - learning loop — a plugin.Extension that writes agent run activity into
//     the fabric so fabriq's embed + distillation workers turn it into future
//     recall material.
//
// Wiring (auto-discovers the fabriq facade from the DI container):
//
//	eng, _ := engine.New(append(
//	    []engine.Option{engine.WithStore(store), engine.WithLLM(client)},
//	    fabriqbrain.EngineOptions(container,
//	        fabriqbrain.WithEmbedder(emb),
//	        fabriqbrain.WithEntities("doc", "note", "agent_memory"),
//	        fabriqbrain.WithWritePolicy(agent.WritePolicy{
//	            Allow: map[string][]command.Op{"agent_memory": {command.OpCreate}},
//	        }),
//	    )...,
//	)...)
//
// Host setup for the learning loop (turnkey):
//
//	// 1. Register the memory entity (dynamic, content vector-indexed).
//	reg.MustRegister(fabriqbrain.MemorySpec("agent_memory"))
//	// 2. Provision its table once (migration, or postgres.Adapter.EnsureDynamic
//	//    in setup) — fabriq.Open does not auto-create dynamic tables.
//	// 3. Allow writes to it. WithMemoryEntity must match the name passed to
//	//    MemorySpec above; hosts using a non-default entity name MUST pass it
//	//    explicitly (the default is "agent_memory").
//	opts := fabriqbrain.EngineOptions(container,
//	    fabriqbrain.WithEmbedder(emb),
//	    fabriqbrain.WithEntities("agent_memory"),
//	    fabriqbrain.WithMemoryEntity("agent_memory"),
//	    fabriqbrain.WithWritePolicy(fabriqbrain.MemoryWritePolicy("agent_memory")),
//	)
//
// The plugin writes {content, meta} rows; fabriq's embed worker vectorizes
// `content` and distillation rolls them up, so future recall surfaces them.
//
// The package directory is integrations/fabriq; the package name is fabriqbrain
// to avoid colliding with github.com/xraph/fabriq.
package fabriqbrain
