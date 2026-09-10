package scout

import (
	"container/list"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// How long a minted playback link is reused before /play mints another.
//
// Well under what any provider gives a link — TorBox's and Real-Debrid's both live for hours — so a memo
// hit is never the reason a link has expired. The memo exists for the minutes around a play: the stream
// list's probe mints a link and throws it away, then /play mints the same file again; a viewer backs out
// and presses play again; the player retries. Each of those was a full resolve against the debrid.
const linkMemoTTL = 10 * time.Minute

// How many links are kept. One household opens a handful of titles in ten minutes; this is room for
// every release a probe fan-out touches across many lists, and a ceiling on what a caller can make the
// process hold, since both the key and the set of keys come from requests.
const linkMemoMaxEntries = 256

// memoLink is one minted link. `checked` says the link has passed checkLink, so a hit can skip it.
type memoLink struct {
	key     string
	link    string
	checked bool
	expires time.Time
}

// linkMemo remembers minted playback links, in process memory only.
//
// Deliberately NOT the shared Cache: that one writes through to disk in production, and a playback link
// is a credential for as long as it lives — anyone holding it streams the file on the account's behalf.
// Losing the memo on a restart costs one resolve per title, which is exactly what happened before it
// existed.
//
// Usable as a zero value, because tests build the handler as a struct literal.
type linkMemo struct {
	mu    sync.Mutex
	ll    *list.List // front = most recently used
	items map[string]*list.Element
}

// get returns a live entry. An expired one is dropped on the way past.
func (m *linkMemo) get(key string, now time.Time) (memoLink, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return memoLink{}, false
	}
	e := el.Value.(*memoLink)
	if !now.Before(e.expires) {
		m.ll.Remove(el)
		delete(m.items, key)
		return memoLink{}, false
	}
	m.ll.MoveToFront(el)
	return *e, true
}

// put stores a freshly minted link, replacing whatever was there and starting its TTL now.
func (m *linkMemo) put(key, link string, checked bool, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.items == nil {
		m.items = make(map[string]*list.Element)
		m.ll = list.New()
	}
	entry := &memoLink{key: key, link: link, checked: checked, expires: now.Add(linkMemoTTL)}
	if el, ok := m.items[key]; ok {
		el.Value = entry
		m.ll.MoveToFront(el)
		return
	}
	m.items[key] = m.ll.PushFront(entry)
	for m.ll.Len() > linkMemoMaxEntries {
		oldest := m.ll.Back()
		m.ll.Remove(oldest)
		delete(m.items, oldest.Value.(*memoLink).key)
	}
}

// markChecked records that the remembered link passed checkLink, without restarting its TTL — the TTL
// counts from when the debrid minted it, not from when it was last looked at.
func (m *linkMemo) markChecked(key, link string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		if e := el.Value.(*memoLink); e.link == link {
			e.checked = true
		}
	}
}

// forget drops an entry: a link that failed, or one the client says it could not open.
func (m *linkMemo) forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.ll.Remove(el)
		delete(m.items, key)
	}
}

// linkMemoKey names the FILE for these accounts: whose debrid minted it, which torrent, which file in it.
//
// The accounts are part of the key because a link is minted on one account's behalf — two installs on one
// instance must never be handed each other's. Hashed, so the tokens are never a map key in memory.
//
// NoAdd is left out on purpose. The probe resolves read-only and /play does not, but they name the same
// file, and the probe's link is the one /play would otherwise mint again.
func linkMemoKey(config *Config, t ResolveTarget) string {
	var accounts strings.Builder
	for _, d := range config.Debrid {
		accounts.WriteString(string(d.Service))
		accounts.WriteByte(0)
		accounts.WriteString(d.Token)
		accounts.WriteByte(0)
	}
	key := "link:" + keyHash(accounts.String()) + ":" + t.InfoHash
	if t.FileIdx != nil {
		key += ":f" + strconv.Itoa(*t.FileIdx)
	}
	if t.Season != nil && t.Episode != nil {
		key += fmt.Sprintf(":s%de%d", *t.Season, *t.Episode)
	}
	return key
}
