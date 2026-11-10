package progress

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"

	tg "github.com/1368129224/tgshelf/internal/tg"
)

// fakeSender is a fake EditorSender: it records the parameters and call count of every call, can
// be configured to return an error, and never touches the network.
type fakeSender struct {
	mu    sync.Mutex
	calls int
	texts []string
	err   error
}

func (f *fakeSender) EditMessageText(_ context.Context, p *tgbot.EditMessageTextParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.texts = append(f.texts, p.Text)
	return nil, f.err
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestRender_BarAndTitle verifies the basename on the title line, the [current/total] counter and
// the 15-cell progress bar.
func TestRender_BarAndTitle(t *testing.T) {
	p := tg.TaskProgress{
		TaskID:     "t1",
		BytesDone:  624,
		BytesTotal: 1000,
		ItemsDone:  2,
		ItemsTotal: 5,
		Current:    "dir/video.mp4",
	}
	out := Render(p, 1000)

	// Title line: basename + [3/5] (2 files done -> downloading the 3rd).
	if !strings.HasPrefix(out, "🎬 video.mp4") {
		t.Fatalf("title line must use the basename and start with 🎬, got %q", firstLine(out))
	}
	if !strings.Contains(out, "[3/5]") {
		t.Fatalf("multiple files must show [current/total]=[3/5], got %q", out)
	}

	// Progress bar: exactly 15 cells, 62.4% -> round(9.36)=9 filled cells.
	bar := strings.Repeat("█", 9) + strings.Repeat("░", 6)
	if !strings.Contains(out, bar+" 62.4%") {
		t.Fatalf("progress bar must have 15 cells with percentage 62.4%%, got %q", out)
	}
}

// TestRender_ItemCounterEdges covers the counter boundaries: the first progress must not be
// [0/N], and a fully complete task must not exceed [N/N] (real acceptance testing hit [0/9]).
func TestRender_ItemCounterEdges(t *testing.T) {
	cases := []struct {
		name       string
		itemsDone  int
		itemsTotal int
		want       string
	}{
		{"first progress", 0, 9, "[1/9]"},
		{"middle", 4, 9, "[5/9]"},
		{"all done", 9, 9, "[9/9]"},
		{"over the limit", 12, 9, "[9/9]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := tg.TaskProgress{
				BytesDone: 1, BytesTotal: 10,
				ItemsDone: c.itemsDone, ItemsTotal: c.itemsTotal,
				Current: "a/b.jpg",
			}
			out := Render(p, 10)
			if !strings.Contains(out, c.want) {
				t.Fatalf("ItemsDone=%d/%d must show %s, got %q", c.itemsDone, c.itemsTotal, c.want, out)
			}
			if strings.Contains(out, "[0/") {
				t.Fatalf("[0/N] must never appear, got %q", out)
			}
		})
	}
}

// TestRender_ZeroAndHundredAndOver verifies clamping at 0%, 100% and above 100%.
func TestRender_ZeroAndHundredAndOver(t *testing.T) {
	cases := []struct {
		name string
		done int64
		want string // progress-bar line prefix (bar + percentage)
	}{
		{"0%", 0, strings.Repeat("░", 15) + " 0.0%"},
		{"100%", 1000, strings.Repeat("█", 15) + " 100.0%"},
		{"over", 1500, strings.Repeat("█", 15) + " 100.0%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Render(tg.TaskProgress{BytesDone: tc.done, BytesTotal: 1000}, 1000)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("want bar %q in output, got %q", tc.want, out)
			}
		})
	}
}

// TestRender_UnknownTotal verifies that BytesTotal<=0 shows the downloaded amount rather than a
// percentage.
func TestRender_UnknownTotal(t *testing.T) {
	p := tg.TaskProgress{BytesDone: 12 * 1024 * 1024} // 12 MiB, no total
	out := Render(p, 0)
	if strings.Contains(out, "%") {
		t.Fatalf("no percentage may appear when the total is unknown, got %q", out)
	}
	if !strings.Contains(out, "Downloaded 12.0 MB") {
		t.Fatalf("the downloaded amount must be shown when the total is unknown, got %q", out)
	}
}

