package argocd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capturedRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   string
}

func newTestServer(t *testing.T, status int, respBody string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	got := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.path = r.URL.EscapedPath()
		got.query = r.URL.RawQuery
		got.auth = r.Header.Get("Authorization")
		got.body = string(b)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestRefresh(t *testing.T) {
	srv, got := newTestServer(t, http.StatusOK, `{"metadata":{"name":"core-api"}}`)
	c := NewClient(srv.URL+"/", "secret-token")

	if err := c.Refresh(context.Background(), "core-api"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.method != http.MethodGet {
		t.Errorf("method = %q, want GET", got.method)
	}
	if got.path != "/api/v1/applications/core-api" {
		t.Errorf("path = %q", got.path)
	}
	if got.query != "refresh=normal" {
		t.Errorf("query = %q, want refresh=normal", got.query)
	}
	if got.auth != "Bearer secret-token" {
		t.Errorf("auth = %q", got.auth)
	}
}

func TestSync(t *testing.T) {
	srv, got := newTestServer(t, http.StatusOK, `{}`)
	c := NewClient(srv.URL, "tok")

	if err := c.Sync(context.Background(), "core-api-prod"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/api/v1/applications/core-api-prod/sync" {
		t.Errorf("path = %q", got.path)
	}
	if got.body != `{"prune":false}` {
		t.Errorf("body = %q", got.body)
	}
	if got.auth != "Bearer tok" {
		t.Errorf("auth = %q", got.auth)
	}
}

func TestNameIsEscaped(t *testing.T) {
	srv, got := newTestServer(t, http.StatusOK, `{}`)
	c := NewClient(srv.URL, "tok")

	if err := c.Sync(context.Background(), "a/b c?x"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.path != "/api/v1/applications/a%2Fb%20c%3Fx/sync" {
		t.Errorf("path = %q", got.path)
	}
	if got.query != "" {
		t.Errorf("query = %q, want empty", got.query)
	}
}

func TestErrorIncludesBodySnippet(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusForbidden, `{"error":"permission denied","code":7}`)
	c := NewClient(srv.URL, "bad")

	err := c.Refresh(context.Background(), "app")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"403", "permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestErrorBodyIsTruncated(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusInternalServerError, strings.Repeat("x", 4096))
	c := NewClient(srv.URL, "tok")

	err := c.Sync(context.Background(), "app")
	if err == nil {
		t.Fatal("expected error")
	}
	if n := strings.Count(err.Error(), "x"); n > maxErrorBodyLen {
		t.Errorf("error body not truncated: %d chars", n)
	}
}
