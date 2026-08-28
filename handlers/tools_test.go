package handlers

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"claude2api/models"
)

func TestExtractToolCalls(t *testing.T) {
	text := `Here is help.

[TOOL_CALL]{"name":"Bash","id":"toolu_1","input":{"command":"echo hi"}}[/TOOL_CALL]

After text.

[TOOL_CALL]{"name":"Read","id":"toolu_2","input":{"file_path":"a.txt"}}[/TOOL_CALL]`

	stripped, calls := extractToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "Bash" || calls[0].ID != "toolu_1" {
		t.Fatalf("first call wrong: %+v", calls[0])
	}
	if calls[1].Name != "Read" {
		t.Fatalf("second call wrong: %+v", calls[1])
	}
	if strings.Contains(stripped, "[TOOL_CALL]") {
		t.Fatalf("stripped still contains marker: %s", stripped)
	}
	if !strings.Contains(stripped, "Here is help") || !strings.Contains(stripped, "After text") {
		t.Fatalf("stripped lost plain text: %s", stripped)
	}
}

func TestExtractToolCallsNone(t *testing.T) {
	text := "plain text with no calls"
	stripped, calls := extractToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("expected 0 calls, got %d", len(calls))
	}
	if stripped != text {
		t.Fatalf("text changed: %s", stripped)
	}
}

func TestToolBashAndRead(t *testing.T) {
	out := executeTool("Bash", map[string]interface{}{"command": "echo hello"})
	if !strings.Contains(out, "hello") {
		t.Fatalf("bash output wrong: %q", out)
	}

	// Write then Read
	defer os.Remove("test_tool_tmp.txt")
	executeTool("Write", map[string]interface{}{"file_path": "test_tool_tmp.txt", "content": "line1\nline2"})
	out = executeTool("Read", map[string]interface{}{"file_path": "test_tool_tmp.txt"})
	if !strings.Contains(out, "line1") {
		t.Fatalf("read output wrong: %q", out)
	}
}

func TestAnthropicContentToToolText(t *testing.T) {
	blocks := []interface{}{
		map[string]interface{}{"type": "tool_use", "name": "Bash", "id": "toolu_1", "input": map[string]interface{}{"command": "echo"}},
		map[string]interface{}{"type": "tool_result", "use_id": "toolu_1", "content": "hello"},
		map[string]interface{}{"type": "text", "text": "done"},
	}
	got := anthropicContentToToolText(blocks)
	if !strings.Contains(got, "Tool Use: Bash (toolu_1)") {
		t.Fatalf("missing tool_use text: %q", got)
	}
	if !strings.Contains(got, "Tool Result: toolu_1") {
		t.Fatalf("missing tool_result text: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "done") {
		t.Fatalf("missing inner text: %q", got)
	}
}

func TestToolDefsPrompt(t *testing.T) {
	tools := []models.AnthropicTool{{Name: "Bash", InputSchema: map[string]interface{}{"type": "object"}}}
	p := buildToolDefsPrompt(tools)
	if !strings.Contains(p, "TOOL_CALL") || !strings.Contains(p, "Bash") {
		t.Fatalf("defs prompt wrong: %q", p)
	}
}

func TestEmptyToolsOverride(t *testing.T) {
	var empty json.RawMessage = json.RawMessage([]byte("[]"))
	if string(empty) != "[]" {
		t.Fatalf("empty tools not []: %s", empty)
	}
}

func TestCacheTracker(t *testing.T) {
	// Fresh tracker for this test.
	ct := &cacheTracker{seen: make(map[string]time.Time), ttl: 5 * time.Minute}

	req := models.AnthropicRequest{
		System: []interface{}{
			map[string]interface{}{
				"type":          "text",
				"text":          "this is a long system prompt that should be cached for sure",
				"cache_control": map[string]interface{}{"type": "ephemeral"},
			},
		},
	}

	creation, read := ct.record("conv-1", req)
	if creation == 0 {
		t.Fatalf("expected cache creation tokens > 0, got %d", creation)
	}
	if read != 0 {
		t.Fatalf("expected read=0 first time, got %d", read)
	}

	// Same request again -> should be a cache read.
	creation2, read2 := ct.record("conv-1", req)
	if creation2 != 0 {
		t.Fatalf("expected creation=0 second time, got %d", creation2)
	}
	if read2 == 0 {
		t.Fatalf("expected cache read tokens > 0, got %d", read2)
	}
}

func TestCollectCacheBlocks(t *testing.T) {
	req := models.AnthropicRequest{
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "cached system", "cache_control": map[string]interface{}{"type": "ephemeral"}},
			map[string]interface{}{"type": "text", "text": "uncached"},
		},
		Messages: []models.AnthropicMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "cached user message", "cache_control": map[string]interface{}{"type": "ephemeral"}},
			}},
		},
	}
	blocks := collectCacheBlocks(req)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 cache blocks, got %d", len(blocks))
	}
}

func TestResolveEffort(t *testing.T) {
	cases := map[string]string{
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
		"xhigh":   "xhigh",
		"max":     "max",
		"HIGH":    "high",
		"  max ":  "max",
		"invalid": "medium",
		"":        "medium",
	}
	for in, want := range cases {
		if got := resolveEffort(in); got != want {
			t.Fatalf("resolveEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveThinking(t *testing.T) {
	cases := []struct {
		in         interface{}
		wantMode   string
		wantBudget int
	}{
		{nil, "auto", 0},
		{map[string]interface{}{"type": "disabled"}, "none", 0},
		{map[string]interface{}{"type": "enabled", "budget_tokens": float64(10000)}, "auto", 10000},
		{map[string]interface{}{"type": "enabled"}, "auto", 0},
		{"invalid", "auto", 0},
	}
	for _, c := range cases {
		mode, budget := resolveThinking(c.in)
		if mode != c.wantMode || budget != c.wantBudget {
			t.Fatalf("resolveThinking(%v) = (%q,%d), want (%q,%d)", c.in, mode, budget, c.wantMode, c.wantBudget)
		}
	}
}
