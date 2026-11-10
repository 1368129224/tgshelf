package naming

import (
	"strings"
	"unicode/utf8"
)

// maxNameBytes is the maximum length in UTF-8 bytes of a sanitised filename.
const maxNameBytes = 200

// windowsReserved holds the Windows reserved device names (case-insensitive).
// The forms with an extension (e.g. CON.txt) are rejected by Windows too, so a
// prefix is added to all of them.
var windowsReserved = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// Sanitize turns an arbitrary string into a safe single filename (no path
// separators, no illegal characters).
//
// Rules:
//  1. Strip path/hostile characters: / \ : * ? " < > |, control characters
//     (0x00-0x1F, 0x7F) and the BOM (U+FEFF).
//  2. Trim leading/trailing whitespace and dots (to block ".." traversal and
//     hidden files); collapse runs of whitespace into one space.
//  3. Truncate to 200 UTF-8 bytes without splitting a multi-byte character (it
//     falls back to a rune boundary).
//  4. Prepend "_" to Windows reserved names (CON/PRN/AUX/NUL/COM1-9/LPT1-9, including
//     extension forms such as CON.txt).
//  5. Return "_" when the result is empty.
func Sanitize(name string) string {
	// 1. Filter illegal characters (control characters and the BOM are dropped here)
	var b []rune
	for _, r := range name {
		if r == '\uFEFF' { // BOM
			continue
		}
		if r < 0x20 || r == 0x7F { // control character
			continue
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			continue
		}
		b = append(b, r)
	}

	// 2. Collapse runs of whitespace (space and other Unicode whitespace) into one space
	var sb []rune
	prevSpace := false
	for _, r := range b {
		if r == ' ' {
			if !prevSpace {
				sb = append(sb, ' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		sb = append(sb, r)
	}

	// Trim leading/trailing whitespace and dots
	s := strings.Trim(string(sb), " .")

	// 3. Truncate to 200 UTF-8 bytes (without splitting a multi-byte character)
	s = truncateBytes(s, maxNameBytes)

	// 4. Windows reserved names (including extension forms such as "CON.txt")
	if isWindowsReserved(s) {
		s = "_" + s
	}

	// 5. Empty result
	if s == "" {
		return "_"
	}
	return s
}

// truncateBytes truncates s to at most n UTF-8 bytes without splitting a multi-byte
// character: if byte n falls in the middle of a multi-byte character, it backs off
// to the rune boundary before that character.
// Sanitize walks its input rune by rune with range and its output is always valid
// UTF-8, so the only possible invalid case after truncation is "cut in the middle of
// a multi-byte character"; utf8.ValidString reports that as invalid, and backing off
// byte by byte reaches a complete rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// isWindowsReserved reports whether the stem of name (before the first '.') is a
// Windows reserved name.
// Windows ignores trailing spaces in the stem of a reserved name and its case, and
// so does this check.
func isWindowsReserved(name string) bool {
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	_, ok := windowsReserved[stem]
	return ok
}
