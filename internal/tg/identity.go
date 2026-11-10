// identity.go - login identity resolution.
//
// # Why this layer exists
//
// Telegram ties a session to the api_id that created it, and it derives the reported platform
// from that application. An unregistered or self-registered api_id is classified as
// platform="Web" (visible in account.getAuthorizations), while the official Telegram Desktop
// client lands in platform="Desktop" - the same application family as the user's desktop
// client. Sessions from an unrelated application can then displace the desktop session.
//
// Two identities are supported:
//   - "tdesktop" (default): the application credentials of the official Telegram Desktop
//     client, {AppID: 2040, AppHash: "b18441a1ff607e10a989891a5462e627"}, and the fixed
//     fingerprint {Desktop, Windows 10, 4.2.4 x64, LangPack: tdesktop};
//   - "custom": the api_id/api_hash the user registered at my.telegram.org, with neutral
//     device defaults (this project does not impersonate Telegram Desktop then).
//
// Which server rule exactly drops another session (a session slot per api_id, a cap on the
// number of active sessions, or the account's "terminate old sessions" setting) cannot be
// proven from client code, so this layer only picks the identity and does not speculate about
// the mechanism.
//
// # Risk
//
// api_id 2040 belongs to the official Telegram Desktop credentials, and a third-party client
// embedding them carries a ToS risk. The identity is therefore **configurable**
// ([telegram] identity = "tdesktop" | "custom"): it defaults to tdesktop for compatibility,
// and users can switch to their own api_id at any time.
package tg

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/gotd/td/telegram"

	"github.com/1368129224/tgshelf/internal/config"
)

// tdesktopAppID/tdexktopAppHash are the application credentials of the official Telegram
// Desktop client.
const (
	tdesktopAppID   = 2040
	tdesktopAppHash = "b18441a1ff607e10a989891a5462e627"
)

// Credentials are the application credentials finally used to build the client.
type Credentials struct {
	AppID   int
	AppHash string
}

// ResolveCredentials picks the application credentials according to [telegram] identity.
//
//   - "tdesktop" (default): always returns the official Telegram Desktop credentials and
//     ignores api_id/api_hash from the config (avoiding the undefined combination of "a TD
//     identity with your own credentials").
//   - "custom": uses api_id/api_hash from the config, both of which must be set together.
//
// An unknown value is a configuration error and is never silently defaulted.
func ResolveCredentials(tc config.TelegramConfig) (Credentials, error) {
	switch tc.EffectiveIdentity() {
	case config.IdentityTDDesktop:
		return Credentials{AppID: tdesktopAppID, AppHash: tdesktopAppHash}, nil
	case config.IdentityCustom:
		if tc.APIID == 0 || strings.TrimSpace(tc.APIHash) == "" {
			return Credentials{}, fmt.Errorf(
				"tg: telegram.identity=custom requires both api_id and api_hash (current api_id=%d)", tc.APIID)
		}
		return Credentials{AppID: tc.APIID, AppHash: strings.TrimSpace(tc.APIHash)}, nil
	default:
		return Credentials{}, fmt.Errorf(
			"tg: unknown telegram.identity %q (valid values: %q or %q)",
			tc.Identity, config.IdentityTDDesktop, config.IdentityCustom)
	}
}

// DeviceConfigFor assembles the device fingerprint (shared by the bot and the CLI path).
//
// Under the tdesktop identity, if the user did not explicitly configure device_model and
// friends, the official Telegram Desktop fingerprint is
// returned. Under the custom identity the user's config is kept, with neutral defaults when
// it is unset - this project deliberately does not impersonate Telegram Desktop.
func DeviceConfigFor(tc config.TelegramConfig) telegram.DeviceConfig {
	if tc.EffectiveIdentity() == config.IdentityTDDesktop && !tdesktopIdentityOverridden(tc) {
		return telegram.DeviceConfig{
			DeviceModel:    "Desktop",
			SystemVersion:  "Windows 10",
			AppVersion:     "4.2.4 x64",
			LangCode:       "en",
			SystemLangCode: "en-US",
			LangPack:       "tdesktop",
		}
	}

	d := telegram.DeviceConfig{
		DeviceModel:    strings.TrimSpace(tc.DeviceModel),
		SystemVersion:  strings.TrimSpace(tc.SystemVersion),
		AppVersion:     strings.TrimSpace(tc.AppVersion),
		LangPack:       strings.TrimSpace(tc.LangPack),
		SystemLangCode: "en",
		LangCode:       "en",
	}
	if d.DeviceModel == "" {
		d.DeviceModel = "tgshelf"
	}
	if d.SystemVersion == "" {
		d.SystemVersion = runtime.GOOS + "/" + runtime.GOARCH
	}
	if d.AppVersion == "" {
		d.AppVersion = "1.0"
	}
	return d
}

// tdesktopIdentityOverridden reports whether the user explicitly configured any device
// fingerprint field. As soon as one is configured, the user's config is respected instead of
// being overwritten with the TD fingerprint.
func tdesktopIdentityOverridden(tc config.TelegramConfig) bool {
	return strings.TrimSpace(tc.DeviceModel) != "" ||
		strings.TrimSpace(tc.SystemVersion) != "" ||
		strings.TrimSpace(tc.AppVersion) != ""
}
