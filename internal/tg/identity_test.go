package tg

import (
	"strings"
	"testing"

	"github.com/1368129224/tgshelf/internal/config"
)

// TestResolveCredentials_TDDesktop: the default identity must use the official TD
// credentials and ignore api_id/api_hash from the config (otherwise it would combine "a TD
// identity with your own credentials").
func TestResolveCredentials_TDDesktop(t *testing.T) {
	// Empty means the default tdesktop; case and surrounding whitespace are normalized
	// (EffectiveIdentity does ToLower+TrimSpace).
	for _, identity := range []string{"", "tdesktop", "TDesktop", "  tdesktop  "} {
		tc := config.TelegramConfig{Identity: identity, APIID: 1234567, APIHash: "userhash"}
		got, err := ResolveCredentials(tc)
		if err != nil {
			t.Fatalf("identity=%q: %v", identity, err)
		}
		if got.AppID != tdesktopAppID || got.AppHash != tdesktopAppHash {
			t.Fatalf("identity=%q should use TD credentials, got %+v", identity, got)
		}
	}
}

// TestResolveCredentials_Custom: the custom identity uses the configured credentials.
func TestResolveCredentials_Custom(t *testing.T) {
	tc := config.TelegramConfig{Identity: "custom", APIID: 1234567, APIHash: " userhash "}
	got, err := ResolveCredentials(tc)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.AppID != 1234567 || got.AppHash != "userhash" {
		t.Fatalf("custom should use the configured credentials (trimmed), got %+v", got)
	}
}

// TestResolveCredentials_CustomMissing: the custom identity without credentials must fail
// instead of falling back to TD.
func TestResolveCredentials_CustomMissing(t *testing.T) {
	if _, err := ResolveCredentials(config.TelegramConfig{Identity: "custom"}); err == nil {
		t.Fatal("custom without credentials should fail")
	}
	if _, err := ResolveCredentials(config.TelegramConfig{Identity: "custom", APIID: 123}); err == nil {
		t.Fatal("custom without api_hash should fail")
	}
}

// TestResolveCredentials_Unknown: an unknown identity is never silently defaulted.
func TestResolveCredentials_Unknown(t *testing.T) {
	_, err := ResolveCredentials(config.TelegramConfig{Identity: "telethon"})
	if err == nil {
		t.Fatal("unknown identity should fail")
	}
	if !strings.Contains(err.Error(), "telethon") {
		t.Fatalf("error should contain the invalid value, got %v", err)
	}
}

// TestDeviceConfig_TDDesktop: with no explicit device fields, the TD fingerprint matching
// tdl is used.
func TestDeviceConfig_TDDesktop(t *testing.T) {
	d := DeviceConfigFor(config.TelegramConfig{})
	if d.DeviceModel != "Desktop" || d.SystemVersion != "Windows 10" ||
		d.AppVersion != "4.2.4 x64" || d.LangPack != "tdesktop" {
		t.Fatalf("the tdesktop identity should use the TD fingerprint, got %+v", d)
	}
}

// TestDeviceConfig_TDDesktopRespectsOverride: explicitly configured device fields must be
// respected, not overwritten.
func TestDeviceConfig_TDDesktopRespectsOverride(t *testing.T) {
	d := DeviceConfigFor(config.TelegramConfig{DeviceModel: "MyDevice"})
	if d.DeviceModel != "MyDevice" {
		t.Fatalf("the configured device_model should be respected, got %+v", d)
	}
	if d.LangPack != "en" && d.LangPack != "" {
		t.Fatalf("the override branch should not carry the tdesktop LangPack, got %+v", d)
	}
}
