package engine

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
)

// builtinTools returns tool definitions for engine-provided tools.
func (e *Engine) builtinTools() []llm.Tool {
	var tools []llm.Tool

	if e.knowledge != nil {
		tools = append(tools, llm.Tool{
			Name:        "knowledge_search",
			Description: "Search the knowledge base for relevant information. Use this tool when you need to look up facts, documentation, or context to answer a question accurately.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query to find relevant knowledge",
					},
					"collection": map[string]any{
						"type":        "string",
						"description": "Optional: name of a specific knowledge collection to search in",
					},
				},
				"required": []string{"query"},
			},
		})
	}

	return tools
}

// executeBuiltinTool attempts to execute a built-in tool. Returns (result, true) if handled.
func (e *Engine) executeBuiltinTool(ctx context.Context, name, arguments string) (string, bool) {
	switch name {
	case "knowledge_search":
		return e.executeKnowledgeSearch(ctx, arguments), true
	default:
		return "", false
	}
}

// executeKnowledgeSearch handles the knowledge_search tool call.
func (e *Engine) executeKnowledgeSearch(ctx context.Context, arguments string) string {
	if e.knowledge == nil {
		return jsonResult("error", "knowledge provider not configured")
	}

	var args struct {
		Query      string `json:"query"`
		Collection string `json:"collection"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return jsonResult("error", "invalid arguments: "+err.Error())
	}
	if args.Query == "" {
		return jsonResult("error", "query is required")
	}

	params := &knowledge.RetrieveParams{
		TopK:       5,
		Collection: args.Collection,
	}

	chunks, err := e.knowledge.Retrieve(ctx, args.Query, params)
	if err != nil {
		return jsonResult("error", "retrieval failed: "+err.Error())
	}

	if len(chunks) == 0 {
		return jsonResult("status", "no relevant results found")
	}

	results := make([]map[string]any, len(chunks))
	for i, c := range chunks {
		results[i] = map[string]any{
			"content":  c.Content,
			"score":    c.Score,
			"source":   c.Source,
			"metadata": c.Metadata,
		}
	}

	b, _ := json.Marshal(map[string]any{ //nolint:errcheck // best-effort JSON encoding
		"status":  "success",
		"results": results,
		"count":   len(results),
	})
	return string(b)
}

// jsonResult is a helper for simple JSON tool results.
func jsonResult(key, value string) string {
	b, _ := json.Marshal(map[string]string{key: value}) //nolint:errcheck // best-effort JSON encoding
	return string(b)
}
