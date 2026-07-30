package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppliesDefaultsAndValidates(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestFile(t, configPath, `{
		"version": 1,
		"database": "`+filepath.Join(dir, "state.db")+`",
		"telegram": {"token_file": "`+filepath.Join(dir, "telegram.token")+`"},
		"clients": [{"id": "openclaw", "token_file": "`+filepath.Join(dir, "openclaw.token")+`"}]
	}`, 0o600)
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen || cfg.Telegram.APIBase != DefaultAPIBase {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
	if cfg.Routing.Mode != "broadcast" || cfg.Retention.MaxPendingPerClient != DefaultMaxPending {
		t.Fatalf("behavior defaults not applied: %#v", cfg)
	}
	if got := ClientIDs(cfg); len(got) != 1 || got[0] != "openclaw" {
		t.Fatalf("ClientIDs = %v", got)
	}
}

func TestLoadRejectsInvalidConfigurations(t *testing.T) {
	dir := t.TempDir()
	base := `{
		"version": 1,
		"database": "` + filepath.Join(dir, "state.db") + `",
		"telegram": {"token_file": "` + filepath.Join(dir, "telegram.token") + `"},
		"clients": [{"id": "openclaw", "token_file": "` + filepath.Join(dir, "openclaw.token") + `"}]
	}`
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"unknown field", strings.Replace(base, `"version": 1,`, `"version": 1, "mystery": true,`, 1), "unknown field"},
		{"trailing value", base + `{}`, "one JSON value"},
		{"wrong version", strings.Replace(base, `"version": 1`, `"version": 2`, 1), "version must be 1"},
		{"relative database", strings.Replace(base, filepath.Join(dir, "state.db"), "state.db", 1), "database must be an absolute"},
		{"remote listen", strings.Replace(base, `"version": 1,`, `"version": 1, "listen": "0.0.0.0:8080",`, 1), "listen must be loopback"},
		{"bad client id", strings.Replace(base, `"openclaw"`, `"OpenClaw"`, 1), "must match"},
		{"duplicate client", strings.Replace(base, `}]`, `},{"id":"openclaw","token_file":"/tmp/other"}]`, 1), "duplicate client"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(test.name, " ", "-")+".json")
			writeTestFile(t, path, test.content, 0o600)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateExclusiveRouting(t *testing.T) {
	cfg := Config{
		Version:   CurrentVersion,
		Listen:    DefaultListen,
		Database:  "/tmp/state.db",
		Telegram:  TelegramConfig{TokenFile: "/tmp/telegram", APIBase: DefaultAPIBase, FileBase: DefaultAPIBase, PollTimeoutSeconds: 30, MaxRetrySeconds: 30},
		Clients:   []ClientConfig{{ID: "openclaw", TokenFile: "/tmp/openclaw"}, {ID: "unyolo", TokenFile: "/tmp/unyolo"}},
		Routing:   RoutingConfig{Mode: "exclusive", Rules: []RoutingRule{{Clients: []string{"unyolo"}, UpdateTypes: []string{"callback_query"}, CallbackDataPrefixes: []string{"bk:"}}}, FallbackClients: []string{"openclaw"}},
		Retention: RetentionConfig{MaxPendingPerClient: 10, AcknowledgedSafetyWindow: 1},
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Routing.Rules[0].Clients = []string{"missing"}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestLoadCredentials(t *testing.T) {
	dir := t.TempDir()
	telegramPath := filepath.Join(dir, "telegram")
	clientPath := filepath.Join(dir, "client")
	writeTestFile(t, telegramPath, "123456:telegram_token_value_abcdefghijklmnopqrstuvwxyz\n", 0o600)
	writeTestFile(t, clientPath, "654321:client_token_value_abcdefghijklmnopqrstuvwxyz\n", 0o600)
	cfg := Config{Telegram: TelegramConfig{TokenFile: telegramPath}, Clients: []ClientConfig{{ID: "openclaw", TokenFile: clientPath}}}
	credentials, err := LoadCredentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.TelegramToken == "" || credentials.ClientTokens["openclaw"] == "" {
		t.Fatal("credentials were not loaded")
	}

	if err := os.Chmod(clientPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(cfg); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("LoadCredentials error = %v", err)
	}
}

func TestLoadCredentialsRejectsDuplicatesAndWeakTokens(t *testing.T) {
	dir := t.TempDir()
	telegramPath := filepath.Join(dir, "telegram")
	clientPath := filepath.Join(dir, "client")
	value := "123456:shared_token_value_abcdefghijklmnopqrstuvwxyz"
	writeTestFile(t, telegramPath, value, 0o600)
	writeTestFile(t, clientPath, value, 0o600)
	cfg := Config{Telegram: TelegramConfig{TokenFile: telegramPath}, Clients: []ClientConfig{{ID: "openclaw", TokenFile: clientPath}}}
	if _, err := LoadCredentials(cfg); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate error = %v", err)
	}
	writeTestFile(t, clientPath, "weak", 0o600)
	if _, err := LoadCredentials(cfg); err == nil || !strings.Contains(err.Error(), "must look like") {
		t.Fatalf("weak error = %v", err)
	}
}

func writeTestFile(t *testing.T, path, value string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
