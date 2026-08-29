package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
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

// MaxToolRounds caps the tool-use loop so a misbehaving model cannot spin forever.
const MaxToolRounds = 24

// toolCall is a parsed [TOOL_CALL]...[/TOOL_CALL] instruction from the model text.
type toolCall struct {
	Name  string
	ID    string
	Input map[string]interface{}
	Raw   string
}

var toolCallRe = regexp.MustCompile(`(?s)\[TOOL_CALL\]\s*(\{[\s\S]*?\})\s*\[/TOOL_CALL\]`)

// extractToolCalls parses all tool calls embedded in the model's text.
// Returns the text stripped of tool-call markers, and the parsed calls.
func extractToolCalls(text string) (string, []toolCall) {
	var calls []toolCall
	matches := toolCallRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return text, nil
	}
	for _, m := range matches {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(m[1]), &raw); err != nil {
			continue
		}
		name, _ := raw["name"].(string)
		id, _ := raw["id"].(string)
		if name == "" {
			continue
		}
		if id == "" {
			id = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
		}
		input, _ := raw["input"].(map[string]interface{})
		if input == nil {
			// allow flat args
			input = map[string]interface{}{}
			for k, v := range raw {
				if k == "name" || k == "id" {
					continue
				}
				input[k] = v
			}
		}
		calls = append(calls, toolCall{Name: name, ID: id, Input: input, Raw: m[0]})
	}
	stripped := toolCallRe.ReplaceAllString(text, "")
	return stripped, calls
}

// runTool executes a single tool call and returns Anthropic content blocks.
func runTool(call toolCall) []models.AnthropicContentBlock {
	out := executeTool(call.Name, call.Input)
	return []models.AnthropicContentBlock{{
		Type:    "text",
		Text:    out,
		ID:      call.ID,
		Name:    call.Name,
		Content: out,
		IsError: boolPtr(false),
	}}
}

func boolPtr(b bool) *bool { return &b }

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
		return fmt.Sprintf("ERROR: read %s: %v", path, err)
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
	if err != nil {
		return fmt.Sprintf("ERROR: glob %s: %v", pattern, err)
	}
	if len(matches) > 2000 {
		matches = matches[:2000]
	}
	return strings.Join(matches, "\n")
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
