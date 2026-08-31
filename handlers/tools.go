package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"claude2api/models"

	"github.com/bmatcuk/doublestar/v4"
)

// toolCall is a parsed [TOOL_CALL]...[/TOOL_CALL] instruction from the model text.
type toolCall struct {
	Name  string
	ID    string
	Input map[string]interface{}
	Raw   string
}

// toolCallRe matches [TOOL_CALL]{...}[/TOOL_CALL], tolerating optional code
// fences (```json ... ```) that Claude sometimes wraps around the block.
var toolCallRe = regexp.MustCompile("(?s)(?:```(?:json)?\\s*)?\\[TOOL_CALL\\]\\s*(\\{[\\s\\S]*?\\})\\s*\\[/TOOL_CALL\\](?:\\s*```)?")

// xmlToolCallRe matches <tool_call>...</tool_call> and <function_call>...</function_call>
// formats that Claude sometimes emits instead of the primary [TOOL_CALL] markers.
var xmlToolCallRe = regexp.MustCompile(`(?s)<(?:tool_call|function_call)>\s*(\{[\s\S]*?\})\s*</(?:tool_call|function_call)>`)

// toolCallsWrapperRe matches the OpenAI-style {"tool_calls":[...]} wrapper that
// Claude occasionally emits when it follows an OpenAI assistant-message pattern.
var toolCallsWrapperRe = regexp.MustCompile(`(?s)\{\s*"tool_calls"\s*:\s*(\[[\s\S]*?\])\s*\}`)

// trailingCommaRe matches trailing commas immediately before a closing bracket
// or brace — a common LLM JSON generation defect.
var trailingCommaRe = regexp.MustCompile(`,\s*([}\]])`)

// extractToolCalls parses all tool calls embedded in the model's text.
// Returns the text stripped of tool-call markers, and the parsed calls.
//
// Detection order:
//  1. Primary [TOOL_CALL]...[/TOOL_CALL] format (with optional code-fence wrapper).
//  2. XML-style <tool_call>...</tool_call> / <function_call>...</function_call> fallback.
//
// Both paths share parseToolCallMatches which also handles the "arguments" key
// (OpenAI-style) in addition to "input", so mix-ups between formats are recovered.
func extractToolCalls(text string) (string, []toolCall) {
	// Pre-process: strip code fences that wrap only [TOOL_CALL] content so the
	// primary regex can match even when Claude buries the markers inside a code block.
	preprocessed := stripToolCallCodeFences(text)

	// Primary format.
	if indices := toolCallRe.FindAllStringSubmatchIndex(preprocessed, -1); len(indices) > 0 {
		return parseToolCallMatches(preprocessed, indices)
	}

	// XML-style fallback: <tool_call>...</tool_call> or <function_call>...</function_call>.
	if indices := xmlToolCallRe.FindAllStringSubmatchIndex(preprocessed, -1); len(indices) > 0 {
		return parseToolCallMatches(preprocessed, indices)
	}

	// OpenAI-style {"tool_calls":[...]} wrapper fallback.
	// Claude sometimes emits this format when following an assistant-message pattern.
	if calls := extractFromToolCallsWrapper(preprocessed); len(calls) > 0 {
		// Strip the wrapper block from the text so it is not forwarded as prose.
		cleaned := toolCallsWrapperRe.ReplaceAllString(preprocessed, "")
		return strings.TrimSpace(cleaned), calls
	}

	// Return preprocessed rather than the original text: code-fence stripping
	// may have already cleaned up TOOL_CALL wrapper syntax.
	return preprocessed, nil
}

// extractFromToolCallsWrapper attempts to parse a {"tool_calls":[...]} wrapper
// and returns the individual calls, or nil if the format does not match.
func extractFromToolCallsWrapper(text string) []toolCall {
	match := toolCallsWrapperRe.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	rawArray := []byte(match[1])
	var items []map[string]interface{}
	if err := json.Unmarshal(rawArray, &items); err != nil {
		// Apply trailing-comma fix before giving up.
		fixed := trailingCommaRe.ReplaceAll(rawArray, []byte("$1"))
		if err2 := json.Unmarshal(fixed, &items); err2 != nil {
			log.Printf("tool_calls wrapper parse failed: %v (raw: %.200s)", err2, string(rawArray))
			return nil
		}
	}
	var calls []toolCall
	for _, item := range items {
		name, _ := item["name"].(string)
		if name == "" {
			continue
		}
		id, _ := item["id"].(string)
		if id == "" {
			id = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
		}
		var input map[string]interface{}
		if v, ok := item["input"].(map[string]interface{}); ok {
			input = v
		} else if v, ok := item["arguments"].(map[string]interface{}); ok {
			input = v
		} else if s, ok := item["arguments"].(string); ok {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(s), &parsed) == nil {
				input = parsed
			}
		}
		if input == nil {
			input = make(map[string]interface{})
		}
		calls = append(calls, toolCall{Name: name, ID: id, Input: input})
	}
	return calls
}

