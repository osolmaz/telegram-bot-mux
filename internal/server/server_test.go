package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osolmaz/telegram-bot-mux/internal/routing"
	"github.com/osolmaz/telegram-bot-mux/internal/store"
	"github.com/osolmaz/telegram-bot-mux/internal/telegram"
)

const (
	serverTelegramToken = "123456:telegram_token_value_abcdefghijklmnopqrstuvwxyz"
	openclawToken       = "654321:openclaw_client_value_abcdefghijklmnopqrstuvwxyz"
	unyoloToken         = "777777:unyolo_client_token_abcdefghijklmnopqrstuvwxyz"
)

func TestHealthAuthenticationAndManagementMethods(t *testing.T) {
	handler, updateStore, upstream := newTestServer(t)
	defer updateStore.Close()
	defer upstream.Close()
	assertStatus(t, handler, http.MethodGet, "/healthz", nil, http.StatusOK)
	assertStatus(t, handler, http.MethodPost, "/client/openclaw/botwrong/getUpdates", nil, http.StatusUnauthorized)
	assertStatus(t, handler, http.MethodPost, clientBotPath("openclaw", openclawToken, "setWebhook"), nil, http.StatusConflict)
	assertStatus(t, handler, http.MethodPost, clientBotPath("openclaw", openclawToken, "deleteWebhook"), nil, http.StatusOK)

	request := httptest.NewRequest(http.MethodPost, clientBotPath("openclaw", openclawToken, "getWebhookInfo"), nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"pending_update_count":0`) {
		t.Fatalf("getWebhookInfo = %s", response.Body.String())
	}
}

func TestGetUpdatesFansOutAndAcknowledgesIndependently(t *testing.T) {
	handler, updateStore, upstream := newTestServer(t)
	defer updateStore.Close()
	defer upstream.Close()
	update := mustServerUpdate(t, `{"update_id":9,"message":{"text":"hello"}}`)
	if err := updateStore.Ingest(context.Background(), []routing.Update{update}, func(routing.Update) []string { return []string{"openclaw", "unyolo"} }); err != nil {
		t.Fatal(err)
	}
	for client, token := range map[string]string{"openclaw": openclawToken, "unyolo": unyoloToken} {
		request := httptest.NewRequest(http.MethodPost, clientBotPath(client, token, "getUpdates"), strings.NewReader(`{"offset":0,"limit":100,"timeout":0}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"update_id":9`) {
			t.Fatalf("%s getUpdates = %d %s", client, response.Code, response.Body.String())
		}
	}
	ack := httptest.NewRequest(http.MethodGet, clientBotPath("openclaw", openclawToken, "getUpdates")+"?offset=10&timeout=0", nil)
	ackResponse := httptest.NewRecorder()
	handler.ServeHTTP(ackResponse, ack)
	if !strings.Contains(ackResponse.Body.String(), `"result":[]`) {
		t.Fatalf("ack response = %s", ackResponse.Body.String())
	}
	if pending, _ := updateStore.PendingCount(context.Background(), "unyolo"); pending != 1 {
		t.Fatalf("unyolo pending = %d", pending)
	}
}

func TestGetUpdatesLongPollWakesOnCommit(t *testing.T) {
	handler, updateStore, upstream := newTestServer(t)
	defer updateStore.Close()
	defer upstream.Close()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodGet, clientBotPath("openclaw", openclawToken, "getUpdates")+"?timeout=2", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		done <- response
	}()
	time.Sleep(30 * time.Millisecond)
	update := mustServerUpdate(t, `{"update_id":10,"message":{"text":"wake"}}`)
	if err := updateStore.Ingest(context.Background(), []routing.Update{update}, func(routing.Update) []string { return []string{"openclaw"} }); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-done:
		if !strings.Contains(response.Body.String(), `"update_id":10`) {
			t.Fatalf("long poll response = %s", response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("long poll did not wake")
	}
}

func TestBotAndFileProxyPreserveStreamingPayloads(t *testing.T) {
	var botBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/file/") {
			response.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(response, "download")
			return
		}
		botBody, _ = io.ReadAll(request.Body)
		response.Header().Set("X-Telegram", "ok")
		io.WriteString(response, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer upstream.Close()
	updateStore := openServerStore(t)
	defer updateStore.Close()
	client := telegram.New(serverTelegramToken, upstream.URL, upstream.URL, upstream.Client())
	handler := New(updateStore, client, map[string]string{"openclaw": openclawToken, "unyolo": unyoloToken}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", "123"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("document", "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(part, "payload")
	writer.Close()
	request := httptest.NewRequest(http.MethodPost, clientBotPath("openclaw", openclawToken, "sendDocument"), &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("X-Telegram") != "ok" || !bytes.Contains(botBody, []byte("payload")) {
		t.Fatalf("proxy response=%d headers=%v body=%q upstream=%q", response.Code, response.Header(), response.Body.String(), botBody)
	}

	fileRequest := httptest.NewRequest(http.MethodGet, "/client/openclaw/file/bot"+openclawToken+"/documents/test.txt", nil)
	fileResponse := httptest.NewRecorder()
	handler.ServeHTTP(fileResponse, fileRequest)
	if fileResponse.Code != http.StatusOK || fileResponse.Body.String() != "download" {
		t.Fatalf("file response = %d %q", fileResponse.Code, fileResponse.Body.String())
	}
}

func TestGetUpdatesRejectsInvalidRequests(t *testing.T) {
	handler, updateStore, upstream := newTestServer(t)
	defer updateStore.Close()
	defer upstream.Close()
	request := httptest.NewRequest(http.MethodPost, clientBotPath("openclaw", openclawToken, "getUpdates"), strings.NewReader(`{"timeout":99,"unknown":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid request status = %d body=%s", response.Code, response.Body.String())
	}
	for _, query := range []string{"offset=abc", "limit=abc", "timeout=abc"} {
		assertStatus(t, handler, http.MethodGet, clientBotPath("openclaw", openclawToken, "getUpdates")+"?"+query, nil, http.StatusBadRequest)
	}
	assertStatus(t, handler, http.MethodDelete, clientBotPath("openclaw", openclawToken, "sendMessage"), nil, http.StatusMethodNotAllowed)
	assertStatus(t, handler, http.MethodPost, "/client/openclaw/bot"+openclawToken+"/bad%20method", nil, http.StatusBadRequest)
}

func TestLongPollTimeoutCancellationAndProxyFailures(t *testing.T) {
	handler, updateStore, upstream := newTestServer(t)
	defer updateStore.Close()
	request := httptest.NewRequest(http.MethodGet, clientBotPath("openclaw", openclawToken, "getUpdates")+"?timeout=1", nil)
	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, request)
	if time.Since(started) < 900*time.Millisecond || !strings.Contains(response.Body.String(), `"result":[]`) {
		t.Fatalf("timeout response after %s: %s", time.Since(started), response.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelRequest := httptest.NewRequest(http.MethodGet, clientBotPath("openclaw", openclawToken, "getUpdates")+"?timeout=50", nil).WithContext(ctx)
	cancelResponse := httptest.NewRecorder()
	handler.ServeHTTP(cancelResponse, cancelRequest)
	if cancelResponse.Body.Len() != 0 {
		t.Fatalf("canceled response = %q", cancelResponse.Body.String())
	}

	upstream.Close()
	assertStatus(t, handler, http.MethodPost, clientBotPath("openclaw", openclawToken, "sendMessage"), strings.NewReader("x"), http.StatusBadGateway)
	assertStatus(t, handler, http.MethodGet, "/client/openclaw/file/bot"+openclawToken+"/file.txt", nil, http.StatusBadGateway)
	assertStatus(t, handler, http.MethodPost, "/client/openclaw/file/bot"+openclawToken+"/file.txt", nil, http.StatusMethodNotAllowed)
	assertStatus(t, handler, http.MethodGet, "/unknown", nil, http.StatusNotFound)
}

func TestWebhookInfoStoreFailureAndClientAPIBase(t *testing.T) {
	handler, updateStore, upstream := newTestServer(t)
	upstream.Close()
	updateStore.Close()
	assertStatus(t, handler, http.MethodPost, clientBotPath("openclaw", openclawToken, "getWebhookInfo"), nil, http.StatusInternalServerError)
	if got := ClientAPIBase(":8080", "openclaw"); got != "http://127.0.0.1:8080/client/openclaw" {
		t.Fatalf("ClientAPIBase = %q", got)
	}
	if got := ClientAPIBase("localhost:9000", "unyolo"); got != "http://localhost:9000/client/unyolo" {
		t.Fatalf("ClientAPIBase = %q", got)
	}
}

func TestFileHeadAndMalformedPaths(t *testing.T) {
	handler, updateStore, upstream := newTestServer(t)
	defer updateStore.Close()
	defer upstream.Close()
	assertStatus(t, handler, http.MethodHead, "/client/openclaw/file/bot"+openclawToken+"/file.txt", nil, http.StatusOK)
	for _, path := range []string{"/client", "/client/openclaw", "/client/openclaw/file", "/client/openclaw/nope/token/value"} {
		assertStatus(t, handler, http.MethodGet, path, nil, http.StatusNotFound)
	}
}

func newTestServer(t *testing.T) (*Server, *store.Store, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		io.WriteString(response, `{"ok":true,"result":true}`)
	}))
	updateStore := openServerStore(t)
	client := telegram.New(serverTelegramToken, upstream.URL, upstream.URL, upstream.Client())
	handler := New(updateStore, client, map[string]string{"openclaw": openclawToken, "unyolo": unyoloToken}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return handler, updateStore, upstream
}

func openServerStore(t *testing.T) *store.Store {
	t.Helper()
	updateStore, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), []string{"openclaw", "unyolo"}, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	return updateStore
}

func mustServerUpdate(t *testing.T, raw string) routing.Update {
	t.Helper()
	update, err := routing.ParseUpdate(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return update
}

func clientBotPath(client, token, method string) string {
	return "/client/" + client + "/bot" + token + "/" + method
}

func assertStatus(t *testing.T, handler http.Handler, method, path string, body io.Reader, want int) {
	t.Helper()
	request := httptest.NewRequest(method, path, body)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s status = %d, want %d; body=%s", method, path, response.Code, want, response.Body.String())
	}
}
