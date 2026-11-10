// Link extraction: pulls t.me message links out of user message text.
//
// This layer only cuts a clean URL out of free text; it does not parse or normalize - parsing belongs to
// internal/tg.ParseLink, and that function does not strip Chinese punctuation (a trailing
// ideographic full stop makes it fail), so fullwidth punctuation has to be excluded here as a
// terminator.
package bot

import (
	"regexp"
	"strings"
)

// linkRe matches t.me message links. The character class explicitly excludes:
//   - whitespace and the ASCII characters <>"' (the usual URL boundary convention);
//   - the fullwidth punctuation produced by Chinese input methods (fullwidth forms of the
//     comma, ideographic comma, full stop, semicolon, colon, exclamation mark, question mark,
//     parentheses, corner brackets, angle brackets and book-title marks). They often sit right
//     against the end of a link, and if they were not excluded they would be swallowed into the
//     URL and make the later ParseLink call fail. They act as terminators, not as characters to
//     be stripped.
//
// The scheme prefix is optional: users often paste "t.me/xxx/44" directly (this happened during
// real acceptance testing), and requiring https:// would treat such input as plain text and
// reply "no links found".
//
// The prefix group (^|[^0-9A-Za-z._/-]) exists so that "t.me/x" inside "example.com/t.me/x" is
// not mistaken for a link: RE2 has no lookbehind, so the boundary can only be expressed by
// matching the leading character and taking the submatch.
// The URL body additionally excludes CJK ranges (Han characters, CJK punctuation, fullwidth
// forms): Telegram usernames and message IDs are ASCII only, so without that exclusion text
// like "share t.me/durov/552" glued straight to a CJK word would swallow the word into the URL
// (any regex over free text has the same problem).
var linkRe = regexp.MustCompile(`(^|[^0-9A-Za-z._/-])((?:https?://)?t\.me/[^\s<>"'，。、；：！？（）【】《》\x{3000}-\x{303f}\x{4e00}-\x{9fff}\x{ff00}-\x{ffef}]+)`)

// trailingPunct is the set of ASCII punctuation that may be stripped from the end of a URL once
// it matches (e.g. the closing parenthesis in "(https://t.me/x/1)").
const trailingPunct = ".,;:!?)\"'<>]}"

// extractLinks pulls Telegram message links out of user text (deduplicated, order preserved,
// trailing punctuation stripped). It returns an empty slice rather than nil when nothing
// matches, so callers can range over it without a nil check.
func extractLinks(text string) []string {
	if text == "" {
		return []string{}
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	// Submatch 2 is the link itself; submatch 1 only expresses the boundary.
	for _, m := range linkRe.FindAllStringSubmatch(text, -1) {
		// Match Python's rstrip: strip set members from the end repeatedly.
		url := strings.TrimRight(m[2], trailingPunct)
		if url == "" {
			continue
		}
		if _, dup := seen[url]; dup {
			continue
		}
		seen[url] = struct{}{}
		out = append(out, url)
	}
	return out
}
