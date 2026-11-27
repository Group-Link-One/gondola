package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGzip_CompressesWhenAccepted verifies the API group gzips responses for
// clients that advertise gzip, and that the compressed body decodes back to the
// original JSON.
func TestGzip_CompressesWhenAccepted(t *testing.T) {
	db, router, cleanup := setupActivityServer(t)
	defer cleanup()
	seedActivityRows(t, db)

	req := httptest.NewRequest("GET", "/api/v1/activity?limit=50", nil)
	req.Header.Set("Authorization", "Bearer test-root-key")
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if enc := rr.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if v := rr.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", v)
	}

	// Body must be valid gzip that decodes to the expected JSON envelope.
	zr, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decompressed body is not valid JSON: %v\nbody: %s", err, raw)
	}
	if len(resp.Data) != 3 {
		t.Errorf("expected 3 activity items, got %d", len(resp.Data))
	}
}

// TestGzip_PlainWhenNotAccepted verifies responses are left uncompressed for
// clients that don't send Accept-Encoding: gzip.
func TestGzip_PlainWhenNotAccepted(t *testing.T) {
	db, router, cleanup := setupActivityServer(t)
	defer cleanup()
	seedActivityRows(t, db)

	req := httptest.NewRequest("GET", "/api/v1/activity?limit=50", nil)
	req.Header.Set("Authorization", "Bearer test-root-key")
	// No Accept-Encoding header.
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if enc := rr.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want empty (plain)", enc)
	}
	// Body must be plain JSON.
	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("plain body not valid JSON: %v", err)
	}
}
