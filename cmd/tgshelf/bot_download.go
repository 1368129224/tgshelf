// Bot download executor wiring: injects the internal/tg resolve pipeline and download kernel into
// internal/bot.
//
// Layering rule: internal/bot knows nothing about gotd types and only receives a DownloadFunc
// callback; this file is the single place that stitches the two together, so every gotd detail is
// contained here.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/1368129224/tgshelf/internal/bot"
	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/fs"
	"github.com/1368129224/tgshelf/internal/naming"
	"github.com/1368129224/tgshelf/internal/state"
	gotdtg "github.com/gotd/td/tg"
	"go.uber.org/zap"

	"github.com/1368129224/tgshelf/internal/tg"
)

// botDownloadTimeout is the fallback upper bound for resolving plus downloading one task, so a
// stuck job cannot hold a concurrency slot forever.
const botDownloadTimeout = 6 * time.Hour

// errAllFiltered means every item of the task was skipped by the filter.
// Detected with errors.Is; the bot layer uses it to show the "all filtered" status text.
var errAllFiltered = errors.New("no items to download")

// tgLoginAPI adapts the login capabilities of *tg.Client to bot.LoginAPI.
//
// Only three methods, all with primitive parameters and return values: that is the cost and the
// benefit of internal/bot not importing gotd. The bot package only knows the interface, and the
// adapter lives in the wiring layer.
type tgLoginAPI struct {
	client *tg.Client
}

// SendCode sends the login code and returns phone_code_hash.
func (a tgLoginAPI) SendCode(ctx context.Context, phone string) (string, error) {
	return a.client.SendCode(ctx, phone)
}

// SignIn submits the login code; needPassword true means a 2FA password is required.
func (a tgLoginAPI) SignIn(ctx context.Context, phone, phoneCodeHash, code string) (bool, error) {
	return a.client.SignIn(ctx, phone, phoneCodeHash, code)
}

// CheckPassword submits the 2FA password.
func (a tgLoginAPI) CheckPassword(ctx context.Context, password string) error {
	return a.client.CheckPassword(ctx, password)
}

