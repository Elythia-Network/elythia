package users

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// router が呼ぶ optional setter は wiring 漏れが起きても静かに機能が落ちるだけ
// なので、少なくとも「呼べて値が入る」ことは押さえておく。
func TestSetChannelMutingRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetChannelMutingRepo(testutil.NewMockChannelMutingRepository())
	assert.NotNil(t, h.channelMutingRepo)
}

// content list 系はどれも upstream paramDef の limit 範囲を強制する。
func TestContentLists_LimitOutOfRangeRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, postStub(h.Clips, `{"userId":"u1","limit":0}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, postStub(h.Flashs, `{"userId":"u1","limit":101}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, postStub(h.GalleryPosts, `{"userId":"u1","limit":0}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, postStub(h.Pages, `{"userId":"u1","limit":101}`, nil).Code)
}
