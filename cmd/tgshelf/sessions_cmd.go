// sessions_cmd.go: tgshelf -sessions lists the active sessions of the current account.
//
// Why this exists (it is not "one more command for convenience"): in practice we saw
// Telegram Desktop on the PC being logged out after logging in with this project. The first fact
// needed when investigating that is "which sessions does the account have right now, is the PC
// still there", and before this the only way was digging through the phone settings.
// This command prints the account.getAuthorizations result directly, in order to:
//  1. compare before and after a login, to confirm whether a session really was removed server-side;
//  2. confirm which application/platform the server classifies this login as (see identity.go).
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/tg"
)

// sessionRow is a readable summary of one session (decoupled from the RPC type, so the formatting
// logic can be unit tested).
type sessionRow struct {
	Current       bool
	AppName       string
	DeviceModel   string
	Platform      string
	SystemVersion string
	APIID         int
	Official      bool
	DateCreated   int
	DateActive    int
}

// runSessions lists and prints the active sessions of the current account.
func runSessions(cfg *config.Config, configPath string) error {
	if err := preflightTelegram(cfg); err != nil {
		return err
	}

	sessionPath := sessionPathOf(cfg, configPath)
	if _, err := os.Stat(sessionPath); err != nil {
		return fmt.Errorf("not logged in: session file %s not found, run tgshelf -login first", sessionPath)
	}

	storage, err := tg.OpenSession(sessionPath)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = storage.Close() }()

	client, err := tg.New(cfg, storage)
	if err != nil {
		return fmt.Errorf("build Telegram client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var rows []sessionRow
	err = client.Run(ctx, func(ctx context.Context) error {
		auths, aerr := client.API().AccountGetAuthorizations(ctx)
		if aerr != nil {
			return aerr
		}
		for _, a := range auths.Authorizations {
			rows = append(rows, sessionRow{
				Current:       a.Current,
				AppName:       a.AppName,
				DeviceModel:   a.DeviceModel,
				Platform:      a.Platform,
				SystemVersion: a.SystemVersion,
				APIID:         a.APIID,
				Official:      a.OfficialApp,
				DateCreated:   a.DateCreated,
				DateActive:    a.DateActive,
			})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fetch session list: %w", err)
	}

	fmt.Print(formatSessions(rows))
	return nil
}

// formatSessions renders the session list as readable text. The current session comes first, the
// rest are ordered by most recent activity in descending order; the unit tests assert this output
// directly.
func formatSessions(rows []sessionRow) string {
	sorted := make([]sessionRow, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Current != sorted[j].Current {
			return sorted[i].Current
		}
		return sorted[i].DateActive > sorted[j].DateActive
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%d active sessions:\n", len(sorted))
	hasDesktop := false
	for _, r := range sorted {
		mark := "  "
		if r.Current {
			mark = "* "
		}
		official := ""
		if r.Official {
			official = " [official app]"
		}
		fmt.Fprintf(&b, "%s%-18s device=%-16s platform=%-8s system=%-14s api_id=%-10d created=%s%s\n",
			mark, r.AppName, r.DeviceModel, r.Platform, r.SystemVersion, r.APIID,
			formatUnix(r.DateCreated), official)
		if strings.Contains(strings.ToLower(r.AppName), "desktop") {
			hasDesktop = true
		}
	}
	if !hasDesktop {
		b.WriteString("\nNote: there is no Telegram Desktop session in the list.\n" +
			"If you expected the PC client to stay logged in, the server really did remove it (this project never calls any logout API,\n" +
			"see the notes on login identity and api_id in internal/tg/identity.go).\n")
	}
	return b.String()
}

// formatUnix formats unix seconds as local time; 0 is shown as "-".
func formatUnix(sec int) string {
	if sec <= 0 {
		return "-"
	}
	return time.Unix(int64(sec), 0).Format("2006-01-02 15:04")
}
