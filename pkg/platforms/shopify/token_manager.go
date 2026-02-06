// Package shopify provides token management for Shopify API authentication
package shopify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenManager manages Shopify access tokens with automatic refresh
type TokenManager struct {
	clientID     string
	clientSecret string
	accessToken  string
	expiry       time.Time
	mu           sync.RWMutex
	httpClient   *http.Client
}

// TokenResponse represents the response from Shopify token endpoint
type TokenResponse struct {
	AccessToken string `json:"access_token"`
}

// JWTClaims represents the decoded JWT claims
type JWTClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	Expiry    int64  `json:"exp"`
	Scopes    string `json:"scopes"`
	JTI       string `json:"jti"`
	Limits    struct {
		Catalog struct {
			Max    int `json:"max"`
			Period int `json:"period"`
		} `json:"catalog"`
	} `json:"limits"`
}

// NewTokenManager creates a new Shopify token manager
func NewTokenManager() (*TokenManager, error) {
	clientID := os.Getenv("SHOPIFY_CLIENT_ID")
	clientSecret := os.Getenv("SHOPIFY_CLIENT_SECRET")

	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("SHOPIFY_CLIENT_ID and SHOPIFY_CLIENT_SECRET must be set")
	}

	tm := &TokenManager{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}

	// Initial token fetch
	if err := tm.refreshToken(); err != nil {
		return nil, fmt.Errorf("failed to fetch initial token: %w", err)
	}

	return tm, nil
}

// GetToken returns the current access token, refreshing if necessary
func (tm *TokenManager) GetToken() (string, error) {
	tm.mu.RLock()
	token := tm.accessToken
	expiry := tm.expiry
	tm.mu.RUnlock()

	// Refresh if token is empty or expires within 5 minutes
	if token == "" || time.Until(expiry) < 5*time.Minute {
		tm.mu.Lock()
		defer tm.mu.Unlock()

		// Double-check after acquiring write lock
		if tm.accessToken == "" || time.Until(tm.expiry) < 5*time.Minute {
			if err := tm.refreshToken(); err != nil {
				return "", fmt.Errorf("failed to refresh token: %w", err)
			}
		}
		token = tm.accessToken
	}

	return token, nil
}

// refreshToken fetches a new access token from Shopify
func (tm *TokenManager) refreshToken() error {
	payload := map[string]string{
		"client_id":     tm.clientID,
		"client_secret": tm.clientSecret,
		"grant_type":    "client_credentials",
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.shopify.com/auth/access_token", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token refresh failed with status: %d", resp.StatusCode)
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	// Decode JWT to get expiry
	claims, err := decodeJWT(tokenResp.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to decode JWT: %w", err)
	}

	tm.accessToken = tokenResp.AccessToken
	tm.expiry = time.Unix(claims.Expiry, 0)

	return nil
}

// decodeJWT decodes a JWT token without verification to extract claims
func decodeJWT(token string) (*JWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}

	// Decode payload (second part)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode payload: %w", err)
	}

	var claims JWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("failed to unmarshal claims: %w", err)
	}

	return &claims, nil
}
