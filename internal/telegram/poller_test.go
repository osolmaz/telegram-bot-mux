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
	muxstore "github.com/osolmaz/telegram-bot-mux/internal/store"
)

type pollerStore struct {
	mu           sync.Mutex
	offset       int64
	updates      []routing.Update
	cancel       context.CancelFunc
	offsetErr    error
	ingestErrors []error
}

func (s *pollerStore) UpstreamOffset(context.Context) (int64, error) { return s.offset, s.offsetErr }

func (s *pollerStore) Ingest(_ context.Context, updates []routing.Update, _ func(routing.Update) []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ingestErrors) > 0 {
		err := s.ingestErrors[0]
		s.ingestErrors = s.ingestErrors[1:]
		return err
	}
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

func TestPollerRetriesTransientPollingAndBacklogFailures(t *testing.T) {
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/bot"+testTelegramToken+"/deleteWebhook" {
			io.WriteString(response, `{"ok":true,"result":true}`)
			return
		}
		polls++
		if polls == 1 {
			response.WriteHeader(http.StatusBadGateway)
			io.WriteString(response, `{"ok":false,"error_code":502}`)
			return
		}
		io.WriteString(response, `{"ok":true,"result":[{"update_id":1,"message":{}}]}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	store := &pollerStore{cancel: cancel, ingestErrors: []error{&muxstore.BacklogError{ClientID: "client", Count: 2, Limit: 1}}}
	client := New(testTelegramToken, server.URL, server.URL, server.Client())
	poller := NewPoller(client, store, func(routing.Update) []string { return []string{"client"} }, nil, 1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := poller.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if polls < 3 || len(store.updates) != 1 {
		t.Fatalf("polls=%d updates=%d", polls, len(store.updates))
	}
}

func TestPollerFailsOnStoreErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		io.WriteString(response, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	client := New(testTelegramToken, server.URL, server.URL, server.Client())
	poller := NewPoller(client, &pollerStore{offsetErr: errors.New("broken store")}, func(routing.Update) []string { return nil }, nil, 1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := poller.Run(context.Background()); err == nil || err.Error() != "broken store" {
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
