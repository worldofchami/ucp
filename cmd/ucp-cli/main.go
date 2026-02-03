package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/worldofchami/ucp/cmd/ucp"
	"github.com/worldofchami/ucp/pkg/models"
)

// CLI for interacting with UCP without MCP.
//
// Examples:
//
//	go run ./cmd/ucp-cli discover --store-url https://example.com
//	go run ./cmd/ucp-cli discover --store-url example.com
//
// Output is shaped like an MCP tool result: {"content":[...]}
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	godotenv.Load()

	http_client := newHTTPClientWithBearerToken(os.Getenv("SHOPIFY_ACCESS_TOKEN"))
	
	shopify_client := &ucp.ShopifyClient{
		HTTPClient: http_client,
	}

	switch os.Args[1] {
	case "discover":
		discover(os.Args[2:])
	case "discover-products":
		discoverProducts(os.Args[2:], shopify_client)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	_, _ = fmt.Fprintln(os.Stderr, "usage:")
	_, _ = fmt.Fprintln(os.Stderr, "  ucp discover --store-url <url>")
	_, _ = fmt.Fprintln(os.Stderr, "  ucp discover-products --query <query> [--context <context>]")
}

func discover(args []string) {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	storeURL := fs.String("store-url", "", "store base url (e.g. https://example.com)")
	_ = fs.Parse(args)

	if *storeURL == "" {
		usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := ucp.NewClient()
	resp, err := client.Discover(ctx, *storeURL)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// bearerTokenTransport wraps an http.RoundTripper and adds a Bearer token header to every request
type bearerTokenTransport struct {
	base   http.RoundTripper
	token  string
}

func (t *bearerTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(req)
}

// newHTTPClientWithBearerToken creates an HTTP client that automatically adds a Bearer token to all requests
func newHTTPClientWithBearerToken(token string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &bearerTokenTransport{
			base:  http.DefaultTransport,
			token: token,
		},
	}
}

func discoverProducts(args []string, shopify_client *ucp.ShopifyClient) {
	fs := flag.NewFlagSet("discover-products", flag.ExitOnError)
	query := fs.String("query", "", "product search query (e.g. \"organic cotton sweater\")")
	context := fs.String("context", "", "optional additional context for the search")
	_ = fs.Parse(args)

	if *query == "" {
		_, _ = fmt.Fprintln(os.Stderr, "discover-products requires --query")
		usage()
		os.Exit(2)
	}

	platformProducts, err := shopify_client.DiscoverProducts(*query, *context)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	standardised := make([]models.Product, 0, len(platformProducts))
	for _, p := range platformProducts {
		standardised = append(standardised, p.Standardise())
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(standardised); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}