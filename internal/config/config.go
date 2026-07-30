package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	CurrentVersion         = 1
	DefaultListen          = "127.0.0.1:8080"
	DefaultAPIBase         = "https://api.telegram.org"
	DefaultPollTimeout     = 30
	DefaultMaxRetry        = 30
	DefaultMaxPending      = 10_000
	DefaultSafetyWindow    = 1_000
	maxConfigBytes         = 1 << 20
	maxSecretBytes         = 4 << 10
	minimumClientTokenSize = 32
)

var (
	clientIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	clientTokenPattern = regexp.MustCompile(`^[0-9]{6,12}:[A-Za-z0-9_-]{32,}$`)
)

type Config struct {
	Version     int             `json:"version"`
	Listen      string          `json:"listen,omitempty"`
	AllowRemote bool            `json:"allow_remote,omitempty"`
	Database    string          `json:"database"`
	Telegram    TelegramConfig  `json:"telegram"`
	Clients     []ClientConfig  `json:"clients"`
	Routing     RoutingConfig   `json:"routing,omitempty"`
	Retention   RetentionConfig `json:"retention,omitempty"`
}

type TelegramConfig struct {
	TokenFile             string   `json:"token_file"`
	APIBase               string   `json:"api_base,omitempty"`
	FileBase              string   `json:"file_base,omitempty"`
	AllowInsecureUpstream bool     `json:"allow_insecure_upstream,omitempty"`
	PollTimeoutSeconds    int      `json:"poll_timeout_seconds,omitempty"`
	MaxRetrySeconds       int      `json:"max_retry_seconds,omitempty"`
	AllowedUpdates        []string `json:"allowed_updates,omitempty"`
}

type ClientConfig struct {
	ID        string `json:"id"`
	TokenFile string `json:"token_file"`
}

type RoutingConfig struct {
	Mode            string        `json:"mode,omitempty"`
	Rules           []RoutingRule `json:"rules,omitempty"`
	FallbackClients []string      `json:"fallback_clients,omitempty"`
}

type RoutingRule struct {
	Clients              []string `json:"clients"`
	UpdateTypes          []string `json:"update_types,omitempty"`
	CallbackDataPrefixes []string `json:"callback_data_prefixes,omitempty"`
}

type RetentionConfig struct {
	MaxPendingPerClient      int `json:"max_pending_per_client,omitempty"`
	AcknowledgedSafetyWindow int `json:"acknowledged_safety_window,omitempty"`
}

type Credentials struct {
	TelegramToken string
	ClientTokens  map[string]string
}

func Load(path string) (Config, error) {
	if !filepath.IsAbs(path) {
		return Config{}, errors.New("config path must be absolute")
	}
	data, err := readBounded(path, maxConfigBytes)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Config{}, err
	}
	applyDefaults(&cfg)
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode config trailing data: %w", err)
	}
	return errors.New("config must contain one JSON value")
}

func applyDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if cfg.Telegram.APIBase == "" {
		cfg.Telegram.APIBase = DefaultAPIBase
	}
	if cfg.Telegram.FileBase == "" {
		cfg.Telegram.FileBase = cfg.Telegram.APIBase
	}
	cfg.Telegram.APIBase = strings.TrimRight(cfg.Telegram.APIBase, "/")
	cfg.Telegram.FileBase = strings.TrimRight(cfg.Telegram.FileBase, "/")
	if cfg.Telegram.PollTimeoutSeconds == 0 {
		cfg.Telegram.PollTimeoutSeconds = DefaultPollTimeout
	}
	if cfg.Telegram.MaxRetrySeconds == 0 {
		cfg.Telegram.MaxRetrySeconds = DefaultMaxRetry
	}
	if cfg.Retention.MaxPendingPerClient == 0 {
		cfg.Retention.MaxPendingPerClient = DefaultMaxPending
	}
	if cfg.Retention.AcknowledgedSafetyWindow == 0 {
		cfg.Retention.AcknowledgedSafetyWindow = DefaultSafetyWindow
	}
	if cfg.Routing.Mode == "" {
		cfg.Routing.Mode = "broadcast"
	}
}

