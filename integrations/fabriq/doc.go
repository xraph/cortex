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
// Host requirements for the learning loop: register the memory entity (default
// "agent_memory") in fabriq's registry WITH a vector index so the proj:embed
// worker vectorizes new rows, and allow {memoryEntity: {create}} in the write
// policy.
//
// The package directory is integrations/fabriq; the package name is fabriqbrain
// to avoid colliding with github.com/xraph/fabriq.
package fabriqbrain
