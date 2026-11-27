package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The logger runs before every handler, so a body it only partially buffers is
// a body the handler never sees in full.
func TestErrorBodyLoggerPassesLargeBodiesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ErrorBodyLogger())

	var got struct {
		Content string `json:"content"`
	}
	r.POST("/echo", func(c *gin.Context) {
		if err := c.ShouldBindJSON(&got); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"len": len(got.Content)})
	})

	content := strings.Repeat("a", 10*errorBodyMaxBytes)
	body, _ := json.Marshal(map[string]string{"content": content})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/echo", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got.Content != content {
		t.Fatalf("handler saw %d bytes, sent %d", len(got.Content), len(content))
	}
}
