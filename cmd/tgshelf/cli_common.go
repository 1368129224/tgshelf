package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/fs"
)

// preflightTelegram checks that api_id and api_hash are usable (cfg.Validate is
// not called: login and resolving do not need a bot_token). Pairing of the
// TG_API_ID/TG_API_HASH environment variables is already done by applyEnv inside
// config.Load, so this only covers credentials missing from the config file.
func preflightTelegram(cfg *config.Config) error {
	// The tdesktop identity uses the built-in Telegram Desktop credentials, so no user api_id is needed.
	if cfg.Telegram.EffectiveIdentity() == config.IdentityTDDesktop {
		return nil
	}
	if cfg.Telegram.APIID == 0 && strings.TrimSpace(cfg.Telegram.APIHash) == "" {
		return fmt.Errorf("missing Telegram API credentials: apply for api_id and api_hash at https://my.telegram.org, " +
			"then fill in the [telegram] section of the config file, or provide both through the TG_API_ID / TG_API_HASH environment variables")
	}
	if cfg.Telegram.APIID == 0 {
		return fmt.Errorf("api_id is not configured (it is 0): get one at https://my.telegram.org " +
			"and set it in the [telegram] section of the config file or via the TG_API_ID environment variable")
	}
	if strings.TrimSpace(cfg.Telegram.APIHash) == "" {
		return fmt.Errorf("api_hash is not configured: get one at https://my.telegram.org " +
			"and set it in the [telegram] section of the config file or via the TG_API_HASH environment variable")
	}
	return nil
}

// sessionPathOf resolves the session file path: relative paths are expanded
// against the config file directory (same semantics as dataDirOf) and an empty
// value falls back to the default data/session.json.
func sessionPathOf(cfg *config.Config, configPath string) string {
	s := strings.TrimSpace(cfg.Telegram.Session)
	if s == "" {
		s = "data/session.json"
	}
	if !filepath.IsAbs(s) {
		s = filepath.Join(filepath.Dir(configPath), s)
	}
	return s
}

// ensureSessionParent makes sure the parent directory of the session file exists (mode 0700).
func ensureSessionParent(path string) error {
	if err := fs.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	return nil
}
