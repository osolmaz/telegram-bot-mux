package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestCommandsRequireArguments(t *testing.T) {
	var output bytes.Buffer
	for _, args := range [][]string{{"serve"}, {"doctor"}, {"backup"}, {"generate-client-token"}} {
		if err := run(args, &output, &output); err == nil {
			t.Fatalf("run(%v) succeeded", args)
		}
	}
}
