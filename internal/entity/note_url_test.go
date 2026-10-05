package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// リモート note の permalink が応答に載ること (#2729)。
//
// **`uri` とは別 field。** Mastodon 系では `uri` が AP object、`url` が Web ページを
// 指すので、両方が出る。
func TestPackNote_CarriesURL(t *testing.T) {
	idGen := newTestIDGen(t)
	uri := "https://remote.example/users/alice/statuses/1"
	url := "https://remote.example/@alice/1"
	e := PackNote(&model.Note{
		ID:         idGen.Generate(time.Now()),
		UserID:     "alice",
		Visibility: model.NoteVisibilityPublic,
		URI:        &uri,
		URL:        &url,
		Reactions:  datatypes.JSON([]byte("{}")),
	}, idGen)

	b, err := json.Marshal(e)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, url, got["url"])
	assert.Equal(t, uri, got["uri"])
}

// **`url` が無い note では key ごと落ちる** (`json:"url,omitempty"`)。`null` では
// ない。upstream も `url: note.url ?? undefined` なので同じ形になる (#2729)。
func TestPackNote_OmitsURLWhenAbsent(t *testing.T) {
	idGen := newTestIDGen(t)
	e := PackNote(&model.Note{
		ID:         idGen.Generate(time.Now()),
		UserID:     "alice",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}, idGen)

	b, err := json.Marshal(e)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	_, ok := got["url"]
	assert.False(t, ok, "url は key ごと落ちる (null ではない)")
}