// TestRender_SingleFileNoCounter verifies a single file does not show [1/1].
func TestRender_SingleFileNoCounter(t *testing.T) {
	out := Render(tg.TaskProgress{BytesDone: 1, BytesTotal: 2, ItemsTotal: 1}, 2)
	if strings.Contains(out, "[") {
		t.Fatalf("a single file must not show a file counter, got %q", out)
	}
}

// TestEditor_ThrottleByTime verifies time-based throttling: 100 updates, advancing 100ms each.
//
// Expected value derivation (t_k = k*100ms, k=0..99):
//   - t=0: first edit (never edited before, so it always goes through) -> lastEdit=0
//   - t=0.1s~2.9s: less than 3s since the previous edit -> dropped (29 updates)
//   - t=3.0s: 3000ms >= 3000ms (only strictly less is dropped) -> edit -> lastEdit=3.0s
//   - t=3.1s~5.9s: dropped; t=6.0s: edit; t=6.1s~8.9s: dropped; t=9.0s: edit
//   - t=9.1s~9.9s: dropped (9 updates)
//
// Edit instants {0s, 3s, 6s, 9s} - so exactly 4 edits are expected.
func TestEditor_ThrottleByTime(t *testing.T) {
	fake := &fakeSender{}
	e := NewEditor(fake, zap.NewNop())
	cur := time.Unix(1_000_000, 0)
	e.now = func() time.Time { return cur }

	for i := 0; i < 100; i++ {
		e.Update(context.Background(), 1, 2, "text", false)
		cur = cur.Add(100 * time.Millisecond)
	}
	const want = 4
	if got := fake.count(); got != want {
		t.Fatalf("want %d edits, got %d", want, got)
	}
}

// TestEditor_FinalSkipsThrottle verifies that final=true skips throttling and edits immediately.
func TestEditor_FinalSkipsThrottle(t *testing.T) {
	fake := &fakeSender{}
	e := NewEditor(fake, zap.NewNop())
	cur := time.Unix(1_000_000, 0)
	e.now = func() time.Time { return cur }

	e.Update(context.Background(), 1, 2, "a", false) // t=0: edit
	cur = cur.Add(100 * time.Millisecond)            // t=0.1s, still inside the window
	e.Update(context.Background(), 1, 2, "b", true)  // final: must edit immediately
	if got := fake.count(); got != 2 {
		t.Fatalf("final must skip throttling, want 2 edits, got %d", got)
	}
	// The terminal text must actually be sent (not silently dropped).
	if last := fake.texts[len(fake.texts)-1]; last != "b" {
		t.Fatalf("the terminal text must be the last edited content, got %q", last)
	}
}

// TestEditor_EWMAGradual verifies EWMA gradualness: speed jumps from 10 to 100 and the smoothed
// value must approach 100 step by step, with intermediate values inside the open interval
// (10, 100) rather than one leap.
func TestEditor_EWMAGradual(t *testing.T) {
	e := NewEditor(&fakeSender{}, zap.NewNop())
	cur := time.Unix(1_000_000, 0)
	e.now = func() time.Time { return cur }

	// First segment: 10 bytes in 1 second -> instantaneous 10 B/s (the first sample builds the
	// baseline, the second sample yields the speed).
	e.sample(1, 2, 0)
	cur = cur.Add(time.Second)
	sp1, ok1 := e.sample(1, 2, 10)
	if !ok1 || sp1 != 10 {
		t.Fatalf("initial speed must be 10, got %v ok=%v", sp1, ok1)
	}

	// Second segment: speed jumps to 100 B/s and sampling continues; EWMA (alpha=0.3) approaches
	// 100 gradually.
	midSeen := false
	for i := 0; i < 8; i++ {
		cur = cur.Add(time.Second)
		sp, _ := e.sample(1, 2, 10+100*int64(i+1)) // +100 bytes per second
		if sp > 10 && sp < 100 {
			midSeen = true
		}
		if sp >= 100 {
			t.Fatalf("sample %d has EWMA=%v already at the instantaneous 100 without smoothing", i+1, sp)
		}
	}
	if !midSeen {
		t.Fatalf("the EWMA must land in the open interval (10,100) at least once")
	}
}

