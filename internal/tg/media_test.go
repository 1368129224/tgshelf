// media_test.go - fully offline unit tests for media.go: they build tg.* structures
// by hand, never touch the network and depend on no session.
package tg

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

// mediaTestChannelID / mediaTestChatID / mediaTestUserID are the fixed peer samples
// used for the DialogID conversions.
const (
	mediaTestChannelID = int64(1234567890)
	mediaTestChatID    = int64(42)
	mediaTestUserID    = int64(777)
)

// mediaTestPhoto builds a photo; the caller decides the photo's sizes.
func mediaTestPhoto(id int64, dcid int, sizes []tg.PhotoSizeClass) *tg.MessageMediaPhoto {
	return &tg.MessageMediaPhoto{
		Photo: &tg.Photo{
			ID:            id,
			AccessHash:    111,
			FileReference: []byte{1, 2, 3},
			Date:          1700000000,
			Sizes:         sizes,
			DCID:          dcid,
		},
	}
}

// mediaTestDocument builds a document; the caller decides mime / size / attributes.
func mediaTestDocument(id int64, mimeType string, size int64, attrs ...tg.DocumentAttributeClass) *tg.MessageMediaDocument {
	return &tg.MessageMediaDocument{
		Document: &tg.Document{
			ID:            id,
			AccessHash:    222,
			FileReference: []byte{4, 5, 6},
			Date:          1700000001,
			MimeType:      mimeType,
			Size:          size,
			DCID:          2,
			Attributes:    attrs,
		},
	}
}

// mediaTestMessage assembles a message carrying media.
func mediaTestMessage(id int, peer tg.PeerClass, media tg.MessageMediaClass) *tg.Message {
	return &tg.Message{ID: id, PeerID: peer, Date: 1700000123, Media: media}
}

