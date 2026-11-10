package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tgpkg "github.com/gotd/td/tg"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/tg"
)

// resolvedFixture builds a Resolved with two media items for the formatting unit tests.
func resolvedFixture() *tg.Resolved {
	return &tg.Resolved{
		Ref:   tg.MessageRef{Kind: tg.RefPublic, Username: "telegram", MsgID: 193},
		Peer:  &tgpkg.InputPeerChannel{ChannelID: 777000, AccessHash: 42},
		MsgID: 193,
		Items: []tg.MediaItem{
			{Kind: tg.MediaVideo, Name: "clip.mp4", Size: 15 * 1024 * 1024, DCID: 2, Mime: "video/mp4"},
			{Kind: tg.MediaPhoto, Name: "cover.jpg", Size: 512 * 1024, DCID: 2, Mime: "image/jpeg"},
		},
	}
}

// TestFormatResolved verifies the report output contains the kind/name/size/ID fragments.
func TestFormatResolved(t *testing.T) {
	out := formatResolved("https://t.me/telegram/193", resolvedFixture())
	for _, want := range []string{
		"https://t.me/telegram/193",
		"resolved",
		"channel:777000",
		"msg_id=193",
		"2 media items",
		"kind=video",
		"name=clip.mp4",
		"size=15.00 MiB",
		"mime=video/mp4",
		"kind=photo",
		"name=cover.jpg",
		"size=512.00 KiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report output is missing fragment %q, got:\n%s", want, out)
		}
	}
	t.Logf("sample output:\n%s", out)
}

// TestPeerLabel verifies peer type rendering.
func TestPeerLabel(t *testing.T) {
	cases := []struct {
		peer tgpkg.InputPeerClass
		want string
	}{
		{&tgpkg.InputPeerChannel{ChannelID: 1}, "channel:1"},
		{&tgpkg.InputPeerUser{UserID: 2}, "user:2"},
		{&tgpkg.InputPeerChat{ChatID: 3}, "chat:3"},
	}
	for _, c := range cases {
		if got := peerLabel(c.peer); got != c.want {
			t.Errorf("peerLabel = %q, want %q", got, c.want)
		}
	}
}

// TestSentinelName verifies sentinel error classification.
func TestSentinelName(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{tg.ErrNotTMeLink, "ErrNotTMeLink"},
		{tg.ErrChannelInaccessible, "ErrChannelInaccessible"},
		{tg.ErrMessageDeleted, "ErrMessageDeleted"},
		{tg.ErrNoMedia, "ErrNoMedia"},
		{os.ErrNotExist, "none"},
	}
	for _, c := range cases {
		if got := sentinelName(c.err); got != c.want {
			t.Errorf("sentinelName(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestPreflightTelegram verifies the error reported when api_id is missing under the custom
// identity.
// The tdesktop identity (the default) uses the built-in TD credentials and needs no user api_id,
// so identity=custom has to be set explicitly to cover the missing-credentials branch.
func TestPreflightTelegram(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.Identity = config.IdentityCustom
	err := preflightTelegram(cfg)
	if err == nil {
		t.Fatal("must return an error when both api_id and api_hash are missing")
	}
	for _, want := range []string{"https://my.telegram.org", "TG_API_ID", "TG_API_HASH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q, got: %v", want, err)
		}
	}
}

// TestResolveRequiresSession verifies the error reported when the session file does not exist.
func TestResolveRequiresSession(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tgshelf_config.toml")
	cfgText := "# test config\n" +
		"bot_token = \"123456:TEST-TOKEN\"\n" +
		"download_path = \"/tmp/tgshelf-test-downloads\"\n" +
		"tags = [\"video\"]\n" +
		"\n" +
		"[telegram]\n" +
		"api_id = 12345\n" +
		"api_hash = \"testhash\"\n" +
		"session = \"data/missing-session.json\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config must load: %v", err)
	}
	err = runResolve(parsed, cfgPath, []string{"https://t.me/telegram/193"})
	if err == nil {
		t.Fatal("must return an error when the session does not exist")
	}
	if !strings.Contains(err.Error(), "not logged in") ||
		!strings.Contains(err.Error(), "run tgshelf -login first") {
		t.Errorf("error must carry login guidance, got: %v", err)
	}
	if !strings.Contains(err.Error(), "missing-session.json") {
		t.Errorf("error must contain the session path, got: %v", err)
	}
}

