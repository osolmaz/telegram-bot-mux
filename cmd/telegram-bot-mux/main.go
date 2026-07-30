package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/osolmaz/telegram-bot-mux/internal/config"
	"github.com/osolmaz/telegram-bot-mux/internal/routing"
	muxserver "github.com/osolmaz/telegram-bot-mux/internal/server"
	"github.com/osolmaz/telegram-bot-mux/internal/store"
	"github.com/osolmaz/telegram-bot-mux/internal/telegram"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "telegram-bot-mux:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: telegram-bot-mux <serve|doctor|backup|generate-client-token|version>")
	}
	switch args[0] {
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		flags.SetOutput(stderr)
		configPath := flags.String("config", "", "absolute path to config.json")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *configPath == "" {
			return errors.New("serve requires --config")
		}
		return serve(*configPath, stderr)
	case "doctor":
		flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
		flags.SetOutput(stderr)
		configPath := flags.String("config", "", "absolute path to config.json")
		offline := flags.Bool("offline", false, "skip Telegram getMe")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *configPath == "" {
			return errors.New("doctor requires --config")
		}
		return doctor(*configPath, *offline, stdout)
	case "backup":
		flags := flag.NewFlagSet("backup", flag.ContinueOnError)
		flags.SetOutput(stderr)
		configPath := flags.String("config", "", "absolute path to config.json")
		outputPath := flags.String("out", "", "absolute output database path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *configPath == "" || *outputPath == "" {
			return errors.New("backup requires --config and --out")
		}
		return backup(*configPath, *outputPath)
	case "generate-client-token":
		flags := flag.NewFlagSet("generate-client-token", flag.ContinueOnError)
		flags.SetOutput(stderr)
		outputPath := flags.String("out", "", "absolute output secret path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *outputPath == "" {
			return errors.New("generate-client-token requires --out")
		}
		return generateClientToken(*outputPath, stdout)
	case "version":
		_, err := fmt.Fprintln(stdout, version)
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(configPath string, logOutput io.Writer) error {
	cfg, credentials, updateStore, telegramClient, err := loadRuntime(configPath)
	if err != nil {
		return err
	}
	defer updateStore.Close()
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))
	router := routing.New(cfg)
	poller := telegram.NewPoller(telegramClient, updateStore, router.Targets, cfg.Telegram.AllowedUpdates, cfg.Telegram.PollTimeoutSeconds, cfg.Telegram.MaxRetrySeconds, logger)
	handler := muxserver.New(updateStore, telegramClient, credentials.ClientTokens, logger)
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- poller.Run(ctx) }()
	go func() { errorsChannel <- httpServer.Serve(listener) }()
	logger.Info("Telegram Bot Mux listening", "address", listener.Addr().String(), "clients", len(cfg.Clients))
	firstErr := <-errorsChannel
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	if firstErr != nil && !errors.Is(firstErr, context.Canceled) && !errors.Is(firstErr, http.ErrServerClosed) {
		return firstErr
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown HTTP server: %w", shutdownErr)
	}
	return nil
}

func loadRuntime(configPath string) (config.Config, config.Credentials, *store.Store, *telegram.Client, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, config.Credentials{}, nil, nil, err
	}
	credentials, err := config.LoadCredentials(cfg)
	if err != nil {
		return config.Config{}, config.Credentials{}, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	updateStore, err := store.Open(ctx, cfg.Database, config.ClientIDs(cfg), cfg.Retention.MaxPendingPerClient, cfg.Retention.AcknowledgedSafetyWindow)
	if err != nil {
		return config.Config{}, config.Credentials{}, nil, nil, err
	}
	client := telegram.New(credentials.TelegramToken, cfg.Telegram.APIBase, cfg.Telegram.FileBase, nil)
	return cfg, credentials, updateStore, client, nil
}

func doctor(configPath string, offline bool, output io.Writer) error {
	cfg, _, updateStore, telegramClient, err := loadRuntime(configPath)
	if err != nil {
		return err
	}
	defer updateStore.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := updateStore.IntegrityCheck(ctx); err != nil {
		return err
	}
	if !offline {
		if err := telegramClient.GetMe(ctx); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(output, "config: ok\ndatabase: ok\ntelegram: %s\nclients: %d\n", map[bool]string{true: "skipped", false: "ok"}[offline], len(cfg.Clients))
	return err
}

func backup(configPath, outputPath string) error {
	_, _, updateStore, _, err := loadRuntime(configPath)
	if err != nil {
		return err
	}
	defer updateStore.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := updateStore.IntegrityCheck(ctx); err != nil {
		return err
	}
	return updateStore.Backup(ctx, outputPath)
}

func generateClientToken(outputPath string, output io.Writer) error {
	if !filepath.IsAbs(outputPath) {
		return errors.New("token output path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create token file: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		file.Close()
		os.Remove(outputPath)
		return fmt.Errorf("generate token: %w", err)
	}
	prefixBytes := make([]byte, 4)
	if _, err := rand.Read(prefixBytes); err != nil {
		file.Close()
		os.Remove(outputPath)
		return fmt.Errorf("generate token prefix: %w", err)
	}
	prefix := 100_000 + binary.BigEndian.Uint32(prefixBytes)%900_000
	token := fmt.Sprintf("%d:%s", prefix, base64.RawURLEncoding.EncodeToString(secret))
	if _, err := fmt.Fprintln(file, token); err != nil {
		file.Close()
		os.Remove(outputPath)
		return fmt.Errorf("write token file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("close token file: %w", err)
	}
	_, err = fmt.Fprintln(output, outputPath)
	return err
}
