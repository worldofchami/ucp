package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/worldofchami/ucp/cmd/ucp"
	"github.com/worldofchami/ucp/pkg/models"
	"github.com/worldofchami/ucp/pkg/platforms/shopify"
	"github.com/worldofchami/ucp/pkg/utils"
)

func main() {
	// Load environment variables from .env file
	_ = godotenv.Load()

	region := os.Getenv("UCP_REGION")
	if strings.TrimSpace(region) == "" {
		region = "ZA"
	}

	s := &server{
		tools: []tool{
			ucpDiscoverTool(),
			shopifySearchProductsTool(region),
			createCheckoutTool(),
		},
		clients: make(map[string]*sseClient),
		mu:      &sync.RWMutex{},
	}

	// SSE endpoint for MCP streaming
	http.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")

		// Generate client ID
		clientID := r.RemoteAddr + "-" + fmt.Sprintf("%d", time.Now().UnixNano())

		// Create SSE client
		client := &sseClient{
			id:     clientID,
			writer: w,
			ch:     make(chan jsonrpcResponse, 10),
		}

		s.mu.Lock()
		s.clients[clientID] = client
		s.mu.Unlock()

		defer func() {
			s.mu.Lock()
			delete(s.clients, clientID)
			s.mu.Unlock()
			close(client.ch)
		}()

		// Handle initial request from query params or body
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err == nil && len(body) > 0 {
				var msg jsonrpcRequest
				if err := json.Unmarshal(body, &msg); err == nil {
					go s.handleRequest(clientID, r.Context(), msg)
				}
			}
		}

		// Stream responses
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		for {
			select {
			case resp, ok := <-client.ch:
				if !ok {
					return
				}
				if err := s.writeSSE(w, resp); err != nil {
					return
				}
				flusher.Flush()
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Second):
				// Send keepalive
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})

	// POST endpoint for sending requests (alternative to query params)
	http.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		dec := json.NewDecoder(r.Body)
		var msg jsonrpcRequest
		if err := dec.Decode(&msg); err != nil {
			http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
			return
		}

		// Get client ID from header or generate one
		clientID := r.Header.Get("X-Client-ID")
		if clientID == "" {
			clientID = r.RemoteAddr + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
		}

		s.mu.RLock()
		_, exists := s.clients[clientID]
		s.mu.RUnlock()

		if exists {
			go s.handleRequest(clientID, r.Context(), msg)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"accepted"}`))
		} else {
			// No SSE client, respond directly
			resp := s.handleDirect(r.Context(), msg)
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(resp)
		}
	})

	// Simple health endpoint
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":8080"
	fmt.Fprintf(os.Stderr, "ucp-mcp SSE server listening on %s\n", addr)
	fmt.Fprintf(os.Stderr, "SSE endpoint: http://localhost%s/sse\n", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

type server struct {
	tools   []tool
	clients map[string]*sseClient
	mu      *sync.RWMutex
}

type sseClient struct {
	id     string
	writer http.ResponseWriter
	ch     chan jsonrpcResponse
}

func (s *server) handleRequest(clientID string, ctx context.Context, req jsonrpcRequest) {
	var resp jsonrpcResponse
	switch req.Method {
	case "initialize":
		resp = s.handleInitialize(req)
	case "tools/list":
		resp = s.handleToolsList(req)
	case "tools/call":
		resp = s.handleToolsCall(ctx, req)
	default:
		resp = s.replyError(req.ID, -32601, "Method not found", map[string]any{
			"method": req.Method,
		})
	}

	s.mu.RLock()
	client, exists := s.clients[clientID]
	s.mu.RUnlock()

	if exists {
		select {
		case client.ch <- resp:
		default:
			// Channel full, drop message
		}
	}
}

func (s *server) handleDirect(ctx context.Context, req jsonrpcRequest) jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		return s.replyError(req.ID, -32601, "Method not found", map[string]any{
			"method": req.Method,
		})
	}
}

func (s *server) writeSSE(w http.ResponseWriter, resp jsonrpcResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(b))
	return err
}

func (s *server) handleInitialize(req jsonrpcRequest) jsonrpcResponse {
	// MCP initialize response follows "capabilities" format used by clients.
	// Keep conservative: just tools.

	result := map[string]any{
		"protocolVersion": "2024-11-05",
		"serverInfo": map[string]any{
			"name":    "ucp-mcp",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
	}
	return s.replyResult(req.ID, result)
}

func (s *server) handleToolsList(req jsonrpcRequest) jsonrpcResponse {
	list := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		list = append(list, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return s.replyResult(req.ID, map[string]any{
		"tools": list,
	})
}

func (s *server) handleToolsCall(ctx context.Context, req jsonrpcRequest) jsonrpcResponse {
	var p toolsCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return s.replyError(req.ID, -32602, "Invalid params", err.Error())
	}

	// Log tool call with parameters
	logToolCall(toolCallLogFile, toolCallLogEntry{
		Time:      time.Now().UTC(),
		RequestID: req.ID,
		Tool:      p.Name,
		Arguments: p.Arguments,
	})

	var t *tool
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			t = &s.tools[i]
			break
		}
	}
	if t == nil {
		return s.replyError(req.ID, -32602, "Invalid params", map[string]any{
			"reason": "unknown tool",
			"name":   p.Name,
		})
	}

	content, err := t.Call(ctx, p.Arguments)
	if err != nil {
		// Log tool error output
		logToolOutput(toolOutputLogFile, toolOutputLogEntry{
			Time:      time.Now().UTC(),
			RequestID: req.ID,
			Tool:      p.Name,
			Error:     err.Error(),
		})
		return s.replyError(req.ID, 1, "Tool execution error", err.Error())
	}

	// Log successful tool output
	logToolOutput(toolOutputLogFile, toolOutputLogEntry{
		Time:      time.Now().UTC(),
		RequestID: req.ID,
		Tool:      p.Name,
		Content:   content,
	})

	// MCP tool result uses `content` array with typed items.
	return s.replyResult(req.ID, map[string]any{
		"content": content,
	})
}

func (s *server) replyResult(id json.RawMessage, result any) jsonrpcResponse {
	return jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  mustMarshalRaw(result),
	}
}

func (s *server) replyError(id json.RawMessage, code int, message string, data any) jsonrpcResponse {
	return jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonrpcError{
			Code:    code,
			Message: message,
			Data:    mustMarshalRaw(data),
		},
	}
}

// --- JSON-RPC types ---

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func mustMarshalRaw(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	b, err := json.Marshal(v)
	if err != nil {
		// In a server context, panic is acceptable here because it indicates a programmer error.
		panic(err)
	}
	return json.RawMessage(b)
}

// --- Tools ---

type tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(ctx context.Context, args map[string]any) ([]ucp.ContentItem, error)
}

type toolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func ucpDiscoverTool() tool {
	return tool{
		Name:        "ucp_discover",
		Description: "Fetch UCP discovery from a store: GET /.well-known/ucp",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"store_url": map[string]any{
					"type":        "string",
					"description": "Store base URL (e.g. https://example.com). If scheme is omitted, https:// is assumed.",
				},
			},
			"required":             []string{"store_url"},
			"additionalProperties": false,
		},
		Call: func(ctx context.Context, args map[string]any) ([]ucp.ContentItem, error) {
			storeURL, ok := asString(args, "store_url")
			if !ok || strings.TrimSpace(storeURL) == "" {
				return nil, fmt.Errorf("missing required argument: store_url")
			}

			client := ucp.NewClient(ucp.WithUserAgent("ucp-mcp/0.1.0"))
			resp, err := client.Discover(ctx, storeURL)
			if err != nil {
				return nil, err
			}
			return resp.Content, nil
		},
	}
}

func shopifySearchProductsTool(region string) tool {
	return tool{
		Name:        "search_products",
		Description: "Search for products via Shopify global discovery using a natural-language query.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "User's natural-language product request (e.g. \"I need a warm winter jacket\").",
				},
				"context": map[string]any{
					"type":        "string",
					"description": "Summary of the request context (e.g. \"buyer looking for a winter jacket\").",
				},
			},
			"required":             []string{"query", "context"},
			"additionalProperties": false,
		},
		Call: func(ctx context.Context, args map[string]any) ([]ucp.ContentItem, error) {
			query, ok := asString(args, "query")
			if !ok || strings.TrimSpace(query) == "" {
				return nil, fmt.Errorf("missing required argument: query")
			}
			contextSummary, ok := asString(args, "context")
			if !ok {
				contextSummary = ""
			}

			// Ensure environment is loaded (in case .env was updated)
			_ = godotenv.Load()

			token := os.Getenv("SHOPIFY_ACCESS_TOKEN")
			if token == "" {
				return nil, fmt.Errorf("SHOPIFY_ACCESS_TOKEN environment variable is not set")
			}

			http_client := utils.NewHTTPClientWithBearerToken(token)

			shopify_client := &shopify.Client{
				HTTPClient: http_client,
				Region:     region,
			}

			platformProducts, err := shopify_client.DiscoverProducts(query, contextSummary)
			if err != nil {
				return nil, err
			}

			standardised := make([]models.Product, 0, len(platformProducts))
			for _, p := range platformProducts {
				standardised = append(standardised, p.Standardise())
			}

			b, err := json.MarshalIndent(standardised, "", "  ")
			if err != nil {
				return nil, err
			}

			return []ucp.ContentItem{
				{
					Type: "text",
					Text: string(b),
				},
			}, nil
		},
	}
}

func asString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func createCheckoutTool() tool {
	return tool{
		Name:        "create_checkout",
		Description: "Create a new checkout session at a UCP-compliant store. Initiates checkout with line items, buyer info, and context.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"store_url": map[string]any{
					"type":        "string",
					"description": "Store base URL (e.g. https://example.com). Used for UCP discovery to find checkout endpoint.",
				},
				"checkout": map[string]any{
					"type":        "object",
					"description": "Checkout session data",
					"properties": map[string]any{
						"line_items": map[string]any{
							"type":        "array",
							"description": "List of line items to checkout. Each item.id should correspond to the product variant id.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"item": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"id": map[string]any{
												"type":        "string",
												"description": "Product variant id",
											},
										},
										"required": []string{"id"},
									},
									"quantity": map[string]any{
										"type":        "integer",
										"description": "Quantity to purchase",
										"minimum":     1,
									},
								},
								"required": []string{"item", "quantity"},
							},
						},
						"buyer": map[string]any{
							"type":        "object",
							"description": "Buyer information (optional)",
							"properties": map[string]any{
								"email": map[string]any{
									"type": "string",
								},
								"first_name": map[string]any{
									"type": "string",
								},
								"last_name": map[string]any{
									"type": "string",
								},
								"phone_number": map[string]any{
									"type": "string",
								},
							},
						},
						"context": map[string]any{
							"type":        "object",
							"description": "Provisional buyer signals for relevance and localization",
							"properties": map[string]any{
								"address_country": map[string]any{
									"type":        "string",
									"description": "ISO 3166-1 alpha-2 country code",
								},
								"address_region": map[string]any{
									"type": "string",
								},
								"postal_code": map[string]any{
									"type": "string",
								},
							},
						},
						"currency": map[string]any{
							"type":        "string",
							"description": "ISO 4217 currency code (optional)",
							"pattern":     "^[A-Z]{3}$",
						},
					},
					"required": []string{"line_items"},
				},
			},
			"required":             []string{"store_url", "checkout"},
			"additionalProperties": false,
		},
		Call: func(ctx context.Context, args map[string]any) ([]ucp.ContentItem, error) {
			storeURL, ok := asString(args, "store_url")
			if !ok || strings.TrimSpace(storeURL) == "" {
				return nil, fmt.Errorf("missing required argument: store_url")
			}

			checkoutData, ok := args["checkout"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("missing required argument: checkout")
			}

			// Step 1: Discover UCP endpoint
			client := ucp.NewClient(ucp.WithUserAgent("ucp-mcp/0.1.0"))
			discoveryResp, err := client.Discover(ctx, storeURL)
			if err != nil {
				return nil, fmt.Errorf("failed to discover UCP endpoint: %w", err)
			}

			// Parse discovery response to find MCP endpoint
			discoveryText := ""
			if len(discoveryResp.Content) > 0 {
				discoveryText = discoveryResp.Content[0].Text
			}

			var discovery struct {
				UCP struct {
					Services map[string][]struct {
						Transport string `json:"transport"`
						Endpoint  string `json:"endpoint"`
					} `json:"services"`
				} `json:"ucp"`
			}

			if err := json.Unmarshal([]byte(discoveryText), &discovery); err != nil {
				return nil, fmt.Errorf("failed to parse discovery response: %w", err)
			}

			// Find MCP endpoint
			var mcpEndpoint string
			for _, service := range discovery.UCP.Services["dev.ucp.shopping"] {
				if service.Transport == "mcp" && service.Endpoint != "" {
					mcpEndpoint = service.Endpoint
					break
				}
			}

			if mcpEndpoint == "" {
				return nil, fmt.Errorf("no MCP endpoint found in UCP discovery")
			}

			// Step 2: Build JSON-RPC request for create_checkout
			reqBody := map[string]any{
				"jsonrpc": "2.0",
				"method":  "tools/call",
				"params": map[string]any{
					"name": "create_checkout",
					"arguments": map[string]any{
						"meta": map[string]any{
							"ucp-agent": map[string]any{
								"profile": "https://platform.example/profiles/shopping-agent.json",
							},
						},
						"checkout": checkoutData,
					},
				},
				"id": 1,
			}

			reqJSON, err := json.Marshal(reqBody)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal request: %w", err)
			}

			// Step 3: Make the request
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpEndpoint, strings.NewReader(string(reqJSON)))
			if err != nil {
				return nil, fmt.Errorf("failed to create request: %w", err)
			}

			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Accept", "application/json")
			httpReq.Header.Set("User-Agent", "ucp-mcp/0.1.0")

			httpResp, err := client.HTTP.Do(httpReq)
			if err != nil {
				return nil, fmt.Errorf("checkout request [1] failed: %w", err)
			}
			defer func() { _ = httpResp.Body.Close() }()

			respBody, err := io.ReadAll(httpResp.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to read response body: %w", err)
			}

			if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
				return nil, fmt.Errorf("checkout request [2] failed: %s (status %d)", string(respBody), httpResp.StatusCode)
			}

			// Pretty print the response
			var respObj any
			if err := json.Unmarshal(respBody, &respObj); err == nil {
				prettyJSON, err := json.MarshalIndent(respObj, "", "  ")
				if err == nil {
					respBody = prettyJSON
				}
			}

			return []ucp.ContentItem{
				{
					Type: "text",
					Text: string(respBody),
				},
			}, nil
		},
	}
}

// --- Logging helpers ---

const (
	toolCallLogFile   = "logs/tool_calls.log"
	toolOutputLogFile = "logs/tool_outputs.log"
)

var logMu sync.Mutex

type toolCallLogEntry struct {
	Time      time.Time       `json:"time"`
	RequestID json.RawMessage `json:"request_id,omitempty"`
	Tool      string          `json:"tool"`
	Arguments map[string]any  `json:"arguments"`
}

type toolOutputLogEntry struct {
	Time      time.Time         `json:"time"`
	RequestID json.RawMessage   `json:"request_id,omitempty"`
	Tool      string            `json:"tool"`
	Content   []ucp.ContentItem `json:"content,omitempty"`
	Error     string            `json:"error,omitempty"`
}

func logToolCall(filename string, entry toolCallLogEntry) {
	logJSONLine(filename, entry)
}

func logToolOutput(filename string, entry toolOutputLogEntry) {
	logJSONLine(filename, entry)
}

func logJSONLine(filename string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log marshal error: %v\n", err)
		return
	}

	logMu.Lock()
	defer logMu.Unlock()

	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log open error: %v\n", err)
		return
	}
	defer f.Close()

	if _, err := f.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "log write error: %v\n", err)
	}
}
