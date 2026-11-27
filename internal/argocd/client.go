// Package argocd is a minimal client for the Argo CD REST API.
package argocd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout  = 10 * time.Second
	maxErrorBodyLen = 512
)

// Client calls the Argo CD API server with a bearer token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a client for baseURL (e.g. http://argocd-server.argocd.svc).
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// Refresh asks Argo CD to re-read the Application's source repo now.
func (c *Client) Refresh(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodGet, "/api/v1/applications/"+url.PathEscape(name)+"?refresh=normal", "")
}

// Sync triggers a sync of the Application without pruning.
func (c *Client) Sync(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/applications/"+url.PathEscape(name)+"/sync", `{"prune":false}`)
}

func (c *Client) do(ctx context.Context, method, path, body string) error {
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("argocd: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("argocd: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyLen))
		return fmt.Errorf("argocd: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