// parseToolCallMatches is the shared extraction loop used by both the primary
// [TOOL_CALL] regex and the XML fallback. It handles:
//   - "input" key (Anthropic format)
//   - "arguments" key as a JSON object or JSON-encoded string (OpenAI format)
//   - Flat key/value pairs when neither "input" nor "arguments" is present
func parseToolCallMatches(text string, indices [][]int) (string, []toolCall) {
	var calls []toolCall
	var sb strings.Builder
	prev := 0
	for _, loc := range indices {
		// Append plain text that precedes this match.
		sb.WriteString(text[prev:loc[0]])
		prev = loc[1]

		// loc[2]:loc[3] is capture group 1 — the raw JSON payload.
		jsonBytes := []byte(text[loc[2]:loc[3]])
		var raw map[string]interface{}
		if err := json.Unmarshal(jsonBytes, &raw); err != nil {
			// Step 1: remove trailing commas — a common LLM JSON defect.
			noTrailing := trailingCommaRe.ReplaceAll(jsonBytes, []byte("$1"))
			// Step 2: collapse interior whitespace so pretty-printed JSON parses.
			compact := compactJSON(noTrailing)
			if err2 := json.Unmarshal(compact, &raw); err2 != nil {
				// Log the failure so maintainers can identify new format variants.
				log.Printf("tool call JSON parse failed (dropping match): %v (raw: %.200s)", err2, string(jsonBytes))
				continue
			}
		}
		name, _ := raw["name"].(string)
		if name == "" {
			continue
		}
		id, _ := raw["id"].(string)
		if id == "" {
			id = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
		}

		// Resolve arguments: prefer "input", then "arguments" (OpenAI format),
		// then fall back to all remaining top-level keys.
		var input map[string]interface{}
		if v, ok := raw["input"].(map[string]interface{}); ok {
			input = v
		} else if args, ok := raw["arguments"]; ok {
			switch v := args.(type) {
			case map[string]interface{}:
				input = v
			case string:
				// arguments may be a JSON-encoded string (common in OpenAI transcripts).
				var parsed map[string]interface{}
				if json.Unmarshal([]byte(v), &parsed) == nil {
					input = parsed
				}
			}
		}
		if input == nil {
			// Flat args: every key except reserved protocol fields becomes an input param.
			input = make(map[string]interface{}, len(raw))
			for k, v := range raw {
				if k == "name" || k == "id" || k == "arguments" {
					continue
				}
				input[k] = v
			}
		}
		calls = append(calls, toolCall{Name: name, ID: id, Input: input, Raw: text[loc[0]:loc[1]]})
	}
	sb.WriteString(text[prev:])
	return sb.String(), calls
}

