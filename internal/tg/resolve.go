// resolve.go - pure-function implementation of t.me link parsing (fully offline,
// no network calls at all).
//
// Scope (so later readers are not misled, the following is explicitly NOT
// done here):
//   - no -100 prefix conversion: <id> in t.me/c/<id> is kept as the raw number,
//     the conversion belongs to the later peer resolution (
//     internal/tg/peer.go);
//   - no access_hash cache lookup and no peer resolution either;
//   - no Chinese punctuation stripping (full-width marks such as U+FF0C and U+3002):
//     that is the job of the upstream extractLinks, and this function must reject
//     such input outright.
//
// Supported shapes (MsgID is always the last segment):
//  1. t.me/<username>/<msgID>                 -> RefPublic
//  2. t.me/c/<channelID>/<msgID>              -> RefPrivate (ChannelID keeps the raw number)
//  3. t.me/<username>/<topicID>/<msgID>       -> RefPublic (topicID dropped)
//  4. t.me/c/<channelID>/<topicID>/<msgID>    -> RefPrivate (topicID dropped)
//  5. any shape above + ?comment=<commentID>  -> RefComment (keeping the main channel info)
//  6. ?thread=<id> is always ignored and does not affect Kind or MsgID.
package tg

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// RefKind identifies the shape category of a link.
type RefKind int

const (
	// RefPublic marks a public channel/group/user link of the form
	// t.me/<username>/....
	RefPublic RefKind = iota
	// RefPrivate marks a private channel invite link of the form
	// t.me/c/<channelID>/....
	RefPrivate
	// RefComment marks a discussion group comment link: any shape above plus
	// ?comment=<id>.
	RefComment
)

// MessageRef is the result of link parsing and carries no network information
// (no peer, no access_hash).
type MessageRef struct {
	Kind      RefKind
	Username  string // non-empty only for RefPublic / RefComment(public)
	ChannelID int64  // non-zero only for RefPrivate / RefComment(private); raw t.me/c/<id> number, no -100 conversion
	MsgID     int    // main message ID (always present; a missing one is an error)
	CommentID int    // non-zero only for RefComment
}

// Sentinel errors: callers decide with errors.Is.
var (
	// ErrNotTMeLink: the hostname is not exactly t.me (including look-alike
	// domains such as telegram.me, t.me.evil.com and not-t.me, and inputs with
	// userinfo / a port that cannot be normalized to t.me).
	ErrNotTMeLink = errors.New("not a t.me link: hostname is not exactly t.me " +
		"(or contains userinfo/port and other illegal parts)")
	// ErrMissingMsgID: the message ID segment is missing (e.g. t.me/telegram, t.me/c/123).
	ErrMissingMsgID = errors.New("missing message ID segment")
	// ErrBadLinkSegments: the path segment count matches none of the supported shapes.
	ErrBadLinkSegments = errors.New("path segment count matches none of the supported shapes")
	// ErrBadUsername: the username charset or length does not satisfy ^[A-Za-z0-9_]{3,32}$.
	ErrBadUsername = errors.New("invalid username (must match ^[A-Za-z0-9_]{3,32}$)")
	// ErrBadNumericID: msgID / channelID / topicID / commentID is not a positive
	// integer (leading 0, negative, non-numeric, empty segment).
	ErrBadNumericID = errors.New("invalid numeric ID (must be a positive integer without a leading 0)")
)

// usernameMaxLen is the maximum Telegram username length (the minimum of 3 is
// enforced by the same regexp).
const usernameMaxLen = 32

// parsePosInt parses a segment that must be a positive integer: empty strings,
// non-numerics, a leading 0, negatives and 0 are rejected. The original text of
// the segment is included in the error so the message names the offending part.
func parsePosInt(seg string) (int, error) {
	if seg == "" {
		return 0, fmt.Errorf("%w: empty segment", ErrBadNumericID)
	}
	if len(seg) > 1 && seg[0] == '0' {
		return 0, fmt.Errorf("%w: segment %q has a leading 0", ErrBadNumericID, seg)
	}
	n, err := strconv.Atoi(seg)
	if err != nil {
		return 0, fmt.Errorf("%w: segment %q is not a number", ErrBadNumericID, seg)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%w: segment %q is not a positive integer", ErrBadNumericID, seg)
	}
	return n, nil
}

