package config

import "strings"

// Config is the single configuration struct of the whole program and maps one-to-one to
// docs/configuration.md.
// Every field is exported and carries a toml tag. Business fields never travel through a
// map[string]interface{}.
type Config struct {
	Debug        bool     `toml:"debug"`
	BotToken     string   `toml:"bot_token"`
	DownloadPath string   `toml:"download_path"`
	Tags         []string `toml:"tags"`
	ProxyURL     string   `toml:"proxy_url"`

	Telegram TelegramConfig `toml:"telegram"`
	Bot      BotConfig      `toml:"bot"`
	Download DownloadConfig `toml:"download"`
}

// TelegramConfig corresponds to the [telegram] section.
type TelegramConfig struct {
	APIID   int    `toml:"api_id"`
	APIHash string `toml:"api_hash"`
	// Identity selects the login identity, which decides where api_id/api_hash and the
	// device fingerprint come from:
	//   "tdesktop" — reuse the official Telegram Desktop app identity (api_id 2040) and the
	//                TD device fingerprint, matching what tdl does;
	//   "custom"   — use api_id/api_hash and the device_model and other fields here.
	// Empty means "tdesktop". See identity.go for the reasoning.
	Identity      string `toml:"identity"`
	Session       string `toml:"session"`
	DeviceModel   string `toml:"device_model"`
	SystemVersion string `toml:"system_version"`
	AppVersion    string `toml:"app_version"`
	LangPack      string `toml:"lang_pack"`
}

// IdentityTDDesktop and IdentityCustom are the legal values of TelegramConfig.Identity.
const (
	IdentityTDDesktop = "tdesktop"
	IdentityCustom    = "custom"
)

// EffectiveIdentity returns the identity actually in effect (tdesktop when Identity is empty).
func (t TelegramConfig) EffectiveIdentity() string {
	v := strings.ToLower(strings.TrimSpace(t.Identity))
	if v == "" {
		return IdentityTDDesktop
	}
	return v
}

// BotConfig corresponds to the [bot] section.
type BotConfig struct {
	MaxConcurrency int     `toml:"max_concurrency"`
	AllowedUserIDs []int64 `toml:"allowed_user_ids"`
	AdminUserIDs   []int64 `toml:"admin_user_ids"`
}

// DownloadConfig corresponds to the [download] section.
type DownloadConfig struct {
	Threads           int      `toml:"threads"`
	FileConcurrency   int      `toml:"file_concurrency"`
	SkipSame          bool     `toml:"skip_same"`
	RewriteExt        bool     `toml:"rewrite_ext"`
	Desc              bool     `toml:"desc"`
	Takeout           bool     `toml:"takeout"`
	Include           []string `toml:"include"`
	Exclude           []string `toml:"exclude"`
	Template          string   `toml:"template"`
	KeepParts         bool     `toml:"keep_parts"`
	CacheDialogsHours int      `toml:"cache_dialogs_hours"`
	// MinSize is the minimum file size in bytes (<=0 means no limit).
	MinSize int64 `toml:"min_size"`
	// MaxSize is the maximum file size in bytes (<=0 means no limit).
	MaxSize int64 `toml:"max_size"`
	// Kinds lists the allowed media kinds (video/audio/voice/photo/document/sticker/gif;
	// empty means all).
	Kinds []string `toml:"kinds"`
}

// defaults returns the built-in default configuration.
func defaults() *Config {
	return &Config{
		Debug:        false,
		BotToken:     "",
		DownloadPath: "./downloads",
		Tags:         []string{"video", "music", "doc"},
		ProxyURL:     "",
		Telegram: TelegramConfig{
			APIID:    0,
			APIHash:  "",
			Identity: IdentityTDDesktop,
			Session:  "data/session.json",
			// The device fingerprint stays **empty** by default: only an empty value means
			// "not explicitly configured", which lets identity=tdesktop use the fixed TD
			// fingerprint and lets tg.DeviceConfigFor fall back to tgshelf/<platform>/1.0
			// for identity=custom.
			// Writing "tgshelf"/"Linux"/"1.0" here would make the tdesktop identity look
			// like "the user configured device fields explicitly", so the TD fingerprint
			// would never be used.
			DeviceModel:   "",
			SystemVersion: "",
			AppVersion:    "",
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
			Template:          "{{ .DialogID }}_{{ .MessageID }}_{{ filenamify .FileName }}",
			KeepParts:         false,
			CacheDialogsHours: 6,
		},
	}
}