// TestResolveLinkFlagParsing verifies that repeated -resolve flags and comma separation are
// equivalent.
func TestResolveLinkFlagParsing(t *testing.T) {
	var a resolveLinks
	for _, v := range []string{"https://t.me/a/1", "https://t.me/b/2,https://t.me/c/3"} {
		if err := a.Set(v); err != nil {
			t.Fatalf("Set(%q) must not fail: %v", v, err)
		}
	}
	if len(a.urls) != 3 {
		t.Fatalf("must parse 3 links, got %d: %v", len(a.urls), a.urls)
	}
	var b resolveLinks
	if err := b.Set("  https://t.me/x/9 , ,"); err != nil {
		t.Fatal(err)
	}
	if len(b.urls) != 1 || b.urls[0] != "https://t.me/x/9" {
		t.Errorf("whitespace and empty segments must be trimmed, got: %v", b.urls)
	}
}

// TestReadSecretPipe verifies the non-terminal fallback of readSecret: pipe input is read as a
// single line.
func TestReadSecretPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, werr := w.WriteString("abc\n"); werr != nil {
		t.Fatal(werr)
	}
	_ = w.Close()

	got, err := readSecret(r)
	if err != nil {
		t.Fatalf("readSecret must not fail: %v", err)
	}
	if got != "abc" {
		t.Errorf("readSecret = %q, want %q", got, "abc")
	}
}

// TestMaskPhone verifies log masking: only the last 4 digits appear.
func TestMaskPhone(t *testing.T) {
	if got := maskPhone("+8613800138000"); got != "****8000" {
		t.Errorf("maskPhone = %q, want ****8000", got)
	}
	if got := maskPhone("123"); got != "****" {
		t.Errorf("a too short phone number must be fully masked, got: %q", got)
	}
}

// TestLoginMissingAPIID verifies that runLogin returns an error when api_id is missing and sends
// no network request.
func TestLoginMissingAPIID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tgshelf_config.toml")
	// Only identity=custom asks for api_id; the default tdesktop identity uses the built-in TD
	// credentials, so without custom this case would really enter the login flow (which once fired
	// a real network request).
	cfgText := "# test config (no API credentials, explicit custom identity)\n" +
		"bot_token = \"123456:TEST-TOKEN\"\n" +
		"download_path = \"/tmp/tgshelf-test-downloads\"\n" +
		"tags = [\"video\"]\n" +
		"\n" +
		"[telegram]\n" +
		"identity = \"custom\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config must load: %v", err)
	}
	err = runLogin(parsed, cfgPath, false)
	if err == nil {
		t.Fatal("must return an error when api_id is missing")
	}
	for _, want := range []string{"https://my.telegram.org", "api_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q, got: %v", want, err)
		}
	}
}

// TestSessionPathOf verifies that a relative session path is expanded against the config file
// directory.
func TestSessionPathOf(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.Session = "data/session.json"
	got := sessionPathOf(cfg, "/etc/tgshelf/tgshelf_config.toml")
	want := filepath.Join("/etc/tgshelf", "data", "session.json")
	if got != want {
		t.Errorf("sessionPathOf = %q, want %q", got, want)
	}
	cfg.Telegram.Session = "/abs/session.json"
	if got := sessionPathOf(cfg, "/etc/tgshelf/x.toml"); got != "/abs/session.json" {
		t.Errorf("an absolute path must not be rewritten, got: %q", got)
	}
	cfg.Telegram.Session = ""
	if got := sessionPathOf(cfg, "/etc/tgshelf/x.toml"); !strings.HasSuffix(got, "data/session.json") {
		t.Errorf("an empty value must fall back to the default path, got: %q", got)
	}
}
