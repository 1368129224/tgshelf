// filter.go - pre-download filtering: filtering by extension / size / kind.
//
// Semantics:
//   - include/exclude are decided by the **filename extension** (not the MIME type) and the
//     two are mutually exclusive;
//   - users may write "mp4" or ".mp4"; internally the dot is stripped and the value lowercased;
//   - size uses the min_size/max_size byte thresholds (boundaries included);
//   - kinds filters by media kind (video/audio/voice/photo/document/sticker/gif).
//
// Filtering happens after Pipeline.ResolveLink and before the download; filtered entries are
// only counted, never enter the TaskRunner, and the task result tells the user "skipped N".
package tg

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// Filter filters entries before a download. All-zero means no filtering (everything passes).
type Filter struct {
	// Include is the extension allowlist (dot stripped, lowercase, e.g. {"mp4","mkv"}). When
	// non-empty only matching entries are downloaded.
	Include map[string]struct{}
	// Exclude is the extension denylist (dot stripped, lowercase). Matches are skipped.
	// Mutually exclusive with Include.
	Exclude map[string]struct{}
	// MinSize is the minimum number of bytes (<=0 means unlimited).
	MinSize int64
	// MaxSize is the maximum number of bytes (<=0 means unlimited).
	MaxSize int64
	// Kinds is the set of allowed media kinds (empty means all kinds).
	Kinds map[MediaKind]bool
}

// NewFilterFromLists builds filter sets from the configured extension lists (dot stripped,
// lowercased, empty entries dropped). A non-empty include together with a non-empty exclude
// returns an error (mutually exclusive, avoiding an ambiguous meaning).
func NewFilterFromLists(include, exclude []string) (inc, exc map[string]struct{}, err error) {
	if len(include) > 0 && len(exclude) > 0 {
		return nil, nil, fmt.Errorf("filter: download.include and download.exclude are mutually exclusive")
	}
	norm := func(list []string) map[string]struct{} {
		m := make(map[string]struct{}, len(list))
		for _, ext := range list {
			e := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ext), ".")))
			if e != "" {
				m[e] = struct{}{}
			}
		}
		return m
	}
	return norm(include), norm(exclude), nil
}

// Pass decides whether a single entry passes the filter. relPath supplies the extension (the
// final name rendered by naming).
func (f *Filter) Pass(kind MediaKind, size int64, relPath string) (bool, string) {
	if len(f.Include) > 0 || len(f.Exclude) > 0 {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(relPath), "."))
		if len(f.Include) > 0 {
			if _, ok := f.Include[ext]; !ok {
				return false, "extension not in the include allowlist"
			}
		} else {
			if _, ok := f.Exclude[ext]; ok {
				return false, "extension matched the exclude denylist"
			}
		}
	}

	if f.MinSize > 0 && size < f.MinSize {
		return false, fmt.Sprintf("size %d bytes is below min_size %d", size, f.MinSize)
	}
	if f.MaxSize > 0 && size > f.MaxSize {
		return false, fmt.Sprintf("size %d bytes exceeds max_size %d", size, f.MaxSize)
	}

	if len(f.Kinds) > 0 {
		if !f.Kinds[kind] {
			return false, "kind not in the kinds allowlist (" + kind.String() + ")"
		}
	}

	return true, ""
}

// ParseKinds parses ["video","audio",...] into a set of MediaKind. Unknown values return an error.
func ParseKinds(names []string) (map[MediaKind]bool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	valid := map[string]MediaKind{
		"video": MediaVideo, "audio": MediaAudio, "voice": MediaVoice,
		"photo": MediaPhoto, "document": MediaDocument,
		"sticker": MediaSticker, "gif": MediaGIF,
	}
	m := make(map[MediaKind]bool, len(names))
	for _, n := range names {
		k, ok := valid[strings.ToLower(strings.TrimSpace(n))]
		if !ok {
			return nil, fmt.Errorf("filter: unknown media kind %q (one of video/audio/voice/photo/document/sticker/gif)", n)
		}
		m[k] = true
	}
	return m, nil
}

// ParseSize parses a size string such as "10MB"/"1.5GB"/"500KiB"/"1073741824" into a byte count
// (base 1024). Supported units: B (default), KB/KiB, MB/MiB, GB/GiB; a bare number means bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if isAllDigits(s) {
		return parsePositiveInt(s)
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	numStr, unit := s[:i], strings.ToUpper(strings.TrimSpace(s[i:]))
	if numStr == "" {
		return 0, fmt.Errorf("filter: cannot parse size %q", s)
	}
	var mult int64
	switch unit {
	case "B", "":
		mult = 1
	case "KB", "KIB":
		mult = 1 << 10
	case "MB", "MIB":
		mult = 1 << 20
	case "GB", "GIB":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("filter: unknown size unit %q (supported: B/KB/MB/GB)", unit)
	}
	if strings.Contains(numStr, ".") {
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return 0, fmt.Errorf("filter: cannot parse size %q", s)
		}
		bytes := f * float64(mult)
		if math.IsInf(bytes, 0) || bytes > float64(math.MaxInt64) {
			return 0, fmt.Errorf("filter: size %q overflows int64", s)
		}
		return int64(math.Round(bytes)), nil
	}
	num, err := parsePositiveInt(numStr)
	if err != nil {
		return 0, fmt.Errorf("filter: cannot parse size %q: %w", s, err)
	}
	if num > math.MaxInt64/mult {
		return 0, fmt.Errorf("filter: size %q overflows int64", s)
	}
	return num * mult, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parsePositiveInt(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative number is not allowed: %s", s)
	}
	return n, nil
}
