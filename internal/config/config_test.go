package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper: write a minimal valid config file into dir and return its path
func writeMinimalConfig(t *testing.T, dir string, extra string) string {
	t.Helper()
	content := `debug = false
bot_token = "test-token-12345"
download_path = "/tmp/downloads"
tags = ["video", "music"]
proxy_url = ""

[telegram]
api_id   = 123456
api_hash = "0123456789abcdef0123456789abcdef"
session  = "data/session.json"
device_model   = "tgshelf"
system_version = "Linux"
app_version    = "1.0"

[bot]
max_concurrency = 3
allowed_user_ids = []
admin_user_ids   = []

[download]
threads        = 4
file_concurrency = 2
skip_same      = true
rewrite_ext    = false
desc           = false
takeout        = false
include        = []
exclude        = []
template       = "{{ .DialogID }}_{{ .MessageID }}"
keep_parts     = false
cache_dialogs_hours = 6
` + extra
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeMinimalConfig: %v", err)
	}
	return path
}

// ============================================================
// Test: basic load
// ============================================================

func TestLoad_Basic(t *testing.T) {
	dir := t.TempDir()
	path := writeMinimalConfig(t, dir, "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.BotToken != "test-token-12345" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "test-token-12345")
	}
	if cfg.Telegram.APIID != 123456 {
		t.Errorf("APIID = %d, want 123456", cfg.Telegram.APIID)
	}
	if cfg.Download.Threads != 4 {
		t.Errorf("Threads = %d, want 4", cfg.Download.Threads)
	}
}

// ============================================================
// Test: missing file → ErrNotExist
// ============================================================

func TestLoad_ErrNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "nonexistent.toml"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNotExist) {
		t.Errorf("expected ErrNotExist, got: %v", err)
	}
}

// ============================================================
// Test: TGSHELF_CONFIG env var takes effect
// ============================================================

func TestLoad_TdlbotConfigEnv(t *testing.T) {
	dir := t.TempDir()
	path := writeMinimalConfig(t, dir, "")

	t.Setenv("TGSHELF_CONFIG", path)
	// pass no path and let Load read TGSHELF_CONFIG
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load via TGSHELF_CONFIG: %v", err)
	}
	if cfg.BotToken != "test-token-12345" {
		t.Errorf("BotToken = %q", cfg.BotToken)
	}
}

// ============================================================
// Test: explicit path wins over TGSHELF_CONFIG
// ============================================================

func TestLoad_ExplicitPathBeatsEnv(t *testing.T) {
	dir := t.TempDir()

	// path1: token = "explicit-token"
	path1 := filepath.Join(dir, "explicit.toml")
	if err := os.WriteFile(path1, []byte(`bot_token = "explicit-token"
download_path = "/d"
tags = ["a"]
[telegram]
api_id = 1
api_hash = "aabbccdd"
[bot]
max_concurrency = 1
[download]
threads = 1
file_concurrency = 1
cache_dialogs_hours = 0
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// path2: token = "env-token"
	path2 := filepath.Join(dir, "env.toml")
	if err := os.WriteFile(path2, []byte(`bot_token = "env-token"
download_path = "/d"
tags = ["a"]
[telegram]
api_id = 1
api_hash = "aabbccdd"
[bot]
max_concurrency = 1
[download]
threads = 1
file_concurrency = 1
cache_dialogs_hours = 0
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TGSHELF_CONFIG", path2)
	cfg, err := Load(path1) // explicit path
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BotToken != "explicit-token" {
		t.Errorf("BotToken = %q, want explicit-token", cfg.BotToken)
	}
}

// ============================================================
// Test: env var overrides bot_token
// ============================================================

func TestLoad_EnvBotToken(t *testing.T) {
	dir := t.TempDir()
	path := writeMinimalConfig(t, dir, "")

	t.Setenv("TGSHELF_BOT_TOKEN", "env-overridden-token")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BotToken != "env-overridden-token" {
		t.Errorf("BotToken = %q, want env-overridden-token", cfg.BotToken)
	}
}

// ============================================================
// Test: TG_API_ID + TG_API_HASH override as a pair
// ============================================================

func TestLoad_EnvAPIIDHashPair(t *testing.T) {
	dir := t.TempDir()
	path := writeMinimalConfig(t, dir, "")

	t.Setenv("TG_API_ID", "999999")
	t.Setenv("TG_API_HASH", "env-hash-value-xxxxxx")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Telegram.APIID != 999999 {
		t.Errorf("APIID = %d, want 999999", cfg.Telegram.APIID)
	}
	if cfg.Telegram.APIHash != "env-hash-value-xxxxxx" {
		t.Errorf("APIHash = %q, want env-hash-value-xxxxxx", cfg.Telegram.APIHash)
	}
}

// ============================================================
// Test: only TG_API_ID → error
// ============================================================

func TestLoad_EnvAPIIDOnly_Error(t *testing.T) {
	dir := t.TempDir()
	path := writeMinimalConfig(t, dir, "")

	t.Setenv("TG_API_ID", "123")
	// TG_API_HASH not set
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when only TG_API_ID is set")
	}
	if !strings.Contains(err.Error(), "TG_API_HASH") {
		t.Errorf("error should mention TG_API_HASH, got: %v", err)
	}
}

// ============================================================
// Test: only TG_API_HASH → error
// ============================================================

func TestLoad_EnvAPIHashOnly_Error(t *testing.T) {
	dir := t.TempDir()
	path := writeMinimalConfig(t, dir, "")

	t.Setenv("TG_API_HASH", "somehash")
	// TG_API_ID not set
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when only TG_API_HASH is set")
	}
	if !strings.Contains(err.Error(), "TG_API_ID") {
		t.Errorf("error should mention TG_API_ID, got: %v", err)
	}
}

