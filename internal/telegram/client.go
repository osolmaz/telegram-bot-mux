package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/osolmaz/telegram-bot-mux/internal/routing"
)

const maxTelegramResponseBytes = 16 << 20

type Client struct {
	token    string
	apiBase  string
	fileBase string
	http     *http.Client
}

type APIError struct {
	StatusCode int
	ErrorCode  int
	RetryAfter int
}

func (e *APIError) Error() string {
	if e.ErrorCode != 0 {
		return fmt.Sprintf("Telegram API returned HTTP %d with error code %d", e.StatusCode, e.ErrorCode)
	}
	return fmt.Sprintf("Telegram API returned HTTP %d", e.StatusCode)
}

func New(token, apiBase, fileBase string, client *http.Client) *Client {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 32
		transport.MaxIdleConnsPerHost = 16
		transport.IdleConnTimeout = 90 * time.Second
		client = &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("Telegram API redirect rejected")
			},
		}
	}
	return &Client{token: token, apiBase: strings.TrimRight(apiBase, "/"), fileBase: strings.TrimRight(fileBase, "/"), http: client}
}

func (c *Client) DeleteWebhook(ctx context.Context) error {
	var result bool
	return c.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, &result, 15*time.Second)
}

func (c *Client) GetMe(ctx context.Context) error {
	var result struct {
		ID int64 `json:"id"`
	}
	if err := c.call(ctx, "getMe", map[string]any{}, &result, 15*time.Second); err != nil {
		return err
	}
	if result.ID <= 0 {
		return errors.New("Telegram getMe returned an invalid bot identity")
	}
	return nil
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSeconds int, allowedUpdates []string) ([]routing.Update, error) {
	payload := map[string]any{
		"offset":  offset,
		"limit":   100,
		"timeout": timeoutSeconds,
	}
	if len(allowedUpdates) > 0 {
		payload["allowed_updates"] = allowedUpdates
	}
	var raw []json.RawMessage
	if err := c.call(ctx, "getUpdates", payload, &raw, time.Duration(timeoutSeconds+10)*time.Second); err != nil {
		return nil, err
	}
	updates := make([]routing.Update, 0, len(raw))
	for _, item := range raw {
		update, err := routing.ParseUpdate(item)
		if err != nil {
			return nil, fmt.Errorf("Telegram returned an invalid update: %w", err)
		}
		updates = append(updates, update)
	}
	return updates, nil
}

func (c *Client) call(ctx context.Context, method string, payload any, result any, timeout time.Duration) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Telegram request: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.botURL(method), bytes.NewReader(encoded))
	if err != nil {
		return errors.New("build Telegram request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("Telegram request failed: %w", c.redact(err))
	}
	defer response.Body.Close()
	return decodeResponse(response, result)
}

func decodeResponse(response *http.Response, result any) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTelegramResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Telegram response: %w", err)
	}
	if len(body) > maxTelegramResponseBytes {
		return errors.New("Telegram response exceeds size limit")
	}
	var envelope struct {
		OK         bool            `json:"ok"`
		Result     json.RawMessage `json:"result"`
		ErrorCode  int             `json:"error_code"`
		Parameters json.RawMessage `json:"parameters"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		return errors.New("Telegram returned invalid JSON")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.OK {
		apiErr := &APIError{StatusCode: response.StatusCode, ErrorCode: envelope.ErrorCode}
		var parameters struct {
			RetryAfter int `json:"retry_after"`
		}
		if len(envelope.Parameters) > 0 && json.Unmarshal(envelope.Parameters, &parameters) == nil {
			apiErr.RetryAfter = parameters.RetryAfter
		}
		return apiErr
	}
	if result == nil {
		return nil
	}
	if len(envelope.Result) == 0 {
		return errors.New("Telegram response is missing result")
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return errors.New("Telegram result has an invalid shape")
	}
	return nil
}

func (c *Client) ForwardBot(ctx context.Context, requestMethod, apiMethod, rawQuery string, headers http.Header, body io.Reader, contentLength int64) (*http.Response, error) {
	return c.forward(ctx, requestMethod, c.botURL(apiMethod), rawQuery, headers, body, contentLength)
}

func (c *Client) ForwardFile(ctx context.Context, requestMethod, filePath, rawQuery string, headers http.Header) (*http.Response, error) {
	cleaned := path.Clean("/" + filePath)
	if cleaned == "/" || strings.Contains(filePath, "\x00") || strings.HasPrefix(cleaned, "/../") {
		return nil, errors.New("invalid Telegram file path")
	}
	endpoint := c.fileBase + "/file/bot" + url.PathEscape(c.token) + cleaned
	return c.forward(ctx, requestMethod, endpoint, rawQuery, headers, nil, 0)
}

func (c *Client) forward(ctx context.Context, requestMethod, endpoint, rawQuery string, headers http.Header, body io.Reader, contentLength int64) (*http.Response, error) {
	if rawQuery != "" {
		endpoint += "?" + rawQuery
	}
	request, err := http.NewRequestWithContext(ctx, requestMethod, endpoint, body)
	if err != nil {
		return nil, errors.New("build Telegram proxy request failed")
	}
	request.Header = cloneEndToEndHeaders(headers)
	request.ContentLength = contentLength
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Telegram proxy request failed: %w", c.redact(err))
	}
	return response, nil
}

func cloneEndToEndHeaders(source http.Header) http.Header {
	target := make(http.Header, len(source))
	for key, values := range source {
		if isHopByHop(key) || strings.EqualFold(key, "Host") {
			continue
		}
		target[key] = append([]string(nil), values...)
	}
	return target
}

func isHopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func (c *Client) botURL(method string) string {
	return c.apiBase + "/bot" + url.PathEscape(c.token) + "/" + method
}

func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "[redacted]"))
}
