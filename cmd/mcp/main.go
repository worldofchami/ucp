package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

	// Initialize Shopify token manager (will use env vars for client_id and client_secret)
	var shopifyTokenManager *shopify.TokenManager
	if os.Getenv("SHOPIFY_CLIENT_ID") != "" && os.Getenv("SHOPIFY_CLIENT_SECRET") != "" {
		var err error
		shopifyTokenManager, err = shopify.NewTokenManager()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to initialize Shopify token manager: %v\n", err)
			fmt.Fprintf(os.Stderr, "Shopify tools will not be available\n")
		}
	} else {
		fmt.Fprintf(os.Stderr, "Warning: SHOPIFY_CLIENT_ID and SHOPIFY_CLIENT_SECRET not set\n")
		fmt.Fprintf(os.Stderr, "Shopify tools will not be available\n")
	}

	s := &server{
		tools: []tool{
			ucpDiscoverTool(),
			shopifySearchProductsTool(region, shopifyTokenManager),
			createCheckoutTool(shopifyTokenManager),
			searchShopCatalogTool(),
			getCartTool(),
			updateCartTool(),
			searchShopPoliciesAndFaqsTool(),
		},
		clients:        make(map[string]*sseClient),
		messageHistory: NewMessageHistory(10),
		mu:             &sync.RWMutex{},
	}

	// SSE endpoint for MCP streaming
	http.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")
		// Disable buffering for nginx/proxy compatibility
		w.Header().Set("X-Accel-Buffering", "no")

		// Generate client ID
		clientID := r.RemoteAddr + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
		fmt.Fprintf(os.Stderr, "[SSE] New connection from %s, clientID: %s\n", r.RemoteAddr, clientID)

		// Create SSE client with larger buffer
		client := &sseClient{
			id:     clientID,
			writer: w,
			ch:     make(chan jsonrpcResponse, 100),
		}

		s.mu.Lock()
		s.clients[clientID] = client
		clientCount := len(s.clients)
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[SSE] Client registered. Total clients: %d\n", clientCount)

		defer func() {
			s.mu.Lock()
			delete(s.clients, clientID)
			remaining := len(s.clients)
			s.mu.Unlock()
			close(client.ch)
			fmt.Fprintf(os.Stderr, "[SSE] Client disconnected: %s. Remaining clients: %d\n", clientID, remaining)
		}()

		// Stream responses
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Send endpoint event first (MCP protocol requirement)
		// This tells the client where to POST messages
		endpointURL := "/rpc"
		if r.URL.Query().Get("sessionId") != "" {
			endpointURL = "/rpc?sessionId=" + r.URL.Query().Get("sessionId")
		}
		if _, err := fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpointURL); err != nil {
			fmt.Fprintf(os.Stderr, "[SSE] Failed to send endpoint event: %v\n", err)
			return
		}
		flusher.Flush()
		fmt.Fprintf(os.Stderr, "[SSE] Sent endpoint event: %s\n", endpointURL)

		// Use shorter keepalive interval (15 seconds instead of 30)
		keepaliveTicker := time.NewTicker(15 * time.Second)
		defer keepaliveTicker.Stop()

		for {
			select {
			case resp, ok := <-client.ch:
				if !ok {
					fmt.Fprintf(os.Stderr, "[SSE] Channel closed for client: %s\n", clientID)
					return
				}
				if err := s.writeSSE(w, resp); err != nil {
					fmt.Fprintf(os.Stderr, "[SSE] Write error for client %s: %v\n", clientID, err)
					return
				}
				flusher.Flush()
			case <-r.Context().Done():
				fmt.Fprintf(os.Stderr, "[SSE] Context done for client: %s\n", clientID)
				return
			case <-keepaliveTicker.C:
				// Send keepalive
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					fmt.Fprintf(os.Stderr, "[SSE] Keepalive write error for client %s: %v\n", clientID, err)
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

	// Twilio webhook endpoint for receiving SMS messages
	http.HandleFunc("/twilio/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse Twilio webhook payload
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form data", http.StatusBadRequest)
			return
		}

		from := r.FormValue("From")
		body := r.FormValue("Body")

		if from == "" || body == "" {
			http.Error(w, "Missing From or Body", http.StatusBadRequest)
			return
		}

		fmt.Fprintf(os.Stderr, "[Twilio] Received message from %s: %s\n", from, body)

		// Store user message in history
		s.messageHistory.AddMessage(from, Message{
			Role:        "user",
			Content:     body,
			Timestamp:   time.Now(),
			PhoneNumber: from,
		})

		// Process the message with context
		response := s.processTwilioMessage(r.Context(), from, body)

		// Store assistant response in history
		s.messageHistory.AddMessage(from, Message{
			Role:        "assistant",
			Content:     response,
			Timestamp:   time.Now(),
			PhoneNumber: from,
		})

		// Send TwiML response
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Message>%s</Message>
</Response>`, escapeXML(response))
	})

	addr := ":8080"
	fmt.Fprintf(os.Stderr, "ucp-mcp SSE server listening on %s\n", addr)
	fmt.Fprintf(os.Stderr, "SSE endpoint: http://localhost%s/sse\n", addr)
	fmt.Fprintf(os.Stderr, "Twilio webhook: http://localhost%s/twilio/webhook\n", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

// Message represents a chat message
type Message struct {
	Role        string    `json:"role"` // "user" or "assistant"
	Content     string    `json:"content"`
	Timestamp   time.Time `json:"timestamp"`
	PhoneNumber string    `json:"phone_number,omitempty"` // For Twilio integration
}

// MessageHistory stores the last N messages per conversation
type MessageHistory struct {
	messages map[string][]Message // key: phone number
	mu       sync.RWMutex
	maxSize  int
}

func NewMessageHistory(maxSize int) *MessageHistory {
	return &MessageHistory{
		messages: make(map[string][]Message),
		maxSize:  maxSize,
	}
}

func (mh *MessageHistory) AddMessage(phoneNumber string, msg Message) {
	mh.mu.Lock()
	defer mh.mu.Unlock()

	mh.messages[phoneNumber] = append(mh.messages[phoneNumber], msg)

	// Keep only last maxSize messages
	if len(mh.messages[phoneNumber]) > mh.maxSize {
		mh.messages[phoneNumber] = mh.messages[phoneNumber][len(mh.messages[phoneNumber])-mh.maxSize:]
	}
}

func (mh *MessageHistory) GetHistory(phoneNumber string) []Message {
	mh.mu.RLock()
	defer mh.mu.RUnlock()

	if msgs, ok := mh.messages[phoneNumber]; ok {
		// Return a copy
		result := make([]Message, len(msgs))
		copy(result, msgs)
		return result
	}
	return []Message{}
}

func (mh *MessageHistory) GetHistoryAsContext(phoneNumber string) string {
	msgs := mh.GetHistory(phoneNumber)
	if len(msgs) == 0 {
		return ""
	}

	var context strings.Builder
	context.WriteString("\n\n=== Recent Conversation History ===\n")
	for _, msg := range msgs {
		role := "User"
		if msg.Role == "assistant" {
			role = "Assistant"
		}
		context.WriteString(fmt.Sprintf("%s: %s\n", role, msg.Content))
	}
	context.WriteString("=== End of History ===\n")
	return context.String()
}

type server struct {
	tools          []tool
	clients        map[string]*sseClient
	messageHistory *MessageHistory
	mu             *sync.RWMutex
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

// processTwilioMessage processes a message from Twilio with context
func (s *server) processTwilioMessage(ctx context.Context, phoneNumber, message string) string {
	// Get conversation history for context
	history := s.messageHistory.GetHistoryAsContext(phoneNumber)

	// For now, return a simple response
	// In production, this would call an LLM or process through available tools
	if history != "" {
		return fmt.Sprintf("I received your message: %s\n\n(Processing with %d messages of context including conversation history)", message, len(s.messageHistory.GetHistory(phoneNumber)))
	}

	return fmt.Sprintf("I received your message: %s", message)
}

// escapeXML escapes special XML characters
func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
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

	// Check if phone_number is in arguments and add context
	if phoneNumber, ok := p.Arguments["phone_number"].(string); ok && phoneNumber != "" {
		history := s.messageHistory.GetHistoryAsContext(phoneNumber)
		if history != "" {
			// Add context to arguments
			p.Arguments["conversation_context"] = history
		}
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

func shopifySearchProductsTool(region string, tokenManager *shopify.TokenManager) tool {
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

			// Get token from token manager (with auto-refresh)
			if tokenManager == nil {
				return nil, fmt.Errorf("Shopify token manager not initialized. Check SHOPIFY_CLIENT_ID and SHOPIFY_CLIENT_SECRET")
			}

			token, err := tokenManager.GetToken()
			if err != nil {
				return nil, fmt.Errorf("failed to get Shopify token: %w", err)
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

func createCheckoutTool(tokenManager *shopify.TokenManager) tool {
	return tool{
		Name:        "create_checkout",
		Description: "Create a new checkout session at a Shopify store. Initiates checkout with line items, buyer info, and context. For Shopify stores, uses the MCP endpoint directly.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"store_url": map[string]any{
					"type":        "string",
					"description": "Shopify store URL (e.g. https://my-store.myshopify.com or https://example.com).",
				},
				"checkout": map[string]any{
					"type":        "object",
					"description": "Checkout session data",
					"properties": map[string]any{
						"line_items": map[string]any{
							"type":        "array",
							"description": "List of line items to checkout. Each item.id should be a Shopify ProductVariant GID (e.g., gid://shopify/ProductVariant/12345678901).",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"item": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"id": map[string]any{
												"type":        "string",
												"description": "Shopify ProductVariant GID (e.g., gid://shopify/ProductVariant/12345678901)",
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
						"currency": map[string]any{
							"type":        "string",
							"description": "ISO 4217 currency code (required)",
							"pattern":     "^[A-Z]{3}$",
						},
						"buyer": map[string]any{
							"type":        "object",
							"description": "Buyer information",
							"properties": map[string]any{
								"email": map[string]any{
									"type":        "string",
									"description": "Buyer's email for order confirmation",
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
								"full_name": map[string]any{
									"type": "string",
								},
								"consent": map[string]any{
									"type":        "object",
									"description": "Buyer consent preferences",
									"properties": map[string]any{
										"analytics": map[string]any{
											"type": "boolean",
										},
										"marketing": map[string]any{
											"type": "boolean",
										},
										"preferences": map[string]any{
											"type": "boolean",
										},
										"sale_of_data": map[string]any{
											"type": "boolean",
										},
									},
								},
							},
						},
						"payment": map[string]any{
							"type":        "object",
							"description": "Payment information including instruments and selected instrument",
							"properties": map[string]any{
								"instruments": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"id": map[string]any{
												"type": "string",
											},
											"handler_id": map[string]any{
												"type": "string",
											},
											"type": map[string]any{
												"type": "string",
											},
											"brand": map[string]any{
												"type": "string",
											},
											"last_digits": map[string]any{
												"type": "string",
											},
											"billing_address": map[string]any{
												"type": "object",
												"properties": map[string]any{
													"full_name": map[string]any{
														"type": "string",
													},
													"address_country": map[string]any{
														"type": "string",
													},
													"street_address": map[string]any{
														"type": "string",
													},
													"address_locality": map[string]any{
														"type": "string",
													},
													"address_region": map[string]any{
														"type": "string",
													},
													"postal_code": map[string]any{
														"type": "string",
													},
													"phone_number": map[string]any{
														"type": "string",
													},
												},
											},
											"credential": map[string]any{
												"type": "object",
												"properties": map[string]any{
													"type": map[string]any{
														"type": "string",
													},
													"card_number_type": map[string]any{
														"type": "string",
													},
												},
											},
										},
									},
								},
								"selected_instrument_id": map[string]any{
									"type": "string",
								},
							},
						},
						"discounts": map[string]any{
							"type":        "object",
							"description": "Applied discount codes",
							"properties": map[string]any{
								"codes": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "string",
									},
								},
							},
						},
						"fulfillment": map[string]any{
							"type":        "object",
							"description": "Fulfillment preferences",
							"properties": map[string]any{
								"methods": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"type": map[string]any{
												"type": "string",
												"enum": []string{"shipping", "pickup"},
											},
											"destinations": map[string]any{
												"type": "array",
												"items": map[string]any{
													"type": "object",
													"properties": map[string]any{
														"first_name":       map[string]any{"type": "string"},
														"last_name":        map[string]any{"type": "string"},
														"street_address":   map[string]any{"type": "string"},
														"address_locality": map[string]any{"type": "string"},
														"address_region":   map[string]any{"type": "string"},
														"postal_code":      map[string]any{"type": "string"},
														"address_country":  map[string]any{"type": "string"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
					"required": []string{"line_items", "currency"},
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

			// Normalize store URL
			storeURL = strings.TrimSpace(storeURL)
			if !strings.HasPrefix(storeURL, "http://") && !strings.HasPrefix(storeURL, "https://") {
				storeURL = "https://" + storeURL
			}

			// Get token from token manager (with auto-refresh)
			if tokenManager == nil {
				return nil, fmt.Errorf("Shopify token manager not initialized. Check SHOPIFY_CLIENT_ID and SHOPIFY_CLIENT_SECRET")
			}

			token, err := tokenManager.GetToken()
			if err != nil {
				return nil, fmt.Errorf("failed to get Shopify token: %w", err)
			}

			// Build Shopify MCP endpoint URL
			mcpEndpoint := strings.TrimSuffix(storeURL, "/") + "/api/ucp/mcp"

			// Generate idempotency key
			idempotencyKey := fmt.Sprintf("%d-%s", time.Now().UnixNano(), storeURL)

			// Build JSON-RPC request for create_checkout per Shopify docs
			reqBody := map[string]any{
				"jsonrpc": "2.0",
				"method":  "tools/call",
				"id":      1,
				"params": map[string]any{
					"name": "create_checkout",
					"arguments": map[string]any{
						"_meta": map[string]any{
							"ucp": map[string]any{
								"profile": "https://ckmybgppjioghrtphdlr.supabase.co/storage/v1/object/public/json/agent_profile.json",
							},
							"idempotency-key": idempotencyKey,
						},
						"checkout": checkoutData,
					},
				},
			}

			reqJSON, err := json.Marshal(reqBody)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal request: %w", err)
			}

			// Make the request
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpEndpoint, strings.NewReader(string(reqJSON)))
			if err != nil {
				return nil, fmt.Errorf("failed to create request: %w", err)
			}

			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Accept", "application/json")
			httpReq.Header.Set("User-Agent", "ucp-mcp/0.1.0")
			httpReq.Header.Set("Authorization", "Bearer "+token)

			client := &http.Client{Timeout: 30 * time.Second}
			httpResp, err := client.Do(httpReq)
			if err != nil {
				return nil, fmt.Errorf("checkout request failed: %w", err)
			}
			defer func() { _ = httpResp.Body.Close() }()

			respBody, err := io.ReadAll(httpResp.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to read response body: %w", err)
			}

			if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
				return nil, fmt.Errorf("checkout request failed: %s (status %d)", string(respBody), httpResp.StatusCode)
			}

			log.Print(string(respBody))

			// Parse the MCP response format
			var mcpResponse struct {
				JSONRPC string `json:"jsonrpc"`
				ID      int    `json:"id"`
				Result  struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
					Data    any    `json:"data"`
				} `json:"error"`
			}

			if err := json.Unmarshal(respBody, &mcpResponse); err != nil {
				// If parsing fails, return raw response
				return []ucp.ContentItem{{
					Type: "text",
					Text: string(respBody),
				}}, nil
			}

			// Handle error response
			if mcpResponse.Error != nil {
				errorJSON, _ := json.MarshalIndent(mcpResponse.Error, "", "  ")
				return nil, fmt.Errorf("checkout error: %s", string(errorJSON))
			}

			// Extract checkout data from content
			if len(mcpResponse.Result.Content) > 0 && mcpResponse.Result.Content[0].Type == "text" {
				// Pretty print the checkout data
				var checkoutData map[string]any
				if err := json.Unmarshal([]byte(mcpResponse.Result.Content[0].Text), &checkoutData); err == nil {
					prettyJSON, _ := json.MarshalIndent(checkoutData, "", "  ")
					return []ucp.ContentItem{{
						Type: "text",
						Text: string(prettyJSON),
					}}, nil
				}
				return []ucp.ContentItem{{
					Type: "text",
					Text: mcpResponse.Result.Content[0].Text,
				}}, nil
			}

			return []ucp.ContentItem{{
				Type: "text",
				Text: string(respBody),
			}}, nil
		},
	}
}

// --- Storefront MCP Helper ---

// storefrontMCPProxy proxies a tool call to a Shopify Storefront MCP server
func storefrontMCPProxy(ctx context.Context, storeURL, toolName string, arguments map[string]any) ([]ucp.ContentItem, error) {
	// Normalize store URL
	storeURL = strings.TrimSpace(storeURL)
	if !strings.HasPrefix(storeURL, "http://") && !strings.HasPrefix(storeURL, "https://") {
		storeURL = "https://" + storeURL
	}

	// Build Storefront MCP endpoint
	mcpEndpoint := strings.TrimSuffix(storeURL, "/") + "/api/mcp"

	// Build JSON-RPC request
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      1,
		"params": map[string]any{
			"name":      toolName,
			"arguments": arguments,
		},
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Make the request (no auth required for Storefront MCP)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpEndpoint, strings.NewReader(string(reqJSON)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "ucp-mcp/0.1.0")

	client := &http.Client{Timeout: 30 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("storefront MCP request failed: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return nil, fmt.Errorf("storefront MCP request failed: %s (status %d)", string(respBody), httpResp.StatusCode)
	}

	// Parse the MCP response format
	var mcpResponse struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data"`
		} `json:"error"`
	}

	if err := json.Unmarshal(respBody, &mcpResponse); err != nil {
		// If parsing fails, return raw response
		return []ucp.ContentItem{{
			Type: "text",
			Text: string(respBody),
		}}, nil
	}

	// Handle error response
	if mcpResponse.Error != nil {
		errorJSON, _ := json.MarshalIndent(mcpResponse.Error, "", "  ")
		return nil, fmt.Errorf("storefront MCP error: %s", string(errorJSON))
	}

	// Return the content from the response
	if len(mcpResponse.Result.Content) > 0 {
		result := make([]ucp.ContentItem, 0, len(mcpResponse.Result.Content))
		for _, item := range mcpResponse.Result.Content {
			// Pretty print JSON content
			var jsonContent any
			if err := json.Unmarshal([]byte(item.Text), &jsonContent); err == nil {
				prettyJSON, _ := json.MarshalIndent(jsonContent, "", "  ")
				result = append(result, ucp.ContentItem{
					Type: item.Type,
					Text: string(prettyJSON),
				})
			} else {
				result = append(result, ucp.ContentItem{
					Type: item.Type,
					Text: item.Text,
				})
			}
		}
		return result, nil
	}

	return []ucp.ContentItem{{
		Type: "text",
		Text: string(respBody),
	}}, nil
}

