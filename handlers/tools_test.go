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
		map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_1", "content": "hello"},
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

func TestToolResultUsesAnthropicToolUseID(t *testing.T) {
	block := models.AnthropicContentBlock{
		Type:    "tool_result",
		UseID:   "toolu_1",
		Content: "hello",
	}
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal tool result: %v", err)
	}
	if !strings.Contains(string(data), `"tool_use_id":"toolu_1"`) {
		t.Fatalf("tool result uses wrong ID field: %s", data)
	}
	if strings.Contains(string(data), `"use_id"`) {
		t.Fatalf("tool result leaked legacy ID field: %s", data)
	}
}

func TestValidateAnthropicToolPairing(t *testing.T) {
	toolUse := func(id string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_use", "id": id, "name": "lookup", "input": map[string]interface{}{}}
	}
	toolResult := func(id string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_result", "tool_use_id": id, "content": "ok"}
	}

	tests := []struct {
		name     string
		messages []models.AnthropicMessage
		wantErr  string
	}{
		{
			name: "valid multi-round transcript",
			messages: []models.AnthropicMessage{
				{Role: "assistant", Content: []interface{}{toolUse("toolu_1")}},
				{Role: "user", Content: []interface{}{toolResult("toolu_1")}},
				{Role: "assistant", Content: []interface{}{toolUse("toolu_2")}},
				{Role: "user", Content: []interface{}{toolResult("toolu_2")}},
			},
		},
		{
			name: "missing result id",
			messages: []models.AnthropicMessage{
				{Role: "assistant", Content: []interface{}{toolUse("toolu_1")}},
				{Role: "user", Content: []interface{}{toolResult("")}},
			},
			wantErr: "empty tool_use_id",
		},
		{
			name: "unknown result id",
			messages: []models.AnthropicMessage{
				{Role: "user", Content: []interface{}{toolResult("toolu_unknown")}},
			},
			wantErr: "unknown tool_use id",
		},
		{
			name: "duplicate result",
			messages: []models.AnthropicMessage{
				{Role: "assistant", Content: []interface{}{toolUse("toolu_1")}},
				{Role: "user", Content: []interface{}{toolResult("toolu_1"), toolResult("toolu_1")}},
			},
			wantErr: "duplicate tool_result",
		},
		{
			name: "out of order results",
			messages: []models.AnthropicMessage{
				{Role: "assistant", Content: []interface{}{toolUse("toolu_1"), toolUse("toolu_2")}},
				{Role: "user", Content: []interface{}{toolResult("toolu_2"), toolResult("toolu_1")}},
			},
			wantErr: "out of order",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAnthropicToolPairing(normalizeAnthropicToolResults(tc.messages))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestToolDefsPrompt(t *testing.T) {
	tools := []models.AnthropicTool{{Name: "Bash", InputSchema: map[string]interface{}{"type": "object"}}}
	p := buildToolDefsPrompt(tools, nil)
	if !strings.Contains(p, "TOOL_CALL") || !strings.Contains(p, "Bash") {
		t.Fatalf("defs prompt wrong: %q", p)
	}
}

func TestRewriteInitialReadCallsMapsLegacyRepoPath(t *testing.T) {
	tools := []models.AnthropicTool{{Name: "Glob"}}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"windows main", `C:\Users\Administrator\Desktop\claudeapi\cmd\server\main.go`, "main.go"},
		{"existing admin file", `C:\Users\Administrator\Desktop\claudeapi\admin\api.go`, "admin/api.go"},
		{"relative config", "claudeapi/internal/config/config.go", "config/config.go"},
		{"directory config", "internal/config/custom.go", "config/custom.go"},
		{"existing model file", "/tmp/workspace/claudeapi/models/request.go", "models/request.go"},
		{"suffix proxy alias", "/tmp/work/claudeapi/internal/proxy/claude.go", "claude/client.go"},
		{"directory handler", "/tmp/work/internal/handlers/extra.go", "handlers/extra.go"},
		{"handler models", "internal/handlers/models.go", "handlers/common.go"},
		{"proxy sse alias", "claudeapi/internal/proxy/sse.go", "utils/sse.go"},
		{"middleware auth", "claudeapi/internal/middleware/auth.go", "middleware/auth.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := []toolCall{{
				Name: "read_file",
				ID:   "call_1",
				Input: map[string]interface{}{
					"file_path": tc.in,
				},
			}}
			rewritten := rewriteInitialReadCalls(nil, tools, calls)
			if len(rewritten) != 1 {
				t.Fatalf("expected 1 call, got %d", len(rewritten))
			}
			if rewritten[0].Name != "read_file" {
				t.Fatalf("expected read_file call to be preserved, got %+v", rewritten[0])
			}
			if rewritten[0].ID != "call_1" {
				t.Fatalf("expected original ID to be preserved, got %+v", rewritten[0])
			}
			if rewritten[0].Input["file_path"] != tc.want {
				t.Fatalf("unexpected mapped path: %+v", rewritten[0].Input)
			}
		})
	}
}

