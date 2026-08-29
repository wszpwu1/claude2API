package tlsclient

import (
	"context"
	"fmt"
	"io"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Client wraps bogdanfinn/tls-client with the Chrome profile and browser headers
// used by claude.ai web requests.
type Client struct {
	httpClient tlsclient.HttpClient
	baseURL    string
}

// New creates a Chrome-146 tls-client wrapper. An optional proxy URL may be
// supplied to keep one account bound to a stable proxy identity.
func New(baseURL string, proxyURL ...string) (*Client, error) {
	jar := tlsclient.NewCookieJar()
	options := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutSeconds(300),
		tlsclient.WithTransportOptions(&tlsclient.TransportOptions{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100,
		}),
		tlsclient.WithClientProfile(profiles.Chrome_146),
		tlsclient.WithCookieJar(jar),
		tlsclient.WithNotFollowRedirects(),
	}
	if len(proxyURL) > 0 && strings.TrimSpace(proxyURL[0]) != "" {
		options = append(options, tlsclient.WithProxyUrl(strings.TrimSpace(proxyURL[0])))
	}

	httpClient, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("create tls-client: %w", err)
	}

	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
	}, nil
}

// BaseURL returns the normalized upstream base URL.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// NewRequest creates an fhttp request against baseURL+path.
func (c *Client) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	c.SetBrowserHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// Do executes the request through tls-client.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	return resp, nil
}

// SetBrowserHeaders applies the common browser-like headers captured from
// claude.ai web requests.
func (c *Client) SetBrowserHeaders(req *http.Request) {
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	req.Header.Set("Origin", c.baseURL)
	req.Header.Set("Referer", c.baseURL+"/")
	req.Header.Set("sec-ch-ua", `"Google Chrome";v="147", "Not.A/Brand";v="8", "Chromium";v="147"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
}
