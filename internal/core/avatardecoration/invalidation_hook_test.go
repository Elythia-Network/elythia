package avatardecoration

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// TestResolver_InvalidationHook pins that Invalidate drops the local catalog
// before calling the hook that tells the other processes (#3664), and that
// InvalidateLocally (what a received invalidation applies) never calls it.
func TestResolver_InvalidationHook(t *testing.T) {
	repo := testutil.NewMockAvatarDecorationRepository()
	repo.Decorations["d1"] = &model.AvatarDecoration{ID: "d1", URL: "https://e/1.png"}
	r := NewResolver(repo)
	calls := 0
	r.SetInvalidationHook(func() {
		calls++
		r.mu.RLock()
		defer r.mu.RUnlock()
		assert.Nil(t, r.urls, "the local catalog is dropped before the hook runs")
	})
	_, ok := r.LookupURL("d1")
	assert.True(t, ok)

	r.Invalidate()
	assert.Equal(t, 1, calls)

	repo.Decorations["d2"] = &model.AvatarDecoration{ID: "d2", URL: "https://e/2.png"}
	_, ok = r.LookupURL("d2")
	assert.True(t, ok)
	repo.Decorations["d3"] = &model.AvatarDecoration{ID: "d3", URL: "https://e/3.png"}
	r.InvalidateLocally()
	_, ok = r.LookupURL("d3")
	assert.True(t, ok)
	assert.Equal(t, 1, calls)

	var nilResolver *Resolver
	nilResolver.InvalidateLocally()
}

// invalidatingDecoRepo invalidates the resolver while List is reading.
type invalidatingDecoRepo struct {
	*testutil.MockAvatarDecorationRepository
	during func()
}

func (f *invalidatingDecoRepo) List() ([]*model.AvatarDecoration, error) {
	rows, err := f.MockAvatarDecorationRepository.List()
	if f.during != nil {
		d := f.during
		f.during = nil
		d()
	}
	return rows, err
}

// TestResolver_InvalidationDuringReadIsNotStored pins that a catalog read
// while an invalidation arrived answers that one lookup but is not kept, so
// the next lookup reads again instead of serving it for the TTL (#3664).
func TestResolver_InvalidationDuringReadIsNotStored(t *testing.T) {
	repo := &invalidatingDecoRepo{MockAvatarDecorationRepository: testutil.NewMockAvatarDecorationRepository()}
	r := NewResolver(repo)
	repo.during = func() {
		repo.Decorations["new"] = &model.AvatarDecoration{ID: "new", URL: "https://e/new.png"}
		r.InvalidateLocally()
	}
	_, ok := r.LookupURL("new")
	assert.False(t, ok, "the in-flight read answers with what it read")
	_, ok = r.LookupURL("new")
	assert.True(t, ok, "the stale read was not kept")
}

func TestEmojiResolver_InvalidationHook(t *testing.T) {
	src := &fakeEmojiSource{rows: []*model.Emoji{{ID: "e1", Name: "one", OriginalURL: "https://e/1.png"}}}
	r := NewEmojiResolver(src)
	calls := 0
	r.SetInvalidationHook(func() {
		calls++
		r.mu.RLock()
		defer r.mu.RUnlock()
		assert.Nil(t, r.entries, "the local map is dropped before the hook runs")
	})
	_, _, ok := r.LookupEmojiDecoration("e1")
	assert.True(t, ok)
	r.Invalidate()
	assert.Equal(t, 1, calls)

	src.rows = append(src.rows, &model.Emoji{ID: "e2", Name: "two", OriginalURL: "https://e/2.png"})
	_, _, ok = r.LookupEmojiDecoration("e1")
	assert.True(t, ok)
	src.rows = append(src.rows, &model.Emoji{ID: "e3", Name: "three", OriginalURL: "https://e/3.png"})
	r.InvalidateLocally()
	_, _, ok = r.LookupEmojiDecoration("e3")
	assert.True(t, ok)
	assert.Equal(t, 1, calls)

	var nilResolver *EmojiResolver
	nilResolver.InvalidateLocally()
}

// invalidatingEmojiSource invalidates the resolver while ListLocal is reading.
type invalidatingEmojiSource struct {
	rows   []*model.Emoji
	during func()
}

func (f *invalidatingEmojiSource) ListLocal() ([]*model.Emoji, error) {
	rows := f.rows
	if f.during != nil {
		d := f.during
		f.during = nil
		d()
	}
	return rows, nil
}

func TestEmojiResolver_InvalidationDuringReadIsNotStored(t *testing.T) {
	src := &invalidatingEmojiSource{}
	r := NewEmojiResolver(src)
	src.during = func() {
		src.rows = []*model.Emoji{{ID: "e1", Name: "one", OriginalURL: "https://e/1.png"}}
		r.InvalidateLocally()
	}
	_, _, ok := r.LookupEmojiDecoration("e1")
	assert.False(t, ok, "the in-flight read answers with what it read")
	_, _, ok = r.LookupEmojiDecoration("e1")
	assert.True(t, ok, "the stale read was not kept")
}
