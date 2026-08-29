package claude

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"claude2api/models"
	browserclient "claude2api/tlsclient"
	"claude2api/utils"

	http "github.com/bogdanfinn/fhttp"
)

// Client is the reverse-engineered claude.ai API client with TLS fingerprint bypass
type Client struct {
	httpClient        *browserclient.Client
	baseURL           string
	sessionKey        string
	claudeCookie      string
	orgMu             sync.Mutex
	orgID             string // cached org UUID
	deviceID          string // anthropic-device-id
	sessionKeyLC      string
	activitySessionID string
	anonymousID       string
	ssid              string
	ddSessionID       string
	ddAppID           string
	fbp               string
	gclAU             string
	ionVK             string
	createdAtMS       int64
}

// NewClient creates a new claude.ai API client using Chrome 146 TLS fingerprint.
// Optional arguments are the full browser cookie followed by a per-account proxy URL.
func NewClient(baseURL, sessionKey string, options ...string) (*Client, error) {
	cookie := ""
	proxyURL := ""
	if len(options) > 0 {
		cookie = options[0]
	}
	if len(options) > 1 {
		proxyURL = options[1]
	}
	deviceID := cookieValue(cookie, "anthropic-device-id")
	if deviceID == "" {
		deviceID = utils.GenerateUUID()
	}
	createdAtMS := time.Now().UnixMilli()
	sessionKeyLC := cookieValue(cookie, "sessionKeyLC")
	if sessionKeyLC == "" {
		sessionKeyLC = fmt.Sprintf("%d", createdAtMS)
	}
	activitySessionID := cookieValue(cookie, "activitySessionId")
	if activitySessionID == "" {
		activitySessionID = utils.GenerateUUID()
	}
	anonymousID := cookieValue(cookie, "ajs_anonymous_id")
	if anonymousID == "" {
		anonymousID = "claudeai.v1." + utils.GenerateUUID()
	}
	ssid := cookieValue(cookie, "__ssid")
	if ssid == "" {
		ssid = utils.GenerateUUID()
	}
	ddSessionID := ddSessionIDFromCookie(cookie)
	if ddSessionID == "" {
		ddSessionID = utils.GenerateUUID()
	}
	ddAppID := utils.GenerateUUID()
	fbp := cookieValue(cookie, "_fbp")
	if fbp == "" {
		fbp = fmt.Sprintf("fb.1.%d.%s", createdAtMS, randomDigits(17))
	}
	gclAU := cookieValue(cookie, "_gcl_au")
	if gclAU == "" {
		gclAU = fmt.Sprintf("1.1.%s.%d", randomDigits(10), createdAtMS/1000)
	}
	ionVK := cookieValue(cookie, "ion-vk")
	if ionVK == "" {
		ionVK = utils.GenerateUUID()
	}

	httpClient, err := browserclient.New(baseURL, proxyURL)
	if err != nil {
		return nil, err
	}

	return &Client{
		httpClient:        httpClient,
		baseURL:           httpClient.BaseURL(),
		sessionKey:        sessionKey,
		claudeCookie:      cookie,
		orgID:             cookieValue(cookie, "lastActiveOrg"),
		deviceID:          deviceID,
		sessionKeyLC:      sessionKeyLC,
		activitySessionID: activitySessionID,
		anonymousID:       anonymousID,
		ssid:              ssid,
		ddSessionID:       ddSessionID,
		ddAppID:           ddAppID,
		fbp:               fbp,
		gclAU:             gclAU,
		ionVK:             ionVK,
		createdAtMS:       createdAtMS,
	}, nil
}

func cookieValue(cookie, name string) string {
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && kv[0] == name {
			return kv[1]
		}
	}
	return ""
}

func ddSessionIDFromCookie(cookie string) string {
	dd := cookieValue(cookie, "_dd_s")
	for _, part := range strings.Split(dd, "&") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && kv[0] == "id" {
			return kv[1]
		}
	}
	return ""
}

func (c *Client) generatedDDSCookie() string {
	expiresAtMS := time.Now().Add(15 * time.Minute).UnixMilli()
	return fmt.Sprintf("aid=%s&rum=2&id=%s&created=%d&expire=%d", c.ddAppID, c.ddSessionID, c.createdAtMS, expiresAtMS)
}

