package plugintest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
)

// **未登録のキーは分かるエラーにする。** typo で「呼んだつもり」になるのを
// 防ぐため、登録済みの一覧も添える。
func TestHandlers_Lookup(t *testing.T) {
	hs := Handlers{
		"POST /b": func(plugin.Request) (any, error) { return nil, nil },
		"GET /a":  func(plugin.Request) (any, error) { return nil, nil },
	}

	got, err := hs.lookup("GET /a")
	require.NoError(t, err)
	assert.NotNil(t, got)

	_, err = hs.lookup("GET /missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GET /missing")
	// 候補が並びで出る (毎回順序が変わると読みにくい)。
	assert.Contains(t, err.Error(), "[GET /a POST /b]")
}

func TestJobSet_Lookup(t *testing.T) {
	j := &JobSet{Handlers: map[string]plugin.JobHandler{"tick": nil}}

	_, err := j.lookup("tick")
	require.NoError(t, err)

	_, err = j.lookup("nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

// WithSecrets の失敗は、どの名前で失敗したかが分かるエラーにする。
func TestHarness_StoreSecretsReportsTheName(t *testing.T) {
	h := New(t).WithName("p")
	err := h.storeSecrets(map[string]string{"ok": "v", "bad name": "v"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"bad name"`)
}

// 置き場所は本番の repository と同じく、プラグイン名で分けて名前順に返す。
func TestMemSecretRepo_ListByPlugin(t *testing.T) {
	h := New(t).WithName("p").WithSecrets(map[string]string{"b": "1", "a": "2"})
	h.WithName("q").WithSecrets(map[string]string{"c": "3"})
	rows, err := h.secretRepo.ListByPlugin(context.Background(), "p")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "a", rows[0].Name)
	assert.Equal(t, "b", rows[1].Name)
	sts, err := h.secrets.Statuses(context.Background(), "q")
	require.NoError(t, err)
	require.Len(t, sts, 1)
	assert.True(t, sts[0].Readable)
}
