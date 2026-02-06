package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/worldofchami/ucp/cmd/ucp"
	"github.com/worldofchami/ucp/pkg/models"
	"github.com/worldofchami/ucp/pkg/platforms/shopify"
	"github.com/worldofchami/ucp/pkg/utils"
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

	// Initialize Shopify token manager
	var shopifyClient *shopify.Client
	if os.Getenv("SHOPIFY_CLIENT_ID") != "" && os.Getenv("SHOPIFY_CLIENT_SECRET") != "" {
		tokenManager, err := shopify.NewTokenManager()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error initializing token manager: %v\n", err)
			os.Exit(1)
		}

		token, err := tokenManager.GetToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting token: %v\n", err)
			os.Exit(1)
		}

		http_client := utils.NewHTTPClientWithBearerToken(token)

		region := os.Getenv("UCP_REGION")
		if strings.TrimSpace(region) == "" {
			region = "ZA"
		}

		shopifyClient = &shopify.Client{
			HTTPClient: http_client,
			Region:     region,
		}
	} else {
		fmt.Fprintf(os.Stderr, "Error: SHOPIFY_CLIENT_ID and SHOPIFY_CLIENT_SECRET must be set\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "discover":
		discover(os.Args[2:])
	case "discover-products":
		discoverProducts(os.Args[2:], shopifyClient)
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

func discoverProducts(args []string, shopify_client *shopify.Client) {
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

func createCheckout(variants []models.Variant) {

}
