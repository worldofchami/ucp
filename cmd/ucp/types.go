package ucp

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/worldofchami/ucp/pkg/models"
)

// ContentItem matches MCP "content" items returned from tools/call.
// Keep this minimal and extensible; other item types (image, resource, etc.)
// can be added later as needed.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolResponse is shaped to be directly usable as an MCP tool result.
// This makes it easy to reuse UCP interactions outside the MCP server.
type ToolResponse struct {
	Content []ContentItem `json:"content"`
}

func Text(s string) ToolResponse {
	return ToolResponse{
		Content: []ContentItem{
			{Type: "text", Text: s},
		},
	}
}

type PlatformDiscoveryProfile struct {
	Version 		string `json:"version"`
	Capabilities	any `json:"capabilities"`
	Services		any `json:"services"`
	PaymentHandlers	any `json:"payment_handlers"`
}

func buildJSONRPCPayload(method string, params any) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method": method,
		"params": params,
		"id": 1,
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

type UCPClient interface {
	DiscoverProducts(query string, context string) ([]models.PlatformProduct, error)
}

type ShopifyClient struct {
	HTTPClient  *http.Client
	AccessToken string
}

func (client *ShopifyClient) DiscoverProducts(query string, context string) ([]models.PlatformProduct, error) {
	payload, err := buildJSONRPCPayload("tools/call", map[string]any{
		"name": "search_global_products",
		"arguments": map[string]any{
			"query":   query,
			"context": context,
			"limit":   10,
		},
	})

	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", "https://discover.shopifyapps.com/global/mcp", bytes.NewBuffer(payload))
	if err != nil {
		log.Println("Error creating request:", err)
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	if client.HTTPClient == nil {
		client.HTTPClient = &http.Client{}
	}

	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		log.Println("Error executing request:", err)
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Println("Error parsing body:", err)
		return nil, err
	}

	var rpc models.JSONRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, err
	}

	// rpc.Result is interface{}; for this payload it should be a map[string]any
	resultMap, ok := rpc.Result.(map[string]any)
	if !ok {
		// Unexpected shape – nothing we can do; return empty slice
		return []models.PlatformProduct{}, nil
	}

	contentAny, ok := resultMap["content"]
	if !ok {
		return []models.PlatformProduct{}, nil
	}

	contentArr, ok := contentAny.([]any)
	if !ok || len(contentArr) == 0 {
		return []models.PlatformProduct{}, nil
	}

	firstContent, ok := contentArr[0].(map[string]any)
	if !ok {
		return []models.PlatformProduct{}, nil
	}

	textVal, ok := firstContent["text"]
	if !ok {
		return []models.PlatformProduct{}, nil
	}

	var innerJSON []byte
	switch t := textVal.(type) {
	case string:
		innerJSON = []byte(t)
	default:
		var marshalErr error
		innerJSON, marshalErr = json.Marshal(t)
		if marshalErr != nil {
			return nil, marshalErr
		}
	}

	
	trimmed := bytes.TrimSpace(innerJSON)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		log.Printf("tool content is not JSON, skipping: %q\n", string(trimmed))
		return []models.PlatformProduct{}, nil
	}

	shopify_products := models.ShopifyProductSearchResponse{}
	if err := json.Unmarshal(trimmed, &shopify_products); err != nil {
		return nil, err
	}

	var products []models.PlatformProduct
	for i := range shopify_products.Offers {
		products = append(products, &shopify_products.Offers[i])
	}

	return products, nil
}