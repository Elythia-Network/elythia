package users

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// content list 系はどれも upstream paramDef の limit 範囲を強制する。
func TestContentLists_LimitOutOfRangeRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, postStub(h.Clips, `{"userId":"u1","limit":0}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, postStub(h.Flashs, `{"userId":"u1","limit":101}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, postStub(h.GalleryPosts, `{"userId":"u1","limit":0}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, postStub(h.Pages, `{"userId":"u1","limit":101}`, nil).Code)
}
