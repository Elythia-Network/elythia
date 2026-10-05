package notes

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// router が呼ぶ optional setter は wiring 漏れが起きても静かに機能が落ちるだけ
// なので、少なくとも「呼べて値が入る」ことは押さえておく。
func TestOptionalSetters(t *testing.T) {
	h, _ := newTestHandler(t)

	h.SetModeratorChecker(nil)
	h.SetPollRepo(testutil.NewMockPollRepository())
	assert.NotNil(t, h.pollRepo)
	h.SetEmojiRepo(testutil.NewMockEmojiRepository())
	assert.NotNil(t, h.emojiRepo)
	h.SetReactionReader(nil)
	h.SetNoteMaterializer(nil)
}

// blockedHosts は meta 未配線なら nil を返し、取得できれば meta の値を返す。
func TestBlockedHosts(t *testing.T) {
	h, _ := newTestHandler(t)

	hosts, err := h.blockedHosts()
	require.NoError(t, err, "meta 未配線は no-op")
	assert.Nil(t, hosts)

	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"bad.example"}}
	h.SetMetaRepo(metaRepo)
	hosts, err = h.blockedHosts()
	require.NoError(t, err)
	assert.Equal(t, []string{"bad.example"}, hosts)
}

// meta の取得失敗は握り潰さず呼び出し側へ伝える。
func TestBlockedHosts_FetchError(t *testing.T) {
	h, _ := newTestHandler(t)
	// Meta 未設定の mock は Fetch でエラーを返す。
	h.SetMetaRepo(testutil.NewMockMetaRepository())

	_, err := h.blockedHosts()
	assert.Error(t, err)
}