func Validate(cfg Config) error {
	if cfg.Version != CurrentVersion {
		return fmt.Errorf("version must be %d", CurrentVersion)
	}
	if !filepath.IsAbs(cfg.Database) {
		return errors.New("database must be an absolute path")
	}
	if !filepath.IsAbs(cfg.Telegram.TokenFile) {
		return errors.New("telegram.token_file must be an absolute path")
	}
	if err := validateListen(cfg.Listen, cfg.AllowRemote); err != nil {
		return err
	}
	if err := validateUpstream("telegram.api_base", cfg.Telegram.APIBase, cfg.Telegram.AllowInsecureUpstream); err != nil {
		return err
	}
	if err := validateUpstream("telegram.file_base", cfg.Telegram.FileBase, cfg.Telegram.AllowInsecureUpstream); err != nil {
		return err
	}
	if cfg.Telegram.PollTimeoutSeconds < 1 || cfg.Telegram.PollTimeoutSeconds > 50 {
		return errors.New("telegram.poll_timeout_seconds must be between 1 and 50")
	}
	if cfg.Telegram.MaxRetrySeconds < 1 || cfg.Telegram.MaxRetrySeconds > 600 {
		return errors.New("telegram.max_retry_seconds must be between 1 and 600")
	}
	if err := validateStringSet("telegram.allowed_updates", cfg.Telegram.AllowedUpdates); err != nil {
		return err
	}
	if len(cfg.Clients) == 0 {
		return errors.New("at least one client is required")
	}
	clientIDs := make(map[string]struct{}, len(cfg.Clients))
	for _, client := range cfg.Clients {
		if !clientIDPattern.MatchString(client.ID) {
			return fmt.Errorf("client id %q must match %s", client.ID, clientIDPattern)
		}
		if _, exists := clientIDs[client.ID]; exists {
			return fmt.Errorf("duplicate client id %q", client.ID)
		}
		if !filepath.IsAbs(client.TokenFile) {
			return fmt.Errorf("client %q token_file must be absolute", client.ID)
		}
		clientIDs[client.ID] = struct{}{}
	}
	if err := validateRouting(cfg.Routing, clientIDs); err != nil {
		return err
	}
	if cfg.Retention.MaxPendingPerClient < 1 {
		return errors.New("retention.max_pending_per_client must be positive")
	}
	if cfg.Retention.AcknowledgedSafetyWindow < 0 {
		return errors.New("retention.acknowledged_safety_window must not be negative")
	}
	return nil
}

func validateListen(address string, allowRemote bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return errors.New("listen must be a host:port address")
	}
	if allowRemote {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen must be loopback unless allow_remote is true")
	}
	return nil
}

func validateUpstream(field, value string, allowInsecure bool) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be an absolute HTTP URL without query or fragment", field)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" {
		return fmt.Errorf("%s must use HTTP or HTTPS", field)
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if !allowInsecure && !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s may use HTTP only for loopback unless allow_insecure_upstream is true", field)
	}
	return nil
}

func validateStringSet(field string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("%s entries must be nonempty and trimmed", field)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s contains duplicate %q", field, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateRouting(routing RoutingConfig, clientIDs map[string]struct{}) error {
	switch routing.Mode {
	case "broadcast":
		if len(routing.Rules) != 0 || len(routing.FallbackClients) != 0 {
			return errors.New("broadcast routing must not define rules or fallback_clients")
		}
		return nil
	case "exclusive":
		if len(routing.Rules) == 0 || len(routing.FallbackClients) == 0 {
			return errors.New("exclusive routing requires rules and fallback_clients")
		}
	default:
		return errors.New("routing.mode must be broadcast or exclusive")
	}
	if err := validateClientReferences("routing.fallback_clients", routing.FallbackClients, clientIDs); err != nil {
		return err
	}
	for index, rule := range routing.Rules {
		label := fmt.Sprintf("routing.rules[%d]", index)
		if err := validateClientReferences(label+".clients", rule.Clients, clientIDs); err != nil {
			return err
		}
		if len(rule.UpdateTypes) == 0 && len(rule.CallbackDataPrefixes) == 0 {
			return fmt.Errorf("%s must define update_types or callback_data_prefixes", label)
		}
		if err := validateStringSet(label+".update_types", rule.UpdateTypes); err != nil {
			return err
		}
		if err := validateStringSet(label+".callback_data_prefixes", rule.CallbackDataPrefixes); err != nil {
			return err
		}
	}
	return nil
}

func validateClientReferences(field string, values []string, clientIDs map[string]struct{}) error {
	if len(values) == 0 {
		return fmt.Errorf("%s must not be empty", field)
	}
	if err := validateStringSet(field, values); err != nil {
		return err
	}
	for _, value := range values {
		if _, exists := clientIDs[value]; !exists {
			return fmt.Errorf("%s references unknown client %q", field, value)
		}
	}
	return nil
}

func LoadCredentials(cfg Config) (Credentials, error) {
	telegramToken, err := readSecret(cfg.Telegram.TokenFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("read telegram token: %w", err)
	}
	credentials := Credentials{TelegramToken: telegramToken, ClientTokens: make(map[string]string, len(cfg.Clients))}
	seen := map[string]string{telegramToken: "telegram"}
	for _, client := range cfg.Clients {
		token, readErr := readSecret(client.TokenFile)
		if readErr != nil {
			return Credentials{}, fmt.Errorf("read client %q token: %w", client.ID, readErr)
		}
		if len(token) < minimumClientTokenSize || !clientTokenPattern.MatchString(token) {
			return Credentials{}, fmt.Errorf("client %q token must look like a Telegram token with at least 32 secret characters", client.ID)
		}
		if owner, exists := seen[token]; exists {
			return Credentials{}, fmt.Errorf("client %q token duplicates %s token", client.ID, owner)
		}
		seen[token] = "client " + client.ID
		credentials.ClientTokens[client.ID] = token
	}
	return credentials, nil
}

func readSecret(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("secret path must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("secret file must not be accessible by group or others")
	}
	data, err := readBounded(path, maxSecretBytes)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("secret file must contain one nonempty line")
	}
	return value, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- explicit trusted configuration path.
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func ClientIDs(cfg Config) []string {
	ids := make([]string, 0, len(cfg.Clients))
	for _, client := range cfg.Clients {
		ids = append(ids, client.ID)
	}
	slices.Sort(ids)
	return ids
}
