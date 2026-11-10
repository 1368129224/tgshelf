package main

import (
	tgpkg "github.com/gotd/td/tg"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/1368129224/tgshelf/internal/naming"
	"github.com/1368129224/tgshelf/internal/tg"
)

// writeFile is a small helper for this test file.
func writeFile(p string, data []byte) error {
	return os.WriteFile(p, data, 0o600)
}

// sampleMedia builds a MediaItem for the naming tests.
func sampleMedia() tg.MediaItem {
	return tg.MediaItem{
		Kind:      tg.MediaVideo,
		Name:      "../../evil name?.mp4",
		Size:      2076378,
		DCID:      1,
		Mime:      "video/mp4",
		MessageID: 193,
		DialogID:  1005640892,
		Date:      1700000000,
	}
}

// TestDownloadRelPath verifies that template rendering + Sanitize + the tag prefix produce a safe
// relative path.
func TestDownloadRelPath(t *testing.T) {
	rel, err := downloadRelPath("{{ .DialogID }}_{{ .MessageID }}_{{ filenamify .FileName }}", sampleMedia(), "")
	if err != nil {
		t.Fatalf("downloadRelPath: %v", err)
	}
	if strings.Contains(rel, "..") || strings.Contains(rel, "?") {
		t.Errorf("the relative path must be sanitized, got %q", rel)
	}
	if !strings.HasPrefix(rel, "1005640892_193_") {
		t.Errorf("the relative path must carry the DialogID_MessageID prefix, got %q", rel)
	}

	relTag, err := downloadRelPath("{{ .FileName }}", sampleMedia(), "video")
	if err != nil {
		t.Fatalf("downloadRelPath(tag): %v", err)
	}
	if relTag != "video/"+naming.Sanitize("evil name?.mp4") {
		t.Errorf("unexpected tag-prefixed path, got %q", relTag)
	}
}

// TestDownloadRelPathTraversal verifies that traversal segments are sanitized away.
func TestDownloadRelPathTraversal(t *testing.T) {
	rel, err := downloadRelPath("{{ .FileName }}", sampleMedia(), "")
	if err != nil {
		t.Fatalf("downloadRelPath: %v", err)
	}
	if _, err := naming.SafeJoin(t.TempDir(), rel); err != nil {
		t.Fatalf("the sanitized path must pass SafeJoin: %v (rel=%q)", err, rel)
	}
}

// TestMediaDocID verifies document ID extraction (a non-document Location returns 0).
func TestMediaDocID(t *testing.T) {
	m := sampleMedia()
	m.Location = nil
	if got := mediaDocID(m); got != 0 {
		t.Errorf("must be 0 when Location is nil, got %d", got)
	}
	m.Location = &tgpkg.InputDocumentFileLocation{ID: 42}
	if got := mediaDocID(m); got != 42 {
		t.Errorf("docID = %d, want 42", got)
	}
}

// TestMd5File verifies the MD5 length and determinism.
func TestMd5File(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/f.txt"
	if err := writeFile(p, []byte("hello tgshelf")); err != nil {
		t.Fatal(err)
	}
	sum, err := md5File(p)
	if err != nil {
		t.Fatalf("md5File: %v", err)
	}
	if len(sum) != 32 {
		t.Errorf("MD5 must be 32 hex characters, got %q", sum)
	}
	sum2, _ := md5File(p)
	if sum2 != sum {
		t.Errorf("MD5 must be deterministic, the two runs differ")
	}
}

// TestRpcStatsFloodSnapshot verifies FLOOD_WAIT event recording and snapshot isolation.
func TestRpcStatsFloodSnapshot(t *testing.T) {
	s := &rpcStats{started: time.Now()}
	s.recordFlood(3 * time.Second)
	s.recordFlood(5 * time.Second)
	got := s.floodSnapshot()
	if len(got) != 2 {
		t.Fatalf("must have 2 events, got %d", len(got))
	}
	if got[0].wait != 3*time.Second || got[1].wait != 5*time.Second {
		t.Errorf("recorded wait durations do not match: %v", got)
	}
	got[0].wait = 99 * time.Second
	if s.floodSnapshot()[0].wait == 99*time.Second {
		t.Error("the snapshot must be isolated from the internal state")
	}
}

// TestDownloadArgsSet verifies multi-value parsing (repeated flags and comma separation).
func TestDownloadArgsSet(t *testing.T) {
	a := &downloadArgs{}
	if err := a.Set("https://t.me/a/1, https://t.me/a/2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := a.Set("https://t.me/a/3"); err != nil {
		t.Fatalf("Set2: %v", err)
	}
	if len(a.urls) != 3 {
		t.Fatalf("must hold 3 links, got %v", a.urls)
	}
	if a.String() != "https://t.me/a/1,https://t.me/a/2,https://t.me/a/3" {
		t.Errorf("String() = %q", a.String())
	}
}

// TestFileStatEmpty is a compile-time guard on the completeness of the fileStat fields.
func TestFileStatEmpty(t *testing.T) {
	var f fileStat
	if f.relPath != "" || f.requests != 0 || f.err != nil {
		t.Error("a zero fileStat must be fully empty")
	}
}
