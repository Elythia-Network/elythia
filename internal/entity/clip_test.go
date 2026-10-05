package entity

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestPackClip_Basic(t *testing.T) {
	idGen := newTestIDGen(t)
	clipID := idGen.Generate(time.Now())
	desc := "my clip"
	owner := &model.User{ID: "u1", Username: "alice"}
	lastClipped := time.Date(2026, 4, 10, 1, 2, 3, 0, time.UTC)
	cl := &model.Clip{ID: clipID, UserID: "u1", Name: "favs", Description: &desc, IsPublic: true, LastClippedAt: &lastClipped}

	fav := true
	notesCount := 5
	out := PackClip(cl, idGen, owner, ClipExtras{FavoritedCount: 3, IsFavorited: &fav, NotesCount: &notesCount})
	assert.Equal(t, clipID, out["id"])
	assert.Equal(t, "u1", out["userId"])
	assert.Equal(t, "favs", out["name"])
	assert.Equal(t, &desc, out["description"])
	assert.Equal(t, true, out["isPublic"])
	assert.Equal(t, 5, out["notesCount"])
	// lastClippedAt は ISO ms 形式 (createdAt と統一)。
	assert.Equal(t, "2026-04-10T01:02:03.000Z", out["lastClippedAt"])
	// misskey_dart の Clip.fromJson が非null必須とする field (#1237)。
	createdAt, ok := out["createdAt"].(string)
	assert.True(t, ok, "createdAt must be a non-null string")
	assert.NotEmpty(t, createdAt)
	// viewer 依存 field は ClipExtras から透過する (#1562)
	assert.Equal(t, int64(3), out["favoritedCount"])
	assert.Equal(t, true, out["isFavorited"])
	user, ok := out["user"].(UserLite)
	assert.True(t, ok, "user must be present")
	assert.Equal(t, "u1", user.ID)
}

// 非 owner 閲覧では notesCount を出さず、匿名では isFavorited も出さない
// (upstream ClipEntityService.pack の undefined と同じ省略、#1562)。
func TestPackClip_ViewerDependentFieldsHidden(t *testing.T) {
	idGen := newTestIDGen(t)
	cl := &model.Clip{ID: idGen.Generate(time.Now()), UserID: "u1", Name: "favs", IsPublic: true}

	out := PackClip(cl, idGen, &model.User{ID: "u1"}, ClipExtras{})
	_, hasNotesCount := out["notesCount"]
	assert.False(t, hasNotesCount, "notesCount must be omitted for non-owner viewers")
	_, hasIsFavorited := out["isFavorited"]
	assert.False(t, hasIsFavorited, "isFavorited must be omitted for anonymous viewers")
	assert.Equal(t, int64(0), out["favoritedCount"], "favoritedCount stays non-null for misskey_dart")
}

func TestPackClip_NilOwnerOmitsUser(t *testing.T) {
	idGen := newTestIDGen(t)
	cl := &model.Clip{ID: idGen.Generate(time.Now()), UserID: "u1", Name: "x"}
	out := PackClip(cl, idGen, nil, ClipExtras{})
	_, hasUser := out["user"]
	assert.False(t, hasUser)
}

func TestPackClip_NilInput(t *testing.T) {
	assert.Nil(t, PackClip(nil, nil, nil, ClipExtras{}))
}
