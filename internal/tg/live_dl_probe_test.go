//go:build live

package tg

import (
	"context"
	"fmt"
	"testing"
	"time"

	gotdtg "github.com/gotd/td/tg"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/state"
)

// live_dl_probe_test.go - live download probe (compiled only with -tags live).
//
// It records two key facts (measured on 2026-11-02, sample
// t.me/telegram/193, video size=2076378, dc=1):
//
//   1. The server's acceptance domain for an UploadGetFile limit is "a power of two in
//      4KiB..1MiB with offset % limit == 0": limit=1024/491520/503808/503514 all get
//      400 LIMIT_INVALID, while limit=4096/524288 succeed.
//   2. When offset+limit runs past the end of the file the server returns "the remaining
//      bytes" (503514) instead of an error -- the tail chunk must therefore be handled
//      with a full-chunk request plus short-response semantics.
//
// Together these prove that truncating the tail chunk's limit in ChunkPlan.Offset does not
// work against the real server (the tail chunk therefore switched to "full chunk request +
// short response" semantics, see the workaround in the cmd layer).

// TestLiveLimitProbe probes how well the server accepts unconventional limits.
func TestLiveLimitProbe(t *testing.T) {
	root := liveRepoRoot(t)
	cfg, err := config.Load(root + "/tgshelf_config.toml")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	storage, err := OpenSession(root + "/data/session.json")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer storage.Close()
	client, err := New(cfg, storage)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	err = client.Run(ctx, func(ctx context.Context) error {
		api := client.API()
		resolver, rerr := NewPeerResolver(api, nil, time.Minute)
		if rerr != nil {
			return rerr
		}
		pipeline, perr := NewPipeline(api, resolver)
		if perr != nil {
			return perr
		}
		res, err := pipeline.ResolveLink(ctx, "https://t.me/telegram/193")
		if err != nil {
			return err
		}
		item := res.Items[0]
		// Tail-chunk offset: 2076378 bytes with 512KiB chunks -> the last chunk starts at
		// 1572864, and the limit truncated to the file size would be 503514 (which the
		// real server rejects).
		const off = int64(1572864)
		for _, limit := range []int{503514, 503808, 491520, 1024, 4096, 524288} {
			buf, err := api.UploadGetFile(ctx, &gotdtg.UploadGetFileRequest{
				Location: item.Location,
				Offset:   off,
				Limit:    limit,
			})
			if err != nil {
				t.Logf("limit=%d: ERR %v", limit, err)
			} else if f, ok := buf.(*gotdtg.UploadFile); ok {
				t.Logf("limit=%d: ok bytes=%d", limit, len(f.Bytes))
			} else {
				t.Logf("limit=%d: ok type=%T", limit, buf)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	_ = state.New
	_ = fmt.Sprint
}
