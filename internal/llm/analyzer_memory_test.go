package llm

import (
	"context"
	"strings"
	"testing"

	"deploy-api/internal/domain"
)

// stubProvider replays a canned completion so the response contract can be
// tested without a network call.
type stubProvider struct {
	response string
	lastReq  CompletionRequest
}

func (s *stubProvider) Complete(_ context.Context, req CompletionRequest) (*CompletionResponse, error) {
	s.lastReq = req
	return &CompletionResponse{Content: s.response}, nil
}

func (s *stubProvider) Name() string { return "stub" }

func analyzeWith(t *testing.T, response string) (*RiskAnalysis, *stubProvider) {
	t.Helper()
	p := &stubProvider{response: response}
	a := NewChangelogAnalyzer(p, "", "")
	res, err := a.AnalyzeChangelog(context.Background(), "app", &domain.ChangelogResponse{}, "## Conventions\n- x")
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return res, p
}

func TestAnalyze_ParsesMemoryUpdates(t *testing.T) {
	res, p := analyzeWith(t, `{
	  "risk_level": "high",
	  "risk_score": 70,
	  "summary": "migrations moved into CI",
	  "concerns": ["schema change"],
	  "recommendations": ["watch the first deploy"],
	  "needs_review": true,
	  "memory_updates": [
	    {"action": "replace", "section": "Conventions", "content": "- migrations run in CI", "reason": "this change automated them"}
	  ]
	}`)

	if res.RiskLevel != "high" || res.RiskScore != 70 {
		t.Fatalf("risk fields must survive the added schema, got %s/%d", res.RiskLevel, res.RiskScore)
	}
	if len(res.MemoryUpdates) != 1 {
		t.Fatalf("expected 1 memory update, got %d", len(res.MemoryUpdates))
	}
	u := res.MemoryUpdates[0]
	if u.Action != "replace" || u.Section != "Conventions" || u.Reason == "" {
		t.Fatalf("unexpected update: %+v", u)
	}

	// The memory has to be in the prompt for the model to propose edits to it,
	// and the guardrail has to describe the field.
	if !strings.Contains(p.lastReq.Messages[0].Content, "## Conventions") {
		t.Fatalf("current memory must be in the user prompt")
	}
	if !strings.Contains(p.lastReq.System, "memory_updates") {
		t.Fatalf("guardrail must describe memory_updates")
	}
}

// The old shape must keep working: a model that omits the field is not an error.
func TestAnalyze_MemoryUpdatesOptional(t *testing.T) {
	res, _ := analyzeWith(t, `{"risk_level":"low","risk_score":10,"summary":"s","concerns":[],"recommendations":[],"needs_review":false}`)
	if res.RiskLevel != "low" {
		t.Fatalf("expected the analysis to parse, got %s", res.RiskLevel)
	}
	if len(res.MemoryUpdates) != 0 {
		t.Fatalf("expected no updates, got %d", len(res.MemoryUpdates))
	}
}

// A malformed entry must never cost us the risk analysis in the same object.
func TestAnalyze_SanitizesMemoryUpdates(t *testing.T) {
	res, _ := analyzeWith(t, `{
	  "risk_level": "medium", "risk_score": 40, "summary": "s", "concerns": [], "recommendations": [], "needs_review": false,
	  "memory_updates": [
	    {"action": "ADD", "content": "  - trimmed and lowercased  "},
	    {"action": "delete", "content": "- unsupported action"},
	    {"action": "add", "content": ""},
	    {"action": "remove", "section": "Gone", "content": ""},
	    {"content": "- action defaults to add"}
	  ]
	}`)

	if res.RiskLevel != "medium" {
		t.Fatalf("bad entries must not break the analysis, got %s", res.RiskLevel)
	}
	if len(res.MemoryUpdates) != 3 {
		t.Fatalf("expected 3 usable updates, got %d: %+v", len(res.MemoryUpdates), res.MemoryUpdates)
	}
	if res.MemoryUpdates[0].Action != domain.MemoryActionAdd || res.MemoryUpdates[0].Content != "- trimmed and lowercased" {
		t.Fatalf("expected action lowercased and content trimmed, got %+v", res.MemoryUpdates[0])
	}
	// Empty content is only meaningful as "remove this whole section".
	if res.MemoryUpdates[1].Action != domain.MemoryActionRemove || res.MemoryUpdates[1].Section != "Gone" {
		t.Fatalf("expected the section removal kept, got %+v", res.MemoryUpdates[1])
	}
	if res.MemoryUpdates[2].Action != domain.MemoryActionAdd {
		t.Fatalf("expected a missing action to default to add, got %+v", res.MemoryUpdates[2])
	}
}

// A model ignoring "an empty list is expected" must not bury the reviewer.
func TestAnalyze_CapsMemoryUpdates(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"risk_level":"low","risk_score":1,"summary":"s","concerns":[],"recommendations":[],"needs_review":false,"memory_updates":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"action":"add","content":"- fact `)
		sb.WriteByte(byte('a' + i))
		sb.WriteString(`"}`)
	}
	sb.WriteString("]}")

	res, _ := analyzeWith(t, sb.String())
	if len(res.MemoryUpdates) != maxMemoryUpdatesPerBuild {
		t.Fatalf("expected the cap of %d, got %d", maxMemoryUpdatesPerBuild, len(res.MemoryUpdates))
	}
}
