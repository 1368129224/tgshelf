// Package tg wraps low-level communication with the Telegram MTProto API and provides session
// management and authentication support.
//
// # Dependency anchors
//
// deps_check.go is a dependency-anchor file, not business code.
// It references every third-party direct dependency of this project through real symbols, ensuring that:
//  1. go mod tidy does not remove those dependencies;
//  2. if a dependency upgrade breaks the API, compilation fails here immediately instead of later in business code.
//
// The current state of the github.com/iyear/tdl/core subpackages (AGPL-3.0, with thanks):
//   - core/dcpool: **used directly by business code** (NewPool in client.go, the download pool in task.go)
//   - core/middlewares/retry, …/recovery: **used directly by business code** (middleware.go)
//   - core/downloader: **unused**, anchored here only. The download core is hand-written (download.go) -
//     upstream has no chunk bitmap and cannot skip completed chunks
//   - core/tmedia: **unused**, anchored here only. Media normalization is hand-written (media.go) -
//     upstream provides no media "kind" dimension, and it goes out of bounds when sizes is empty
//
// For client initialization see internal/tg/client.go (the project's own implementation).
package tg

import (
	tgbot "github.com/go-telegram/bot"
	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/downloader"
	"github.com/iyear/tdl/core/tmedia"
	"github.com/pelletier/go-toml/v2"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// The variables below only anchor dependencies so that go mod tidy does not remove them.
// They take part in no business logic.

// _depDownloaderNew anchors core/downloader by holding a reference to the downloader.New function.
// Note: this project does **not call** it (the download core is hand-written); it only pins the
// dependency and its version.
var _depDownloaderNew = downloader.New

// _depDCPool anchors core/dcpool by declaring a dcpool.Pool interface variable (dcpool is used by
// business code).
var _depDCPool dcpool.Pool

// _depTMediaExtractMedia anchors core/tmedia by holding a reference to the tmedia.ExtractMedia function.
// Note: this project does **not call** it (media normalization is hand-written); it only pins the
// dependency and its version.
var _depTMediaExtractMedia = tmedia.ExtractMedia

// _depBotOption anchors github.com/go-telegram/bot by declaring a bot.Option type variable.
// bot.Option is the option-function type of the Telegram Bot API client, used for real by the bot package.
var _depBotOption tgbot.Option

// _depZapLogger anchors go.uber.org/zap by declaring a *zap.Logger pointer variable.
// zap.Logger is the project-wide structured logger, used for real by the logging package.
var _depZapLogger *zap.Logger

// _depTomlUnmarshal anchors github.com/pelletier/go-toml/v2 by holding a reference to toml.Unmarshal.
// toml.Unmarshal parses TOML config files, used for real by the config package.
var _depTomlUnmarshal = toml.Unmarshal

// _depUnixLockEX anchors golang.org/x/sys/unix by referencing the unix.LOCK_EX constant.
// Together with unix.Flock, unix.LOCK_EX implements the file lock, used by the fs package.
const _depUnixLockEX = unix.LOCK_EX
