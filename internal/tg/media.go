// media.go -- normalizes the media field of a gotd message into MediaItem and supports album
// (grouped_id) expansion.
//
// # Approach: this project implements media normalization itself
//
// ExtractMedia from the upstream library github.com/iyear/tdl/core/tmedia (AGPL-3.0, with thanks)
// returns only Name/Size/DC/Location/Date and has no "kind" dimension, so it cannot distinguish
// video/audio/voice/sticker/gif/photo -- yet kind is exactly what this file must produce (MediaKind
// + DocumentAttribute priority + Invoice recursion). Its extension inference also depends on
// github.com/gabriel-vasile/mimetype (an indirect dependency in this project's go.mod), and it
// indexes out of range on an empty sizes array. So this file implements the logic above itself and
// adds kind detection, Invoice recursion and an extension table (see mediaMaxPhotoSize).
//
// # Responsibility boundaries (explicitly [not done])
//   - Only "downloadable" media is handled. geo/contact/poll/webpage/dice/story/empty/unsupported
//     all return ok=false and never use ConvInputMedia (that is for "forwarding" and has completely
//     different semantics).
//   - Name is the raw filename and is [not sanitized]: path traversal and illegal characters are
//     internal/naming's responsibility.
//   - No network calls are made (MediaFromMessage is a pure function); the only network action is
//     the HistoryFetcherFunc injected by the caller into ExpandAlbum.
//   - No shared -100 prefix conversion helper: PeerIDFromChannelID in peer.go is implemented by a
//     concurrent subagent, so this file uses the private mediaPeerID for the same conversion
//     (unified in pipeline.go).
package tg

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"sort"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// MediaKind is the kind of a downloadable media item.
type MediaKind int

const (
	// MediaVideo denotes an ordinary video (DocumentAttributeVideo).
	MediaVideo MediaKind = iota
	// MediaAudio denotes a music/audio file (DocumentAttributeAudio with Voice=false).
	MediaAudio
	// MediaVoice denotes a voice message (DocumentAttributeAudio with Voice=true).
	MediaVoice
	// MediaPhoto denotes a picture (messageMediaPhoto).
	MediaPhoto
	// MediaDocument denotes an ordinary file without a more specific attribute.
	MediaDocument
	// MediaSticker denotes a sticker (DocumentAttributeSticker).
	MediaSticker
	// MediaGIF denotes an animation (DocumentAttributeAnimated).
	MediaGIF
)

// String returns the readable name of the kind, used in logs and template output.
func (k MediaKind) String() string {
	switch k {
	case MediaVideo:
		return "video"
	case MediaAudio:
		return "audio"
	case MediaVoice:
		return "voice"
	case MediaPhoto:
		return "photo"
	case MediaDocument:
		return "document"
	case MediaSticker:
		return "sticker"
	case MediaGIF:
		return "gif"
	default:
		return "unknown"
	}
}

// MediaItem is the normalized description of the downloadable media in one message.
type MediaItem struct {
	Kind      MediaKind
	Name      string // raw filename, **not sanitized**; sanitizing is internal/naming's job
	Size      int64
	DCID      int
	Mime      string
	FileRef   []byte // volatile, valid only for the current task
	Location  tg.InputFileLocationClass
	MessageID int
	DialogID  int64
	Date      int64
}

// errMediaNilFetcher means album expansion is missing a HistoryFetcherFunc.
var errMediaNilFetcher = errors.New("tg: HistoryFetcherFunc is nil while expanding an album")

// mediaPhotoNameMime / mediaPhotoExt is the uniform extension and MIME of Telegram pictures: the
// server always compresses them as JPEG, so mime cannot be used to infer them.
const (
	mediaPhotoExt  = ".jpg"
	mediaPhotoMime = "image/jpeg"
)

