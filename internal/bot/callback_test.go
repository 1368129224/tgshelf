package bot

import (
	"strings"
	"testing"
)

// Round-trip encoding: all six actions verified one by one.
func TestCallbackRoundTrip(t *testing.T) {
	cases := []Callback{
		{Action: ActionSelect, ShortID: "0a1b2c3d", Page: 2, TagIdx: 17},
		{Action: ActionCancelSelect, ShortID: "deadbeef"},
		{Action: ActionPage, ShortID: "0a1b2c3d", Page: 3},
		{Action: ActionCancelTask, TaskID: "task-123"},
		{Action: ActionRetry, TaskID: "task-456"},
		{Action: ActionNoop},
	}
	for _, want := range cases {
		s, err := want.Encode()
		if err != nil {
			t.Fatalf("Encode(%+v): %v", want, err)
		}
		if len(s) > CallbackDataMaxBytes {
			t.Fatalf("Encode(%+v) = %q is over the limit at %d bytes", want, s, len(s))
		}
		got, err := ParseCallback(s)
		if err != nil {
			t.Fatalf("ParseCallback(%q): %v", s, err)
		}
		if got != want {
			t.Fatalf("round trip mismatch: want %+v got %+v (%q)", want, got, s)
		}
	}
}

// CJK tags plus long URLs: tags go by index and links by short ID, so the encoding must stay
// within 64 bytes.
func TestCallbackChineseTagLongURLWithin64Bytes(t *testing.T) {
	// Short IDs are always 8 hex digits; use large page/index values to simulate the boundary.
	cb := Callback{Action: ActionSelect, ShortID: "ffffffff", Page: 999, TagIdx: 9999}
	s, err := cb.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(s) > CallbackDataMaxBytes {
		t.Fatalf("select encoding is %d bytes, over the limit: %q", len(s), s)
	}
	// Even a very long taskID (the extreme case where a long URL is used as the ID directly)
	// must fail loudly rather than silently overflow.
	longID := strings.Repeat("x", 100)
	if _, err := (Callback{Action: ActionCancelTask, TaskID: longID}).Encode(); err == nil {
		t.Fatal("Encode must fail for an over-long TaskID")
	}
}

// MustEncode must panic past the limit (asserted via recover).
func TestMustEncodePanicsWhenTooLong(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustEncode did not panic past the limit")
		}
	}()
	MustEncode(Callback{Action: ActionCancelTask, TaskID: strings.Repeat("x", 100)})
}

// Encode returns ErrCallbackDataTooLong past the limit.
func TestEncodeTooLongReturnsError(t *testing.T) {
	_, err := (Callback{Action: ActionCancelTask, TaskID: strings.Repeat("x", 100)}).Encode()
	if err == nil {
		t.Fatal("Encode did not fail past the limit")
	}
	if !strings.Contains(err.Error(), "64") {
		t.Fatalf("the error message must mention the 64-byte limit: %v", err)
	}
}

// Parsing invalid input must return an error rather than panic.
func TestParseCallbackInvalid(t *testing.T) {
	bad := []string{"", "zz:1", "t:1:2", "t:a:b:c", "p:abc", "x:", "c:", "r:", "t:a:1:x"}
	for _, s := range bad {
		if _, err := ParseCallback(s); err == nil {
			t.Fatalf("ParseCallback(%q) must fail", s)
		}
	}
}