// validUsername validates the username charset and length: ^[A-Za-z0-9_]{3,32}$.
func validUsername(u string) bool {
	if len(u) < 3 || len(u) > usernameMaxLen {
		return false
	}
	for i := 0; i < len(u); i++ {
		c := u[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// ParseLink parses a t.me link and returns a MessageRef.
// raw accepts three entry forms: https://t.me/..., http://t.me/... and a bare
// t.me/...; any other hostname, userinfo or port returns ErrNotTMeLink.
// Error messages are readable and name the offending segment.
func ParseLink(raw string) (MessageRef, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return MessageRef{}, fmt.Errorf("%w: empty input", ErrNotTMeLink)
	}

	// Normalize the scheme: allow an https:// or http:// prefix, and a bare t.me/ start.
	s := trimmed
	switch {
	case strings.HasPrefix(s, "https://"):
		s = s[len("https://"):]
	case strings.HasPrefix(s, "http://"):
		s = s[len("http://"):]
	}

	// Split off the query and the fragment (the fragment is dropped; the query only
	// contributes comment/thread).
	var queryPart string
	if i := strings.IndexByte(s, '?'); i >= 0 {
		end := strings.IndexByte(s[i:], '#')
		if end < 0 {
			queryPart = s[i+1:]
			s = s[:i]
		} else {
			queryPart = s[i+1 : i+end]
			s = s[:i] + s[i+end:] // put back any path part that followed the fragment
		}
	}
	// Drop the fragment: '#' and everything after it is discarded.
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}

	// Split host and path. If anything else appears before the '/', (userinfo@,
	// :port, spaces, ...), the hostname cannot be normalized to exactly t.me.
	host := s
	path := ""
	if i := strings.IndexAny(s, "/"); i >= 0 {
		host, path = s[:i], s[i+1:]
	}
	if host != "t.me" {
		if strings.ContainsAny(host, "@: \t") || strings.Contains(host, " ") {
			return MessageRef{}, fmt.Errorf("%w: %q contains userinfo/port or illegal characters",
				ErrNotTMeLink, host)
		}
		return MessageRef{}, fmt.Errorf("%w: hostname is %q", ErrNotTMeLink, host)
	}

	// Parse comment and thread from the query (all other parameters are ignored).
	commentID := 0
	hasComment := false
	if queryPart != "" {
		q, err := url.ParseQuery(queryPart)
		if err != nil {
			return MessageRef{}, fmt.Errorf("%w: cannot parse query", ErrNotTMeLink)
		}
		if v, ok := q["comment"]; ok && len(v) > 0 && v[0] != "" {
			n, err := parsePosInt(v[0])
			if err != nil {
				return MessageRef{}, fmt.Errorf("comment=%q %w", v[0], err)
			}
			commentID = n
			hasComment = true
		}
		// thread=<id> is always ignored; nothing to do with it.
		_ = q["thread"]
	}

	// Split the path into segments on '/'. A single trailing '/' is allowed and
	// ignored (same as an empty last segment); any other empty segment (consecutive
	// '//' etc.) is an error.
	segs := strings.Split(path, "/")
	if n := len(segs); n > 0 && segs[n-1] == "" {
		segs = segs[:n-1]
	}
	for _, seg := range segs {
		if seg == "" {
			return MessageRef{}, fmt.Errorf("%w: consecutive '/' or an extra empty segment",
				ErrBadLinkSegments)
		}
	}

	// Segment count and shape: public takes 2/3 segments, c takes 3/4; anything
	// else is an error.
	var ref MessageRef
	switch {
	case len(segs) == 0:
		return MessageRef{}, fmt.Errorf("%w: no path segment after t.me", ErrMissingMsgID)
	case len(segs) == 1:
		// Only a username segment, no msgID: t.me/<username>.
		return MessageRef{}, fmt.Errorf("%w: t.me/%s is missing the message ID segment",
			ErrMissingMsgID, segs[0])
	case segs[0] == "c":
		// Private channel shape: c/<channelID>/<msgID> or c/<channelID>/<topicID>/<msgID>.
		if len(segs) < 3 {
			if len(segs) == 2 {
				return MessageRef{}, fmt.Errorf("%w: t.me/c is missing the message ID segment",
					ErrMissingMsgID)
			}
			return MessageRef{}, fmt.Errorf("%w: the t.me/c shape needs 3 or 4 segments, got %d",
				ErrBadLinkSegments, len(segs))
		}
		if len(segs) > 4 {
			return MessageRef{}, fmt.Errorf("%w: the t.me/c shape takes at most 4 segments, got %d",
				ErrBadLinkSegments, len(segs))
		}
		cid, err := strconv.ParseInt(segs[1], 10, 64)
		if err != nil {
			return MessageRef{}, fmt.Errorf("%w: channelID segment %q is not a valid integer",
				ErrBadNumericID, segs[1])
		}
		if cid <= 0 || (len(segs[1]) > 1 && segs[1][0] == '0') {
			return MessageRef{}, fmt.Errorf(
				"%w: channelID segment %q is not a positive integer (or has a leading 0)",
				ErrBadNumericID, segs[1])
		}
		msgID, err := parsePosInt(segs[len(segs)-1])
		if err != nil {
			return MessageRef{}, fmt.Errorf("msgID %w", err)
		}
		// With 3 segments there is no topicID; with 4 segments segs[2] is the topicID
		// and must be a positive integer (then dropped).
		if len(segs) == 4 {
			if _, err := parsePosInt(segs[2]); err != nil {
				return MessageRef{}, fmt.Errorf("topicID %w", err)
			}
		}
		ref = MessageRef{Kind: RefPrivate, ChannelID: cid, MsgID: msgID}
	case len(segs) >= 2 && len(segs) <= 3:
		// Public shape: <username>/<msgID> or <username>/<topicID>/<msgID>.
		if !validUsername(segs[0]) {
			return MessageRef{}, fmt.Errorf("%w: segment %q does not satisfy ^[A-Za-z0-9_]{3,32}$",
				ErrBadUsername, segs[0])
		}
		msgID, err := parsePosInt(segs[len(segs)-1])
		if err != nil {
			return MessageRef{}, fmt.Errorf("msgID %w", err)
		}
		// With 3 segments segs[1] is the topicID and must be a positive integer (then
		// dropped).
		if len(segs) == 3 {
			if _, err := parsePosInt(segs[1]); err != nil {
				return MessageRef{}, fmt.Errorf("topicID %w", err)
			}
		}
		ref = MessageRef{Kind: RefPublic, Username: segs[0], MsgID: msgID}
	default:
		return MessageRef{}, fmt.Errorf("%w: the t.me shape needs 2 or 3 segments, got %d",
			ErrBadLinkSegments, len(segs))
	}

	if hasComment {
		ref.Kind = RefComment
		ref.CommentID = commentID
	}
	return ref, nil
}
