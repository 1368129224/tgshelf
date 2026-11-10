// callback_data encoding and decoding: a compact format that fits Telegram's hard 64-byte limit.
// Short IDs and tag indices replace long links and Chinese tag text.
package bot

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CallbackDataMaxBytes is Telegram's hard limit (in bytes) on
// InlineKeyboardButton.callback_data. Exceeding it makes the whole keyboard fail to send.
const CallbackDataMaxBytes = 64

// Callback action prefixes:
//
//	t:<shortID>:<page>:<tagIdx>  select a tag
//	x:<shortID>                  cancel the selection
//	p:<shortID>:<page>           change page
//	c:<taskID>                   cancel a task
//	r:<taskID>                   retry
//	n                            no-op (page indicator button)
const (
	ActionSelect       = "select"
	ActionCancelSelect = "cancel_select"
	ActionPage         = "page"
	ActionCancelTask   = "cancel_task"
	ActionRetry        = "retry"
	ActionNoop         = "noop"
)

// ErrCallbackDataTooLong means the encoded callback_data is longer than 64 bytes.
var ErrCallbackDataTooLong = errors.New("bot: callback_data exceeds the 64-byte limit")

// Callback is the parsed or encoded callback_data structure.
// Fields are used per Action: select needs ShortID/Page/TagIdx; page needs ShortID/Page;
// cancel_task/retry need TaskID; cancel_select needs ShortID; noop has no fields.
type Callback struct {
	Action  string
	ShortID string
	Page    int
	TagIdx  int
	TaskID  string
}

// Encode encodes a Callback as a callback_data string; it returns an error above 64 bytes.
func (c Callback) Encode() (string, error) {
	var s string
	switch c.Action {
	case ActionSelect:
		s = fmt.Sprintf("t:%s:%d:%d", c.ShortID, c.Page, c.TagIdx)
	case ActionCancelSelect:
		s = "x:" + c.ShortID
	case ActionPage:
		s = fmt.Sprintf("p:%s:%d", c.ShortID, c.Page)
	case ActionCancelTask:
		s = "c:" + c.TaskID
	case ActionRetry:
		s = "r:" + c.TaskID
	case ActionNoop:
		s = "n"
	default:
		return "", fmt.Errorf("bot: unknown callback action %q", c.Action)
	}
	if len(s) > CallbackDataMaxBytes {
		return "", fmt.Errorf("%w: %d > %d", ErrCallbackDataTooLong, len(s), CallbackDataMaxBytes)
	}
	return s, nil
}

// MustEncode is Encode but panics when the limit is exceeded. It is used only in tests and when
// building keyboards (with a short ID the limit cannot realistically be hit).
func MustEncode(c Callback) string {
	s, err := c.Encode()
	if err != nil {
		panic(err)
	}
	return s
}

// ParseCallback parses callback_data; malformed input yields a readable error.
func ParseCallback(data string) (Callback, error) {
	if data == "" {
		return Callback{}, errors.New("bot: callback_data is empty")
	}
	if data == "n" {
		return Callback{Action: ActionNoop}, nil
	}
	head, rest, ok := strings.Cut(data, ":")
	if !ok {
		return Callback{}, fmt.Errorf("bot: callback_data is missing the separator %q", data)
	}
	switch head {
	case "t":
		parts := strings.Split(rest, ":")
		if len(parts) != 3 {
			return Callback{}, fmt.Errorf("bot: malformed select payload %q", data)
		}
		page, err := strconv.Atoi(parts[1])
		if err != nil {
			return Callback{}, fmt.Errorf("bot: invalid page field %q: %w", parts[1], err)
		}
		idx, err := strconv.Atoi(parts[2])
		if err != nil {
			return Callback{}, fmt.Errorf("bot: invalid tag index field %q: %w", parts[2], err)
		}
		return Callback{Action: ActionSelect, ShortID: parts[0], Page: page, TagIdx: idx}, nil
	case "p":
		shortID, pageS, ok := strings.Cut(rest, ":")
		if !ok {
			return Callback{}, fmt.Errorf("bot: malformed page payload %q", data)
		}
		page, err := strconv.Atoi(pageS)
		if err != nil {
			return Callback{}, fmt.Errorf("bot: invalid page field %q: %w", pageS, err)
		}
		return Callback{Action: ActionPage, ShortID: shortID, Page: page}, nil
	case "x":
		if rest == "" {
			return Callback{}, fmt.Errorf("bot: cancel_select is missing the short ID %q", data)
		}
		return Callback{Action: ActionCancelSelect, ShortID: rest}, nil
	case "c":
		if rest == "" {
			return Callback{}, fmt.Errorf("bot: cancel_task is missing the task ID %q", data)
		}
		return Callback{Action: ActionCancelTask, TaskID: rest}, nil
	case "r":
		if rest == "" {
			return Callback{}, fmt.Errorf("bot: retry is missing the task ID %q", data)
		}
		return Callback{Action: ActionRetry, TaskID: rest}, nil
	default:
		return Callback{}, fmt.Errorf("bot: unknown callback action %q", head)
	}
}