// newBotDownloader builds the download executor injected into the bot.
//
// One call does it all: resolve the link -> render a safe relative path -> disk precheck ->
// chunked download (with progress). The disk precheck must finish before any download artifact
// exists. A task whose link carries the search: prefix bypasses the resolve step and reads its
// media items from the search result cache instead.
func newBotDownloader(cfg *config.Config, pipeline *tg.Pipeline, runner *tg.TaskRunner, searchStore *bot.SearchStore, logger *zap.Logger) bot.DownloadFunc {
	root := strings.TrimSpace(cfg.DownloadPath)
	if logger == nil {
		logger = zap.NewNop()
	}

	// Build the filter from config (once at startup; include/exclude exclusivity is already
	// caught by Validate).
	inc, exc, ferr := tg.NewFilterFromLists(cfg.Download.Include, cfg.Download.Exclude)
	if ferr != nil {
		logger.Error("build download filter failed (mutually exclusive config)", zap.Error(ferr))
	}
	kindSet, kerr := tg.ParseKinds(cfg.Download.Kinds)
	if kerr != nil {
		logger.Error("parse download.kinds failed", zap.Error(kerr))
	}
	filter := &tg.Filter{
		Include: inc,
		Exclude: exc,
		MinSize: cfg.Download.MinSize,
		MaxSize: cfg.Download.MaxSize,
		Kinds:   kindSet,
	}
	if len(inc) > 0 || len(exc) > 0 || filter.MinSize > 0 || filter.MaxSize > 0 || len(kindSet) > 0 {
		logger.Info("download filter enabled",
			zap.Int("include_count", len(inc)),
			zap.Int("exclude_count", len(exc)),
			zap.Int64("min_size", filter.MinSize),
			zap.Int64("max_size", filter.MaxSize),
			zap.Int("kinds_count", len(kindSet)),
		)
	}

	return func(ctx context.Context, task state.Task, report func(tg.TaskProgress)) error {
		ctx, cancel := context.WithTimeout(ctx, botDownloadTimeout)
		defer cancel()

		logger.Info("start resolving download link",
			zap.String("task_id", task.ID),
			zap.String("link", task.Link),
			zap.String("tag", task.Tag),
		)

		// Search tasks carry their media items in the in-memory cache; a t.me link goes through
		// the normal resolve pipeline. Everything below the resolve step is shared.
		var sourceItems []tg.MediaItem
		if rest, ok := strings.CutPrefix(task.Link, bot.SearchLinkPrefix); ok {
			if searchStore == nil {
				return errors.New("search result store is not wired")
			}
			items, ok := searchStore.Get(rest)
			if !ok || len(items) == 0 {
				return errors.New("search results expired (they are kept in memory only), search again")
			}
			sourceItems = items
			logger.Info("search task resolved from cache",
				zap.String("task_id", task.ID),
				zap.String("search_id", rest),
				zap.Int("items_count", len(items)),
			)
		} else {
			resolved, err := pipeline.ResolveLink(ctx, task.Link)
			if err != nil {
				logger.Warn("resolve download link failed",
					zap.String("task_id", task.ID),
					zap.String("link", task.Link),
					zap.Error(err),
				)
				return fmt.Errorf("resolve link: %w", err)
			}
			if len(resolved.Items) == 0 {
				logger.Warn("no usable media in the resolve result",
					zap.String("task_id", task.ID),
					zap.String("link", task.Link),
				)
				return errors.New("this link has no downloadable media (probably a text or service message)")
			}
			sourceItems = resolved.Items
		}

		items := make([]tg.TaskItem, 0, len(sourceItems))
		var skipped []string // descriptions of the filtered-out items (reported to the user at the end)
		isSearchTask := strings.HasPrefix(task.Link, bot.SearchLinkPrefix)
		for _, it := range sourceItems {
			rel, rerr := downloadRelPath(cfg.Download.Template, it, task.Tag)
			if rerr != nil {
				return rerr
			}
			// Three-dimension filter (extension/size/kind). Filtered items are only counted,
			// they never enter the download queue.
			if pass, reason := filter.Pass(it.Kind, it.Size, rel); !pass {
				logger.Info("item skipped by filter",
					zap.String("task_id", task.ID),
					zap.String("file", it.Name),
					zap.Int64("size", it.Size),
					zap.String("reason", reason),
				)
				skipped = append(skipped, fmt.Sprintf("%s (%s)", it.Name, reason))
				continue
			}
			// Path traversal protection: reject the whole task when SafeJoin fails, never write to
			// disk.
			if _, jerr := naming.SafeJoin(root, rel); jerr != nil {
				return fmt.Errorf("path validation failed %q: %w", rel, jerr)
			}
			itemCopy := it
			var refreshFn tg.RefreshFunc
			if isSearchTask {
				// A search task has no single link to re-resolve: the item knows its dialog and
				// message, so the message is fetched again to get a fresh file reference. Large
				// batches run for hours and every reference in the batch expires meanwhile.
				refreshFn = func(rctx context.Context) (gotdtg.InputFileLocationClass, error) {
					return pipeline.RefreshItemLocation(rctx, itemCopy.DialogID, itemCopy.MessageID, mediaDocID(itemCopy))
				}
			} else {
				refreshFn = func(rctx context.Context) (gotdtg.InputFileLocationClass, error) {
					res, rerr := pipeline.ResolveLink(rctx, task.Link)
					if rerr != nil {
						return nil, rerr
					}
					for _, m := range res.Items {
						if m.MessageID == itemCopy.MessageID && mediaDocID(m) == mediaDocID(itemCopy) && m.Location != nil {
							return m.Location, nil
						}
					}
					return nil, errors.New("refresh file_reference: original media not found in the re-resolved result")
				}
			}
			items = append(items, tg.TaskItem{
				Item:    itemCopy,
				RelPath: rel,
				Mtime:   time.Unix(it.Date, 0),
				Refresh: refreshFn,
			})
		}

		// Do not start an empty task when every item was filtered out.
		if len(items) == 0 {
			logger.Info("all items filtered, task not started",
				zap.String("task_id", task.ID),
				zap.String("link", task.Link),
				zap.Int("skipped_count", len(skipped)),
			)
			return fmt.Errorf("%w: all %d items were filtered out (first reason: %s)",
				errAllFiltered, len(skipped), skipped[0])
		}

		// Log when some items were skipped (the bot layer reports the count in the final
		// message).
		if len(skipped) > 0 {
			logger.Info("some items skipped by filter",
				zap.String("task_id", task.ID),
				zap.Int("skipped_count", len(skipped)),
				zap.Int("will_download", len(items)),
				zap.Strings("skipped", skipped),
			)
		}

		var total int64
		for _, it := range items {
			if it.Item.Size > 0 {
				total += it.Item.Size
			}
		}
		if err := fs.EnsureDir(root, 0o755); err != nil {
			return fmt.Errorf("create download directory (no data downloaded): %w", err)
		}
		if err := fs.CheckSpace(root, total); err != nil {
			return fmt.Errorf("disk space precheck (no data downloaded): %w", err)
		}

		logger.Info("download precheck passed, starting chunked download",
			zap.String("task_id", task.ID),
			zap.String("link", task.Link),
			zap.String("tag", task.Tag),
			zap.Int("items_count", len(items)),
			zap.Int64("total_bytes", total),
		)

		result, rerr := runner.Run(ctx, task.ID, items, report)
		if rerr != nil {
			logger.Warn("download task failed",
				zap.String("task_id", task.ID),
				zap.String("link", task.Link),
				zap.Error(rerr),
			)
			return rerr
		}
		if result == nil {
			return nil
		}
		// Task-level terminal state: anything other than DONE returns the reason so the bot layer
		// can show a failure message.
		if result.State != state.StateDone && result.Err != nil {
			logger.Warn("download task not fully successful",
				zap.String("task_id", task.ID),
				zap.String("link", task.Link),
				zap.String("state", string(result.State)),
				zap.Error(result.Err),
			)
			return result.Err
		}
		logger.Info("chunked download finished",
			zap.String("task_id", task.ID),
			zap.String("link", task.Link),
			zap.String("tag", task.Tag),
			zap.Int("items_count", len(items)),
			zap.Int64("total_bytes", total),
		)
		return nil
	}
}