func TestRewriteInitialReadCallsSkipsFollowUpReads(t *testing.T) {
	tools := []models.AnthropicTool{{Name: "Glob"}}
	messages := []models.AnthropicMessage{{
		Role: "user",
		Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "call_0", "content": "handlers/chat.go"},
		},
	}}
	calls := []toolCall{{
		Name: "Read",
		ID:   "call_1",
		Input: map[string]interface{}{
			"file_path": "handlers/chat.go",
		},
	}}

	rewritten := rewriteInitialReadCalls(messages, tools, calls)
	if rewritten[0].Name != "Read" {
		t.Fatalf("follow-up read should not be rewritten: %+v", rewritten[0])
	}
	if rewritten[0].Input["file_path"] != "handlers/chat.go" {
		t.Fatalf("follow-up read input changed: %+v", rewritten[0].Input)
	}
}

func TestRewriteInitialReadCallsFallsBackToListFiles(t *testing.T) {
	tools := []models.AnthropicTool{{Name: "list_files"}}
	calls := []toolCall{{
		Name: "Read",
		ID:   "call_1",
		Input: map[string]interface{}{
			"file_path": "claudeapi/internal/unknown/missing.go",
		},
	}}

	rewritten := rewriteInitialReadCalls(nil, tools, calls)
	if rewritten[0].Name != "list_files" {
		t.Fatalf("expected list_files call, got %+v", rewritten[0])
	}
	if rewritten[0].Input["path"] != "." || rewritten[0].Input["recursive"] != true {
		t.Fatalf("unexpected list_files input: %+v", rewritten[0].Input)
	}
}

func TestRewriteInitialReadCallsFallsBackToGlobWhenNoLegacyMap(t *testing.T) {
	tools := []models.AnthropicTool{{Name: "Glob"}}
	calls := []toolCall{{
		Name: "Read",
		ID:   "call_1",
		Input: map[string]interface{}{
			"file_path": `C:\Users\Administrator\Desktop\claudeapi\other\missing.go`,
		},
	}}

	rewritten := rewriteInitialReadCalls(nil, tools, calls)
	if rewritten[0].Name != "Glob" {
		t.Fatalf("expected Glob fallback, got %+v", rewritten[0])
	}
	if rewritten[0].Input["pattern"] != "**/missing.go" || rewritten[0].Input["path"] != "." {
		t.Fatalf("unexpected Glob fallback input: %+v", rewritten[0].Input)
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

func TestOpenAIToolsToAnthropic(t *testing.T) {
	tools := []models.OpenAITool{{
		Type: "function",
		Function: models.OpenAIToolFunction{
			Name:        "lookup",
			Description: "Look up a value",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}},
			},
		},
	}}
	converted := openAIToolsToAnthropic(tools)
	if len(converted) != 1 || converted[0].Name != "lookup" {
		t.Fatalf("unexpected converted tools: %+v", converted)
	}
	if converted[0].InputSchema["type"] != "object" {
		t.Fatalf("input schema was not preserved: %+v", converted[0].InputSchema)
	}
}

