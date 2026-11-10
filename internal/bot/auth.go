// Authorization: two-tier checks (allowlist and admin) shared by message and callback handlers.
package bot

import (
	"github.com/1368129224/tgshelf/internal/config"
)

// isAllowed reports whether userID may use this bot.
//
// The semantics match the old Python implementation's handlers._is_allowed: an empty
// allowed_user_ids means no restriction (convenient for single-user local deployments),
// while a non-empty list allows only the users it contains.
// An empty allowlist lets everyone through, but New logs a WARN at startup (see bot.go).
func isAllowed(cfg *config.Config, userID int64) bool {
	allowed := cfg.Bot.AllowedUserIDs
	if len(allowed) == 0 {
		return true
	}
	for _, id := range allowed {
		if id == userID {
			return true
		}
	}
	return false
}

// isAdmin reports whether userID is an admin. Admins may run management commands such as
// /cancel_all; when allowed_user_ids is empty (unrestricted), admin status holds independently
// of the allowlist.
func isAdmin(cfg *config.Config, userID int64) bool {
	for _, id := range cfg.Bot.AdminUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}
