package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testTelegramToken = "123456:telegram_token_value_abcdefghijklmnopqrstuvwxyz"

func TestClientLifecycleAndUpdates(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.URL.Path)
		response.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/deleteWebhook"):
			io.WriteString(response, `{"ok":true,"result":true}`)
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			io.WriteString(response, `{"ok":true,"result":{"id":123}}`)
		case strings.HasSuffix(request.URL.Path, "/getUpdates"):
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["offset"] != float64(7) || payload["timeout"] != float64(2) {
				t.Errorf("getUpdates payload = %#v", payload)
			}
			io.WriteString(response, `{"ok":true,"result":[{"update_id":7,"message":{"text":"hello"}}]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := New(testTelegramToken, server.URL, server.URL, server.Client())
	if err := client.DeleteWebhook(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.GetMe(context.Background()); err != nil {
		t.Fatal(err)
	}
	updates, err := client.GetUpdates(context.Background(), 7, 2, []string{"message"})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].ID != 7 || updates[0].Type != "message" {
		t.Fatalf("updates = %#v", updates)
	}
	if len(methods) != 3 {
		t.Fatalf("methods = %v", methods)
	}
}

func TestClientRejectsTelegramErrorsAndInvalidResults(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		want string
	}{
		{"api error", `{"ok":false,"error_code":429,"parameters":{"retry_after":9}}`, http.StatusTooManyRequests, "error code 429"},
		{"invalid json", `{`, http.StatusOK, "invalid JSON"},
		{"invalid update", `{"ok":true,"result":[{"update_id":-1,"message":{}}]}`, http.StatusOK, "invalid update"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(test.code)
				io.WriteString(response, test.body)
			}))
			defer server.Close()
			client := New(testTelegramToken, server.URL, server.URL, server.Client())
			_, err := client.GetUpdates(context.Background(), 0, 1, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
			if strings.Contains(err.Error(), testTelegramToken) {
				t.Fatal("error leaked Telegram token")
			}
		})
	}
}

func TestClientForwardsBotAndFileRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/file/") {
			if request.Method != http.MethodGet {
				t.Errorf("file method = %s", request.Method)
			}
			response.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(response, "file-data")
			return
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != "payload" || request.URL.RawQuery != "x=1" {
			t.Errorf("proxy request body=%q query=%q", body, request.URL.RawQuery)
		}
		response.Header().Set("X-Upstream", "yes")
		io.WriteString(response, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	client := New(testTelegramToken, server.URL, server.URL, server.Client())
	response, err := client.ForwardBot(context.Background(), http.MethodPost, "sendMessage", "x=1", http.Header{"Content-Type": {"text/plain"}}, bytes.NewBufferString("payload"), 7)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != `{"ok":true,"result":true}` || response.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("proxy response = %q headers=%v", body, response.Header)
	}
	fileResponse, err := client.ForwardFile(context.Background(), http.MethodGet, "documents/file.bin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fileBody, _ := io.ReadAll(fileResponse.Body)
	fileResponse.Body.Close()
	if string(fileBody) != "file-data" {
		t.Fatalf("file response = %q", fileBody)
	}
	for _, invalid := range []string{"", "../secret", "dir/./file", "dir//file"} {
		if _, err := client.ForwardFile(context.Background(), http.MethodGet, invalid, "", nil); err == nil {
			t.Fatalf("invalid file path %q was accepted", invalid)
		}
	}
}

func TestGetMeAndResponseShapeFailures(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{`{"ok":true,"result":{"id":0}}`, "invalid bot identity"},
		{`{"ok":true}`, "missing result"},
		{`{"ok":true,"result":"wrong"}`, "invalid shape"},
		{`{"ok":false}`, "HTTP 200"},
	}
	for _, test := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			io.WriteString(response, test.body)
		}))
		client := New(testTelegramToken, server.URL, server.URL, server.Client())
		err := client.GetMe(context.Background())
		server.Close()
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("GetMe error = %v, want %q", err, test.want)
		}
	}
	if got := (&APIError{StatusCode: 500}).Error(); got != "Telegram API returned HTTP 500" {
		t.Fatalf("APIError = %q", got)
	}
}

func TestProxyRejectsRedirectsAndStripsHopHeaders(t *testing.T) {
	var upstreamHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		upstreamHeader = request.Header.Clone()
		if request.URL.Path == "/redirect" {
			http.Redirect(response, request, serverURL(request)+"/final", http.StatusFound)
			return
		}
		io.WriteString(response, `{}`)
	}))
	defer server.Close()
	client := New(testTelegramToken, server.URL, server.URL, nil)
	response, err := client.ForwardBot(context.Background(), http.MethodPost, "sendMessage", "", http.Header{"Connection": {"close"}, "X-Test": {"yes"}}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if upstreamHeader.Get("Connection") != "" || upstreamHeader.Get("X-Test") != "yes" {
		t.Fatalf("forwarded headers = %v", upstreamHeader)
	}
	if got := client.redact(nil); got != nil {
		t.Fatalf("redact(nil) = %v", got)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, serverURL(request)+"/final", http.StatusFound)
	}))
	defer redirect.Close()
	redirectClient := New(testTelegramToken, redirect.URL, redirect.URL, nil)
	if _, err := redirectClient.ForwardBot(context.Background(), http.MethodPost, "sendMessage", "", nil, nil, 0); err == nil || !strings.Contains(err.Error(), "redirect rejected") {
		t.Fatalf("redirect error = %v", err)
	}
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}

func TestRedactsNetworkErrors(t *testing.T) {
	client := New(testTelegramToken, "http://127.0.0.1:1", "http://127.0.0.1:1", nil)
	_, err := client.GetUpdates(context.Background(), 0, 1, nil)
	if err == nil {
		t.Fatal("GetUpdates succeeded")
	}
	if strings.Contains(err.Error(), testTelegramToken) {
		t.Fatal("network error leaked Telegram token")
	}
	reserved := "123456:secret/with%reserved"
	redacted := New(reserved, "http://127.0.0.1:1", "http://127.0.0.1:1", nil).redact(errors.New(url.PathEscape(reserved)))
	if strings.Contains(redacted.Error(), reserved) || strings.Contains(redacted.Error(), url.PathEscape(reserved)) {
		t.Fatal("escaped Telegram token was not redacted")
	}
}