// TestEditor_EditErrorNonFatal verifies an edit failure neither panics nor interrupts anything and
// that later updates keep working.
func TestEditor_EditErrorNonFatal(t *testing.T) {
	fake := &fakeSender{err: errors.New("message is not modified")}
	e := NewEditor(fake, zap.NewNop())
	cur := time.Unix(1_000_000, 0)
	e.now = func() time.Time { return cur }

	// Every edit fails, so Update must swallow the error (a panic would blow the test up).
	e.Update(context.Background(), 1, 2, "a", false)
	cur = cur.Add(4 * time.Second)
	e.Update(context.Background(), 1, 2, "b", false)
	cur = cur.Add(4 * time.Second)
	e.Update(context.Background(), 1, 2, "c", true) // the terminal state fails too: log only
	if got := fake.count(); got != 3 {
		t.Fatalf("updates must continue after a failure, want 3, got %d", got)
	}
}

// TestEditor_PerMessageState verifies sampling and throttling state is isolated per
// (chatID, messageID): the first edit of another message is not affected by the first message's
// throttle window.
func TestEditor_PerMessageState(t *testing.T) {
	fake := &fakeSender{}
	e := NewEditor(fake, zap.NewNop())
	cur := time.Unix(1_000_000, 0)
	e.now = func() time.Time { return cur }

	e.Update(context.Background(), 1, 2, "a", false)
	cur = cur.Add(100 * time.Millisecond)
	e.Update(context.Background(), 1, 3, "b", false) // different messageID: must edit immediately
	if got := fake.count(); got != 2 {
		t.Fatalf("per-message state must be isolated, want 2, got %d", got)
	}
}

// TestEditor_ConcurrentUpdateRace calls Update and UpdateProgress concurrently under -race to
// verify the locking around the per-message map. The clock is an atomic variable and the
// goroutines share no non-atomic writes.
func TestEditor_ConcurrentUpdateRace(t *testing.T) {
	fake := &fakeSender{}
	e := NewEditor(fake, zap.NewNop())
	var nanos atomic.Int64
	nanos.Store(time.Unix(1_000_000, 0).UnixNano())
	e.now = func() time.Time { return time.Unix(0, nanos.Load()) }

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			chat := int64(1 + w%2)
			msg := 10 + w
			for i := 0; i < 20; i++ {
				p := tg.TaskProgress{BytesDone: int64(i * 100), BytesTotal: 1000}
				e.UpdateProgress(context.Background(), chat, msg, p, 1000, false)
				nanos.Add(500 * int64(time.Millisecond))
			}
		}(w)
	}
	wg.Wait()
}

// TestSpeedLine_Computing verifies that an unavailable speed shows "calculating...".
func TestSpeedLine_Computing(t *testing.T) {
	if got := speedLine(0, false, 1000, 0); got != "calculating..." {
		t.Fatalf("an unavailable speed must show calculating..., got %q", got)
	}
}

// TestSpeedLine_ETA verifies that a known total yields a remaining time estimated from the
// smoothed speed.
func TestSpeedLine_ETA(t *testing.T) {
	// 100 B/s with 1000 B left -> 10s -> "00:10".
	got := speedLine(100, true, 1000, 0)
	if !strings.Contains(got, "100 B/s") || !strings.Contains(got, "00:10") {
		t.Fatalf("speed/ETA line does not look as expected, got %q", got)
	}
}

// TestFormatETA verifies the mm:ss and HH:MM:SS formats and the negative clamp.
func TestFormatETA(t *testing.T) {
	if got := formatETA(42); got != "00:42" {
		t.Fatalf("42s -> 00:42, got %q", got)
	}
	if got := formatETA(3700); got != "01:01:40" {
		t.Fatalf("3700s -> 01:01:40, got %q", got)
	}
	if got := formatETA(-5); got != "00:00" {
		t.Fatalf("a negative value must clamp to 00:00, got %q", got)
	}
}

// firstLine returns the first line of the text.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
