package plugintest

import (
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
