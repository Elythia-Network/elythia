package clips

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// router が呼ぶ optional setter は wiring 漏れが起きても静かに機能が落ちるだけ
// なので、少なくとも「呼べて値が入る」ことは押さえておく。
func TestOptionalSetters(t *testing.T) {
	h, _, _, _ := newHandler(t)

	assert.Nil(t, h.instanceLookup(), "未配線なら lookup は nil")

	h.SetNoteFieldResolver(entity.NewNoteFieldResolver(nil, nil, nil, nil, nil, nil, nil))
	assert.NotNil(t, h.fieldRes)

	h.SetInstanceRepo(testutil.NewMockInstanceRepository())
	assert.NotNil(t, h.instanceLookup())

	h.SetEmojiRepo(testutil.NewMockEmojiRepository())
	assert.NotNil(t, h.emojiLookup())

	assert.Nil(t, h.reactionReader(), "未配線なら buffered reactions を読まない")
	reader := stubReactionReader{}
	h.SetReactionReader(reader)
	assert.Equal(t, reader, h.reactionReader())

	rc := repository.NewRelationCache(nil)
	h.SetRelationCache(rc)
	assert.Same(t, rc, h.relationCache)
}

// stubReactionReader is a BufferedReactionsReader with nothing buffered.
type stubReactionReader struct{}

func (stubReactionReader) GetBufferedMany(context.Context, []string) (map[string]map[string]int64, error) {
	return nil, nil
}

// blockedHosts は meta 未配線なら nil、配線済みなら meta の値を返す。
func TestBlockedHosts(t *testing.T) {
	h, _, _, _ := newHandler(t)

	hosts, err := h.blockedHosts()
	require.NoError(t, err)
	assert.Nil(t, hosts)

	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"bad.example"}}
	h.metaRepo = metaRepo
	hosts, err = h.blockedHosts()
	require.NoError(t, err)
	assert.Equal(t, []string{"bad.example"}, hosts)

	// Meta 未設定の mock は Fetch でエラーを返す。握り潰さず伝播すること。
	h.metaRepo = testutil.NewMockMetaRepository()
	_, err = h.blockedHosts()
	assert.Error(t, err)
}
