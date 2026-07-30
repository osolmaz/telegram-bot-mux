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
		Retention: RetentionConfig{MaxPendingPerClient: 10, AcknowledgedSafetyWindow: intPointer(1)},
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Routing.Rules[0].Clients = []string{"missing"}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestLoadPreservesExplicitZeroSafetyWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeTestFile(t, path, `{
		"version": 1,
		"database": "`+filepath.Join(dir, "state.db")+`",
		"telegram": {"token_file": "`+filepath.Join(dir, "telegram.token")+`"},
		"clients": [{"id": "client", "token_file": "`+filepath.Join(dir, "client.token")+`"}],
		"retention": {"acknowledged_safety_window": 0}
	}`, 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retention.SafetyWindow() != 0 {
		t.Fatalf("safety window = %d", cfg.Retention.SafetyWindow())
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
	writeTestFile(t, telegramPath, "invalid/telegram%token", 0o600)
	if _, err := LoadCredentials(cfg); err == nil || !strings.Contains(err.Error(), "telegram token must look like") {
		t.Fatalf("physical token error = %v", err)
	}
}

func TestValidateBoundaryCases(t *testing.T) {
	base := Config{
		Version: CurrentVersion, Listen: DefaultListen, Database: "/tmp/state.db",
		Telegram: TelegramConfig{TokenFile: "/tmp/telegram", APIBase: DefaultAPIBase, FileBase: DefaultAPIBase, PollTimeoutSeconds: 30, MaxRetrySeconds: 30},
		Clients:  []ClientConfig{{ID: "client", TokenFile: "/tmp/client"}},
		Routing:  RoutingConfig{Mode: "broadcast"}, Retention: RetentionConfig{MaxPendingPerClient: 1},
	}
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"bad listen", func(c *Config) { c.Listen = "bad" }, "host:port"},
		{"remote allowed", func(c *Config) { c.Listen = "0.0.0.0:80"; c.AllowRemote = true }, ""},
		{"invalid api", func(c *Config) { c.Telegram.APIBase = "://" }, "absolute HTTP URL"},
		{"api query", func(c *Config) { c.Telegram.APIBase = "https://example.com?a=1" }, "without query"},
		{"bad scheme", func(c *Config) { c.Telegram.APIBase = "ftp://example.com" }, "HTTP or HTTPS"},
		{"remote http", func(c *Config) { c.Telegram.APIBase = "http://example.com" }, "only for loopback"},
		{"remote http allowed", func(c *Config) { c.Telegram.APIBase = "http://example.com"; c.Telegram.AllowInsecureUpstream = true }, ""},
		{"poll timeout", func(c *Config) { c.Telegram.PollTimeoutSeconds = 51 }, "poll_timeout_seconds"},
		{"retry timeout", func(c *Config) { c.Telegram.MaxRetrySeconds = 0 }, "max_retry_seconds"},
		{"duplicate updates", func(c *Config) { c.Telegram.AllowedUpdates = []string{"message", "message"} }, "duplicate"},
		{"no clients", func(c *Config) { c.Clients = nil }, "at least one client"},
		{"relative client token", func(c *Config) { c.Clients[0].TokenFile = "token" }, "must be absolute"},
		{"unknown routing mode", func(c *Config) { c.Routing.Mode = "magic" }, "broadcast or exclusive"},
		{"broadcast fields", func(c *Config) { c.Routing.Rules = []RoutingRule{{}} }, "must not define"},
		{"bad retention", func(c *Config) { c.Retention.MaxPendingPerClient = -1 }, "must be positive"},
		{"bad safety window", func(c *Config) { c.Retention.AcknowledgedSafetyWindow = intPointer(-1) }, "must not be negative"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Clients = append([]ClientConfig(nil), base.Clients...)
			test.edit(&cfg)
			err := Validate(cfg)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSecretFileShapeErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Telegram: TelegramConfig{TokenFile: filepath.Join(dir, "telegram")}, Clients: []ClientConfig{{ID: "client", TokenFile: filepath.Join(dir, "client")}}}
	writeTestFile(t, cfg.Telegram.TokenFile, "123456:telegram_token_value_abcdefghijklmnopqrstuvwxyz", 0o600)
	if err := os.Mkdir(cfg.Clients[0].TokenFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(cfg); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory secret error = %v", err)
	}
	if err := os.Remove(cfg.Clients[0].TokenFile); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, cfg.Clients[0].TokenFile, "123456:first\nsecond", 0o600)
	if _, err := LoadCredentials(cfg); err == nil || !strings.Contains(err.Error(), "one nonempty line") {
		t.Fatalf("multiline error = %v", err)
	}
	writeTestFile(t, cfg.Clients[0].TokenFile, strings.Repeat("x", maxSecretBytes+1), 0o600)
	if _, err := LoadCredentials(cfg); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize error = %v", err)
	}
}

func intPointer(value int) *int { return &value }

func writeTestFile(t *testing.T, path, value string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
