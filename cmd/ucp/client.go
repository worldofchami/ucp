package ucp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	HTTP      *http.Client
	UserAgent string
}

type Option func(*Client)

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.HTTP = h
		}
	}
}

func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if strings.TrimSpace(ua) != "" {
			c.UserAgent = ua
		}
	}
}

func NewClient(opts ...Option) *Client {
	c := &Client{
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
		},
		UserAgent: "ucp/0.1.0",
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Discover fetches UCP discovery from a store URL via GET /.well-known/ucp.
// It returns an MCP-shaped ToolResponse so it can be reused by an MCP server or other callers.
func (c *Client) Discover(ctx context.Context, storeURL string) (ToolResponse, error) {
	discoveryURL, err := Resolve(storeURL, "/.well-known/ucp")
	if err != nil {
		return ToolResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return ToolResponse{}, err
	}
	req.Header.Set("Accept", "application/json, */*;q=0.9")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ToolResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ToolResponse{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ToolResponse{}, &HTTPError{
			StatusCode:        resp.StatusCode,
			Status:            resp.Status,
			URL:               discoveryURL,
			BodyPreviewBase64: previewBase64(body, 2048),
		}
	}

	return Text(formatPossiblyJSON(body)), nil
}

// Advertise platform profile
func (c *Client) GetPlatformProfile(ctx context.Context) PlatformDiscoveryProfile {
	return PlatformDiscoveryProfile{
		Version: "",
		Capabilities: map[string]any{

		},
		Services: map[string]any{

		},
		PaymentHandlers: map[string]any{
			
		},
	}
}

// Resolve normalizes a base URL and returns a URL with the provided absolute path.
// If the base has no scheme, https:// is assumed.
func Resolve(baseURL string, absolutePath string) (string, error) {
	in := strings.TrimSpace(baseURL)
	if in == "" {
		return "", fmt.Errorf("base URL is empty")
	}
	if !strings.Contains(in, "://") {
		in = "https://" + in
	}

	u, err := url.Parse(in)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("unsupported URL scheme %q (must be http or https)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid base URL (missing host): %q", baseURL)
	}
	if !strings.HasPrefix(absolutePath, "/") {
		return "", fmt.Errorf("absolutePath must start with '/': %q", absolutePath)
	}

	u.Path = absolutePath
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

type HTTPError struct {
	StatusCode        int    `json:"statusCode"`
	Status            string `json:"status"`
	URL               string `json:"url"`
	BodyPreviewBase64 string `json:"bodyPreviewBase64,omitempty"`
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "http error"
	}
	return fmt.Sprintf("http request failed: %s (%s)", e.Status, e.URL)
}

func previewBase64(b []byte, max int) string {
	if len(b) > max {
		b = b[:max]
	}
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

func formatPossiblyJSON(b []byte) string {
	trim := strings.TrimSpace(string(b))
	if trim == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(trim), &v); err == nil {
		out, err := json.MarshalIndent(v, "", "  ")
		if err == nil {
			return string(out)
		}
	}
	return string(b)
}