// MediaFromMessage is a pure function (it makes no network calls). ok=false means the message has
// no downloadable media.
func MediaFromMessage(msg *tg.Message) (MediaItem, bool) {
	if msg == nil {
		return MediaItem{}, false
	}
	// Read the field directly instead of msg.GetMedia(): both are equivalent on a decoded message,
	// and reading the field also supports construction scenarios where the flag is unset but the
	// field is populated.
	if msg.Media == nil {
		return MediaItem{}, false
	}

	item, ok := mediaFromMedia(msg.Media)
	if !ok {
		return MediaItem{}, false
	}

	item.MessageID = msg.ID
	item.DialogID = mediaPeerID(msg.PeerID)
	item.Date = int64(msg.Date)
	return item, true
}

// mediaFromMedia handles a single-level MessageMediaClass. The extended media of an Invoice also
// goes through here (recursion).
func mediaFromMedia(media tg.MessageMediaClass) (MediaItem, bool) {
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return MediaItem{}, false
		}
		return mediaFromPhoto(photo)
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return MediaItem{}, false
		}
		return mediaFromDocument(doc)
	case *tg.MessageMediaInvoice:
		return mediaFromExtendedMedia(m.ExtendedMedia)
	default:
		// geo/contact/poll/webpage/dice/story/empty/unsupported and friends are never downloadable
		// media.
		return MediaItem{}, false
	}
}

// mediaFromExtendedMedia recursively handles the extended media of an Invoice (the underlying
// photo/document).
func mediaFromExtendedMedia(ext tg.MessageExtendedMediaClass) (MediaItem, bool) {
	m, ok := ext.(*tg.MessageExtendedMedia)
	if !ok {
		return MediaItem{}, false
	}
	if m.Media == nil {
		return MediaItem{}, false
	}
	return mediaFromMedia(m.Media)
}

// mediaFromPhoto takes the largest size variant from a tg.Photo.
func mediaFromPhoto(p *tg.Photo) (MediaItem, bool) {
	// mediaMaxPhotoSize returns false for an empty sizes array, so ok=false is returned here as
	// well: never index, never panic.
	thumb, size, ok := mediaMaxPhotoSize(p.Sizes)
	if !ok {
		return MediaItem{}, false
	}

	return MediaItem{
		Kind: MediaPhoto,
		// Telegram pictures have no real filename, so build a stable name from photo.id + .jpg.
		Name:    strconv.FormatInt(p.ID, 10) + mediaPhotoExt,
		Size:    size,
		DCID:    p.DCID,
		Mime:    mediaPhotoMime,
		FileRef: p.FileReference,
		Location: &tg.InputPhotoFileLocation{
			ID:            p.ID,
			AccessHash:    p.AccessHash,
			FileReference: p.FileReference,
			ThumbSize:     thumb,
		},
	}, true
}

// mediaMaxPhotoSize picks the largest size variant from sizes and returns its Type and byte count.
//
// Both PhotoSize and PhotoSizeProgressive must be covered: large pictures are often delivered by
// the server in Progressive form, so honoring only PhotoSize would miss the largest variant. Both
// cases have a length guard.
func mediaMaxPhotoSize(sizes []tg.PhotoSizeClass) (string, int64, bool) {
	var (
		bestType string
		bestSize int64 = -1
	)

	for _, s := range sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			if n := int64(v.Size); n > bestSize {
				bestType, bestSize = v.Type, n
			}
		case *tg.PhotoSizeProgressive:
			// The actual size of a Progressive variant is the length of the last (most complete)
			// prefix.
			if n := len(v.Sizes); n > 0 {
				if size := int64(v.Sizes[n-1]); size > bestSize {
					bestType, bestSize = v.Type, size
				}
			}
		}
	}

	if bestSize < 0 {
		return "", 0, false
	}
	return bestType, bestSize, true
}

// mediaFromDocument determines the kind by DocumentAttribute priority and derives the name.
func mediaFromDocument(d *tg.Document) (MediaItem, bool) {
	kind, name := mediaDocumentKindAndName(d)

	return MediaItem{
		Kind:    kind,
		Name:    name,
		Size:    d.Size,
		DCID:    d.DCID,
		Mime:    d.MimeType,
		FileRef: d.FileReference,
		Location: &tg.InputDocumentFileLocation{
			ID:            d.ID,
			AccessHash:    d.AccessHash,
			FileReference: d.FileReference,
		},
	}, true
}

