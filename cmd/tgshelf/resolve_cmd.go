package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tgpkg "github.com/gotd/td/tg"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/fs"
	"github.com/1368129224/tgshelf/internal/tg"
)

// resolveLinks is a flag.Value carrying the multi-value -resolve argument: repeated flags
// (-resolve a -resolve b) and comma separation (-resolve a,b) both work.
type resolveLinks struct{ urls []string }

func (r *resolveLinks) String() string { return strings.Join(r.urls, ",") }

func (r *resolveLinks) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			r.urls = append(r.urls, part)
		}
	}
	return nil
}

// runResolve resolves t.me links one by one with a real session and prints a readable report.
// This is the manual resolution path: cfg.Validate() is not called (no bot_token is
// needed).
//
// Exit code semantics: return nil when everything succeeded; return a summary error when any link
// failed (main then exits 1).
func runResolve(cfg *config.Config, configPath string, urls []string) error {
	if err := preflightTelegram(cfg); err != nil {
		return err
	}
	if len(urls) == 0 {
		return errors.New("usage: tgshelf -resolve <t.me link> [-resolve <link2> ...] [-config X]")
	}

	sessionPath := sessionPathOf(cfg, configPath)
	if _, err := os.Stat(sessionPath); err != nil {
		return fmt.Errorf("not logged in: session file %s not found, run tgshelf -login first", sessionPath)
	}

	storage, err := tg.OpenSession(sessionPath)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = storage.Close() }()

	client, err := tg.New(cfg, storage)
	if err != nil {
		return fmt.Errorf("build Telegram client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var okCount, failCount int
	err = client.Run(ctx, func(ctx context.Context) error {
		api := client.API()
		resolver, rerr := tg.NewPeerResolver(api, nil, time.Minute)
		if rerr != nil {
			return fmt.Errorf("build peer resolver: %w", rerr)
		}
		pipeline, perr := tg.NewPipeline(api, resolver)
		if perr != nil {
			return fmt.Errorf("build resolve pipeline: %w", perr)
		}
		for _, raw := range urls {
			res, err := pipeline.ResolveLink(ctx, raw)
			if err != nil {
				failCount++
				fmt.Printf("[%s] resolve failed (%s): %v\n", raw, sentinelName(err), err)
				continue
			}
			okCount++
			fmt.Print(formatResolved(raw, res))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("run resolve: %w", err)
	}

	fmt.Printf("total %d links, %d succeeded, %d failed\n", len(urls), okCount, failCount)
	if failCount > 0 {
		return fmt.Errorf("resolve finished but %d links failed", failCount)
	}
	return nil
}

// formatResolved renders one Resolved as a readable text report.
// The unit tests assert this output directly, no real network is needed.
func formatResolved(raw string, res *tg.Resolved) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] resolved: peer=%s msg_id=%d %d media items\n",
		raw, peerLabel(res.Peer), res.MsgID, len(res.Items))
	for i, item := range res.Items {
		fmt.Fprintf(&b, "  %d. kind=%s name=%s size=%s dc=%d mime=%s\n",
			i+1, item.Kind.String(), item.Name, fs.HumanBytes(item.Size), item.DCID, item.Mime)
	}
	return b.String()
}

// peerLabel renders an InputPeerClass as a readable "type:ID" fragment.
func peerLabel(p tgpkg.InputPeerClass) string {
	switch v := p.(type) {
	case *tgpkg.InputPeerChannel:
		return fmt.Sprintf("channel:%d", v.ChannelID)
	case *tgpkg.InputPeerUser:
		return fmt.Sprintf("user:%d", v.UserID)
	case *tgpkg.InputPeerChat:
		return fmt.Sprintf("chat:%d", v.ChatID)
	default:
		return fmt.Sprintf("%T", p)
	}
}

// sentinelName describes the failure class by the name of the sentinel matched by errors.Is, and
// returns "none" when nothing matches.
func sentinelName(err error) string {
	switch {
	case errors.Is(err, tg.ErrNotTMeLink):
		return "ErrNotTMeLink"
	case errors.Is(err, tg.ErrChannelInaccessible):
		return "ErrChannelInaccessible"
	case errors.Is(err, tg.ErrMessageDeleted):
		return "ErrMessageDeleted"
	case errors.Is(err, tg.ErrNoMedia):
		return "ErrNoMedia"
	default:
		return "none"
	}
}
