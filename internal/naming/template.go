package naming

import (
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/1368129224/tgshelf/internal/fs"
)

// Fields holds all input fields available to template rendering.
// Note: this package must not import internal/tg, hence this dedicated input struct, which
// the caller fills in by converting Telegram message fields.
type Fields struct {
	DialogID     int64     // dialog ID (channel/chat/user)
	MessageID    int       // message ID
	MessageDate  time.Time // message send time
	FileName     string    // file name (from Telegram document metadata, untrusted!)
	FileCaption  string    // message caption (untrusted!)
	FileSize     int64     // file size (bytes)
	DownloadDate time.Time // download start time
}

// funcMap holds the whitelisted functions available to templates. Any function not
// listed here is an error (text/template fails at Parse time for unknown functions).
func funcMap() template.FuncMap {
	return template.FuncMap{
		// lower converts the string to lowercase.
		"lower": strings.ToLower,
		// upper converts the string to uppercase.
		"upper": strings.ToUpper,
		// replace(s, old, new) replaces all substrings.
		"replace": func(s, old, new string) string { return strings.ReplaceAll(s, old, new) },
		// filenamify sanitises an arbitrary string into a safe filename fragment (via
		// Sanitize).
		"filenamify": Sanitize,
		// truncate(s, n) truncates to at most n characters by rune (n<0 counts as 0).
		"truncate": func(s string, n int) string {
			if n < 0 {
				n = 0
			}
			r := []rune(s)
			if len(r) <= n {
				return s
			}
			return string(r[:n])
		},
		// date(t, layout) formats a time with the given Go layout.
		"date": func(t time.Time, layout string) string { return t.Format(layout) },
		// now returns the current time.
		"now": time.Now,
		// size formats a byte count as a human-readable string (reuses
		// internal/fs.HumanBytes).
		"size": fs.HumanBytes,
	}
}

// Render renders template tpl with text/template and Fields as the context
// (templates access {{ .FileName }} and friends directly).
//
// Safety behaviour:
//   - missingkey=error: an unknown field ({{ .NoSuchField }}) returns an error.
//   - whitelisted functions: an unknown function ({{ nosuchfunc .FileName }}) fails
//     at parse time.
//   - an empty (or whitespace-only) rendering returns an error, preventing empty
//     filenames.
func Render(tpl string, f Fields) (string, error) {
	t, err := template.New("naming").Option("missingkey=error").Funcs(funcMap()).Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("naming.Render: parse template %q: %w", tpl, err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, f); err != nil {
		return "", fmt.Errorf("naming.Render: execute template %q: %w", tpl, err)
	}
	out := sb.String()
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("naming.Render: template %q rendered an empty result", tpl)
	}
	return out, nil
}