// --- Storefront MCP Tools ---

func searchShopCatalogTool() tool {
	return tool{
		Name:        "search_shop_catalog",
		Description: "Search the store's product catalog to find items that match customer needs. Returns product details including name, price, variant ID, URL, and image.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"store_url": map[string]any{
					"type":        "string",
					"description": "Shopify store URL (e.g. https://my-store.myshopify.com or https://example.com).",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "The search query to find related products (required)",
				},
				"context": map[string]any{
					"type":        "string",
					"description": "Additional information to help tailor results (required)",
				},
			},
			"required":             []string{"store_url", "query", "context"},
			"additionalProperties": false,
		},
		Call: func(ctx context.Context, args map[string]any) ([]ucp.ContentItem, error) {
			storeURL, ok := asString(args, "store_url")
			if !ok || strings.TrimSpace(storeURL) == "" {
				return nil, fmt.Errorf("missing required argument: store_url")
			}

			query, ok := asString(args, "query")
			if !ok || strings.TrimSpace(query) == "" {
				return nil, fmt.Errorf("missing required argument: query")
			}

			context, ok := asString(args, "context")
			if !ok {
				context = ""
			}

			arguments := map[string]any{
				"query":   query,
				"context": context,
			}

			return storefrontMCPProxy(ctx, storeURL, "search_shop_catalog", arguments)
		},
	}
}