// stripToolCallCodeFences removes code fences that surround [TOOL_CALL] blocks.
// Claude sometimes writes:
//
//	```json
//	[TOOL_CALL]{...}[/TOOL_CALL]
//	```
//
// This strips the fence so the regex can match cleanly.
var codeFenceToolCallRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\[TOOL_CALL\\][\\s\\S]*?\\[/TOOL_CALL\\])\\s*```")

func stripToolCallCodeFences(text string) string {
	return codeFenceToolCallRe.ReplaceAllString(text, "$1")
}

// compactJSON re-encodes JSON bytes using the standard encoder so whitespace
// and minor formatting differences don't prevent unmarshalling.
func compactJSON(src []byte) []byte {
	var v interface{}
	if err := json.Unmarshal(src, &v); err != nil {
		return src
	}
	out, err := json.Marshal(v)
	if err != nil {
		return src
	}
	return out
}

// openAIToolsToAnthropic converts OpenAI/New API function definitions into
// Anthropic tool definitions while preserving their JSON Schemas.
func openAIToolsToAnthropic(tools []models.OpenAITool) []models.AnthropicTool {
	out := make([]models.AnthropicTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		if tool.Function.Name == "" {
			continue
		}
		out = append(out, models.AnthropicTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: tool.Function.Parameters,
		})
	}
	return out
}

// openAIToolChoiceToAnthropic normalizes OpenAI/New API tool_choice values to
// Anthropic's auto/any/none/tool selection representation.
func openAIToolChoiceToAnthropic(choice interface{}) interface{} {
	switch value := choice.(type) {
	case nil:
		return nil
	case string:
		switch value {
		case "auto", "none":
			return map[string]interface{}{"type": value}
		case "required", "any":
			return map[string]interface{}{"type": "any"}
		default:
			return nil
		}
	case map[string]interface{}:
		if typ, _ := value["type"].(string); typ == "function" {
			if function, ok := value["function"].(map[string]interface{}); ok {
				if name, _ := function["name"].(string); name != "" {
					return map[string]interface{}{"type": "tool", "name": name}
				}
			}
		}
		return value
	default:
		return nil
	}
}

// anthropicToolChoiceToOpenAI converts Anthropic tool_choice into the closest
// OpenAI/New API equivalent.
func anthropicToolChoiceToOpenAI(choice interface{}) interface{} {
	value, ok := choice.(map[string]interface{})
	if !ok {
		return choice
	}
	switch typ, _ := value["type"].(string); typ {
	case "auto", "none":
		return typ
	case "any":
		return "required"
	case "tool":
		if name, _ := value["name"].(string); name != "" {
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}
		}
	}
	return choice
}

// anthropicToolUseToOpenAI converts a tool_use block into an OpenAI tool call.
func anthropicToolUseToOpenAI(block models.AnthropicContentBlock) models.OpenAIToolCall {
	arguments := mustJSON(block.Input)
	return models.OpenAIToolCall{
		ID:   block.ID,
		Type: "function",
		Function: models.OpenAIFunctionCall{
			Name:      block.Name,
			Arguments: arguments,
		},
	}
}

// openAIToolCallToAnthropic converts an OpenAI assistant tool call into an
// Anthropic tool_use content block. Malformed argument JSON is retained in a
// compatibility wrapper instead of silently dropping it.
func openAIToolCallToAnthropic(call models.OpenAIToolCall) models.AnthropicContentBlock {
	input := map[string]interface{}{}
	if strings.TrimSpace(call.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			input = map[string]interface{}{"_raw_arguments": call.Function.Arguments}
		}
	}
	return models.AnthropicContentBlock{
		Type:  "tool_use",
		ID:    call.ID,
		Name:  call.Function.Name,
		Input: input,
	}
}

func toolContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// executeTool runs the requested tool and returns its textual output.
func executeTool(name string, input map[string]interface{}) string {
	switch name {
	case "Bash":
		return toolBash(input)
	case "Read":
		return toolRead(input)
	case "Write":
		return toolWrite(input)
	case "Edit":
		return toolEdit(input)
	case "Glob":
		return toolGlob(input)
	case "Grep":
		return toolGrep(input)
	case "WebFetch":
		return toolUnsupported(name, input)
	default:
		return fmt.Sprintf("ERROR: unknown tool %q", name)
	}
}

func toolBash(input map[string]interface{}) string {
	cmd, _ := input["command"].(string)
	if cmd == "" {
		return "ERROR: Bash requires 'command'"
	}
	// Safety: block obviously dangerous interactive / destructive patterns.
	dangerous := []string{"rm -rf /", "mkfs", ":(){:|:&};:", "dd if=/dev/zero"}
	for _, d := range dangerous {
		if strings.Contains(cmd, d) {
			return fmt.Sprintf("ERROR: blocked dangerous command pattern: %s", d)
		}
	}
	timeout := 120 * time.Second
	if t, ok := input["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Millisecond
	}
	ctx, cancel := toolContext(timeout)
	defer cancel()

	execCmd := exec.CommandContext(ctx, "cmd", "/c", cmd)
	if os.PathSeparator == '/' {
		execCmd = exec.CommandContext(ctx, "bash", "-c", cmd)
	}
	out, err := execCmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("ERROR: command timed out after %v\n%s", timeout, truncate(string(out), 2000))
	}
	if err != nil {
		return fmt.Sprintf("ERROR: %v\n%s", err, truncate(string(out), 2000))
	}
	return truncate(string(out), 8000)
}

func toolRead(input map[string]interface{}) string {
	path, _ := input["file_path"].(string)
	if path == "" {
		return "ERROR: Read requires 'file_path'"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// File not found: immediately search the tree for files with the same
		// base name so the model gets the real path in this single round-trip
		// instead of requiring a separate Glob call followed by another Read.
		base := filepath.Base(path)
		var found []string
		if matches, gErr := doublestar.Glob(os.DirFS("."), "**/"+base, doublestar.WithFilesOnly()); gErr == nil && len(matches) > 0 {
			found = matches
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("ERROR: file not found: %s\n\n", path))
		sb.WriteString("IMPORTANT: Do NOT guess or invent another path.\n")
		if len(found) > 0 {
			sb.WriteString(fmt.Sprintf("Found %d file(s) named %q — use one of these exact paths with Read:\n", len(found), base))
			for _, f := range found {
				sb.WriteString("  " + f + "\n")
			}
		} else {
			sb.WriteString(fmt.Sprintf("No file named %q found in the current tree.\n", base))
			sb.WriteString("Use Glob to search: Glob pattern=\"**/<filename>\" path=\".\"\n")
		}
		return sb.String()
	}
	if isMediaFile(path) {
		mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return fmt.Sprintf("[Media file: %s; media_type=%s; encoding=base64]\n%s", filepath.Base(path), mediaType, base64.StdEncoding.EncodeToString(data))
	}
	content := string(data)
	if off, ok := input["offset"].(float64); ok && off > 0 {
		lines := strings.Split(content, "\n")
		start := int(off)
		if start < len(lines) {
			content = strings.Join(lines[start:], "\n")
		} else {
			content = ""
		}
	}
	if lim, ok := input["limit"].(float64); ok && lim > 0 {
		lines := strings.Split(content, "\n")
		if int(lim) < len(lines) {
			content = strings.Join(lines[:int(lim)], "\n")
		}
	}
	return truncate(content, 8000)
}

func toolWrite(input map[string]interface{}) string {
	path, _ := input["file_path"].(string)
	content, _ := input["content"].(string)
	if path == "" {
		return "ERROR: Write requires 'file_path'"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "" {
		return fmt.Sprintf("ERROR: mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("ERROR: write %s: %v", path, err)
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

func toolEdit(input map[string]interface{}) string {
	path, _ := input["file_path"].(string)
	oldStr, _ := input["old_string"].(string)
	newStr, _ := input["new_string"].(string)
	if path == "" || oldStr == "" {
		return "ERROR: Edit requires 'file_path' and 'old_string'"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("ERROR: read %s: %v", path, err)
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return fmt.Sprintf("ERROR: old_string not found in %s", path)
	}
	if count > 1 {
		return fmt.Sprintf("ERROR: old_string appears %d times in %s; provide more surrounding context", count, path)
	}
	content = strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("ERROR: write %s: %v", path, err)
	}
	return fmt.Sprintf("Edited %s", path)
}

func toolGlob(input map[string]interface{}) string {
	pattern, _ := input["pattern"].(string)
	path, _ := input["path"].(string)
	if pattern == "" {
		pattern = "**/*"
	}
	if path == "" {
		path = "."
	}
	matches, err := doublestar.Glob(os.DirFS(path), pattern, doublestar.WithFilesOnly())
	if err != nil || len(matches) == 0 {
		// Fall back to OS-level listing so the model gets real paths instead of guessing.
		fallback := globFallback(path, pattern)
		if err != nil {
			return fmt.Sprintf("ERROR: glob %s: %v\n%s", pattern, err, fallback)
		}
		return fallback
	}
	if len(matches) > 2000 {
		matches = matches[:2000]
	}
	return strings.Join(matches, "\n")
}

// globFallback uses the OS shell to list files when the doublestar glob finds
// nothing, so the model always sees real filenames and never guesses paths.
func globFallback(path, pattern string) string {
	ctx, cancel := toolContext(30 * time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if os.PathSeparator == '/' {
		// On Linux/Mac, use find with the basename part of the pattern.
		base := filepath.Base(pattern)
		if base == "" || base == "." || base == "*" {
			base = "*"
		}
		cmd = exec.CommandContext(ctx, "find", path, "-name", base, "-type", "f")
	} else {
		// On Windows, use dir /s /b; escape the path properly.
		dirTarget := filepath.Join(path)
		cmd = exec.CommandContext(ctx, "cmd", "/c", "dir", "/s", "/b", dirTarget)
	}

	out, _ := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))

	if result == "" {
		// Last resort: bare listing of the target directory.
		var cmd2 *exec.Cmd
		if os.PathSeparator == '/' {
			cmd2 = exec.CommandContext(ctx, "ls", "-1", path)
		} else {
			cmd2 = exec.CommandContext(ctx, "cmd", "/c", "dir", "/b", path)
		}
		out2, _ := cmd2.CombinedOutput()
		result = strings.TrimSpace(string(out2))
		if result == "" {
			return fmt.Sprintf("No files found in %q matching pattern %q", path, pattern)
		}
		return fmt.Sprintf("[Fallback: dir listing of %s]\n%s", path, truncate(result, 4000))
	}
	return fmt.Sprintf("[Fallback: shell listing]\n%s", truncate(result, 4000))
}

func toolGrep(input map[string]interface{}) string {
	pattern, _ := input["pattern"].(string)
	if pattern == "" {
		return "ERROR: Grep requires 'pattern'"
	}
	path, _ := input["path"].(string)
	if path == "" {
		path = "."
	}
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("ERROR: invalid regex %s: %v", pattern, err)
	}
	var results []string
	walkErr := doublestar.GlobWalk(os.DirFS(path), "**/*", func(p string, d os.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		full := filepath.Join(path, p)
		info, err := os.Stat(full)
		if err != nil || info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil
		}
		if isBinary(data) {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if regex.MatchString(line) {
				results = append(results, fmt.Sprintf("%s:%d:%s", full, i+1, line))
				if len(results) >= 2000 {
					return fs.SkipDir
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Sprintf("ERROR: walk %s: %v", path, walkErr)
	}
	if len(results) == 0 {
		return fmt.Sprintf("No matches found for pattern %q in %q", pattern, path)
	}
	return strings.Join(results, "\n")
}

func toolUnsupported(name string, input map[string]interface{}) string {
	b, _ := json.Marshal(input)
	return fmt.Sprintf("ERROR: tool %q is not supported by claude2api yet. Input: %s", name, truncate(string(b), 500))
}

// --- helpers ---

var mediaExtensions = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".webp": {}, ".gif": {}, ".svg": {}, ".bmp": {}, ".tif": {}, ".tiff": {}, ".ico": {}, ".heic": {}, ".heif": {}, ".avif": {},
	".wav": {}, ".mp3": {}, ".m4a": {}, ".ogg": {}, ".oga": {}, ".flac": {}, ".aac": {}, ".opus": {}, ".wma": {}, ".mid": {}, ".midi": {},
	".mp4": {}, ".webm": {}, ".mov": {}, ".mkv": {}, ".avi": {}, ".m4v": {}, ".3gp": {}, ".wmv": {}, ".flv": {}, ".mpeg": {}, ".mpg": {},
	".pdf": {}, ".doc": {}, ".docx": {}, ".xls": {}, ".xlsx": {}, ".ppt": {}, ".pptx": {}, ".odt": {}, ".ods": {}, ".odp": {}, ".rtf": {},
	".zip": {}, ".rar": {}, ".7z": {}, ".tar": {}, ".gz": {}, ".tgz": {}, ".bz2": {}, ".xz": {}, ".zst": {},
}

func isMediaFile(path string) bool {
	lower := strings.ToLower(path)
	for _, compound := range []string{".tar.gz", ".tar.bz2", ".tar.xz"} {
		if strings.HasSuffix(lower, compound) {
			return true
		}
	}
	_, ok := mediaExtensions[strings.ToLower(filepath.Ext(path))]
	return ok
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n... [truncated %d chars]", len(s)-n)
}

func isBinary(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

// containsFileToolCall reports whether any of the parsed tool calls is a
// file-access tool (Glob, Grep, Read, Write, Edit, Bash). This is used by the
// runToolLoop guard to distinguish genuine file-search attempts from prose
// answers or switch_mode escapes.
func containsFileToolCall(calls []toolCall) bool {
	fileTools := map[string]struct{}{
		"Glob": {}, "Grep": {}, "Read": {}, "Write": {}, "Edit": {}, "Bash": {},
	}
	for _, c := range calls {
		if _, ok := fileTools[c.Name]; ok {
			return true
		}
	}
	return false
}
