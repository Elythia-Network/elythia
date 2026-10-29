package federation

import (
	"sync"
	"time"
)

const (
	localReferenceCacheTTL = 5 * time.Minute
	localReferenceCacheCap = 4096
)

type localNoteReference struct {
	authorID string
	at       time.Time
}

// localReferenceCache stores only confirmed positive results. Missing,
// disabled, and unobserved references are never cached.
type localReferenceCache struct {
	mu    sync.Mutex
	users map[string]time.Time
	notes map[string]localNoteReference
}

func (c *localReferenceCache) hasUser(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen, ok := c.users[id]
	if !ok {
		return false
	}
	if now.Sub(seen) >= localReferenceCacheTTL {
		delete(c.users, id)
		return false
	}
	return true
}

func (c *localReferenceCache) noteAuthor(id string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.notes[id]
	if !ok {
		return "", false
	}
	if now.Sub(entry.at) >= localReferenceCacheTTL {
		delete(c.notes, id)
		return "", false
	}
	return entry.authorID, true
}

func (c *localReferenceCache) rememberUser(id string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.users == nil {
		c.users = make(map[string]time.Time)
	}
	pruneLocalUserReferences(c.users, now)
	if _, exists := c.users[id]; !exists && len(c.users) >= localReferenceCacheCap {
		evictOldestLocalUserReference(c.users)
	}
	c.users[id] = now
}

func (c *localReferenceCache) rememberNote(id, authorID string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notes == nil {
		c.notes = make(map[string]localNoteReference)
	}
	pruneLocalNoteReferences(c.notes, now)
	if _, exists := c.notes[id]; !exists && len(c.notes) >= localReferenceCacheCap {
		evictOldestLocalNoteReference(c.notes)
	}
	c.notes[id] = localNoteReference{authorID: authorID, at: now}
}

func pruneLocalUserReferences(users map[string]time.Time, now time.Time) {
	for id, seen := range users {
		if now.Sub(seen) >= localReferenceCacheTTL {
			delete(users, id)
		}
	}
}

func evictOldestLocalUserReference(users map[string]time.Time) {
	var oldestID string
	var oldest time.Time
	for id, seen := range users {
		if oldestID == "" || seen.Before(oldest) {
			oldestID, oldest = id, seen
		}
	}
	delete(users, oldestID)
}

func pruneLocalNoteReferences(notes map[string]localNoteReference, now time.Time) {
	for id, entry := range notes {
		if now.Sub(entry.at) >= localReferenceCacheTTL {
			delete(notes, id)
		}
	}
}

func evictOldestLocalNoteReference(notes map[string]localNoteReference) {
	var oldestID string
	var oldest time.Time
	for id, entry := range notes {
		if oldestID == "" || entry.at.Before(oldest) {
			oldestID, oldest = id, entry.at
		}
	}
	delete(notes, oldestID)
}