// mediaDocumentKindAndName scans the attributes: first the kind by priority, then the filename.
//
// Priority (from most specific to most general; the order is the priority):
//
//	sticker > animated(gif) > video > voice > audio > document
//
// Rationale for the order (dictated by real media shapes, not arbitrary):
//   - An MP4 animation carries both DocumentAttributeAnimated and DocumentAttributeVideo, so
//     animated must be checked before video, otherwise the gif branch would be unreachable for the
//     most common case;
//   - An animated sticker (webm-like) carries DocumentAttributeSticker and sometimes also Video, so
//     sticker must be checked before video, otherwise stickers would be misdetected as videos.
//
// That is: the more "exclusive" an attribute, the earlier it is; video is a generic attribute and
// comes after the three exclusive ones, but still before voice/audio -- an audio container carrying
// the Video attribute (rare) is treated as video.
func mediaDocumentKindAndName(d *tg.Document) (MediaKind, string) {
	var (
		hasVideo       bool
		hasAudio       bool
		voice          bool
		hasAnimated    bool
		hasSticker     bool
		filename       string
		audioMetaSet   bool
		audioPerformer string
		audioTitle     string
	)

	for _, attr := range d.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeVideo:
			hasVideo = true
		case *tg.DocumentAttributeAudio:
			hasAudio = true
			if a.Voice {
				voice = true
			} else if !audioMetaSet {
				p := strings.TrimSpace(a.Performer)
				t := strings.TrimSpace(a.Title)
				if p != "" || t != "" {
					audioPerformer = p
					audioTitle = t
					audioMetaSet = true
				}
			}
		case *tg.DocumentAttributeAnimated:
			hasAnimated = true
		case *tg.DocumentAttributeSticker:
			hasSticker = true
		case *tg.DocumentAttributeFilename:
			// Only the first filename attribute counts, so a later attribute cannot override the
			// real filename.
			if filename == "" {
				filename = a.FileName
			}
		}
	}

	kind := MediaDocument
	switch {
	case hasSticker:
		kind = MediaSticker
	case hasAnimated:
		kind = MediaGIF
	case hasVideo:
		kind = MediaVideo
	case voice:
		kind = MediaVoice
	case hasAudio:
		kind = MediaAudio
	}

	if filename == "" && !voice && audioMetaSet {
		ext := mediaExtOf(d.MimeType)
		switch {
		case audioPerformer != "" && audioTitle != "":
			filename = audioPerformer + " - " + audioTitle + ext
		case audioTitle != "":
			filename = audioTitle + ext
		case audioPerformer != "":
			filename = audioPerformer + ext
		}
	}
	if filename == "" {
		// No filename: use the stable id plus an extension inferred from the mime type, which makes
		// deduplication such as --skip-same possible.
		filename = strconv.FormatInt(d.ID, 10) + mediaExtOf(d.MimeType)
	}

	return kind, filename
}

// mediaMimeExt is a hand-built MIME -> extension table covering the most common types in download
// scenarios. The mimetype library is deliberately not imported: all that is needed here is "guess a
// suffix from the declared type", not content sniffing.
var mediaMimeExt = map[string]string{
	// video
	"video/mp4":        ".mp4",
	"video/x-matroska": ".mkv",
	"video/webm":       ".webm",
	"video/x-msvideo":  ".avi",
	"video/avi":        ".avi",
	"video/quicktime":  ".mov",
	"video/x-m4v":      ".m4v",
	"video/mpeg":       ".mpeg",
	// audio
	"audio/mpeg":   ".mp3",
	"audio/mp3":    ".mp3",
	"audio/mp4":    ".m4a",
	"audio/x-m4a":  ".m4a",
	"audio/m4a":    ".m4a",
	"audio/ogg":    ".ogg",
	"audio/opus":   ".opus",
	"audio/flac":   ".flac",
	"audio/x-flac": ".flac",
	"audio/wav":    ".wav",
	"audio/x-wav":  ".wav",
	"audio/wave":   ".wav",
	// image
	"image/jpeg": ".jpg",
	"image/jpg":  ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	// documents and archives
	"application/pdf":                         ".pdf",
	"application/zip":                         ".zip",
	"application/x-zip-compressed":            ".zip",
	"application/x-rar-compressed":            ".rar",
	"application/vnd.rar":                     ".rar",
	"application/x-7z-compressed":             ".7z",
	"application/vnd.android.package-archive": ".apk",
	"text/plain":                              ".txt",
	"application/json":                        ".json",
	"application/x-msdownload":                ".exe",
	"application/x-bittorrent":                ".torrent",
}

