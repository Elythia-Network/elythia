package pages

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/model"
)

// TestUnlike_PrivatePage pins who gets NO_SUCH_PAGE from pages/unlike on a
// private page (#3479).
func TestUnlike_PrivatePage(t *testing.T) {
	cases := []struct {
		name       string
		caller     string
		liked      bool
		wantCode   int
		wantInBody string
	}{
		// like していない他人に NOT_LIKED を返すと、ID から Page の存在が分かる。
		{name: "stranger who has not liked gets NO_SUCH_PAGE", caller: "bob", wantCode: http.StatusBadRequest, wantInBody: "NO_SUCH_PAGE"},
		// 公開中に like した人は、非公開になった後も外せる。
		{name: "stranger who liked can still unlike", caller: "bob", liked: true, wantCode: http.StatusNoContent},
		{name: "author who has not liked gets NOT_LIKED", caller: "alice", wantCode: http.StatusBadRequest, wantInBody: "NOT_LIKED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, likeRepo := newHandler(t)
			repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPrivate}
			if tc.liked {
				require.NoError(t, likeRepo.Create(&model.PageLike{ID: "l1", UserID: tc.caller, PageID: "p1"}))
			}
			c, rec := newReq(t, `{"pageId":"p1"}`)
			setUser(c, tc.caller)
			require.NoError(t, h.Unlike(c))
			assert.Equal(t, tc.wantCode, rec.Code)
			if tc.wantInBody != "" {
				assert.Contains(t, rec.Body.String(), tc.wantInBody)
			}
		})
	}
}

// TestPagePush_PrivatePage checks that only the author can emit events for a
// private page (#3479).
func TestPagePush_PrivatePage(t *testing.T) {
	cases := []struct {
		name      string
		caller    string
		wantCode  int
		wantCalls int
	}{
		{name: "stranger gets NO_SUCH_PAGE and nothing is published", caller: "bob", wantCode: http.StatusBadRequest},
		{name: "author can push", caller: "alice", wantCode: http.StatusNoContent, wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _ := newHandler(t)
			pub := &stubMainStreamPublisher{}
			h.SetMainStreamPublisher(pub)
			h.SetUserSource(&stubUserSource{
				bundle: &coreuser.UserWithProfile{User: &model.User{ID: tc.caller, Username: tc.caller}},
			})
			repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPrivate}

			c, rec := newReq(t, `{"pageId":"p1","event":"click"}`)
			setUser(c, tc.caller)
			require.NoError(t, h.PagePush(c))
			assert.Equal(t, tc.wantCode, rec.Code)
			if tc.wantCode != http.StatusNoContent {
				assert.Contains(t, rec.Body.String(), "NO_SUCH_PAGE")
			}
			assert.Len(t, pub.calls, tc.wantCalls)
		})
	}
}

// TestUpdateDelete_NonOwnerErrorCode pins the API error a non-owner gets from
// pages/update and pages/delete (#3479): ACCESS_DENIED for public pages like
// upstream, NO_SUCH_PAGE for private ones.
func TestUpdateDelete_NonOwnerErrorCode(t *testing.T) {
	cases := []struct {
		visibility model.PageVisibility
		want       string
	}{
		{visibility: model.PageVisibilityPublic, want: "ACCESS_DENIED"},
		{visibility: model.PageVisibilityPrivate, want: "NO_SUCH_PAGE"},
	}
	for _, tc := range cases {
		t.Run(string(tc.visibility), func(t *testing.T) {
			h, repo, _ := newHandler(t)
			repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "n", Title: "t", Visibility: tc.visibility}

			c, rec := newReq(t, `{"pageId":"p1","title":"x","name":"n"}`)
			setUser(c, "bob")
			require.NoError(t, h.Update(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.want)

			c, rec = newReq(t, `{"pageId":"p1"}`)
			setUser(c, "bob")
			require.NoError(t, h.Delete(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.want)
			require.Contains(t, repo.Pages, "p1")
		})
	}
}