func getCartTool() tool {
	return tool{
		Name:        "get_cart",
		Description: "Retrieves the current contents of a cart, including item details and checkout URL.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"store_url": map[string]any{
					"type":        "string",
					"description": "Shopify store URL (e.g. https://my-store.myshopify.com or https://example.com).",
				},
				"cart_id": map[string]any{
					"type":        "string",
					"description": "ID of an existing cart (e.g., gid://shopify/Cart/abc123def456).",
				},
			},
			"required":             []string{"store_url", "cart_id"},
			"additionalProperties": false,
		},
		Call: func(ctx context.Context, args map[string]any) ([]ucp.ContentItem, error) {
			storeURL, ok := asString(args, "store_url")
			if !ok || strings.TrimSpace(storeURL) == "" {
				return nil, fmt.Errorf("missing required argument: store_url")
			}

			cartID, ok := asString(args, "cart_id")
			if !ok || strings.TrimSpace(cartID) == "" {
				return nil, fmt.Errorf("missing required argument: cart_id")
			}

			arguments := map[string]any{
				"cart_id": cartID,
			}

			return storefrontMCPProxy(ctx, storeURL, "get_cart", arguments)
		},
	}
}

func updateCartTool() tool {
	return tool{
		Name:        "update_cart",
		Description: "Updates quantities of items in an existing cart or adds new items. Creates a new cart if no cart ID is provided. Set quantity to 0 to remove an item.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"store_url": map[string]any{
					"type":        "string",
					"description": "Shopify store URL (e.g. https://my-store.myshopify.com or https://example.com).",
				},
				"cart_id": map[string]any{
					"type":        "string",
					"description": "ID of the cart to update. Creates a new cart if not provided.",
				},
				"add_items": map[string]any{
					"type":        "array",
					"description": "Array of items to add to the cart (required)",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"line_item_id": map[string]any{
								"type":        "string",
								"description": "Existing cart line item ID (for updating existing items)",
							},
							"product_variant_id": map[string]any{
								"type":        "string",
								"description": "Product variant ID (e.g., gid://shopify/ProductVariant/123456)",
							},
							"quantity": map[string]any{
								"type":        "integer",
								"description": "Quantity (set to 0 to remove item)",
								"minimum":     0,
							},
						},
						"required": []string{"quantity"},
					},
				},
			},
			"required":             []string{"store_url", "add_items"},
			"additionalProperties": false,
		},
		Call: func(ctx context.Context, args map[string]any) ([]ucp.ContentItem, error) {
			storeURL, ok := asString(args, "store_url")
			if !ok || strings.TrimSpace(storeURL) == "" {
				return nil, fmt.Errorf("missing required argument: store_url")
			}

			addItems, ok := args["add_items"].([]any)
			if !ok || len(addItems) == 0 {
				return nil, fmt.Errorf("missing required argument: add_items (must be a non-empty array)")
			}

			// Transform items from agent format (item.id) to Storefront MCP format (product_variant_id)
			transformedItems := make([]map[string]any, 0, len(addItems))
			for _, item := range addItems {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}

				transformedItem := make(map[string]any)

				// Extract quantity
				if qty, ok := itemMap["quantity"].(float64); ok {
					transformedItem["quantity"] = int(qty)
				}

				// Handle both formats: item.id (from agent) and product_variant_id (direct)
				if itemObj, ok := itemMap["item"].(map[string]any); ok {
					// Format from agent: {"item": {"id": "..."}, "quantity": 1}
					if id, ok := asString(itemObj, "id"); ok && id != "" {
						transformedItem["product_variant_id"] = id
					}
				} else if variantID, ok := asString(itemMap, "product_variant_id"); ok && variantID != "" {
					// Direct format: {"product_variant_id": "...", "quantity": 1}
					transformedItem["product_variant_id"] = variantID
				}

				// Add line_item_id if present
				if lineItemID, ok := asString(itemMap, "line_item_id"); ok && lineItemID != "" {
					transformedItem["line_item_id"] = lineItemID
				}

				if len(transformedItem) > 0 {
					transformedItems = append(transformedItems, transformedItem)
				}
			}

			if len(transformedItems) == 0 {
				return nil, fmt.Errorf("no valid items to add to cart")
			}

			arguments := map[string]any{
				"add_items": transformedItems,
			}

			// Add optional cart_id if provided
			if cartID, ok := asString(args, "cart_id"); ok && cartID != "" {
				arguments["cart_id"] = cartID
			}

			return storefrontMCPProxy(ctx, storeURL, "update_cart", arguments)
		},
	}
}