func TestToolChoiceBidirectionalTranslation(t *testing.T) {
	cases := []struct {
		openAI    interface{}
		anthropic interface{}
	}{
		{"auto", map[string]interface{}{"type": "auto"}},
		{"none", map[string]interface{}{"type": "none"}},
		{"required", map[string]interface{}{"type": "any"}},
		{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "lookup"}}, map[string]interface{}{"type": "tool", "name": "lookup"}},
	}
	for _, tc := range cases {
		got := openAIToolChoiceToAnthropic(tc.openAI)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(tc.anthropic)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("OpenAI to Anthropic: got %s, want %s", gotJSON, wantJSON)
		}
		roundTrip := anthropicToolChoiceToOpenAI(got)
		if tc.openAI == "required" {
			if roundTrip != "required" {
				t.Fatalf("required round trip = %#v", roundTrip)
			}
		}
	}
}

func TestToolCallBidirectionalTranslation(t *testing.T) {
	call := models.OpenAIToolCall{
		ID:       "call_1",
		Type:     "function",
		Function: models.OpenAIFunctionCall{Name: "lookup", Arguments: `{"query":"value"}`},
	}
	block := openAIToolCallToAnthropic(call)
	if block.Type != "tool_use" || block.ID != "call_1" || block.Name != "lookup" || block.Input["query"] != "value" {
		t.Fatalf("unexpected Anthropic block: %+v", block)
	}
	converted := anthropicToolUseToOpenAI(block)
	if converted.ID != call.ID || converted.Function.Name != call.Function.Name {
		t.Fatalf("unexpected OpenAI call: %+v", converted)
	}
	var arguments map[string]interface{}
	if err := json.Unmarshal([]byte(converted.Function.Arguments), &arguments); err != nil || arguments["query"] != "value" {
		t.Fatalf("unexpected arguments: %q (%v)", converted.Function.Arguments, err)
	}
}

func TestOpenAIToolCallMalformedArgumentsArePreserved(t *testing.T) {
	block := openAIToolCallToAnthropic(models.OpenAIToolCall{
		ID:       "call_bad",
		Function: models.OpenAIFunctionCall{Name: "lookup", Arguments: "not-json"},
	})
	if block.Input["_raw_arguments"] != "not-json" {
		t.Fatalf("malformed arguments were lost: %+v", block.Input)
	}
}

func TestResponseToolOutput(t *testing.T) {
	items := responseToolOutput([]models.AnthropicContentBlock{
		{Type: "text", Text: "before"},
		{Type: "tool_use", ID: "call_1", Name: "lookup", Input: map[string]interface{}{"query": "value"}},
		{Type: "text", Text: "after"},
	})
	if len(items) != 3 {
		t.Fatalf("expected 3 output items, got %d: %+v", len(items), items)
	}
	if items[1].Type != "function_call" || items[1].CallID != "call_1" || items[1].Name != "lookup" {
		t.Fatalf("unexpected function_call item: %+v", items[1])
	}
}

