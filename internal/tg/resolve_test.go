// resolve_test.go - unit tests for ParseLink (all offline, no network).
//
// Covers shapes 1-6 (see the top comment of resolve.go) and every strictness
// requirement: each t.Run subtest corresponds to one explicit requirement of the brief.
package tg

import (
	"errors"
	"testing"
)

// TestParseLinkForms - correctness of shapes 1-4.
func TestParseLinkForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want MessageRef
	}{
		{"shape1_public_two_segments_https", "https://t.me/durov/57",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
		{"shape1_public_two_segments_http", "http://t.me/durov/57",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
		{"shape1_public_two_segments_bare_domain", "t.me/durov/57",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
		{"shape2_private_two_numbers_no_-100_conversion", "https://t.me/c/1234567890/42",
			MessageRef{Kind: RefPrivate, ChannelID: 1234567890, MsgID: 42}},
		{"shape3_public_topic_three_segments", "https://t.me/durov/12/57",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
		{"shape4_private_topic_four_segments", "https://t.me/c/1234567890/12/57",
			MessageRef{Kind: RefPrivate, ChannelID: 1234567890, MsgID: 57}},
		{"shape1_username_with_underscore_and_mixed_case", "t.me/Test_User_1/1",
			MessageRef{Kind: RefPublic, Username: "Test_User_1", MsgID: 1}},
		{"shape2_single_trailing_slash", "t.me/c/999/1/",
			MessageRef{Kind: RefPrivate, ChannelID: 999, MsgID: 1}},
		{"shape1_single_trailing_slash", "t.me/durov/57/",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
		{"shape3_single_trailing_slash", "t.me/durov/12/57/",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseLink(c.raw)
			if err != nil {
				t.Fatalf("ParseLink(%q) returned an error: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("ParseLink(%q) = %+v, want %+v", c.raw, got, c.want)
			}
		})
	}
}

// TestParseLinkComment - shape 5: any shape plus ?comment=<id> yields Kind=RefComment
// while keeping the main channel's Username/ChannelID/MsgID and filling CommentID.
func TestParseLinkComment(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want MessageRef
	}{
		{"shape5_public_link_with_comment", "https://t.me/durov/57?comment=100",
			MessageRef{Kind: RefComment, Username: "durov", MsgID: 57, CommentID: 100}},
		{"shape5_private_link_with_comment", "https://t.me/c/1234567890/42?comment=200",
			MessageRef{Kind: RefComment, ChannelID: 1234567890, MsgID: 42, CommentID: 200}},
		{"shape5_public_topic_shape_with_comment", "t.me/durov/12/57?comment=300",
			MessageRef{Kind: RefComment, Username: "durov", MsgID: 57, CommentID: 300}},
		{"shape5_private_topic_shape_with_comment", "t.me/c/1234567890/12/57?comment=400",
			MessageRef{Kind: RefComment, ChannelID: 1234567890, MsgID: 57, CommentID: 400}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseLink(c.raw)
			if err != nil {
				t.Fatalf("ParseLink(%q) returned an error: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("ParseLink(%q) = %+v, want %+v", c.raw, got, c.want)
			}
		})
	}
}

// TestParseLinkQueryTolerance - shape 6 and leniency: thread is always ignored; other
// unrelated query parameters and fragments must not cause errors.
func TestParseLinkQueryTolerance(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want MessageRef
	}{
		{"shape6_thread_ignored_public", "https://t.me/durov/57?thread=999",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
		{"shape6_thread_ignored_kind_and_msgid_unchanged", "https://t.me/durov/57?comment=100&thread=999",
			MessageRef{Kind: RefComment, Username: "durov", MsgID: 57, CommentID: 100}},
		{"unrelated_param_single_ignored", "https://t.me/durov/57?single",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
		{"fragment_ignored", "https://t.me/durov/57#anchor",
			MessageRef{Kind: RefPublic, Username: "durov", MsgID: 57}},
		{"query_and_fragment_together", "https://t.me/durov/57?comment=5#x",
			MessageRef{Kind: RefComment, Username: "durov", MsgID: 57, CommentID: 5}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseLink(c.raw)
			if err != nil {
				t.Fatalf("ParseLink(%q) returned an error: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("ParseLink(%q) = %+v, want %+v", c.raw, got, c.want)
			}
		})
	}
}

// TestParseLinkHostStrictness - hostname strictness: only exactly t.me is accepted;
// look-alike domains, userinfo and ports are rejected (all ErrNotTMeLink).
func TestParseLinkHostStrictness(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"lookalike_domain_telegram.me", "https://telegram.me/durov/57"},
		{"suffix_spoof_t.me.evil.com", "https://t.me.evil.com/durov/57"},
		{"prefix_spoof_not-t.me", "https://not-t.me/durov/57"},
		{"bare_domain_prefix_spoof_not-t.me", "not-t.me/durov/57"},
		{"reject_userinfo", "https://evil@t.me/x/1"},
		{"reject_port_with_scheme", "https://t.me:8080/x/1"},
		{"reject_port_bare_domain", "t.me:8080/x/1"},
		{"reject_empty_input", ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseLink(c.raw)
			if err == nil {
				t.Fatalf("ParseLink(%q) expected an error, got success", c.raw)
			}
			if !errors.Is(err, ErrNotTMeLink) {
				t.Fatalf("ParseLink(%q) expected ErrNotTMeLink, got %v", c.raw, err)
			}
		})
	}
}

// TestParseLinkFullwidthPunctuation - explicitly no Chinese punctuation stripping:
// input carrying full-width punctuation must fail (stripping is the job of the upstream
// extractLinks).
func TestParseLinkFullwidthPunctuation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"trailing_fullwidth_period", "https://t.me/durov/57。"},
		{"trailing_fullwidth_comma", "https://t.me/durov/57，"},
		{"trailing_fullwidth_period_private_shape", "https://t.me/c/123/57。"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseLink(c.raw)
			if err == nil {
				t.Fatalf("ParseLink(%q) expected an error because of the full-width punctuation, got success", c.raw)
			}
			// The full-width punctuation sits inside the last segment (msgID) and lands in
			// ErrBadNumericID; if it is caught before the segment-count/host checks it may
			// be another sentinel, but success is never allowed.
		})
	}
}

