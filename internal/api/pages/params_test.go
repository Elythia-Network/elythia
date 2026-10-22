package pages

import (
	"encoding/json"
	"net/http"
	"testing"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家の paramDef は空の title を通し、本家はそのまま保存する (#3772)。
func TestCreate_EmptyTitle(t *testing.T) {
	h, repo, _ := newHandler(t)
	c, rec := newReq(t, `{"title":"","name":"alpha","content":[],"variables":[],"script":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "", got["title"])
	require.Contains(t, repo.Pages, got["id"])
}

// 欠けた title は、検証を通らずに届いたときも拒否する。
func TestCreate_MissingTitle(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha","content":[],"variables":[],"script":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	testutil.AssertInvalidParam(t, rec)
}

// 本家は pageId があればそれで引き、無ければ name と username で引く。空文字も
// そのまま引くので NO_SUCH_PAGE になる (#3772)。
func TestShow_EmptyStringsAreNoSuchPage(t *testing.T) {
	for _, body := range []string{
		`{"name":"","username":"alice"}`,
		`{"name":"alpha","username":""}`,
		`{"pageId":"","name":"alpha","username":"alice"}`,
	} {
		t.Run(body, func(t *testing.T) {
			h, repo, _ := newHandler(t)
			repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic}
			src := &stubUserSource{byUsernameErr: assert.AnError}
			// username が "alice" のときだけ利用者が見つかる。
			if body != `{"name":"alpha","username":""}` {
				src = &stubUserSource{byUsernameBundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice"}}}
			}
			h.SetUserSource(src)
			c, rec := newReq(t, body)
			require.NoError(t, h.Show(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "NO_SUCH_PAGE")
		})
	}
}

// 本家の paramDef は空の event を通す (#3772)。
func TestPagePush_EmptyEvent(t *testing.T) {
	h, repo, _ := newHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	h.SetUserSource(&stubUserSource{bundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice"}}})
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Name: "test", Visibility: model.PageVisibilityPublic}

	c, rec := newReq(t, `{"pageId":"p1","event":""}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "", pub.calls[0].body.(map[string]any)["event"])

	c, rec = newReq(t, `{"pageId":"p1"}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	testutil.AssertInvalidParam(t, rec)
}

// anyOf は片方の形を満たせば通すので、使わない側の項目の型が違っても本家は
// 通す (#3772)。
func TestShow_IgnoresTypesOfTheOtherShape(t *testing.T) {
	for body, want := range map[string]int{
		`{"pageId":"p1","name":1}`:                             http.StatusOK,
		`{"pageId":"p1","username":true}`:                      http.StatusOK,
		`{"pageId":null,"name":"alpha","username":"alice"}`:    http.StatusOK,
		`{"pageId":123,"name":"alpha","username":"alice"}`:     http.StatusBadRequest,
		`{"pageId":{"a":1},"name":"alpha","username":"alice"}`: http.StatusBadRequest,
	} {
		t.Run(body, func(t *testing.T) {
			h, repo, _ := newHandler(t)
			repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic}
			h.SetUserSource(&stubUserSource{byUsernameBundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice"}}})
			c, rec := newReq(t, body)
			require.NoError(t, h.Show(c))
			require.Equal(t, want, rec.Code, rec.Body.String())
			if want != http.StatusOK {
				assert.Contains(t, rec.Body.String(), "NO_SUCH_PAGE")
			}
		})
	}
}

// どちらの形も満たさない入力は、検証を通らずに届いたときも拒否する。
func TestShow_NeitherShape(t *testing.T) {
	for _, body := range []string{`{}`, `{"name":"alpha"}`, `{"name":"alpha","username":1}`, `{"pageId":null,"name":"alpha"}`, `{"name":null,"username":"alice"}`} {
		t.Run(body, func(t *testing.T) {
			h, _, _ := newHandler(t)
			h.SetUserSource(&stubUserSource{byUsernameBundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice"}}})
			c, rec := newReq(t, body)
			require.NoError(t, h.Show(c))
			testutil.AssertInvalidParam(t, rec)
		})
	}
}
