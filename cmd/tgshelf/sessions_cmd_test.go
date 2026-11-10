package main

import (
	"strings"
	"testing"
)

// TestFormatSessions_CurrentFirstAndDesktopWarn checks that:
//   - the current session comes first and carries the "*" marker;
//   - a clear note appears when the list has no Telegram Desktop session (exactly the scenario the
//     user hit).
func TestFormatSessions_CurrentFirstAndDesktopWarn(t *testing.T) {
	rows := []sessionRow{
		{AppName: "Telegram iOS", DeviceModel: "iPhone 15 Pro", Platform: "iOS", APIID: 1, Official: true, DateActive: 300},
		{AppName: "myapp", DeviceModel: "tgshelf", Platform: "Web", APIID: 1234567, Current: true, DateActive: 100},
	}
	out := formatSessions(rows)

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.Contains(lines[1], "* ") {
		t.Fatalf("the first row must be the current session (with the * marker), got: %q", lines[1])
	}
	if !strings.Contains(lines[1], "tgshelf") {
		t.Fatalf("the current session must be tgshelf, got: %q", lines[1])
	}
	if !strings.Contains(out, "no Telegram Desktop session") {
		t.Fatalf("must warn when the TD session is missing, got: %s", out)
	}
	if !strings.Contains(out, "[official app]") {
		t.Fatalf("the official app must carry a marker, got: %s", out)
	}
}

// TestFormatSessions_HasDesktopNoWarn: no warning when a TD session is present.
func TestFormatSessions_HasDesktopNoWarn(t *testing.T) {
	out := formatSessions([]sessionRow{
		{AppName: "Telegram Desktop", DeviceModel: "Desktop", Platform: "Windows", APIID: 2040, Official: true, Current: true},
	})
	if strings.Contains(out, "no Telegram Desktop session") {
		t.Fatalf("must not warn when a TD session is present, got: %s", out)
	}
}

// TestFormatSessions_Empty: an empty list still has to produce sensible output without panicking.
func TestFormatSessions_Empty(t *testing.T) {
	if out := formatSessions(nil); !strings.Contains(out, "0 active sessions") {
		t.Fatalf("unexpected output for an empty list: %s", out)
	}
}

// TestFormatUnix: 0 and negative values are shown as "-".
func TestFormatUnix(t *testing.T) {
	if got := formatUnix(0); got != "-" {
		t.Errorf("formatUnix(0) = %q, want \"-\"", got)
	}
	if got := formatUnix(-5); got != "-" {
		t.Errorf("formatUnix(-5) = %q, want \"-\"", got)
	}
	if got := formatUnix(1700000000); got == "-" || got == "" {
		t.Errorf("formatUnix(1700000000) = %q, must be a readable time", got)
	}
}