// ============================================================
// Test: int written as a string in TOML
// ============================================================

func TestLoad_TOML_IntAsString(t *testing.T) {
	// go-toml v2 does not convert a string to an int automatically,
	// so this test verifies that api_id = "123456" in TOML is a parse error
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte(`bot_token = "tok"
download_path = "/d"
tags = ["a"]
[telegram]
api_id = "not-a-number"
api_hash = "hash"
[bot]
max_concurrency = 1
[download]
threads = 1
file_concurrency = 1
cache_dialogs_hours = 0
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error for api_id = 'not-a-number'")
	}
}

// ============================================================
// Test: asBool helper
// ============================================================

func TestAsBool(t *testing.T) {
	cases := []struct {
		input   interface{}
		want    bool
		wantErr bool
	}{
		{true, true, false},
		{false, false, false},
		{"true", true, false},
		{"True", true, false},
		{"1", true, false},
		{"yes", true, false},
		{"YES", true, false},
		{"false", false, false},
		{"False", false, false},
		{"0", false, false},
		{"no", false, false},
		{"NO", false, false},
		{"abc", false, true},
		{"maybe", false, true},
		{2, false, true},
		{-1, false, true},
		{int(1), true, false},
		{int(0), false, false},
	}

	for _, tc := range cases {
		got, err := asBool("testfield", tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("asBool(%v): expected error, got nil", tc.input)
			} else if !strings.Contains(err.Error(), "testfield") {
				t.Errorf("asBool(%v): error should contain field name 'testfield', got: %v", tc.input, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("asBool(%v): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("asBool(%v) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// ============================================================
// Test: asInt helper
// ============================================================

func TestAsInt(t *testing.T) {
	cases := []struct {
		input   interface{}
		want    int
		wantErr bool
	}{
		{42, 42, false},
		{"42", 42, false},
		{" 42 ", 42, false},
		{int64(100), 100, false},
		{"abc", 0, true},
		{"3.14", 0, true},
	}

	for _, tc := range cases {
		got, err := asInt("testfield", tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("asInt(%v): expected error, got nil", tc.input)
			} else if !strings.Contains(err.Error(), "testfield") {
				t.Errorf("asInt(%v): error should contain field name 'testfield', got: %v", tc.input, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("asInt(%v): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("asInt(%v) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

// ============================================================
// Test: asBool error message names the field and the original value
// ============================================================

func TestAsBool_ErrorContainsFieldAndValue(t *testing.T) {
	_, err := asBool("download.skip_same", "invalid-value")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "download.skip_same") {
		t.Errorf("error should contain field name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid-value") {
		t.Errorf("error should contain original value, got: %v", err)
	}
}

// ============================================================
// Test: asInt error message names the field and the original value
// ============================================================

func TestAsInt_ErrorContainsFieldAndValue(t *testing.T) {
	_, err := asInt("download.threads", "abc")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "download.threads") {
		t.Errorf("error should contain field name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "abc") {
		t.Errorf("error should contain original value, got: %v", err)
	}
}

// ============================================================
// Test: Validate - empty bot_token
// ============================================================

func TestValidate_EmptyBotToken(t *testing.T) {
	cfg := validConfig()
	cfg.BotToken = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty bot_token")
	}
	if !strings.Contains(err.Error(), "bot_token") {
		t.Errorf("error should mention bot_token, got: %v", err)
	}
}

// ============================================================
// Test: Validate - empty download_path
// ============================================================

func TestValidate_EmptyDownloadPath(t *testing.T) {
	cfg := validConfig()
	cfg.DownloadPath = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty download_path")
	}
	if !strings.Contains(err.Error(), "download_path") {
		t.Errorf("error should mention download_path, got: %v", err)
	}
}

// ============================================================
// Test: Validate - empty tags
// ============================================================

func TestValidate_EmptyTags(t *testing.T) {
	cfg := validConfig()
	cfg.Tags = []string{}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty tags")
	}
	if !strings.Contains(err.Error(), "tags") {
		t.Errorf("error should mention tags, got: %v", err)
	}
}

// ============================================================
// Test: Validate - duplicate tag
// ============================================================

func TestValidate_DuplicateTag(t *testing.T) {
	cfg := validConfig()
	cfg.Tags = []string{"video", "music", "video"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate tag")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate, got: %v", err)
	}
}

// ============================================================
// Test: Validate - tag contains /
// ============================================================

func TestValidate_TagWithSlash(t *testing.T) {
	cfg := validConfig()
	cfg.Tags = []string{"good", "bad/tag"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for tag with /")
	}
	if !strings.Contains(err.Error(), "/") {
		t.Errorf("error should mention '/', got: %v", err)
	}
}

// ============================================================
// Test: Validate - tag contains ..
// ============================================================

func TestValidate_TagWithDotDot(t *testing.T) {
	cfg := validConfig()
	cfg.Tags = []string{"good", "../evil"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for tag with ..")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("error should mention '..', got: %v", err)
	}
}

// ============================================================
// Test: Validate - threads = 0
// ============================================================

func TestValidate_ThreadsZero(t *testing.T) {
	cfg := validConfig()
	cfg.Download.Threads = 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for threads = 0")
	}
	if !strings.Contains(err.Error(), "threads") {
		t.Errorf("error should mention threads, got: %v", err)
	}
}

// ============================================================
// Test: Validate - threads = 33
// ============================================================

func TestValidate_ThreadsTooHigh(t *testing.T) {
	cfg := validConfig()
	cfg.Download.Threads = 33
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for threads = 33")
	}
	if !strings.Contains(err.Error(), "threads") {
		t.Errorf("error should mention threads, got: %v", err)
	}
}

// ============================================================
// Test: Validate - max_concurrency = 0
// ============================================================

func TestValidate_MaxConcurrencyZero(t *testing.T) {
	cfg := validConfig()
	cfg.Bot.MaxConcurrency = 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for max_concurrency = 0")
	}
	if !strings.Contains(err.Error(), "max_concurrency") {
		t.Errorf("error should mention max_concurrency, got: %v", err)
	}
}

// ============================================================
// Test: Validate - only one of api_id/api_hash is set
// ============================================================

func TestValidate_APIIDOnly(t *testing.T) {
	cfg := validConfig()
	cfg.Telegram.APIID = 123
	cfg.Telegram.APIHash = "" // no hash
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for api_id without api_hash")
	}
	if !strings.Contains(err.Error(), "api_hash") {
		t.Errorf("error should mention api_hash, got: %v", err)
	}
}

func TestValidate_APIHashOnly(t *testing.T) {
	cfg := validConfig()
	cfg.Telegram.APIID = 0 // no id
	cfg.Telegram.APIHash = "somehash"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for api_hash without api_id")
	}
	if !strings.Contains(err.Error(), "api_id") {
		t.Errorf("error should mention api_id, got: %v", err)
	}
}

// ============================================================
// Test: Validate - a valid config passes
// ============================================================

func TestValidate_Valid(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error for valid config, got: %v", err)
	}
}

// ============================================================
// Test: error aggregation - several illegal fields yield >= 2 errors
// ============================================================

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := validConfig()
	cfg.BotToken = ""          // error 1
	cfg.Download.Threads = 0   // error 2
	cfg.Bot.MaxConcurrency = 0 // error 3
	cfg.Tags = []string{}      // error 4

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error")
	}

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if len(ve.Errors) < 2 {
		t.Errorf("expected >= 2 validation errors, got %d: %v", len(ve.Errors), ve.Errors)
	}
}

// ============================================================
// Test: WriteDefault + Load is parseable
// ============================================================

func TestWriteDefault_LoadParseable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "default.toml")

	if err := WriteDefault(path); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}

	// check the file exists and has content
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("WriteDefault produced empty file")
	}
	// check it contains comments
	if !strings.Contains(string(data), "#") {
		t.Error("WriteDefault file should contain comments")
	}

	// check Load can parse it (no Validate, since the default config has an empty bot_token)
	_, err = Load(path)
	if err != nil {
		t.Fatalf("Load(default config): %v", err)
	}
}

// ============================================================
// Test: WriteDefault does not overwrite an existing file
// ============================================================

func TestWriteDefault_NoOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.toml")
	if err := os.WriteFile(path, []byte("# existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteDefault(path)
	if err == nil {
		t.Fatal("expected error when file already exists")
	}
}

// ============================================================
// Test: defaults() returns sensible built-in defaults
// ============================================================

func TestDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.Bot.MaxConcurrency != 3 {
		t.Errorf("default max_concurrency = %d, want 3", cfg.Bot.MaxConcurrency)
	}
	if cfg.Download.Threads != 4 {
		t.Errorf("default threads = %d, want 4", cfg.Download.Threads)
	}
	if cfg.Download.CacheDialogsHours != 6 {
		t.Errorf("default cache_dialogs_hours = %d, want 6", cfg.Download.CacheDialogsHours)
	}
	if !cfg.Download.SkipSame {
		t.Error("default skip_same should be true")
	}
}

// ============================================================
// Test: Save + Load round trip
// ============================================================

func TestSaveLoad_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "saved.toml")

	original := validConfig()
	original.Debug = true
	original.ProxyURL = "socks5://127.0.0.1:1080"

	if err := Save(path, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Debug != original.Debug {
		t.Errorf("Debug = %v, want %v", loaded.Debug, original.Debug)
	}
	if loaded.ProxyURL != original.ProxyURL {
		t.Errorf("ProxyURL = %q, want %q", loaded.ProxyURL, original.ProxyURL)
	}
	if loaded.BotToken != original.BotToken {
		t.Errorf("BotToken = %q, want %q", loaded.BotToken, original.BotToken)
	}
}

// ============================================================
// Test: asBool handles "false"/"0"/"no" correctly (explicitly required)
// ============================================================

func TestAsBool_FalseyStrings(t *testing.T) {
	for _, s := range []string{"false", "False", "FALSE", "0", "no", "NO", "No"} {
		got, err := asBool("field", s)
		if err != nil {
			t.Errorf("asBool(%q): unexpected error: %v", s, err)
			continue
		}
		if got != false {
			t.Errorf("asBool(%q) = %v, want false", s, got)
		}
	}
}

// ============================================================
// Test: non-numeric TG_API_ID → error
// ============================================================

func TestLoad_EnvAPIID_NonNumeric(t *testing.T) {
	dir := t.TempDir()
	path := writeMinimalConfig(t, dir, "")

	t.Setenv("TG_API_ID", "not-a-number")
	t.Setenv("TG_API_HASH", "somehash")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for non-numeric TG_API_ID")
	}
	if !strings.Contains(err.Error(), "TG_API_ID") {
		t.Errorf("error should mention TG_API_ID, got: %v", err)
	}
}

// ============================================================
// Test: asInt64 helper coverage
// ============================================================

func TestAsInt64(t *testing.T) {
	cases := []struct {
		input   interface{}
		want    int64
		wantErr bool
	}{
		{int64(42), 42, false},
		{int(100), 100, false},
		{int32(200), 200, false},
		{float64(3), 3, false},
		{"9999", 9999, false},
		{" 42 ", 42, false},
		{"abc", 0, true},
		{true, 0, true},
	}

	for _, tc := range cases {
		got, err := asInt64("testfield", tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("asInt64(%v): expected error, got nil", tc.input)
			} else if !strings.Contains(err.Error(), "testfield") {
				t.Errorf("asInt64(%v): error should contain field name, got: %v", tc.input, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("asInt64(%v): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("asInt64(%v) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

// ============================================================
// Test: normalizeStringList helper coverage
// ============================================================

func TestNormalizeStringList(t *testing.T) {
	// nil input
	got, err := normalizeStringList("field", nil)
	if err != nil {
		t.Errorf("normalizeStringList(nil): unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("normalizeStringList(nil) = %v, want []", got)
	}

	// []string input
	got, err = normalizeStringList("field", []string{"a", " b ", "", "c"})
	if err != nil {
		t.Errorf("normalizeStringList([]string): unexpected error: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("normalizeStringList([]string) = %v, want [a b c]", got)
	}

	// []interface{} input
	got, err = normalizeStringList("field", []interface{}{"x", " y "})
	if err != nil {
		t.Errorf("normalizeStringList([]interface{}): unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("normalizeStringList([]interface{}) = %v, want [x y]", got)
	}

	// []interface{} with a non-string element
	_, err = normalizeStringList("field", []interface{}{"x", 42})
	if err == nil {
		t.Error("normalizeStringList: expected error for non-string element")
	}
	if !strings.Contains(err.Error(), "field") {
		t.Errorf("error should contain field name, got: %v", err)
	}

	// a completely unsupported type
	_, err = normalizeStringList("field", 12345)
	if err == nil {
		t.Error("normalizeStringList: expected error for unsupported type")
	}
}

// ============================================================
// Test: asBool int/int64 boundaries
// ============================================================

func TestAsBool_IntBoundaries(t *testing.T) {
	// int 1 = true
	got, err := asBool("f", int(1))
	if err != nil || !got {
		t.Errorf("asBool(int(1)) = %v, %v; want true, nil", got, err)
	}
	// int 0 = false
	got, err = asBool("f", int(0))
	if err != nil || got {
		t.Errorf("asBool(int(0)) = %v, %v; want false, nil", got, err)
	}
	// int 2 = error
	_, err = asBool("f", int(2))
	if err == nil {
		t.Error("asBool(int(2)): expected error")
	}
	// int64 1 = true
	got, err = asBool("f", int64(1))
	if err != nil || !got {
		t.Errorf("asBool(int64(1)) = %v, %v; want true, nil", got, err)
	}
	// int64 0 = false
	got, err = asBool("f", int64(0))
	if err != nil || got {
		t.Errorf("asBool(int64(0)) = %v, %v; want false, nil", got, err)
	}
	// int64 2 = error
	_, err = asBool("f", int64(2))
	if err == nil {
		t.Error("asBool(int64(2)): expected error")
	}
	// unsupported type
	_, err = asBool("f", []string{"x"})
	if err == nil {
		t.Error("asBool([]string): expected error")
	}
}

// ============================================================
// Test: asInt further type coverage
// ============================================================

func TestAsInt_MoreTypes(t *testing.T) {
	// int32
	got, err := asInt("f", int32(77))
	if err != nil || got != 77 {
		t.Errorf("asInt(int32(77)) = %d, %v; want 77, nil", got, err)
	}
	// int16
	got, err = asInt("f", int16(88))
	if err != nil || got != 88 {
		t.Errorf("asInt(int16(88)) = %d, %v; want 88, nil", got, err)
	}
	// int8
	got, err = asInt("f", int8(9))
	if err != nil || got != 9 {
		t.Errorf("asInt(int8(9)) = %d, %v; want 9, nil", got, err)
	}
	// float32
	got, err = asInt("f", float32(5.9))
	if err != nil || got != 5 {
		t.Errorf("asInt(float32(5.9)) = %d, %v; want 5, nil", got, err)
	}
	// float64
	got, err = asInt("f", float64(10.1))
	if err != nil || got != 10 {
		t.Errorf("asInt(float64(10.1)) = %d, %v; want 10, nil", got, err)
	}
	// unsupported
	_, err = asInt("f", true)
	if err == nil {
		t.Error("asInt(true): expected error")
	}
	if !strings.Contains(err.Error(), "f") {
		t.Errorf("error should contain field name, got: %v", err)
	}
}

// ============================================================
// Test: Validate - negative cache_dialogs_hours
// ============================================================

func TestValidate_CacheDialogsHoursNegative(t *testing.T) {
	cfg := validConfig()
	cfg.Download.CacheDialogsHours = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative cache_dialogs_hours")
	}
	if !strings.Contains(err.Error(), "cache_dialogs_hours") {
		t.Errorf("error should mention cache_dialogs_hours, got: %v", err)
	}
}

// ============================================================
// Test: Validate - file_concurrency = 0
// ============================================================

func TestValidate_FileConcurrencyZero(t *testing.T) {
	cfg := validConfig()
	cfg.Download.FileConcurrency = 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for file_concurrency = 0")
	}
	if !strings.Contains(err.Error(), "file_concurrency") {
		t.Errorf("error should mention file_concurrency, got: %v", err)
	}
}

// ============================================================
// Test: Validate - max_concurrency = 17
// ============================================================

func TestValidate_MaxConcurrencyTooHigh(t *testing.T) {
	cfg := validConfig()
	cfg.Bot.MaxConcurrency = 17
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for max_concurrency = 17")
	}
	if !strings.Contains(err.Error(), "max_concurrency") {
		t.Errorf("error should mention max_concurrency, got: %v", err)
	}
}

// ============================================================
// Test: Validate - tag contains a backslash
// ============================================================

func TestValidate_TagWithBackslash(t *testing.T) {
	cfg := validConfig()
	// Use string concatenation to embed a literal backslash without Go escape issues
	badTag := "bad" + string([]byte{0x5c}) + "tag" // 0x5c = backslash
	cfg.Tags = []string{"good", badTag}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error for tag with backslash (tag=%q)", badTag)
	}
	// error mentions the tag
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error should mention the bad tag, got: %v", err)
	}
}

// ============================================================
// Test: ValidationError.Error() format for a single error
// ============================================================

func TestValidationError_SingleError(t *testing.T) {
	ve := &ValidationError{Errors: []string{"only one error"}}
	msg := ve.Error()
	if !strings.Contains(msg, "only one error") {
		t.Errorf("single error format wrong: %v", msg)
	}
	// a single error must not have a count prefix
	if strings.Contains(msg, "(1 errors)") {
		t.Errorf("single error should not show count, got: %v", msg)
	}
}

// ============================================================
// Test: Load does not error when the TOML file has api_id/api_hash both zero
// ============================================================

func TestValidate_APIBothZero_NoPairError(t *testing.T) {
	// both zero is legal (provided through env vars)
	cfg := validConfig()
	cfg.Telegram.APIID = 0
	cfg.Telegram.APIHash = ""
	// Validate must not complain about 0 + empty (paired zero values are legal)
	err := cfg.Validate()
	if err != nil {
		// other errors may exist, but no api pairing error
		if strings.Contains(err.Error(), "api_id") && strings.Contains(err.Error(), "api_hash") {
			t.Errorf("both zero should not trigger pair error, got: %v", err)
		}
	}
}

// ============================================================
// helper: build a valid config
// ============================================================

func validConfig() *Config {
	return &Config{
		Debug:        false,
		BotToken:     "valid-bot-token",
		DownloadPath: "/downloads",
		Tags:         []string{"video", "music"},
		ProxyURL:     "",
		Telegram: TelegramConfig{
			APIID:         123456,
			APIHash:       "abcdef0123456789",
			Session:       "data/session.json",
			DeviceModel:   "tgshelf",
			SystemVersion: "Linux",
			AppVersion:    "1.0",
		},
		Bot: BotConfig{
			MaxConcurrency: 3,
			AllowedUserIDs: []int64{},
			AdminUserIDs:   []int64{},
		},
		Download: DownloadConfig{
			Threads:           4,
			FileConcurrency:   2,
			SkipSame:          true,
			RewriteExt:        false,
			Desc:              false,
			Takeout:           false,
			Include:           []string{},
			Exclude:           []string{},
			Template:          "{{ .DialogID }}_{{ .MessageID }}",
			KeepParts:         false,
			CacheDialogsHours: 6,
		},
	}
}
