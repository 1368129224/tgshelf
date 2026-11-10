package bot

import (
	"fmt"
	"sync"
	"testing"
)

// The same link reuses the same ID; different links get different IDs; an ID is 8 hex digits.
func TestLinkStorePutReuse(t *testing.T) {
	ls := NewLinkStore(10)
	a := "https://t.me/foo/1"
	id1 := ls.Put(a)
	id2 := ls.Put(a)
	if id1 != id2 {
		t.Fatalf("the same link must reuse the same ID: %q vs %q", id1, id2)
	}
	if len(id1) != 8 {
		t.Fatalf("a short ID must be 8 characters: %q", id1)
	}
	for _, c := range id1 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("a short ID must be hexadecimal: %q", id1)
		}
	}
	b := "https://t.me/foo/2"
	if ls.Put(b) == id1 {
		t.Fatal("different links must not share an ID")
	}
}

// The capacity limit evicts the least recently used entry; a get hit refreshes the LRU order.
func TestLinkStoreLRUEviction(t *testing.T) {
	ls := NewLinkStore(3)
	ids := make([]string, 4)
	for i := 0; i < 4; i++ {
		ids[i] = ls.Put(fmt.Sprintf("https://t.me/ch/%d", i))
	}
	// 0 is the oldest and must be evicted.
	if _, ok := ls.Get(ids[0]); ok {
		t.Fatal("the oldest entry must be evicted")
	}
	// 1/2/3 are still there.
	for _, id := range ids[1:] {
		if _, ok := ls.Get(id); !ok {
			t.Fatalf("entry %q must not be evicted", id)
		}
	}

	// A get hit refreshes the LRU order: touch 1, then insert two new entries; 2 must be
	// evicted rather than 1.
	ls2 := NewLinkStore(3)
	i1 := ls2.Put("l1")
	i2 := ls2.Put("l2")
	i3 := ls2.Put("l3")
	if _, ok := ls2.Get(i1); !ok {
		t.Fatal("i1 must exist")
	}
	ls2.Put("l4") // evicts i2 (least recently used)
	if _, ok := ls2.Get(i2); ok {
		t.Fatal("i2 must be evicted")
	}
	if _, ok := ls2.Get(i1); !ok {
		t.Fatal("i1 must not be evicted after a get refreshed it")
	}
	if _, ok := ls2.Get(i3); !ok {
		t.Fatal("i3 must not be evicted")
	}
	// Reuse: putting l1 again still returns i1.
	if again := ls2.Put("l1"); again != i1 {
		t.Fatalf("putting an entry refreshed by get must return the same ID: %q vs %q", again, i1)
	}
}

// The 2000 cap: after 2500 inserts only 2000 remain, and the oldest 500 are evicted.
func TestLinkStoreCap2000(t *testing.T) {
	ls := NewLinkStore(LinkStoreMaxSize)
	first := make([]string, 500)
	for i := 0; i < 2500; i++ {
		id := ls.Put(fmt.Sprintf("https://t.me/ch/%d", i))
		if i < 500 {
			first[i] = id
		}
	}
	if got := ls.Len(); got != LinkStoreMaxSize {
		t.Fatalf("capacity must be %d, got %d", LinkStoreMaxSize, got)
	}
	for i, id := range first {
		if _, ok := ls.Get(id); ok {
			t.Fatalf("the oldest entry #%d must not survive", i)
		}
	}
}

// Concurrency safety (with -race).
func TestLinkStoreConcurrent(t *testing.T) {
	ls := NewLinkStore(400) // 8x50 distinct links exactly fill the capacity
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				link := fmt.Sprintf("https://t.me/ch/%d/%d", g, i%50)
				id := ls.Put(link)
				if _, ok := ls.Get(id); !ok {
					t.Errorf("Get missed after concurrent Put: %q", id)
				}
			}
		}(g)
	}
	wg.Wait()
}
