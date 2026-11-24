package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Validate checks that the configuration is legal and returns the aggregated errors
// (if any).
// Checked items:
//   - bot_token is not empty
//   - download_path is not empty
//   - tags is not empty, has no duplicates, and no tag contains / \\ or .. (path traversal
//     guard)
//   - api_id and api_hash must be set as a pair (both zero or both non-zero)
//   - max_concurrency: [1, 16]
//   - threads: [1, 32]
//   - file_concurrency >= 1
//   - cache_dialogs_hours >= 0
func (c *Config) Validate() error {
	var errs []string

	// bot_token must not be empty
	if strings.TrimSpace(c.BotToken) == "" {
		errs = append(errs, "bot_token: must not be empty")
	}

	// download_path must not be empty
	if strings.TrimSpace(c.DownloadPath) == "" {
		errs = append(errs, "download_path: must not be empty")
	}

	// tags must not be empty
	if len(c.Tags) == 0 {
		errs = append(errs, "tags: must contain at least one tag")
	} else {
		seen := make(map[string]struct{}, len(c.Tags))
		for _, tag := range c.Tags {
			if strings.TrimSpace(tag) == "" {
				errs = append(errs, "tags: tag must not be empty or whitespace")
				continue
			}
			if tag == "." {
				errs = append(errs, `tags: tag "." is not allowed`)
			}
			// duplicate check
			if _, dup := seen[tag]; dup {
				errs = append(errs, fmt.Sprintf("tags: duplicate tag %q", tag))
			} else {
				seen[tag] = struct{}{}
			}
			// path traversal guard: no / \\ or ..
			if strings.Contains(tag, "/") {
				errs = append(errs, fmt.Sprintf("tags: tag %q must not contain '/'", tag))
			}
			if strings.Contains(tag, "\\") {
				errs = append(errs, fmt.Sprintf("tags: tag %q must not contain '\\\\'", tag))
			}
			if strings.Contains(tag, "..") {
				errs = append(errs, fmt.Sprintf("tags: tag %q must not contain '..'", tag))
			}
		}
	}

	// proxy_url syntax check when configured
	if rawProxy := strings.TrimSpace(c.ProxyURL); rawProxy != "" {
		if u, err := url.Parse(rawProxy); err != nil || u.Scheme == "" {
			errs = append(errs, fmt.Sprintf("proxy_url: invalid URL %q", c.ProxyURL))
		}
	}

	// identity accepts only the two known values; an unknown value never falls back
	// silently (that would quietly change the access identity).
	identity := c.Telegram.EffectiveIdentity()
	switch identity {
	case IdentityTDDesktop, IdentityCustom:
	default:
		errs = append(errs, fmt.Sprintf(
			"telegram.identity: must be %q or %q, got %q",
			IdentityTDDesktop, IdentityCustom, c.Telegram.Identity))
	}

	// api_id and api_hash must be set as a pair (and are mandatory under "custom" identity).
	if c.Telegram.APIID < 0 {
		errs = append(errs, fmt.Sprintf("telegram.api_id: must be >= 0, got %d", c.Telegram.APIID))
	}
	hasAPIID := c.Telegram.APIID > 0
	hasAPIHash := strings.TrimSpace(c.Telegram.APIHash) != ""
	if hasAPIID != hasAPIHash {
		if hasAPIID {
			errs = append(errs, "telegram.api_id is set but telegram.api_hash is empty; they must be set together")
		} else {
			errs = append(errs, "telegram.api_hash is set but telegram.api_id is 0; they must be set together")
		}
	} else if identity == IdentityCustom && !hasAPIID {
		errs = append(errs, `telegram.identity is "custom" but telegram.api_id / telegram.api_hash are not set`)
	}

	// max_concurrency: [1, 16]
	if c.Bot.MaxConcurrency < 1 || c.Bot.MaxConcurrency > 16 {
		errs = append(errs, fmt.Sprintf("bot.max_concurrency: must be between 1 and 16, got %d", c.Bot.MaxConcurrency))
	}

	// threads: [1, 32]
	if c.Download.Threads < 1 || c.Download.Threads > 32 {
		errs = append(errs, fmt.Sprintf("download.threads: must be between 1 and 32, got %d", c.Download.Threads))
	}

	// file_concurrency >= 1
	if c.Download.FileConcurrency < 1 {
		errs = append(errs, fmt.Sprintf("download.file_concurrency: must be >= 1, got %d", c.Download.FileConcurrency))
	}

	// cache_dialogs_hours >= 0
	if c.Download.CacheDialogsHours < 0 {
		errs = append(errs, fmt.Sprintf("download.cache_dialogs_hours: must be >= 0, got %d", c.Download.CacheDialogsHours))
	}

	// include/exclude are mutually exclusive (guards against ambiguous filtering)
	if len(c.Download.Include) > 0 && len(c.Download.Exclude) > 0 {
		errs = append(errs, "download.include and download.exclude are mutually exclusive; set only one")
	}
	// min_size / max_size range
	if c.Download.MinSize < 0 {
		errs = append(errs, fmt.Sprintf("download.min_size: must be >= 0, got %d", c.Download.MinSize))
	}
	if c.Download.MaxSize < 0 {
		errs = append(errs, fmt.Sprintf("download.max_size: must be >= 0, got %d", c.Download.MaxSize))
	}
	if c.Download.MinSize > 0 && c.Download.MaxSize > 0 && c.Download.MinSize > c.Download.MaxSize {
		errs = append(errs, fmt.Sprintf("download.min_size (%d) must be <= download.max_size (%d)", c.Download.MinSize, c.Download.MaxSize))
	}
	// kinds value allowlist
	validKinds := map[string]struct{}{
		"video": {}, "audio": {}, "voice": {}, "photo": {},
		"document": {}, "sticker": {}, "gif": {},
	}
	for _, k := range c.Download.Kinds {
		if _, ok := validKinds[strings.ToLower(strings.TrimSpace(k))]; !ok {
			errs = append(errs, fmt.Sprintf(
				"download.kinds: unknown kind %q (must be video/audio/voice/photo/document/sticker/gif)", k))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}

// ValidationError aggregates several validation errors.
type ValidationError struct {
	Errors []string
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 1 {
		return fmt.Sprintf("config validation failed: %s", e.Errors[0])
	}
	return fmt.Sprintf("config validation failed (%d errors):\n  - %s",
		len(e.Errors), strings.Join(e.Errors, "\n  - "))
}
