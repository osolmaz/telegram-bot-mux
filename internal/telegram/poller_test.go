package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/osolmaz/telegram-bot-mux/internal/routing"
)

type pollerStore struct {
	mu      sync.Mutex
	offset  int64
	updates []routing.Update
	cancel  context.CancelFunc
}

func (s *pollerStore) UpstreamOffset(context.Context) (int64, error) { return s.offset, nil }

func (s *pollerStore) Ingest(_ context.Context, updates []routing.Update, _ func(routing.Update) []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, updates...)
	for _, update := range updates {
		s.offset = max(s.offset, update.ID+1)
	}
	if len(updates) > 0 && s.cancel != nil {
		s.cancel()
	}
	return nil
}

func TestPollerStoresUpdatesAndStopsWithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/bot"+testTelegramToken+"/deleteWebhook" {
			io.WriteString(response, `{"ok":true,"result":true}`)
			return
		}
		io.WriteString(response, `{"ok":true,"result":[{"update_id":4,"message":{"text":"hello"}}]}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	store := &pollerStore{offset: 4, cancel: cancel}
	client := New(testTelegramToken, server.URL, server.URL, server.Client())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	poller := NewPoller(client, store, func(routing.Update) []string { return []string{"client"} }, nil, 1, 2, logger)
	err := poller.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if len(store.updates) != 1 || store.offset != 5 {
		t.Fatalf("store = %#v", store)
	}
}

func TestPollerFailsClosedOnAuthenticationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusUnauthorized)
		io.WriteString(response, `{"ok":false,"error_code":401}`)
	}))
	defer server.Close()
	client := New(testTelegramToken, server.URL, server.URL, server.Client())
	poller := NewPoller(client, &pollerStore{}, func(routing.Update) []string { return nil }, nil, 1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := poller.Run(context.Background()); err == nil || !fatalTelegramError(err) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestRetryDelayHonorsTelegramAndMaximum(t *testing.T) {
	if got := retryDelay(&APIError{RetryAfter: 9}, time.Second, 5*time.Second); got != 5*time.Second {
		t.Fatalf("retry delay = %s", got)
	}
	if got := retryDelay(errors.New("network"), 2*time.Second, 5*time.Second); got != 2*time.Second {
		t.Fatalf("fallback delay = %s", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepContext(ctx, time.Hour) {
		t.Fatal("sleepContext ignored cancellation")
	}
}
