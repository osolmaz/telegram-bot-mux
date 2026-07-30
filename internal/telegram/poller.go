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

func (p *Poller) Run(ctx context.Context) error {
	if err := p.client.DeleteWebhook(ctx); err != nil {
		return err
	}
	offset, err := p.store.UpstreamOffset(ctx)
	if err != nil {
		return err
	}
	p.logger.Info("Telegram polling started", "offset", offset)
	delay := time.Second
	for ctx.Err() == nil {
		updates, pollErr := p.client.GetUpdates(ctx, offset, p.timeoutSeconds, p.allowedUpdates)
		if pollErr != nil {
			if fatalTelegramError(pollErr) {
				return pollErr
			}
			p.logger.Warn("Telegram polling failed; retrying", "error", pollErr, "delay", delay)
			if !sleepContext(ctx, retryDelay(pollErr, delay, p.maxRetry)) {
				break
			}
			delay = min(delay*2, p.maxRetry)
			continue
		}
		if err := p.store.Ingest(ctx, updates, p.targets); err != nil {
			if !errors.Is(err, store.ErrBacklogFull) {
				return err
			}
			p.logger.Warn("downstream backlog is full; upstream polling paused", "error", err, "delay", delay)
			if !sleepContext(ctx, delay) {
				break
			}
			delay = min(delay*2, p.maxRetry)
			continue
		}
		for _, update := range updates {
			if update.ID+1 > offset {
				offset = update.ID + 1
			}
		}
		if len(updates) > 0 {
			p.logger.Debug("stored Telegram updates", "count", len(updates), "next_offset", offset)
		}
		delay = time.Second
	}
	return ctx.Err()
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