// TestParseLinkMissingMsgID - a missing msgID must fail (ErrMissingMsgID) and must never
// return a successful MsgID=0 result.
func TestParseLinkMissingMsgID(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"public_shape_username_only", "t.me/telegram"},
		{"private_shape_missing_msgid", "t.me/c/123"},
		{"bare_domain_root_path", "t.me"},
		{"root_path_with_scheme", "https://t.me/"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ref, err := ParseLink(c.raw)
			if err == nil {
				t.Fatalf("ParseLink(%q) expected an error, got %+v (MsgID=0 leak)", c.raw, ref)
			}
			if !errors.Is(err, ErrMissingMsgID) {
				t.Fatalf("ParseLink(%q) expected ErrMissingMsgID, got %v", c.raw, err)
			}
			if ref.MsgID != 0 {
				t.Fatalf("ParseLink(%q) must not carry MsgID=%d on failure", c.raw, ref.MsgID)
			}
		})
	}
}

// TestParseLinkNumericStrictness - numeric segment strictness: a leading 0, negatives,
// non-numerics and empty segments all fail (ErrBadNumericID or ErrBadLinkSegments,
// depending on where the empty segment is).
func TestParseLinkNumericStrictness(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantSent error
	}{
		{"msgID_leading_0", "t.me/durov/01", ErrBadNumericID},
		{"msgID_negative", "t.me/durov/-1", ErrBadNumericID},
		{"msgID_not_a_number", "t.me/durov/abc", ErrBadNumericID},
		{"msgID_zero", "t.me/durov/0", ErrBadNumericID},
		{"channelID_leading_0", "t.me/c/00123/1", ErrBadNumericID},
		{"channelID_negative", "t.me/c/-100/1", ErrBadNumericID},
		{"channelID_not_a_number", "t.me/c/abc/1", ErrBadNumericID},
		{"topicID_not_a_number", "t.me/durov/abc/57", ErrBadNumericID},
		{"topicID_leading_0", "t.me/durov/007/57", ErrBadNumericID},
		{"private_topicID_not_a_number", "t.me/c/123/abc/57", ErrBadNumericID},
		{"consecutive_slashes_empty_segment", "t.me//durov/57", ErrBadLinkSegments},
		{"private_shape_consecutive_slashes", "t.me/c//1", ErrBadLinkSegments},
		{"commentID_not_a_number", "t.me/durov/57?comment=abc", ErrBadNumericID},
		{"commentID_leading_0", "t.me/durov/57?comment=01", ErrBadNumericID},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseLink(c.raw)
			if err == nil {
				t.Fatalf("ParseLink(%q) expected an error, got success", c.raw)
			}
			if !errors.Is(err, c.wantSent) {
				t.Fatalf("ParseLink(%q) expected %v, got %v", c.raw, c.wantSent, err)
			}
		})
	}
}

