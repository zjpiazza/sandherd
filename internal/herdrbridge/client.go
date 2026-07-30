package herdrbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
)

type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Retryable bool
}

func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s (request %s)", e.Message, e.RequestID)
	}
	return e.Message
}

type Client struct {
	baseURL   *url.URL
	tokenFile string
	http      *http.Client
}

func NewClient(configuration Config, httpClient *http.Client) (*Client, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	baseURL, _ := url.Parse(configuration.BaseURL)
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: baseURL, tokenFile: configuration.TokenFile, http: httpClient}, nil
}

func (c *Client) BaseURL() string { return strings.TrimSuffix(c.baseURL.String(), "/") }

func (c *Client) Create(ctx context.Context, request lifecycle.CreateRequest) (lifecycle.Agent, error) {
	var result lifecycle.Agent
	err := c.do(ctx, http.MethodPost, "/v1alpha1/agents", request, &result, http.Header{"Idempotency-Key": []string{lifecycle.NewID()}})
	return result, err
}

func (c *Client) List(ctx context.Context) (lifecycle.AgentList, error) {
	var result lifecycle.AgentList
	err := c.do(ctx, http.MethodGet, "/v1alpha1/agents?limit=200", nil, &result, nil)
	return result, err
}

func (c *Client) Get(ctx context.Context, agentID string) (lifecycle.Agent, error) {
	var result lifecycle.Agent
	err := c.do(ctx, http.MethodGet, "/v1alpha1/agents/"+url.PathEscape(agentID), nil, &result, nil)
	return result, err
}

func (c *Client) Stop(ctx context.Context, agentID string) (lifecycle.Agent, error) {
	return c.mutate(ctx, agentID, ":stop", http.MethodPost)
}

func (c *Client) Resume(ctx context.Context, agentID string) (lifecycle.Agent, error) {
	return c.mutate(ctx, agentID, ":resume", http.MethodPost)
}

func (c *Client) Delete(ctx context.Context, agentID string) (lifecycle.Agent, error) {
	return c.mutate(ctx, agentID, "", http.MethodDelete)
}

func (c *Client) mutate(ctx context.Context, agentID, suffix, method string) (lifecycle.Agent, error) {
	var result lifecycle.Agent
	err := c.do(ctx, method, "/v1alpha1/agents/"+url.PathEscape(agentID)+suffix, nil, &result, nil)
	return result, err
}

func (c *Client) TerminalURL(agentID string) string {
	result := *c.baseURL
	if result.Scheme == "https" {
		result.Scheme = "wss"
	} else {
		result.Scheme = "ws"
	}
	result.Path = path.Join(result.Path, "/v1alpha1/agents/"+url.PathEscape(agentID)+"/terminal")
	result.RawQuery = ""
	return result.String()
}

func (c *Client) AuthorizationHeader() (string, error) {
	contents, err := os.ReadFile(filepath.Clean(c.tokenFile))
	if err != nil {
		return "", fmt.Errorf("read Sandherd credential: %w", err)
	}
	token := strings.TrimSpace(string(contents))
	if len(token) < 16 {
		return "", fmt.Errorf("Sandherd credential must contain at least 16 non-whitespace bytes")
	}
	return "Bearer " + token, nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, body, target any, headers http.Header) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	requestURL := *c.baseURL
	requestURL.Path = strings.TrimSuffix(requestURL.Path, "/") + strings.Split(endpoint, "?")[0]
	if queryIndex := strings.IndexByte(endpoint, '?'); queryIndex >= 0 {
		requestURL.RawQuery = endpoint[queryIndex+1:]
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), reader)
	if err != nil {
		return err
	}
	authorization, err := c.AuthorizationHeader()
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", lifecycle.NewID())
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		request.Header[name] = append([]string(nil), values...)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response)
	}
	if target == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4*1024*1024))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode Sandherd response: %w", err)
	}
	return nil
}

func decodeAPIError(response *http.Response) error {
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"requestId"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&envelope); err != nil || envelope.Error.Code == "" {
		return &APIError{Status: response.StatusCode, Code: "http_error", Message: http.StatusText(response.StatusCode)}
	}
	return &APIError{Status: response.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message, RequestID: envelope.Error.RequestID, Retryable: envelope.Error.Retryable}
}

func IsAPIError(err error, code string) bool {
	var target *APIError
	return errors.As(err, &target) && target.Code == code
}
