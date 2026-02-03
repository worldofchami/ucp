package ucp

import (
	"github.com/worldofchami/ucp/pkg/models"
)

// ContentItem represents a content item in a response.
// Keep this minimal and extensible; other item types (image, resource, etc.)
// can be added later as needed.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolResponse represents a response containing content items.
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

// UCPClient is an interface for platform-specific product discovery implementations.
type UCPClient interface {
	DiscoverProducts(query string, context string) ([]models.PlatformProduct, error)
}