func (c *Client) addDatadogHeaders(req *http.Request) {
	traceID := randomUint63()
	parentID := randomUint63()
	traceHigh := randomUint63()
	traceHex := fmt.Sprintf("%016x%016x", traceHigh, traceID)
	parentHex := fmt.Sprintf("%016x", parentID)
	req.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-01", traceHex, parentHex))
	req.Header.Set("tracestate", "dd=s:1;o:rum")
	req.Header.Set("x-datadog-origin", "rum")
	req.Header.Set("x-datadog-parent-id", fmt.Sprintf("%d", parentID))
	req.Header.Set("x-datadog-sampling-priority", "1")
	req.Header.Set("x-datadog-trace-id", fmt.Sprintf("%d", traceID))
}

func randomUint63() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano()) & ((1 << 63) - 1)
	}
	return binary.BigEndian.Uint64(b[:]) & ((1 << 63) - 1)
}

func randomDigits(n int) string {
	const digits = "0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		fallback := fmt.Sprintf("%d", time.Now().UnixNano())
		if len(fallback) >= n {
			return fallback[:n]
		}
		return fallback
	}
	for i := range b {
		b[i] = digits[int(b[i])%len(digits)]
	}
	return string(b)
}

// doRequest performs an authenticated HTTP request to claude.ai
func (c *Client) doRequest(ctx context.Context, method, path string, body io.Reader, refererPath ...string) (*http.Response, error) {
	req, err := c.httpClient.NewRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}

	if len(refererPath) > 0 && refererPath[0] != "" {
		req.Header.Set("Referer", c.baseURL+refererPath[0])
	}

	// Claude.ai specific headers
	req.Header.Set("anthropic-client-platform", "web_claude_ai")
	req.Header.Set("anthropic-device-id", c.deviceID)
	c.addDatadogHeaders(req)

	// Authentication: prefer the full browser Cookie header when available.
	if c.claudeCookie != "" {
		req.Header.Set("Cookie", c.claudeCookie)
	} else {
		req.AddCookie(&http.Cookie{Name: "sessionKey", Value: c.sessionKey})
		req.AddCookie(&http.Cookie{Name: "sessionKeyLC", Value: c.sessionKeyLC})
		req.AddCookie(&http.Cookie{Name: "anthropic-device-id", Value: c.deviceID})
		req.AddCookie(&http.Cookie{Name: "activitySessionId", Value: c.activitySessionID})
		req.AddCookie(&http.Cookie{Name: "ajs_anonymous_id", Value: c.anonymousID})
		req.AddCookie(&http.Cookie{Name: "__ssid", Value: c.ssid})
		req.AddCookie(&http.Cookie{Name: "CH-prefers-color-scheme", Value: "light"})
		req.AddCookie(&http.Cookie{Name: "user-sidebar-visible-on-load", Value: "true"})
		req.AddCookie(&http.Cookie{Name: "user-sidebar-pinned", Value: "true"})
		req.AddCookie(&http.Cookie{Name: "_fbp", Value: c.fbp})
		req.AddCookie(&http.Cookie{Name: "_gcl_au", Value: c.gclAU})
		req.AddCookie(&http.Cookie{Name: "ion-vk", Value: c.ionVK})
		req.AddCookie(&http.Cookie{Name: "_dd_s", Value: c.generatedDDSCookie()})
	}

	return c.httpClient.Do(req)
}

// GetOrganization returns the cached organization UUID when available and
// otherwise fetches it from claude.ai.
func (c *Client) GetOrganization(ctx context.Context) (string, error) {
	return c.getOrganization(ctx, false)
}

// ValidateOrganization always contacts claude.ai instead of trusting the
// organization UUID cached from cookies or a previous request. Health checks
// must use this method so expired, blocked, or banned accounts are detected.
func (c *Client) ValidateOrganization(ctx context.Context) (string, error) {
	return c.getOrganization(ctx, true)
}

func (c *Client) getOrganization(ctx context.Context, forceRefresh bool) (string, error) {
	c.orgMu.Lock()
	defer c.orgMu.Unlock()
	if !forceRefresh && c.orgID != "" {
		return c.orgID, nil
	}

	resp, err := c.doRequest(ctx, "GET", "/api/organizations", nil, "/new")
	if err != nil {
		return "", fmt.Errorf("get organizations: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get organizations: status %d: %s", resp.StatusCode, string(body))
	}

	var orgs models.ClaudeOrganizationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&orgs); err != nil {
		return "", fmt.Errorf("decode organizations: %w", err)
	}

	if len(orgs) == 0 {
		return "", fmt.Errorf("no organizations found")
	}

	c.orgID = orgs[0].UUID
	return c.orgID, nil
}