func TestExtractFromToolCallsWrapperOpenAIFormat(t *testing.T) {
	text := `prefix {"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"abc\"}"}}]} suffix`
	calls := extractFromToolCallsWrapper(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "lookup" || calls[0].ID != "call_1" {
		t.Fatalf("wrong call: %+v", calls[0])
	}
	if calls[0].Input["query"] != "abc" {
		t.Fatalf("wrong arguments: %+v", calls[0].Input)
	}
}

func TestExtractFromToolCallsWrapperMultipleBlocks(t *testing.T) {
	text := `{"tool_calls":[{"id":"call_1","name":"Read","input":{"file_path":"a.txt"}}]}
{"tool_calls":[{"id":"call_2","name":"Read","input":{"file_path":"b.txt"}}]}`
	calls := extractFromToolCallsWrapper(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[1].ID != "call_2" {
		t.Fatalf("unexpected call IDs: %+v", calls)
	}
}

func TestResponseInputToAnthropicMessagesFunctionCallOutput(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{"type": "function_call_output", "call_id": "call_1", "output": "ok-1"},
		map[string]interface{}{"type": "function_call_output", "call_id": "call_2", "output": map[string]interface{}{"status": "ok"}},
	}
	msgs := responseInputToAnthropicMessages(input)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected user role, got %q", msgs[0].Role)
	}
	parts, ok := msgs[0].Content.([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %#v", msgs[0].Content)
	}
	first, _ := parts[0].(map[string]interface{})
	second, _ := parts[1].(map[string]interface{})
	if first["type"] != "tool_result" || first["tool_use_id"] != "call_1" || first["content"] != "ok-1" {
		t.Fatalf("unexpected first block: %#v", first)
	}
	if second["type"] != "tool_result" || second["tool_use_id"] != "call_2" {
		t.Fatalf("unexpected second block: %#v", second)
	}
	if _, ok := second["content"].(string); !ok {
		t.Fatalf("expected second content serialized as string, got %#v", second["content"])
	}
}

func TestResponseInputToAnthropicMessagesFunctionCallOutputPendingFlush(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{"type": "function_call_output", "call_id": "call_1", "output": "r1"},
		map[string]interface{}{"type": "reasoning", "summary": []interface{}{map[string]interface{}{"text": "thinking"}}},
		map[string]interface{}{"type": "function_call_output", "call_id": "call_2", "output": "r2"},
		map[string]interface{}{"type": "message", "role": "user", "content": "next"},
	}
	msgs := responseInputToAnthropicMessages(input)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d: %#v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected first message user, got %q", msgs[0].Role)
	}
	parts, ok := msgs[0].Content.([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("expected first message to carry 2 tool_results, got %#v", msgs[0].Content)
	}
	first, _ := parts[0].(map[string]interface{})
	second, _ := parts[1].(map[string]interface{})
	if first["tool_use_id"] != "call_1" || second["tool_use_id"] != "call_2" {
		t.Fatalf("unexpected tool_use_id order: %#v", parts)
	}
	if msgs[1].Role != "user" || msgs[1].Content != "next" {
		t.Fatalf("unexpected second message: %#v", msgs[1])
	}
}

func TestResponseInputToAnthropicMessagesLiteLLMOutputTypes(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{"type": "custom_tool_call_output", "call_id": "call_custom", "output": "c1"},
		map[string]interface{}{"type": "computer_call_output", "tool_call_id": "call_computer", "content": []interface{}{
			map[string]interface{}{"type": "text", "text": "ok"},
		}},
	}
	msgs := responseInputToAnthropicMessages(input)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	parts, ok := msgs[0].Content.([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %#v", msgs[0].Content)
	}
	first, _ := parts[0].(map[string]interface{})
	second, _ := parts[1].(map[string]interface{})
	if first["tool_use_id"] != "call_custom" || second["tool_use_id"] != "call_computer" {
		t.Fatalf("unexpected tool_use_id values: %#v", parts)
	}
	if first["content"] != "c1" {
		t.Fatalf("unexpected first content: %#v", first["content"])
	}
	if _, ok := second["content"].([]interface{}); !ok {
		t.Fatalf("expected second content as []interface{}, got %#v", second["content"])
	}
}

func TestResponseInputToAnthropicMessagesFunctionCallNestedFunction(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type":    "function_call",
			"call_id": "call_1",
			"function": map[string]interface{}{
				"name":      "lookup",
				"arguments": `{"query":"abc"}`,
			},
		},
	}
	msgs := responseInputToAnthropicMessages(input)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "assistant" {
		t.Fatalf("expected assistant role, got %q", msgs[0].Role)
	}
	blocks, ok := msgs[0].Content.([]interface{})
	if !ok || len(blocks) != 1 {
		t.Fatalf("expected one block, got %#v", msgs[0].Content)
	}
	block, _ := blocks[0].(map[string]interface{})
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "lookup" {
		t.Fatalf("unexpected tool_use block: %#v", block)
	}
	inputMap, _ := block["input"].(map[string]interface{})
	if inputMap["query"] != "abc" {
		t.Fatalf("unexpected function arguments: %#v", inputMap)
	}
}
