package bot

import (
	"path/filepath"
	"testing"

	"github.com/1368129224/tgshelf/internal/config"
)

// testConfig builds a default config (the config package's defaults() is unexported, so
// write the default TOML with WriteDefault and load it back).
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.toml")
	if err := config.WriteDefault(path); err != nil {
		t.Fatalf("write default config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// Users outside the allowlist are rejected.
func TestIsAllowedDeniedOutsideWhitelist(t *testing.T) {
	cfg := testConfig(t)
	cfg.Bot.AllowedUserIDs = []int64{100, 200}
	if isAllowed(cfg, 300) {
		t.Fatal("a user outside the allowlist must not be allowed")
	}
	if !isAllowed(cfg, 100) {
		t.Fatal("a user in the allowlist must be allowed")
	}
	if !isAllowed(cfg, 200) {
		t.Fatal("a user in the allowlist must be allowed")
	}
}

// An empty allowlist lets everyone through (contract: empty means unrestricted).
func TestIsAllowedEmptyWhitelistAllowsAll(t *testing.T) {
	cfg := testConfig(t)
	cfg.Bot.AllowedUserIDs = nil
	if !isAllowed(cfg, 99999) {
		t.Fatal("an empty allowlist must allow everyone")
	}
}

// A non-admin running an admin command is rejected: isAdmin only consults admin_user_ids.
func TestIsAdminGate(t *testing.T) {
	cfg := testConfig(t)
	cfg.Bot.AdminUserIDs = []int64{7}
	if isAdmin(cfg, 8) {
		t.Fatal("a non-admin must not be treated as an admin")
	}
	if !isAdmin(cfg, 7) {
		t.Fatal("a user in admin_user_ids must be an admin")
	}
	// Admin status is independent of the allowlist: even with an empty allowlist (everyone
	// allowed), admin commands still only recognise the admin list.
	if isAdmin(testConfig(t), 7) {
		t.Fatal("the default config must have no admins")
	}
}
