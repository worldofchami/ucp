package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/worldofchami/ucp/pkg/models"
)

// Client implements product discovery for Shopify's global discovery API.
type Client struct {
	HTTPClient *http.Client
	// Region is the ISO country code to pass as `ships_to` to Shopify's
	// global discovery API. If empty, it defaults to "ZA".
	Region string
}

// DiscoverProducts searches for products using Shopify's global discovery API.
func (client *Client) DiscoverProducts(query string, context string) ([]models.PlatformProduct, error) {
	region := client.Region
	if region == "" {
		region = "ZA"
	}

	payload, err := buildJSONRPCPayload("tools/call", map[string]any{
		"name": "search_global_products",
		"arguments": map[string]any{
			"query":    query,
			"context":  context,
			"limit":    10,
			"ships_to": region,
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

// StoreDomainFromVariant extracts the Shopify store domain from a standardised
// variant URL (for example, "https://storedomain.com/products/...").
func StoreDomainFromVariant(variant models.Variant) (string, error) {
	if variant.Url == "" {
		return "", fmt.Errorf("variant URL is empty")
	}

	u, err := url.Parse(variant.Url)
	if err != nil {
		return "", fmt.Errorf("invalid variant URL %q: %w", variant.Url, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("variant URL %q does not contain a host", variant.Url)
	}

	return u.Host, nil
}

// StorefrontSearchCatalog is a thin wrapper over the Shopify Storefront MCP
// `search_shop_catalog` tool. Callers are responsible for constructing the
// natural-language query and context strings.
//
// Docs: https://shopify.dev/docs/agents/catalog/storefront-mcp
func (client *Client) StorefrontSearchCatalog(
	ctx context.Context,
	storeDomain string,
	query string,
	contextStr string,
) (string, error) {
	return callStorefrontTool(ctx, storeDomain, "search_shop_catalog", map[string]any{
		"query":   query,
		"context": contextStr,
	})
}

// StorefrontSearchPoliciesAndFaqs is a thin wrapper over the Shopify
// Storefront MCP `search_shop_policies_and_faqs` tool. Callers are responsible
// for constructing the natural-language query and context strings.
//
// Docs: https://shopify.dev/docs/agents/catalog/storefront-mcp
func (client *Client) StorefrontSearchPoliciesAndFaqs(
	ctx context.Context,
	storeDomain string,
	query string,
	contextStr string,
) (string, error) {
	return callStorefrontTool(ctx, storeDomain, "search_shop_policies_and_faqs", map[string]any{
		"query":   query,
		"context": contextStr,
	})
}

// buildJSONRPCPayload constructs a JSON-RPC 2.0 request payload.
func buildJSONRPCPayload(method string, params any) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// callStorefrontTool posts a JSON‑RPC tools/call request to the given store's
// Storefront MCP endpoint and returns the first text content item, if any.
func callStorefrontTool(ctx context.Context, storeDomain, toolName string, arguments map[string]any) (string, error) {
	payload, err := buildJSONRPCPayload("tools/call", map[string]any{
		"name":      toolName,
		"arguments": arguments,
	})
	if err != nil {
		return "", err
	}

	endpoint := fmt.Sprintf("https://%s/api/mcp", storeDomain)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		log.Println("Error creating Storefront MCP request:", err)
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Println("Error executing Storefront MCP request:", err)
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Println("Error reading Storefront MCP response body:", err)
		return "", err
	}

	// Extract the first text content item from the MCP tools/call response.
	text, err := extractFirstTextFromMCPResult(body)
	if err != nil {
		return "", err
	}
	return text, nil
}

// extractFirstTextFromMCPResult parses a JSON‑RPC response from an MCP tools/call
// and returns the `text` field of the first content item, if present.
func extractFirstTextFromMCPResult(body []byte) (string, error) {
	var rpc models.JSONRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return "", err
	}

	resultMap, ok := rpc.Result.(map[string]any)
	if !ok {
		return "", nil
	}

	contentAny, ok := resultMap["content"]
	if !ok {
		return "", nil
	}

	contentArr, ok := contentAny.([]any)
	if !ok || len(contentArr) == 0 {
		return "", nil
	}

	firstContent, ok := contentArr[0].(map[string]any)
	if !ok {
		return "", nil
	}

	textVal, ok := firstContent["text"]
	if !ok {
		return "", nil
	}

	switch t := textVal.(type) {
	case string:
		return t, nil
	default:
		innerJSON, err := json.Marshal(t)
		if err != nil {
			return "", err
		}
		return string(innerJSON), nil
	}
}
