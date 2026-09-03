// Package admission enforces an optional shared render-miss budget.
package admission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Client asks a shared admission service before unique rendering work begins.
type Client struct {
	endpoint *url.URL
	token    string
	client   *http.Client
}

// NewClient creates a fail-closed shared admission client.
func NewClient(endpoint, token string) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("cluster admission endpoint must be an absolute HTTP URL")
	}
	hostIP := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" && (hostIP == nil || !hostIP.IsLoopback()) {
		return nil, errors.New("cluster admission endpoint must use HTTPS")
	}
	if len(token) < 32 {
		return nil, errors.New("cluster admission token must contain at least 32 bytes")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 2 * time.Second
	return &Client{
		endpoint: parsed,
		token:    token,
		client:   &http.Client{Transport: transport, Timeout: 3 * time.Second},
	}, nil
}

// Admit returns true only when the shared service explicitly grants a slot.
func (c *Client) Admit(ctx context.Context) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), nil)
	if err != nil {
		return false, fmt.Errorf("create cluster admission request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("request cluster admission: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4_096))
	switch response.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusTooManyRequests:
		return false, nil
	default:
		return false, fmt.Errorf("cluster admission returned HTTP %d", response.StatusCode)
	}
}