// CreateConversation creates a new conversation and returns its UUID
func (c *Client) CreateConversation(ctx context.Context, title string) (string, error) {
	orgID, err := c.GetOrganization(ctx)
	if err != nil {
		return "", err
	}

	reqBody := models.ClaudeCreateConversationRequest{
		Name:             title,
		OrganizationUUID: orgID,
	}
	body, _ := utils.JSONToReader(reqBody)

	resp, err := c.doRequest(ctx, "POST",
		fmt.Sprintf("/api/organizations/%s/chat_conversations", orgID), body, "/new")
	if err != nil {
		return "", fmt.Errorf("create conversation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create conversation: status %d: %s", resp.StatusCode, string(respBody))
	}

	var conv models.ClaudeConversation
	if err := json.NewDecoder(resp.Body).Decode(&conv); err != nil {
		return "", fmt.Errorf("decode conversation: %w", err)
	}

	return conv.UUID, nil
}

// DeleteConversation deletes a conversation by UUID
func (c *Client) DeleteConversation(ctx context.Context, convID string) error {
	orgID, err := c.GetOrganization(ctx)
	if err != nil {
		return err
	}

	resp, err := c.doRequest(ctx, "DELETE",
		fmt.Sprintf("/api/organizations/%s/chat_conversations/%s", orgID, convID), nil, "/chat/"+convID)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete conversation: status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// SendMessage sends a message and returns a channel of SSE completion events.
func (c *Client) SendMessage(ctx context.Context, convID string, req *models.ClaudeCompletionRequest) (<-chan models.ClaudeCompletionEvent, error) {
	orgID, err := c.GetOrganization(ctx)
	if err != nil {
		return nil, err
	}

	body, _ := utils.JSONToReader(req)

	path := fmt.Sprintf("/api/organizations/%s/chat_conversations/%s/completion",
		orgID, convID)

	resp, err := c.doRequest(ctx, "POST", path, body, "/chat/"+convID)
	if err != nil {
		return nil, fmt.Errorf("send message: %w", err)
	}

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("send message: status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse SSE stream in background
	events := make(chan models.ClaudeCompletionEvent, 64)
	go func() {
		defer close(events)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)

		var eventType, data string
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()

			if line == "" {
				if data != "" {
					select {
					case events <- parseCompletionEvent(eventType, data):
					case <-ctx.Done():
						return
					}
				}
				eventType = ""
				data = ""
				continue
			}

			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				if data != "" {
					data += "\n"
				}
				data += payload
			}
		}

		if data != "" {
			select {
			case events <- parseCompletionEvent(eventType, data):
			case <-ctx.Done():
				return
			}
		}
	}()

	return events, nil
}

func parseCompletionEvent(eventType, data string) models.ClaudeCompletionEvent {
	var evt models.ClaudeCompletionEvent
	evt.Type = eventType
	evt.Data = data
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		return evt
	}
	if len(evt.Delta) > 0 {
		switch evt.Type {
		case "content_block_delta":
			var delta models.ClaudeCompletionDelta
			if err := json.Unmarshal(evt.Delta, &delta); err == nil {
				evt.TextDelta = &delta
			}
		case "message_delta":
			var delta models.MessageDeltaPayload
			if err := json.Unmarshal(evt.Delta, &delta); err == nil {
				evt.MessageDelta = &delta
			}
		}
	}
	return evt
}

// BuildPrompt converts OpenAI messages into a single prompt string for claude.ai
func BuildPrompt(messages []models.Message) string {
	var parts []string
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			parts = append(parts, "[System]\n"+msg.Content)
		case "user":
			parts = append(parts, "[Human]\n"+msg.Content)
		case "assistant":
			parts = append(parts, "[Assistant]\n"+msg.Content)
		default:
			parts = append(parts, msg.Content)
		}
	}
	parts = append(parts, "[Assistant]\n")
	return strings.Join(parts, "\n\n")
}

// ExtractTextFromSSE extracts text from a claude.ai SSE event
func ExtractTextFromSSE(evt models.ClaudeCompletionEvent) string {
	// content_block_delta with text_delta
	if evt.TextDelta != nil && evt.TextDelta.Text != "" {
		return evt.TextDelta.Text
	}
	return ""
}

// ExtractThinkingFromSSE extracts thinking text from a claude.ai SSE event
func ExtractThinkingFromSSE(evt models.ClaudeCompletionEvent) string {
	if evt.TextDelta != nil && evt.TextDelta.Type == "thinking_delta" {
		return evt.TextDelta.Thinking
	}
	return ""
}

// IsStopEvent returns true if the event signals end of stream
func IsStopEvent(evt models.ClaudeCompletionEvent) bool {
	return evt.Type == "message_stop" || evt.Type == "message_delta" && evt.MessageDelta != nil && evt.MessageDelta.StopReason != ""
}
