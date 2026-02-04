package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nlpodyssey/openai-agents-go/agents"
)

// chatRequest is the payload accepted by the /chat endpoint.
type chatRequest struct {
	Prompt string `json:"prompt"`
}

// chatResponse is the JSON shape returned by the /chat endpoint.
type chatResponse struct {
	Output string `json:"output"`
}

func main() {
	addr := getEnv("CHAT_SERVER_ADDR", ":8090")

	r := chi.NewRouter()

	// Simple health check.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Main chat endpoint.
	r.Post("/chat", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		req.Prompt = strings.TrimSpace(req.Prompt)
		if req.Prompt == "" {
			http.Error(w, "prompt is required", http.StatusBadRequest)
			return
		}

		// Build a shopping-assistant agent on each request.
		agent := agents.New("ShoppingAssistant").
			WithInstructions(baseInstructions()).
			WithModel("gpt-4o").
			WithTools(
				mcpDiscoverStoreTool(),
				mcpSearchProductsTool(),
			)

		// Run the agent against the user's prompt.
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		result, err := agents.Run(ctx, agent, req.Prompt)
		if err != nil {
			http.Error(w, fmt.Sprintf("agent error: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(chatResponse{Output: result.FinalOutput}); err != nil {
			http.Error(w, "failed to encode response", http.StatusInternalServerError)
			return
		}
	})

	fmt.Fprintf(os.Stderr, "chat server listening on %s\n", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		fmt.Fprintf(os.Stderr, "chat server error: %v\n", err)
		os.Exit(1)
	}
}

// baseInstructions provides the system prompt for the shopping assistant.
func baseInstructions() string {
	return strings.TrimSpace(`
You are a **personal shopping assistant**.

Your goal is to help customers discover and compare products that best match what they are looking for.

Guidelines:
- Ask brief clarification questions when the request is ambiguous.
- Use the available tools to discover stores and search for products rather than guessing.
- When showing products, summarize key attributes (price range, style, color, size options, shipping region).
- When appropriate, suggest a small, curated set of options instead of an exhaustive list.
- Always explain *why* a product is a good fit for the customer's request.
`)
}

// getEnv returns the value of the environment variable or a default.
func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// --- MCP integration helpers ------------------------------------------------

const mcpServerURL = "http://localhost:8080/rpc"

// mcpDiscoverStoreParams is the parameter shape for the discover-store tool.
type mcpDiscoverStoreParams struct {
	StoreURL string `json:"store_url"`
}

// mcpSearchProductsParams is the parameter shape for the search-products tool.
type mcpSearchProductsParams struct {
	Query   string `json:"query"`
	Context string `json:"context"`
}

// mcpToolsCallParams mirrors the JSON-RPC "tools/call" params expected by the
// MCP server implemented in cmd/mcp/main.go.
type mcpToolsCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// mcpRPCRequest is a minimal JSON-RPC 2.0 request payload.
type mcpRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// mcpContentItem mirrors the ucp.ContentItem shape well enough for our usage.
type mcpContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// mcpRPCResult is the expected result field for tools/call as implemented in
// cmd/mcp/main.go (a map with a "content" array).
type mcpRPCResult struct {
	Content []mcpContentItem `json:"content"`
}

// mcpRPCError is a minimal JSON-RPC error shape.
type mcpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// mcpRPCResponse captures just enough of the MCP JSON-RPC response.
type mcpRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  *mcpRPCResult `json:"result,omitempty"`
	Error   *mcpRPCError  `json:"error,omitempty"`
}

// callMCP abstracts a single tools/call request to the MCP server and returns
// a concatenated text output built from all content items.
func callMCP(ctx context.Context, toolName string, args map[string]interface{}) (string, error) {
	reqBody := mcpRPCRequest{
		JSONRPC: "2.0",
		ID:      int(time.Now().UnixNano() / int64(time.Millisecond)),
		Method:  "tools/call",
		Params: mcpToolsCallParams{
			Name:      toolName,
			Arguments: args,
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal MCP request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpServerURL, bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("build MCP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("call MCP server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("MCP server returned status %s", resp.Status)
	}

	var rpcResp mcpRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return "", fmt.Errorf("decode MCP response: %w", err)
	}

	if rpcResp.Error != nil {
		return "", fmt.Errorf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil {
		return "", fmt.Errorf("MCP response missing result")
	}

	var builder strings.Builder
	for i, c := range rpcResp.Result.Content {
		if c.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		// Prefix each block slightly so the model can understand where it came from.
		fmt.Fprintf(&builder, "Result %d (%s):\n%s", i+1, c.Type, c.Text)
	}

	return builder.String(), nil
}

// mcpDiscoverStoreTool exposes the MCP ucp_discover tool to the agent.
func mcpDiscoverStoreTool() *agents.FunctionTool[mcpDiscoverStoreParams, string] {
	return agents.NewFunctionTool(
		"discover_store",
		"Discover a store's UCP configuration using the MCP server. Use this first when you know the store URL.",
		func(ctx context.Context, params mcpDiscoverStoreParams) (string, error) {
			params.StoreURL = strings.TrimSpace(params.StoreURL)
			if params.StoreURL == "" {
				return "", fmt.Errorf("store_url is required")
			}
			out, err := callMCP(ctx, "ucp_discover", map[string]interface{}{
				"store_url": params.StoreURL,
			})
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}

// mcpSearchProductsTool exposes the MCP search_products tool to the agent.
func mcpSearchProductsTool() *agents.FunctionTool[mcpSearchProductsParams, string] {
	return agents.NewFunctionTool(
		"search_products",
		"Search for products using the MCP-backed Shopify discovery. Use natural language queries and provide helpful context.",
		func(ctx context.Context, params mcpSearchProductsParams) (string, error) {
			params.Query = strings.TrimSpace(params.Query)
			if params.Query == "" {
				return "", fmt.Errorf("query is required")
			}
			out, err := callMCP(ctx, "search_products", map[string]interface{}{
				"query":   params.Query,
				"context": strings.TrimSpace(params.Context),
			})
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}
