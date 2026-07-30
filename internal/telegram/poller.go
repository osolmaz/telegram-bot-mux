package telegram

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/osolmaz/telegram-bot-mux/internal/routing"
	"github.com/osolmaz/telegram-bot-mux/internal/store"
)

type UpdateStore interface {
	UpstreamOffset(context.Context) (int64, error)
	Ingest(context.Context, []routing.Update, func(routing.Update) []string) error
}

type Poller struct {
	client         *Client
	store          UpdateStore
	targets        func(routing.Update) []string
	allowedUpdates []string
	timeoutSeconds int
	maxRetry       time.Duration
	logger         *slog.Logger
}

func NewPoller(client *Client, updateStore UpdateStore, targets func(routing.Update) []string, allowedUpdates []string, timeoutSeconds, maxRetrySeconds int, logger *slog.Logger) *Poller {
	return &Poller{
		client:         client,
		store:          updateStore,
		targets:        targets,
		allowedUpdates: append([]string(nil), allowedUpdates...),
		timeoutSeconds: timeoutSeconds,
		maxRetry:       time.Duration(maxRetrySeconds) * time.Second,
		logger:         logger,
	}
}

type pollState struct {
	offset int64
	delay  time.Duration
}

func (p *Poller) Run(ctx context.Context) error {
	state, err := p.initialize(ctx)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		if err := p.step(ctx, state); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (p *Poller) initialize(ctx context.Context) (*pollState, error) {
	if err := p.client.DeleteWebhook(ctx); err != nil {
		return nil, err
	}
	offset, err := p.store.UpstreamOffset(ctx)
	if err != nil {
		return nil, err
	}
	p.logger.Info("Telegram polling started", "offset", offset)
	return &pollState{offset: offset, delay: time.Second}, nil
}

func (p *Poller) step(ctx context.Context, state *pollState) error {
	updates, err := p.client.GetUpdates(ctx, state.offset, p.timeoutSeconds, p.allowedUpdates)
	if err != nil {
		return p.handlePollFailure(ctx, state, err)
	}
	if err := p.store.Ingest(ctx, updates, p.targets); err != nil {
		return p.handleIngestFailure(ctx, state, err)
	}
	state.offset = nextOffset(state.offset, updates)
	if len(updates) > 0 {
		p.logger.Debug("stored Telegram updates", "count", len(updates), "next_offset", state.offset)
	}
	state.delay = time.Second
	return nil
}

func (p *Poller) handlePollFailure(ctx context.Context, state *pollState, err error) error {
	if fatalTelegramError(err) {
		return err
	}
	p.logger.Warn("Telegram polling failed; retrying", "error", err, "delay", state.delay)
	if !sleepContext(ctx, retryDelay(err, state.delay, p.maxRetry)) {
		return ctx.Err()
	}
	state.delay = min(state.delay*2, p.maxRetry)
	return nil
}

func (p *Poller) handleIngestFailure(ctx context.Context, state *pollState, err error) error {
	if !errors.Is(err, store.ErrBacklogFull) {
		return err
	}
	p.logger.Warn("downstream backlog is full; upstream polling paused", "error", err, "delay", state.delay)
	if !sleepContext(ctx, state.delay) {
		return ctx.Err()
	}
	state.delay = min(state.delay*2, p.maxRetry)
	return nil
}

func nextOffset(current int64, updates []routing.Update) int64 {
	for _, update := range updates {
		current = max(current, update.ID+1)
	}
	return current
}

func fatalTelegramError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == 401 || apiErr.StatusCode == 403
}

func retryDelay(err error, fallback, maximum time.Duration) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return min(time.Duration(apiErr.RetryAfter)*time.Second, maximum)
	}
	return min(fallback, maximum)
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
