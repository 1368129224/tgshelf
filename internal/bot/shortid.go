// Link short-ID registry: works around Telegram's 64-byte callback_data limit.
// The registry is bounded (LRU): the oldest entry is evicted once the cap is reached.
package bot

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// LinkStoreMaxSize caps the registry so a long-running process cannot grow memory without bound.
const LinkStoreMaxSize = 2000

// lruEntry is a node in the LRU doubly linked list.
type lruEntry struct {
	id   string
	link string
	prev *lruEntry
	next *lruEntry
}

// LinkStore is a bounded bidirectional link ↔ shortID mapping with LRU eviction.
// The same link reuses the same shortID; a get hit refreshes the LRU order. It is safe for
// concurrent use.
//
// The trade-off: old buttons stop working after a restart - but those buttons only ever served
// the download flow of the current session anyway.
type LinkStore struct {
	mu      sync.Mutex
	maxSize int
	// head.next is the oldest entry, tail.prev the newest.
	head   *lruEntry
	tail   *lruEntry
	byID   map[string]string
	byLink map[string]*lruEntry
}

// NewLinkStore creates an empty registry with capacity max (values <= 0 are treated as 1).
func NewLinkStore(max int) *LinkStore {
	if max <= 0 {
		max = 1
	}
	ls := &LinkStore{
		maxSize: max,
		head:    &lruEntry{},
		tail:    &lruEntry{},
		byID:    make(map[string]string),
		byLink:  make(map[string]*lruEntry),
	}
	ls.head.next = ls.tail
	ls.tail.prev = ls.head
	return ls
}

// Put registers a link and returns its short ID; the same link reuses the same ID (and
// refreshes the LRU order).
func (ls *LinkStore) Put(link string) string {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if e, ok := ls.byLink[link]; ok {
		ls.moveBack(e)
		return e.id
	}

	id := newShortID()
	for {
		if _, exists := ls.byID[id]; !exists {
			break
		}
		id = newShortID()
	}
	e := &lruEntry{id: id, link: link}
	ls.byID[id] = link
	ls.byLink[link] = e
	ls.pushBack(e)
	ls.evict()
	return id
}

// Get looks a link up by short ID; a hit refreshes the LRU order, a miss returns ("", false).
func (ls *LinkStore) Get(id string) (string, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	link, ok := ls.byID[id]
	if !ok {
		return "", false
	}
	ls.moveBack(ls.byLink[link])
	return link, true
}

// Len returns the current number of entries (for tests).
func (ls *LinkStore) Len() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.byID)
}

// newShortID generates an 8-character hexadecimal random ID (4 bytes).
func newShortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failures are extremely rare and unrecoverable; a panic matches the
		// behaviour of the global rand helpers.
		panic("bot: crypto/rand read failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// pushBack appends e to the tail of the list (newest).
func (ls *LinkStore) pushBack(e *lruEntry) {
	e.prev = ls.tail.prev
	e.next = ls.tail
	ls.tail.prev.next = e
	ls.tail.prev = e
}

// remove unlinks e from the list.
func (ls *LinkStore) remove(e *lruEntry) {
	e.prev.next = e.next
	e.next.prev = e.prev
	e.prev, e.next = nil, nil
}

// moveBack moves e to the tail of the list (newest; used on Put/Get hits).
func (ls *LinkStore) moveBack(e *lruEntry) {
	ls.remove(e)
	ls.pushBack(e)
}

// evict drops least-recently-used entries until the capacity limit is met again.
func (ls *LinkStore) evict() {
	for len(ls.byID) > ls.maxSize {
		oldest := ls.head.next
		if oldest == ls.tail {
			return
		}
		ls.remove(oldest)
		delete(ls.byID, oldest.id)
		delete(ls.byLink, oldest.link)
	}
}