func searchShopPoliciesAndFaqsTool() tool {
	return tool{
		Name:        "search_shop_policies_and_faqs",
		Description: "Answers questions about the store's policies, products, and services to build customer trust.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"store_url": map[string]any{
					"type":        "string",
					"description": "Shopify store URL (e.g. https://my-store.myshopify.com or https://example.com).",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "The question about policies or FAQs (required)",
				},
				"context": map[string]any{
					"type":        "string",
					"description": "Additional context like current product (optional)",
				},
			},
			"required":             []string{"store_url", "query"},
			"additionalProperties": false,
		},
		Call: func(ctx context.Context, args map[string]any) ([]ucp.ContentItem, error) {
			storeURL, ok := asString(args, "store_url")
			if !ok || strings.TrimSpace(storeURL) == "" {
				return nil, fmt.Errorf("missing required argument: store_url")
			}

			query, ok := asString(args, "query")
			if !ok || strings.TrimSpace(query) == "" {
				return nil, fmt.Errorf("missing required argument: query")
			}

			arguments := map[string]any{
				"query": query,
			}

			// Add optional context if provided
			if context, ok := asString(args, "context"); ok && context != "" {
				arguments["context"] = context
			}

			return storefrontMCPProxy(ctx, storeURL, "search_shop_policies_and_faqs", arguments)
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
