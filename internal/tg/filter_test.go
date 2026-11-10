package tg

import (
	"testing"
)

// TestFilterExtInclude allowlist hits and misses.
func TestFilterExtInclude(t *testing.T) {
	inc, _, err := NewFilterFromLists([]string{"mp4", ".MKV"}, nil)
	if err != nil {
		t.Fatalf("NewFilterFromLists: %v", err)
	}
	f := &Filter{Include: inc}

	if ok, _ := f.Pass(MediaVideo, 100, "a/b.mp4"); !ok {
		t.Fatal("b.mp4 should match the mp4 allowlist")
	}
	if ok, _ := f.Pass(MediaVideo, 100, "a/b.MP4"); !ok {
		t.Fatal("extension comparison should be case-insensitive")
	}
	if ok, _ := f.Pass(MediaVideo, 100, "a/b.mkv"); !ok {
		t.Fatal(".MKV should match mkv after normalization")
	}
	if ok, reason := f.Pass(MediaVideo, 100, "a/b.avi"); ok {
		t.Fatal("avi is not in the allowlist and should be rejected")
	} else if reason == "" {
		t.Fatal("a rejection should come with a reason")
	}
}

// TestFilterExtExclude denylist.
func TestFilterExtExclude(t *testing.T) {
	_, exc, err := NewFilterFromLists(nil, []string{"gif"})
	if err != nil {
		t.Fatalf("NewFilterFromLists: %v", err)
	}
	f := &Filter{Exclude: exc}

	if ok, _ := f.Pass(MediaVideo, 100, "a/v.mp4"); !ok {
		t.Fatal("mp4 is not on the denylist and should pass")
	}
	if ok, _ := f.Pass(MediaGIF, 100, "a/x.gif"); ok {
		t.Fatal("gif matches the denylist and should be rejected")
	}
}

// TestFilterIncludeExcludeMutuallyExclusive allowlist and denylist are mutually exclusive.
func TestFilterIncludeExcludeMutuallyExclusive(t *testing.T) {
	_, _, err := NewFilterFromLists([]string{"mp4"}, []string{"gif"})
	if err == nil {
		t.Fatal("include together with exclude should fail")
	}
}

// TestFilterSize lower and upper size bounds (boundaries included).
func TestFilterSize(t *testing.T) {
	f := &Filter{MinSize: 100, MaxSize: 1000}

	if ok, _ := f.Pass(MediaVideo, 100, "a.bin"); !ok {
		t.Fatal("size==min should pass (boundary included)")
	}
	if ok, _ := f.Pass(MediaVideo, 1000, "a.bin"); !ok {
		t.Fatal("size==max should pass (boundary included)")
	}
	if ok, _ := f.Pass(MediaVideo, 99, "a.bin"); ok {
		t.Fatal("size<min should be rejected")
	}
	if ok, _ := f.Pass(MediaVideo, 1001, "a.bin"); ok {
		t.Fatal("size>max should be rejected")
	}
}

// TestFilterKinds kind filtering.
func TestFilterKinds(t *testing.T) {
	kinds, err := ParseKinds([]string{"Video", "audio"})
	if err != nil {
		t.Fatalf("ParseKinds: %v", err)
	}
	f := &Filter{Kinds: kinds}

	if ok, _ := f.Pass(MediaVideo, 100, "a.mp4"); !ok {
		t.Fatal("video should pass")
	}
	if ok, _ := f.Pass(MediaAudio, 100, "a.mp3"); !ok {
		t.Fatal("audio should pass")
	}
	if ok, _ := f.Pass(MediaPhoto, 100, "a.jpg"); ok {
		t.Fatal("photo is not in kinds and should be rejected")
	}
}

// TestParseKindsUnknown an unknown kind returns an error.
func TestParseKindsUnknown(t *testing.T) {
	if _, err := ParseKinds([]string{"movie"}); err == nil {
		t.Fatal("an unknown kind should fail")
	}
}

// TestParseSize size string parsing.
func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1073741824", 1073741824, true},
		{"10MB", 10 << 20, true},
		{"10mb", 10 << 20, true},
		{"1GB", 1 << 30, true},
		{"512KB", 512 << 10, true},
		{"500KiB", 500 << 10, true},
		{"abc", 0, false},
		{"10TB", 0, false},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("ParseSize(%q) should succeed, got err=%v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
			}
		} else {
			if err == nil {
				t.Errorf("ParseSize(%q) should error, got %d", c.in, got)
			}
		}
	}
}

// TestFilterAllZero with no filtering everything passes.
func TestFilterAllZero(t *testing.T) {
	f := &Filter{}
	for _, k := range []MediaKind{MediaVideo, MediaAudio, MediaPhoto, MediaSticker} {
		if ok, _ := f.Pass(k, 0, "anything.any"); !ok {
			t.Fatal("a zero-value Filter should let everything through")
		}
	}
}
