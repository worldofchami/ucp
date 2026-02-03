package utils

import (
	"net/http"
	"time"
)

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

func NewHTTPClientWithBearerToken(token string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &bearerTokenTransport{
			base:  http.DefaultTransport,
			token: token,
		},
	}
}