// TestMediaKindsAndNames covers requirements 1, 2 and 8: kind detection and naming for
// every media type.
func TestMediaKindsAndNames(t *testing.T) {
	peer := &tg.PeerChannel{ChannelID: mediaTestChannelID}

	t.Run("photo_with_regular_sizes", func(t *testing.T) {
		msg := mediaTestMessage(10, peer, mediaTestPhoto(5000, 4, []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "s", W: 100, H: 100, Size: 100},
			&tg.PhotoSize{Type: "x", W: 800, H: 800, Size: 80000},
		}))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Kind != MediaPhoto {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaPhoto)
		}
		if want := "5000.jpg"; item.Name != want {
			t.Errorf("Name = %q, want %q", item.Name, want)
		}
		if want := int64(80000); item.Size != want {
			t.Errorf("Size = %d, want the largest tier %d", item.Size, want)
		}
		if item.DCID != 4 {
			t.Errorf("DCID = %d, want 4", item.DCID)
		}
		if item.MessageID != 10 || item.DialogID != -1000000000000-mediaTestChannelID || item.Date != 1700000123 {
			t.Errorf("MessageID/DialogID/Date = %d/%d/%d", item.MessageID, item.DialogID, item.Date)
		}
		loc, isPhoto := item.Location.(*tg.InputPhotoFileLocation)
		if !isPhoto {
			t.Fatalf("Location type = %T, want *tg.InputPhotoFileLocation", item.Location)
		}
		if loc.ThumbSize != "x" {
			t.Errorf("ThumbSize = %q, want \"x\" of the largest tier", loc.ThumbSize)
		}
		if loc.ID != 5000 || string(loc.FileReference) != string([]byte{1, 2, 3}) {
			t.Errorf("Location.ID/FileRef were not passed through correctly")
		}
	})

	t.Run("photo_progressive_sizes_only", func(t *testing.T) {
		// Requirement 1: tg.PhotoSizeProgressive must be recognized, and its last
		// prefix length is the one to take.
		msg := mediaTestMessage(11, peer, mediaTestPhoto(6000, 2, []tg.PhotoSizeClass{
			&tg.PhotoSizeProgressive{Type: "m", W: 320, H: 320, Sizes: []int{1000, 4000}},
			&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 1280, Sizes: []int{9000, 55555}},
		}))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("Progressive was not recognized: ok=false")
		}
		if item.Kind != MediaPhoto {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaPhoto)
		}
		if want := int64(55555); item.Size != want {
			t.Errorf("Size = %d, want the largest Progressive tier %d", item.Size, want)
		}
		if loc, ok := item.Location.(*tg.InputPhotoFileLocation); !ok || loc.ThumbSize != "y" {
			t.Errorf("Location = %#v, want ThumbSize \"y\"", item.Location)
		}
	})

	t.Run("photo_empty_sizes_does_not_panic", func(t *testing.T) {
		// The defect fixed by requirement 1 plus acceptance item 5: empty sizes must
		// yield ok=false and must never panic.
		msg := mediaTestMessage(12, peer, mediaTestPhoto(7000, 1, []tg.PhotoSizeClass{}))
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("empty sizes panicked: %v", r)
			}
		}()

		item, ok := MediaFromMessage(msg)
		if ok {
			t.Fatalf("empty sizes: want ok=false, got %#v", item)
		}
		// MediaItem holds a []byte, so it cannot be compared directly; assert the zero
		// value field by field.
		if item.Kind != MediaVideo || item.Name != "" || item.Size != 0 || item.DCID != 0 ||
			item.Mime != "" || item.FileRef != nil || item.Location != nil ||
			item.MessageID != 0 || item.DialogID != 0 || item.Date != 0 {
			t.Errorf("empty sizes: want a zero-value MediaItem, got %#v", item)
		}
	})

	t.Run("video_with_filename", func(t *testing.T) {
		msg := mediaTestMessage(20, peer, mediaTestDocument(9000, "video/mp4", 1048576,
			&tg.DocumentAttributeVideo{W: 1920, H: 1080, Duration: 30},
			&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Kind != MediaVideo {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaVideo)
		}
		if item.Name != "clip.mp4" {
			t.Errorf("Name = %q, want the original file name clip.mp4", item.Name)
		}
		if item.Size != 1048576 || item.DCID != 2 || item.Mime != "video/mp4" {
			t.Errorf("Size/DCID/Mime = %d/%d/%q", item.Size, item.DCID, item.Mime)
		}
		if _, ok := item.Location.(*tg.InputDocumentFileLocation); !ok {
			t.Errorf("Location type = %T, want *tg.InputDocumentFileLocation", item.Location)
		}
	})

	t.Run("video_without_filename", func(t *testing.T) {
		msg := mediaTestMessage(21, peer, mediaTestDocument(9100, "video/x-matroska", 2048,
			&tg.DocumentAttributeVideo{W: 640, H: 480},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if want := "9100.mkv"; item.Name != want {
			t.Errorf("Name = %q, want %q", item.Name, want)
		}
	})

	t.Run("audio_with_filename", func(t *testing.T) {
		msg := mediaTestMessage(30, peer, mediaTestDocument(9200, "audio/mpeg", 4096,
			&tg.DocumentAttributeAudio{Duration: 200, Title: "song"},
			&tg.DocumentAttributeFilename{FileName: "song.mp3"},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Kind != MediaAudio {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaAudio)
		}
		if item.Name != "song.mp3" {
			t.Errorf("Name = %q, want song.mp3", item.Name)
		}
	})

	t.Run("audio_without_filename", func(t *testing.T) {
		msg := mediaTestMessage(31, peer, mediaTestDocument(9201, "audio/flac", 8192,
			&tg.DocumentAttributeAudio{Duration: 60},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if want := "9201.flac"; item.Name != want {
			t.Errorf("Name = %q, want %q", item.Name, want)
		}
	})

	t.Run("voice_flag_true", func(t *testing.T) {
		msg := mediaTestMessage(40, peer, mediaTestDocument(9300, "audio/ogg", 512,
			&tg.DocumentAttributeAudio{Duration: 5, Voice: true},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Kind != MediaVoice {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaVoice)
		}
		if want := "9300.ogg"; item.Name != want {
			t.Errorf("Name = %q, want %q", item.Name, want)
		}
	})

	t.Run("voice_flag_false_is_audio", func(t *testing.T) {
		// Requirement 2: same DocumentAttributeAudio, but Voice=false must land on
		// Audio rather than Voice.
		msg := mediaTestMessage(41, peer, mediaTestDocument(9301, "audio/ogg", 512,
			&tg.DocumentAttributeAudio{Duration: 5, Voice: false},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Kind != MediaAudio {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaAudio)
		}
	})

	t.Run("animated_is_gif", func(t *testing.T) {
		// A document carrying only Animated is a gif; the Animated+Video combination
		// is covered by TestMediaAttributePriority.
		msg := mediaTestMessage(50, peer, mediaTestDocument(9400, "image/gif", 1024,
			&tg.DocumentAttributeAnimated{},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Kind != MediaGIF {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaGIF)
		}
		if want := "9400.gif"; item.Name != want {
			t.Errorf("Name = %q, want %q", item.Name, want)
		}
	})

	t.Run("sticker_with_filename", func(t *testing.T) {
		msg := mediaTestMessage(60, peer, mediaTestDocument(9500, "image/webp", 256,
			&tg.DocumentAttributeSticker{Alt: ""},
			&tg.DocumentAttributeFilename{FileName: "sticker.webp"},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Kind != MediaSticker {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaSticker)
		}
		if item.Name != "sticker.webp" {
			t.Errorf("Name = %q, want sticker.webp", item.Name)
		}
	})

	t.Run("sticker_without_filename", func(t *testing.T) {
		msg := mediaTestMessage(61, peer, mediaTestDocument(9501, "image/webp", 256,
			&tg.DocumentAttributeSticker{Alt: ""},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if want := "9501.webp"; item.Name != want {
			t.Errorf("Name = %q, want %q", item.Name, want)
		}
	})

	t.Run("document_no_attributes_no_filename", func(t *testing.T) {
		msg := mediaTestMessage(70, peer, mediaTestDocument(9600, "application/pdf", 65536))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Kind != MediaDocument {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaDocument)
		}
		if want := "9600.pdf"; item.Name != want {
			t.Errorf("Name = %q, want %q", item.Name, want)
		}
	})

	t.Run("document_with_filename", func(t *testing.T) {
		msg := mediaTestMessage(71, peer, mediaTestDocument(9601, "application/pdf", 65536,
			&tg.DocumentAttributeFilename{FileName: "Report v1.pdf"},
		))

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if item.Name != "Report v1.pdf" {
			t.Errorf("Name = %q, want the original file name Report v1.pdf", item.Name)
		}
	})

	t.Run("nil_message_returns_ok_false", func(t *testing.T) {
		if item, ok := MediaFromMessage(nil); ok {
			t.Errorf("nil message: want ok=false, got %#v", item)
		}
		if item, ok := MediaFromMessage(&tg.Message{ID: 1}); ok {
			t.Errorf("message without media: want ok=false, got %#v", item)
		}
	})
}

// TestMediaMimeExt covers requirement 3: the mime-to-extension table, the standard
// library fallback and the .bin last resort.
func TestMediaMimeExt(t *testing.T) {
	cases := []struct{ mimeType, want string }{
		{"video/mp4", ".mp4"},
		{"video/x-matroska", ".mkv"},
		{"video/webm", ".webm"},
		{"video/x-msvideo", ".avi"},
		{"video/quicktime", ".mov"},
		{"video/x-m4v", ".m4v"},
		{"audio/mpeg", ".mp3"},
		{"audio/mp4", ".m4a"},
		{"audio/ogg", ".ogg"},
		{"audio/opus", ".opus"},
		{"audio/flac", ".flac"},
		{"audio/wav", ".wav"},
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"application/pdf", ".pdf"},
		{"application/zip", ".zip"},
		{"application/x-rar-compressed", ".rar"},
		{"application/x-7z-compressed", ".7z"},
		{"application/vnd.android.package-archive", ".apk"},
		{"text/plain", ".txt"},
		{"application/json", ".json"},
		{"application/x-msdownload", ".exe"},
		{"application/x-bittorrent", ".torrent"},
		// Case and parameters must be normalized.
		{"VIDEO/MP4", ".mp4"},
		{"text/plain; charset=utf-8", ".txt"},
		// An unknown mime must fall back to .bin, not panic or yield an empty string.
		{"application/x-unknown-xxx", mediaExtFallback},
		{"", mediaExtFallback},
	}

	for _, c := range cases {
		if got := mediaExtOf(c.mimeType); got != c.want {
			t.Errorf("mediaExtOf(%q) = %q, want %q", c.mimeType, got, c.want)
		}
	}

	// A type outside the table but known to the standard library: as long as it is
	// non-empty and starts with a dot it is fine (the exact suffix depends on the
	// system mime.types).
	if got := mediaExtOf("application/x-tar"); !strings.HasPrefix(got, ".") && got != mediaExtFallback {
		t.Errorf("mediaExtOf(application/x-tar) = %q, want something dot-prefixed or .bin", got)
	}
}

// TestMediaUnknownMimeFallsBackToBin covers the intersection of requirements 2 and 3:
// no file name plus an unknown mime.
func TestMediaUnknownMimeFallsBackToBin(t *testing.T) {
	msg := mediaTestMessage(80, &tg.PeerUser{UserID: mediaTestUserID},
		mediaTestDocument(9700, "application/x-unknown-xxx", 128))

	item, ok := MediaFromMessage(msg)
	if !ok {
		t.Fatalf("want ok=true")
	}
	if want := "9700.bin"; item.Name != want {
		t.Errorf("Name = %q, want the unknown-mime fallback %q", item.Name, want)
	}
	if item.Mime != "application/x-unknown-xxx" {
		t.Errorf("Mime = %q, want it preserved verbatim", item.Mime)
	}
}

// TestMediaNonDownloadable covers requirement 5: every non-downloadable media type must
// yield ok=false and must not panic.
func TestMediaNonDownloadable(t *testing.T) {
	peer := &tg.PeerChannel{ChannelID: mediaTestChannelID}

	cases := []struct {
		name  string
		media tg.MessageMediaClass
	}{
		{"geo", &tg.MessageMediaGeo{Geo: &tg.GeoPoint{Lat: 1, Long: 2}}},
		{"contact", &tg.MessageMediaContact{PhoneNumber: "+10000000000", FirstName: "A"}},
		{"poll", &tg.MessageMediaPoll{Poll: tg.Poll{ID: 1}}},
		{"webpage", &tg.MessageMediaWebPage{}},
		{"dice", &tg.MessageMediaDice{Value: 3, Emoticon: "🎲"}},
		{"story", &tg.MessageMediaStory{Peer: peer, ID: 9}},
		{"empty", &tg.MessageMediaEmpty{}},
		{"unsupported", &tg.MessageMediaUnsupported{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s panicked: %v", c.name, r)
				}
			}()
			item, ok := MediaFromMessage(mediaTestMessage(90, peer, c.media))
			if ok {
				t.Errorf("%s: want ok=false, got %#v", c.name, item)
			}
		})
	}
}

// TestMediaInvoice covers requirement 4: Invoice recurses into its extended media.
func TestMediaInvoice(t *testing.T) {
	peer := &tg.PeerChannel{ChannelID: mediaTestChannelID}

	t.Run("invoice_photo", func(t *testing.T) {
		invoice := &tg.MessageMediaInvoice{
			Title: "merchandise",
			ExtendedMedia: &tg.MessageExtendedMedia{
				Media: mediaTestPhoto(8888, 3, []tg.PhotoSizeClass{
					&tg.PhotoSize{Type: "x", W: 800, H: 800, Size: 40960},
				}),
			},
		}
		msg := mediaTestMessage(100, peer, invoice)
		// An Invoice message's media carries no context beyond the PeerID; only the
		// recursion result is checked here.
		msg.Media = invoice

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("Invoice recursion into photo failed: ok=false")
		}
		if item.Kind != MediaPhoto {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaPhoto)
		}
		if want := "8888.jpg"; item.Name != want {
			t.Errorf("Name = %q, want %q", item.Name, want)
		}
		if item.Size != 40960 || item.DCID != 3 {
			t.Errorf("Size/DCID = %d/%d, want 40960/3", item.Size, item.DCID)
		}
	})

	t.Run("invoice_document", func(t *testing.T) {
		msg := mediaTestMessage(101, peer, &tg.MessageMediaInvoice{
			Title: "merchandise",
			ExtendedMedia: &tg.MessageExtendedMedia{
				Media: mediaTestDocument(9999, "application/zip", 2048,
					&tg.DocumentAttributeFilename{FileName: "goods.zip"},
				),
			},
		})

		item, ok := MediaFromMessage(msg)
		if !ok {
			t.Fatalf("Invoice recursion into document failed: ok=false")
		}
		if item.Kind != MediaDocument {
			t.Errorf("Kind = %v, want %v", item.Kind, MediaDocument)
		}
		if item.Name != "goods.zip" {
			t.Errorf("Name = %q, want goods.zip", item.Name)
		}
	})

	t.Run("invoice_without_extended_media", func(t *testing.T) {
		msg := mediaTestMessage(102, peer, &tg.MessageMediaInvoice{Title: "empty"})
		if item, ok := MediaFromMessage(msg); ok {
			t.Errorf("no extended media: want ok=false, got %#v", item)
		}
	})
}

// TestMediaDialogID covers requirement 6: dialog id conversion for the channel / chat /
// user peer kinds.
func TestMediaDialogID(t *testing.T) {
	media := mediaTestDocument(1234, "application/pdf", 10)

	cases := []struct {
		name string
		peer tg.PeerClass
		want int64
	}{
		{"channel", &tg.PeerChannel{ChannelID: mediaTestChannelID}, -1000000000000 - mediaTestChannelID},
		{"chat", &tg.PeerChat{ChatID: mediaTestChatID}, -mediaTestChatID},
		{"user", &tg.PeerUser{UserID: mediaTestUserID}, mediaTestUserID},
		{"nil_peer", nil, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			item, ok := MediaFromMessage(mediaTestMessage(5, c.peer, media))
			if !ok {
				t.Fatalf("want ok=true")
			}
			if item.DialogID != c.want {
				t.Errorf("DialogID = %d, want %d", item.DialogID, c.want)
			}
		})
	}
}

// TestMediaKindString covers MediaKind.String (including an out-of-range value).
func TestMediaKindString(t *testing.T) {
	cases := map[MediaKind]string{
		MediaVideo:    "video",
		MediaAudio:    "audio",
		MediaVoice:    "voice",
		MediaPhoto:    "photo",
		MediaDocument: "document",
		MediaSticker:  "sticker",
		MediaGIF:      "gif",
		MediaKind(99): "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("MediaKind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
}

// TestExpandAlbum covers requirement 7: album expansion, filtering, sorting, the
// single-element case and error propagation.
func TestExpandAlbum(t *testing.T) {
	const groupedID = int64(555666777)

	newMsg := func(id int, gid int64) *tg.Message {
		return &tg.Message{ID: id, GroupedID: gid, PeerID: &tg.PeerChannel{ChannelID: 1}}
	}

	t.Run("same_group_out_of_order_to_ascending", func(t *testing.T) {
		self := newMsg(100, groupedID)
		batch := []tg.MessageClass{
			newMsg(102, groupedID),
			self,
			newMsg(101, groupedID),
		}

		var gotOffset, gotLimit int
		fetch := func(_ context.Context, offsetID, limit int) ([]tg.MessageClass, error) {
			gotOffset, gotLimit = offsetID, limit
			return batch, nil
		}

		album, err := ExpandAlbum(context.Background(), self, fetch)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []int{100, 101, 102}
		if len(album) != len(want) {
			t.Fatalf("album length = %d, want %d", len(album), len(want))
		}
		for i, id := range want {
			if album[i].ID != id {
				t.Errorf("entry %d ID = %d, want %d (must be ascending by ID)", i, album[i].ID, id)
			}
		}
		if gotOffset != self.ID+11 || gotLimit != 20 {
			t.Errorf("fetch args = (%d, %d), want (%d, 20)", gotOffset, gotLimit, self.ID+11)
		}
	})

	t.Run("different_grouped_id_filtered_out", func(t *testing.T) {
		self := newMsg(200, groupedID)
		batch := []tg.MessageClass{
			newMsg(201, groupedID),
			newMsg(202, groupedID+1), // another album, must be filtered out
			newMsg(203, 0),           // not an album message, must be filtered out
		}

		album, err := ExpandAlbum(context.Background(), self, func(context.Context, int, int) ([]tg.MessageClass, error) {
			return batch, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(album) != 2 {
			t.Fatalf("album length = %d, want 2 (self + 201)", len(album))
		}
		if album[0].ID != 200 || album[1].ID != 201 {
			t.Errorf("album IDs = %d,%d, want 200,201", album[0].ID, album[1].ID)
		}
	})

	t.Run("fetch_without_self_still_includes_msg", func(t *testing.T) {
		self := newMsg(300, groupedID)
		batch := []tg.MessageClass{newMsg(301, groupedID)}

		album, err := ExpandAlbum(context.Background(), self, func(context.Context, int, int) ([]tg.MessageClass, error) {
			return batch, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(album) != 2 || album[0].ID != 300 || album[1].ID != 301 {
			t.Fatalf("want the result to include self in ascending order, got %d entries", len(album))
		}
	})

	t.Run("fetch_returning_self_is_not_duplicated", func(t *testing.T) {
		self := newMsg(400, groupedID)
		batch := []tg.MessageClass{self, newMsg(401, groupedID)}

		album, err := ExpandAlbum(context.Background(), self, func(context.Context, int, int) ([]tg.MessageClass, error) {
			return batch, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(album) != 2 {
			t.Fatalf("album length = %d, want 2 (self must not be duplicated)", len(album))
		}
	})

	t.Run("no_grouped_id_returns_single_element", func(t *testing.T) {
		self := newMsg(500, 0)
		called := false
		fetch := func(context.Context, int, int) ([]tg.MessageClass, error) {
			called = true
			return nil, errors.New("must not be called")
		}

		album, err := ExpandAlbum(context.Background(), self, fetch)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Errorf("fetch must not be called without a grouped_id")
		}
		if len(album) != 1 || album[0] != self {
			t.Fatalf("want only msg itself, got %#v", album)
		}
	})

	t.Run("fetch_error_propagated", func(t *testing.T) {
		self := newMsg(600, groupedID)
		sentinel := errors.New("flood wait 42s")
		album, err := ExpandAlbum(context.Background(), self, func(context.Context, int, int) ([]tg.MessageClass, error) {
			return nil, sentinel
		})
		if err == nil {
			t.Fatalf("want an error")
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("error was not wrapped verbatim (errors.Is failed): %v", err)
		}
		if album != nil {
			t.Errorf("no album must be returned on error, got %#v", album)
		}
	})

	t.Run("nil_message", func(t *testing.T) {
		album, err := ExpandAlbum(context.Background(), nil, nil)
		if err != nil || album != nil {
			t.Errorf("nil message: want (nil, nil), got (%#v, %v)", album, err)
		}
	})

	t.Run("grouped_id_with_nil_fetch", func(t *testing.T) {
		self := newMsg(700, groupedID)
		if _, err := ExpandAlbum(context.Background(), self, nil); err == nil {
			t.Errorf("want an error (missing fetcher)")
		}
	})
}

// TestMediaAttributePriority pins the kind priority of DocumentAttribute:
// sticker > animated(gif) > video > voice > audio > document.
//
// This priority is not an aesthetic matter: a Telegram MP4 animation carries both the
// Animated and Video attributes, and an animated sticker carries both Sticker and Video.
// With video first, the gif and sticker branches would be unreachable for the most
// realistic media shapes -- making a later branch in the priority table unreachable is
// a defect.
func TestMediaAttributePriority(t *testing.T) {
	peer := &tg.PeerChannel{ChannelID: mediaTestChannelID}

	cases := []struct {
		name  string
		id    int64
		mime  string
		attrs []tg.DocumentAttributeClass
		want  MediaKind
	}{
		{
			name: "sticker_plus_video_is_sticker",
			id:   11000,
			mime: "video/webm",
			attrs: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeSticker{Alt: "party"},
				&tg.DocumentAttributeVideo{W: 512, H: 512, RoundMessage: true},
			},
			want: MediaSticker,
		},
		{
			name: "animated_plus_video_is_gif",
			id:   11001,
			mime: "video/mp4",
			attrs: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeAnimated{},
				&tg.DocumentAttributeVideo{W: 480, H: 480},
			},
			want: MediaGIF,
		},
		{
			name: "sticker_animated_video_still_sticker",
			id:   11002,
			mime: "video/webm",
			attrs: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeVideo{W: 512, H: 512},
				&tg.DocumentAttributeAnimated{},
				&tg.DocumentAttributeSticker{Alt: "party"},
			},
			want: MediaSticker,
		},
		{
			name: "video_only_is_video",
			id:   11003,
			mime: "video/mp4",
			attrs: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeVideo{W: 1920, H: 1080, Duration: 12},
			},
			want: MediaVideo,
		},
		{
			name: "animated_only_is_gif",
			id:   11004,
			mime: "video/mp4",
			attrs: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeAnimated{},
			},
			want: MediaGIF,
		},
		{
			name: "video_plus_voice_is_video",
			id:   11005,
			mime: "video/mp4",
			attrs: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeVideo{W: 640, H: 360},
				&tg.DocumentAttributeAudio{Duration: 9, Voice: true},
			},
			want: MediaVideo,
		},
		{
			name: "voice_beats_audio",
			id:   11006,
			mime: "audio/ogg",
			attrs: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeAudio{Duration: 5, Voice: true},
			},
			want: MediaVoice,
		},
		{
			name: "no_specific_attribute_is_document",
			id:   11007,
			mime: "application/pdf",
			attrs: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "a.pdf"},
			},
			want: MediaDocument,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := mediaTestMessage(120, peer, mediaTestDocument(c.id, c.mime, 4096, c.attrs...))
			item, ok := MediaFromMessage(msg)
			if !ok {
				t.Fatalf("want ok=true")
			}
			if item.Kind != c.want {
				t.Errorf("Kind = %v, want %v (priority sticker > animated > video > voice > audio > document)",
					item.Kind, c.want)
			}
		})
	}
}
