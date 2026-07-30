package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunVersionAndUnknownCommand(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"version"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != version {
		t.Fatalf("version output = %q", output.String())
	}
	if err := run([]string{"unknown"}, &output, &output); err == nil {
		t.Fatal("unknown command succeeded")
	}
	if err := run(nil, &output, &output); err == nil {
		t.Fatal("empty invocation succeeded")
	}
}

func TestGenerateClientTokenAndDoctorBackup(t *testing.T) {
	dir := t.TempDir()
	clientTokenPath := filepath.Join(dir, "client.token")
	var output bytes.Buffer
	if err := run([]string{"generate-client-token", "--out", clientTokenPath}, &output, &output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(clientTokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(data))) < 39 {
		t.Fatalf("generated token is too short: %d", len(data))
	}
	if info, _ := os.Stat(clientTokenPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o", info.Mode().Perm())
	}
	if err := run([]string{"generate-client-token", "--out", clientTokenPath}, &output, &output); err == nil {
		t.Fatal("token command overwrote an existing file")
	}

	telegramTokenPath := filepath.Join(dir, "telegram.token")
	if err := os.WriteFile(telegramTokenPath, []byte("123456:telegram_token_value_abcdefghijklmnopqrstuvwxyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	config := `{
		"version":1,
		"database":"` + filepath.Join(dir, "state.db") + `",
		"telegram":{"token_file":"` + telegramTokenPath + `"},
		"clients":[{"id":"client","token_file":"` + clientTokenPath + `"}]
	}`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"doctor", "--config", configPath, "--offline"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "database: ok") || !strings.Contains(output.String(), "telegram: skipped") {
		t.Fatalf("doctor output = %q", output.String())
	}
	backupPath := filepath.Join(dir, "backup.db")
	if err := run([]string{"backup", "--config", configPath, "--out", backupPath}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatal(err)
	}
}

func TestServeLifecycleAndOnlineDoctor(t *testing.T) {
	telegram := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			io.WriteString(response, `{"ok":true,"result":{"id":123}}`)
		case strings.HasSuffix(request.URL.Path, "/getUpdates"):
			time.Sleep(20 * time.Millisecond)
			io.WriteString(response, `{"ok":true,"result":[]}`)
		default:
			io.WriteString(response, `{"ok":true,"result":true}`)
		}
	}))
	defer telegram.Close()
	dir := t.TempDir()
	configPath := writeRuntimeConfig(t, dir, telegram.URL)
	var output bytes.Buffer
	if err := run([]string{"doctor", "--config", configPath}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "telegram: ok") {
		t.Fatalf("doctor output = %q", output.String())
	}

	done := make(chan error, 1)
	go func() { done <- serve(configPath, &output) }()
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

func TestCommandsRequireArguments(t *testing.T) {
	var output bytes.Buffer
	for _, args := range [][]string{{"serve"}, {"doctor"}, {"backup"}, {"generate-client-token"}} {
		if err := run(args, &output, &output); err == nil {
			t.Fatalf("run(%v) succeeded", args)
		}
	}
	if err := generateClientToken("relative", &output); err == nil {
		t.Fatal("relative token path succeeded")
	}
	if err := backup("relative", "/tmp/backup"); err == nil {
		t.Fatal("relative config path succeeded")
	}
}

func writeRuntimeConfig(t *testing.T, dir, apiBase string) string {
	t.Helper()
	telegramTokenPath := filepath.Join(dir, "telegram.token")
	clientTokenPath := filepath.Join(dir, "client.token")
	if err := os.WriteFile(telegramTokenPath, []byte("123456:telegram_token_value_abcdefghijklmnopqrstuvwxyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientTokenPath, []byte("654321:client_token_value_abcdefghijklmnopqrstuvwxyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "runtime.json")
	content := `{
		"version":1,
		"listen":"127.0.0.1:0",
		"database":"` + filepath.Join(dir, "runtime.db") + `",
		"telegram":{"token_file":"` + telegramTokenPath + `","api_base":"` + apiBase + `","file_base":"` + apiBase + `"},
		"clients":[{"id":"client","token_file":"` + clientTokenPath + `"}]
	}`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}