// mediaExtFallback is the fallback extension used when every inference method fails.
const mediaExtFallback = ".bin"

// mediaExtOf infers an extension (with a leading dot) from a MIME type.
//
// Order: hand-built table -> standard library mime.ExtensionsByType -> ".bin".
// The input may carry parameters (e.g. "text/plain; charset=utf-8") and case differences.
func mediaExtOf(mimeType string) string {
	mt := strings.TrimSpace(mimeType)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	mt = strings.ToLower(mt)

	if ext, ok := mediaMimeExt[mt]; ok {
		return ext
	}

	// The standard library reads the system mime.types; when several candidates match, take the
	// first so the result is stable.
	if exts, err := mime.ExtensionsByType(mt); err == nil && len(exts) > 0 {
		return exts[0]
	}

	return mediaExtFallback
}

// mediaPeerID converts a gotd PeerClass into a Bot API style dialog id:
// channel -> -100 prefix (reusing PeerIDFromChannelID from peer.go, unified in pipeline.go);
// basic chat -> -chatID; user -> userID.
func mediaPeerID(peer tg.PeerClass) int64 {
	switch p := peer.(type) {
	case *tg.PeerChannel:
		return PeerIDFromChannelID(p.ChannelID)
	case *tg.PeerChat:
		return -p.ChatID
	case *tg.PeerUser:
		return p.UserID
	default:
		return 0
	}
}

// HistoryFetcherFunc is injected by the caller (the production implementation is
// messages.getHistory) to make offline unit tests possible.
type HistoryFetcherFunc func(ctx context.Context, offsetID, limit int) ([]tg.MessageClass, error)

// Fixed parameters for album expansion: fetch 20 messages starting at msg.ID+11 (a Telegram album
// holds at most 10 messages).
const (
	mediaAlbumOffset = 11
	mediaAlbumLimit  = 20
)

// ExpandAlbum expands an album into the messages of the same group, in ascending message ID order.
// When msg has no grouped_id it returns a single-element slice containing only msg.
func ExpandAlbum(ctx context.Context, msg *tg.Message, fetch HistoryFetcherFunc) ([]*tg.Message, error) {
	if msg == nil {
		return nil, nil
	}

	// Read the field directly instead of GetGroupedID(): a zero field and an unset flag are the
	// same thing, both meaning "not an album".
	if msg.GroupedID == 0 {
		return []*tg.Message{msg}, nil
	}
	if fetch == nil {
		return nil, errMediaNilFetcher
	}

	batch, err := fetch(ctx, msg.ID+mediaAlbumOffset, mediaAlbumLimit)
	if err != nil {
		// Wrapped as is: the caller can use errors.Is/As to test the underlying error (e.g.
		// FloodWait).
		return nil, fmt.Errorf("tg: expand album: %w", err)
	}

	// msg itself must appear in the result; fetch usually includes it already, but deduplicate by
	// the same ID to prevent duplicates.
	seen := map[int]struct{}{msg.ID: {}}
	album := []*tg.Message{msg}
	for _, m := range batch {
		item, ok := m.(*tg.Message)
		if !ok || item.GroupedID != msg.GroupedID {
			continue
		}
		if _, dup := seen[item.ID]; dup {
			continue
		}
		seen[item.ID] = struct{}{}
		album = append(album, item)
	}

	sort.Slice(album, func(i, j int) bool { return album[i].ID < album[j].ID })
	return album, nil
}