// TestParseLinkSegmentCount - segment counts: public allows only 2/3, c allows only 3/4,
// anything else fails (ErrBadLinkSegments).
func TestParseLinkSegmentCount(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantSent error
	}{
		{"public_4_segments", "t.me/durov/1/2/3", ErrBadLinkSegments},
		{"private_5_segments", "t.me/c/1/2/3/4", ErrBadLinkSegments},
		{"private_2_segments_c_id_missing_msgid", "t.me/c/123", ErrMissingMsgID},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseLink(c.raw)
			if err == nil {
				t.Fatalf("ParseLink(%q) expected an error, got success", c.raw)
			}
			if !errors.Is(err, c.wantSent) {
				t.Fatalf("ParseLink(%q) expected %v, got %v", c.raw, c.wantSent, err)
			}
		})
	}
}

// TestParseLinkUsernameStrictness - username charset ^[A-Za-z0-9_]{3,32}$.
func TestParseLinkUsernameStrictness(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"too_short_2_chars", "t.me/ab/1"},
		{"too_long_33_chars", "t.me/abcdefghijklmnopqrstuvwxyz0123456/1"},
		{"contains_hyphen", "t.me/bad-name/1"},
		{"contains_dot", "t.me/bad.name/1"},
		{"contains_Chinese", "t.me/频道名/1"},
		{"reserved_segment_c_goes_private_not_username", "t.me/c/123/57"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseLink(c.raw)
			if c.name == "reserved_segment_c_goes_private_not_username" {
				// t.me/c/123/57 is a legal private shape and must not fail.
				if err != nil {
					t.Fatalf("ParseLink(%q) expected no error, got %v", c.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseLink(%q) expected an error, got success", c.raw)
			}
			if !errors.Is(err, ErrBadUsername) {
				t.Fatalf("ParseLink(%q) expected ErrBadUsername, got %v", c.raw, err)
			}
		})
	}
}

// TestParseLinkErrSentinels - the three required sentinel errors are non-nil and
// distinguishable from each other.
func TestParseLinkErrSentinels(t *testing.T) {
	if ErrNotTMeLink == nil || ErrMissingMsgID == nil || ErrBadLinkSegments == nil {
		t.Fatal("sentinel errors must not be nil")
	}
	if errors.Is(ErrNotTMeLink, ErrMissingMsgID) || errors.Is(ErrMissingMsgID, ErrBadLinkSegments) {
		t.Fatal("sentinel errors must not match each other with errors.Is")
	}
}
