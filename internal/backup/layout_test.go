package backup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewIDRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 10, 4, 0, 5, 999, time.FixedZone("JST", 9*3600))
	id := NewID(at)
	assert.Equal(t, "20261009T190005Z", id)
	assert.True(t, ValidID(id))
	got, err := IDTime(id)
	require.NoError(t, err)
	assert.True(t, got.Equal(at.Truncate(time.Second)))
}

func TestValidIDRejectsPathLikeInput(t *testing.T) {
	for _, id := range []string{"", "latest", "../20261009T190005Z", "20261009T190005Z/..", "20261009T190005", "2026100T190005Z"} {
		assert.False(t, ValidID(id), id)
		_, err := IDTime(id)
		assert.Error(t, err, id)
	}
}

func TestKeyAndGenerationIDFromKey(t *testing.T) {
	id := "20261009T190005Z"
	assert.Equal(t, "generations/20261009T190005Z/meta.json", Key(id, MetaFile))
	assert.Equal(t, id, GenerationIDFromKey(Key(id, DumpFileAge)))
	assert.Equal(t, "generations/", GenerationPrefix())
	for _, key := range []string{"meta.json", "generations/", "generations/" + id, "generations/latest/meta.json", "other/" + id + "/meta.json"} {
		assert.Equal(t, "", GenerationIDFromKey(key), key)
	}
}
