package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ErrNotExist is the sentinel error returned when the config file does not exist.
// Callers can use errors.Is(err, ErrNotExist) to decide whether to generate a default config.
var ErrNotExist = errors.New("config file does not exist")

// Legacy environment variable names (the project was called tdfb before v2.0.0). They are
// still read for compatibility with existing deployments and log a warning when used.
// The new name wins; when both are set the new one is authoritative.
const (
	legacyEnvConfig   = "TDFB_CONFIG"
	legacyEnvBotToken = "TDFB_BOT_TOKEN" // #nosec G101 -- env var name, not a credential
)

// getenvCompat reads the new variable name first and falls back to the legacy one when
// unset.
// It returns (value, whether it came from the legacy name).
func getenvCompat(newName, legacyName string) (string, bool) {
	if v := os.Getenv(newName); v != "" {
		return v, false
	}
	if v := os.Getenv(legacyName); v != "" {
		return v, true
	}
	return "", false
}

// Load loads the config file at the given path, applying defaults, TOML and environment
// variables in that order.
// When path is empty it checks the TGSHELF_CONFIG environment variable and finally falls
// back to "config.toml".
// When the file does not exist it returns a wrapped ErrNotExist.
func Load(path string) (*Config, error) {
	// precedence: explicit argument > TGSHELF_CONFIG env var > default path
	if path == "" {
		if envPath, _ := getenvCompat("TGSHELF_CONFIG", legacyEnvConfig); envPath != "" {
			path = envPath
		} else {
			path = "config.toml"
		}
	}

	// start from the built-in defaults
	cfg := defaults()

	// read the TOML file
	data, err := os.ReadFile(path) // #nosec G304 -- config path comes from CLI flag or TGSHELF_CONFIG env var
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, path)
		}
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	// apply environment variable overrides
	if err := applyEnv(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// applyEnv applies environment variable overrides to the configuration.
// Precedence: environment variables > TOML > built-in defaults.
// api_id and api_hash must be overridden as a pair; providing only one is illegal.
func applyEnv(cfg *Config) error {
	// TGSHELF_BOT_TOKEN (legacy TDFB_BOT_TOKEN also accepted)
	if v, _ := getenvCompat("TGSHELF_BOT_TOKEN", legacyEnvBotToken); v != "" {
		cfg.BotToken = v
	}

	// TG_API_ID and TG_API_HASH must be set as a pair
	apiIDStr := os.Getenv("TG_API_ID")
	apiHashStr := os.Getenv("TG_API_HASH")

	hasID := apiIDStr != ""
	hasHash := apiHashStr != ""

	if hasID != hasHash {
		// only one of them is set
		if hasID {
			return fmt.Errorf("env: TG_API_ID is set but TG_API_HASH is missing; api_id and api_hash must be set together")
		}
		return fmt.Errorf("env: TG_API_HASH is set but TG_API_ID is missing; api_id and api_hash must be set together")
	}

	if hasID && hasHash {
		id, err := strconv.Atoi(strings.TrimSpace(apiIDStr))
		if err != nil {
			return fmt.Errorf("env TG_API_ID: cannot convert %q to int: %w", apiIDStr, err)
		}
		cfg.Telegram.APIID = id
		cfg.Telegram.APIHash = strings.TrimSpace(apiHashStr)
	}

	return nil
}

// Save serializes the configuration to TOML and writes it to the given path.
//
// Note (see doc.go): this function drops every comment and is meant for initialization and
// tests only.
// To generate a commented default config use [WriteDefault].
func Save(path string, cfg *Config) error {
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing config to %q: %w", path, err)
	}
	return nil
}

// defaultConfigTemplate is the commented default configuration template string.
// It never goes through toml.Marshal, so the comments are kept intact.
const defaultConfigTemplate = `# tgshelf configuration template
# Generated on first run; edit it as needed and restart the program.
# Field meanings and defaults: docs/configuration.md
#
# Debug mode (true enables verbose logging)
debug = false

# Telegram bot token (required, from @BotFather)
bot_token = ""

# Root download directory (required; the container's working directory is /, so ./downloads is /downloads)
download_path = "./downloads"

# Tag list = download subdirectories; may not contain /, \ or .. path traversal
tags = ["video", "music", "doc"]

# HTTP/SOCKS proxy URL, e.g. "socks5://127.0.0.1:1080"; leave empty to not use one
proxy_url = ""

[telegram]
# Login identity: "tdesktop" (default) or "custom"
#   tdesktop —— use the official Telegram Desktop app credentials (api_id 2040) and the
#               TD device fingerprint (Desktop / Windows 10 / 4.2.4 x64);
#               no api_id application of your own is needed.
#   custom   —— use your own api_id / api_hash below; neutral defaults are used when the
#               device fields are empty.
identity = "tdesktop"
# Telegram API ID (required when identity = "custom"; the TG_API_ID environment variable
# also works; api_id and api_hash must be set together)
api_id = 0
# Telegram API Hash (the TG_API_HASH environment variable also works)
api_hash = ""
# Session file path (a relative path resolves against the config file's directory)
session = "data/session.json"
# Device fingerprint: empty = default by identity (tdesktop → TD fingerprint;
# custom → tgshelf/<platform>/1.0). Setting any field overrides the default.
device_model = ""
system_version = ""
app_version = ""
# Language pack (usually empty; the tdesktop identity defaults to tdesktop)
lang_pack = ""

[bot]
# Maximum concurrent downloads (1-16)
max_concurrency = 3
# User IDs allowed to use the bot; empty = no restriction (public deployment is not recommended)
allowed_user_ids = []
# Admin user IDs (may run /login /logout /sync /show_config)
admin_user_ids = []

[download]
# Download threads per file (1-32)
threads = 4
# Concurrent files (>=1)
file_concurrency = 2
# Skip files that already exist with the same size
skip_same = true
# Correct the extension from the file header
rewrite_ext = false
# Download the description file
desc = false
# Use the takeout API
takeout = false
# Extension allowlist, e.g. [".mp4", ".mkv"]; empty = no restriction
include = []
# Extension blocklist
exclude = []
# Filename template
template = '{{ .DialogID }}_{{ .MessageID }}_{{ filenamify .FileName }}'
# Keep the .parts state file after success (failures always keep it)
keep_parts = false
# Dialog cache TTL in hours; 0 or negative = built-in default of 6 hours
cache_dialogs_hours = 6
`

// WriteDefault writes the commented default configuration to the given path.
// It returns an error instead of overwriting when the target file already exists.
// The comments come from a hard-coded template string and never pass through toml.Marshal,
// so they are kept intact.
func WriteDefault(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- config path comes from CLI flag
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("config file already exists at %q, refusing to overwrite", path)
		}
		return fmt.Errorf("writing default config to %q: %w", path, err)
	}
	if _, err := f.WriteString(defaultConfigTemplate); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing default config to %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing default config to %q: %w", path, err)
	}
	return nil
}
