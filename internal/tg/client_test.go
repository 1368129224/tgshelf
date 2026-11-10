// client_test.go - argument validation and pure-logic tests for New/Client (no network).
// New only assembles objects and makes no network connection (gotd connects when Run is called).
package tg

import (
	"context"
	"testing"

	"github.com/gotd/td/tgerr"

	"github.com/1368129224/tgshelf/internal/config"
)

// validConfig returns the minimal config that passes New's validation. The session path points
// at t.TempDir.
func validConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Telegram: config.TelegramConfig{
			APIID:         123456,
			APIHash:       "0123456789abcdef0123456789abcdef",
			Session:       t.TempDir() + "/session.json",
			DeviceModel:   "tgshelf",
			SystemVersion: "Linux",
			AppVersion:    "1.0",
		},
	}
}

// TestNew_ValidConfig a valid config can construct a client (without touching the network).
func TestNew_ValidConfig(t *testing.T) {
	cfg := validConfig(t)
	c, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New(valid config) should not fail: %v", err)
	}
	if c == nil || c.Client == nil {
		t.Fatal("New should return a non-nil client")
	}
	// Pure-logic cleanup: Close releases the session lock and should be idempotent without errors.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("repeated Close should be idempotent: %v", err)
	}
}

// TestNew_NilConfig a nil config should fail.
func TestNew_NilConfig(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Fatal("nil config should fail")
	}
}

// TestNew_EmptyAPICredentials missing api_id / api_hash should fail.
//
// This only applies to the custom identity: the tdesktop identity uses the built-in TD
// credentials and never needs a user-supplied api_id (see identity.go).
func TestNew_EmptyAPICredentials(t *testing.T) {
	cfg := validConfig(t)
	cfg.Telegram.Identity = config.IdentityCustom
	cfg.Telegram.APIID = 0
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("api_id of 0 should fail under the custom identity")
	}

	cfg = validConfig(t)
	cfg.Telegram.Identity = config.IdentityCustom
	cfg.Telegram.APIHash = "   "
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("an empty api_hash should fail under the custom identity")
	}
}

// TestNew_EmptySessionPath an empty session path without an explicit storage should fail;
// passing storage explicitly means the session path is not required.
func TestNew_EmptySessionPath(t *testing.T) {
	cfg := validConfig(t)
	cfg.Telegram.Session = " "
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("an empty session path without explicit storage should fail")
	}

	store, err := OpenSession(t.TempDir() + "/s.json")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _ = store.Close() }()
	c, err := New(cfg, store)
	if err != nil || c == nil {
		t.Fatalf("with explicit storage the session path may be empty: err=%v", err)
	}
	_ = c.Close()
}

// TestNew_WithProxy New should construct a client under every proxy configuration (without
// connecting).
func TestNew_WithProxy(t *testing.T) {
	for _, tc := range []struct {
		name, proxy string
		wantErr     bool
	}{
		{"empty string, direct connection", "", false},
		{"whitespace only, direct connection", "   ", false},
		{"socks5", "socks5://127.0.0.1:1080", false},
		{"http", "http://127.0.0.1:8080", false},
		{"https", "https://proxy.example.com:3128", false},
		{"mtproxy", "mtproxy://127.0.0.1:443?secret=dd000102030405060708090a0b0c0d0e0f", false},
		{"unknown scheme", "ftp://127.0.0.1:21", true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.ProxyURL = tc.proxy
			_, err := New(cfg, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("proxy=%q err=%v, wantErr=%v", tc.proxy, err, tc.wantErr)
			}
		})
	}
}

// TestDeviceConfig_CustomIdentity custom identity: keep the configured device parameters and
// do not impersonate TD.
func TestDeviceConfig_CustomIdentity(t *testing.T) {
	cfg := validConfig(t)
	cfg.Telegram.Identity = config.IdentityCustom
	d := DeviceConfigFor(cfg.Telegram)
	if d.DeviceModel != "tgshelf" || d.SystemVersion != "Linux" || d.AppVersion != "1.0" {
		t.Fatalf("the configured device parameters should be kept, got %+v", d)
	}

	cfg.Telegram.DeviceModel = ""
	cfg.Telegram.SystemVersion = ""
	cfg.Telegram.AppVersion = ""
	d = DeviceConfigFor(cfg.Telegram)
	if d.DeviceModel == "Desktop" || d.SystemVersion == "Windows 10" || d.LangPack == "tdesktop" {
		t.Fatalf("the custom identity must not impersonate the Telegram Desktop fingerprint, got %+v", d)
	}
	if d.DeviceModel == "" || d.SystemVersion == "" || d.AppVersion == "" {
		t.Fatalf("the default device parameters should not be empty, got %+v", d)
	}
}

// TestClient_RunNilGuard Run/Close on a zero-value Client should return an error instead of
// panicking.
func TestClient_RunNilGuard(t *testing.T) {
	c := &Client{}
	if err := c.Run(context.Background(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("Run on an uninitialized client should return an error instead of panicking")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("nil-safe Close: %v", err)
	}
}

// TestIsUnauthorized recognizes 401-class errors.
func TestIsUnauthorized(t *testing.T) {
	if !tgerrIsUnauthorized(tgerr.New(401, "AUTH_KEY_UNREGISTERED")) {
		t.Fatal("401 should be treated as unauthorized")
	}
	if tgerrIsUnauthorized(tgerr.New(420, "FLOOD_WAIT_3")) {
		t.Fatal("420 should not be treated as unauthorized")
	}
}
