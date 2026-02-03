package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/worldofchami/ucp/cmd/ucp"
)

func main() {
	ctx := context.Background()

	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()

	s := &server{
		out: out,
		tools: []tool{
			ucpDiscoverTool(),
		},
	}

	dec := json.NewDecoder(in)
	for {
		var msg jsonrpcRequest
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			// Best-effort: can't respond without a valid request id.
			return
		}
		_ = s.handle(ctx, msg)
	}
}

type server struct {
	out   *bufio.Writer
	tools []tool
}

func (s *server) handle(ctx context.Context, req jsonrpcRequest) error {
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

func (s *server) handleInitialize(req jsonrpcRequest) error {
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

func (s *server) handleToolsList(req jsonrpcRequest) error {
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

func (s *server) handleToolsCall(ctx context.Context, req jsonrpcRequest) error {
	var p toolsCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return s.replyError(req.ID, -32602, "Invalid params", err.Error())
	}

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
		return s.replyError(req.ID, 1, "Tool execution error", err.Error())
	}

	// MCP tool result uses `content` array with typed items.
	return s.replyResult(req.ID, map[string]any{
		"content": content,
	})
}

func (s *server) replyResult(id json.RawMessage, result any) error {
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  mustMarshalRaw(result),
	}
	return s.write(resp)
}

func (s *server) replyError(id json.RawMessage, code int, message string, data any) error {
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonrpcError{
			Code:    code,
			Message: message,
			Data:    mustMarshalRaw(data),
		},
	}
	return s.write(resp)
}

func (s *server) write(v any) error {
	enc := json.NewEncoder(s.out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return s.out.Flush()
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

func asString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
