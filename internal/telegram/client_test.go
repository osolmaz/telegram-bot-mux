package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	if _, err := client.ForwardFile(context.Background(), http.MethodGet, "", "", nil); err == nil {
		t.Fatal("empty file path was accepted")
	}
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
}
