package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunHelp verifies that -h exits with code 0 (run returns nil) and produces no side effects.
func TestRunHelp(t *testing.T) {
	if err := run([]string{"-h"}); err != nil {
		t.Fatalf("run(-h) must return nil, got: %v", err)
	}
}

// TestRunMissingConfigGeneratesDefault verifies that when the config file is missing:
//  1. a commented default config file is generated;
//  2. run returns an error carrying the hint (main then exits 1).
func TestRunMissingConfigGeneratesDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tgshelf_config.toml")

	err := run([]string{"-config", cfgPath})
	if err == nil {
		t.Fatal("run must return an error when the config is missing")
	}
	if !strings.Contains(err.Error(), "generated default config") ||
		!strings.Contains(err.Error(), "fill in bot_token and try again") {
		t.Fatalf("error must carry the hint, got: %v", err)
	}
	if !strings.Contains(err.Error(), cfgPath) {
		t.Fatalf("error must contain the config path, got: %v", err)
	}

	// The default config must exist and carry comments.
	data, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		t.Fatalf("default config must be generated: %v", rerr)
	}
	content := string(data)
	if !strings.Contains(content, "# tgshelf") {
		t.Errorf("default config must carry a comment header, first 80 bytes: %q", content[:min(80, len(content))])
	}
	if !strings.Contains(content, "bot_token") {
		t.Errorf("default config must contain the bot_token field")
	}

	// No data directory must be created (the run exits early when config loading fails).
	if _, serr := os.Stat(filepath.Join(dir, "data")); serr == nil {
		t.Errorf("data/ must not be created when config loading fails")
	}
}

// TestRunValidConfig verifies that a valid config gets through the lock and state opening and
// finally fails in the bot startup chain.
func TestRunValidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tgshelf_config.toml")
	cfg := `# test config
debug = false
bot_token = "123456:TEST-TOKEN"
download_path = "/tmp/tgshelf-test-downloads"
tags = ["video"]

[telegram]
api_id = 12345
api_hash = "testhash"
session = "data/session.json"

[bot]
max_concurrency = 1

[download]
threads = 2
file_concurrency = 1
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"-config", cfgPath})
	if err == nil {
		t.Fatal("run must return an error with an invalid token")
	}
	// startBot runs the bot startup chain: the error must come from it (client
	// construction or run failure), not from a stub anymore.
	if !strings.Contains(err.Error(), "start bot") {
		t.Fatalf("must report start bot, got: %v", err)
	}

	// Real artifacts from the earlier chain: data/session.lock and data/state.json.
	for _, name := range []string{"session.lock", "state.json"} {
		if _, serr := os.Stat(filepath.Join(dir, "data", name)); serr != nil {
			t.Errorf("missing %s: %v", name, serr)
		}
	}
}

// TestMaskToken verifies token masking so logs never contain the plaintext.
func TestMaskToken(t *testing.T) {
	if got := maskToken("123456:ABC-SECRET"); got != "***" {
		t.Errorf("non-empty token must be masked as ***, got: %q", got)
	}
	if got := maskToken(""); got != "(not set)" {
		t.Errorf("empty token must report not set, got: %q", got)
	}
}

// TestHealthCheck verifies the health check command (used by Docker HEALTHCHECK).
//
// Semantics: no session file means /login has not happened yet, which is a legal state (on first
// deployment the container must be healthy, otherwise the "compose up, then /login in Telegram"
// path looks like a failure).
func TestHealthCheck(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tgshelf_config.toml")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(dataDir, "session.json")
	if err := os.WriteFile(sessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	content := fmt.Sprintf(`
download_path = %q
[telegram]
session = %q
`, dir, sessionPath)
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Normal case (-health)
	if err := run([]string{"-config", cfgPath, "-health"}); err != nil {
		t.Fatalf("health check must succeed, got: %v", err)
	}

	// Missing session case (-healthcheck): being logged out is legal, so it must stay healthy.
	_ = os.Remove(sessionPath)
	if err := run([]string{"-config", cfgPath, "-healthcheck"}); err != nil {
		t.Fatalf("health check must succeed while logged out (session missing), got: %v", err)
	}

	// Missing config case: the health check must fail (otherwise the probe cannot detect a bad
	// mount).
	if err := run([]string{"-config", filepath.Join(dir, "nope.toml"), "-health"}); err == nil {
		t.Fatal("health check must fail when the config is missing")
	}
}
