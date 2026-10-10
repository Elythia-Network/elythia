package pages

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/require"
)

// enum 列が受け付ける値だけを通すこと。
//
// **通さないと 500 になる。** `page_visibility_enum` に無い値はそのまま
// INSERT へ行き、PostgreSQL がクエリごと落とす。認証済みなら誰でも起こせた。
func TestValidPageVisibility(t *testing.T) {
	t.Parallel()

	// 受け入れる値はリテラルで書く (定数を参照すると、集合を広げる変異と
	// 一緒に期待値まで動く)。
	for _, v := range []model.PageVisibility{"", "public", "private"} {
		require.True(t, validPageVisibility(v), "%q は受け入れること", v)
	}
	// followers / specified は本家の enum の値だが、#3479 で 2 値にしたので拒否する。
	for _, v := range []model.PageVisibility{"followers", "specified", "home", "bogus", "PUBLIC", "public "} {
		require.False(t, validPageVisibility(v), "%q は拒否すること", v)
	}
}

// model 側の定数と取りこぼしが無いこと。
func TestValidPageVisibilityCoversModelConstants(t *testing.T) {
	t.Parallel()

	for _, v := range []model.PageVisibility{
		model.PageVisibilityPublic,
		model.PageVisibilityPrivate,
	} {
		require.True(t, validPageVisibility(v),
			"model の定数 %q を拒否している (列が受け付ける値を弾いてはいけない)", v)
	}
}

// ハンドラが検証を通していること。
//
// **これが無いと呼び出しを外す変異を検出できない。** 述語だけのテストは
// 述語が呼ばれているかを見ない。
func TestCreate_RejectsUnknownVisibility(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"title":"t","name":"alpha","content":[],"variables":[],"visibility":"followers"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"enum 列に無い visibility は 400 で弾くこと (通すと INSERT が落ちて 500 になる)")
}

func TestCreate_AcceptsKnownVisibility(t *testing.T) {
	for _, v := range []string{"public", "private"} {
		t.Run(v, func(t *testing.T) {
			h, _, _ := newHandler(t)
			c, rec := newReq(t, `{"title":"t","name":"alpha","content":[],"variables":[],"visibility":"`+v+`"}`)
			setUser(c, "alice")
			require.NoError(t, h.Create(c))
			require.Equal(t, http.StatusOK, rec.Code, "%s は通ること", v)
		})
	}
}

func TestUpdate_RejectsUnknownVisibility(t *testing.T) {
	h, repo, _ := newHandler(t)
	// **実在するページを用意する。** 用意しないと not-found が先に返り、
	// visibility の検証を外しても 400 のままでテストが空振りする
	// (初版で実際にそうなった)。
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Title: "t"}
	c, rec := newReq(t, `{"pageId":"p1","visibility":"specified"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"update 側も同じ検証を通すこと")

	// 対照: 正しい値なら通ること (400 が別の理由で出ていないことの確認)。
	h2, repo2, _ := newHandler(t)
	repo2.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Title: "t"}
	c2, rec2 := newReq(t, `{"pageId":"p1","visibility":"private"}`)
	setUser(c2, "alice")
	require.NoError(t, h2.Update(c2))
	require.Equal(t, http.StatusOK, rec2.Code, "正しい値は通ること")
}

// **update では空文字も弾くこと。**
//
// create は値型なので「省略」と「空文字」を区別できず、`validPageVisibility` が
// 空文字を通す (service が `public` へ正規化する)。update はポインタなので
// 省略を nil で表せるのに、空文字をそのまま通していた。`fields["visibility"] = ""`
// が enum 列へ行き、**認証済みの一般利用者が 500 を起こせた**
// (`invalid input value for enum page_visibility_enum: ""`)。
//
// **mock repository では 500 にならない** (値を素通しするだけ) ので、
// ここで見るのは「400 で弾くこと」。列が落ちることは repository 側のテストが
// 押さえる。
func TestUpdate_RejectsEmptyVisibility(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Title: "t"}
	c, rec := newReq(t, `{"pageId":"p1","visibility":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"空文字は enum 列に入らないので 400 で弾くこと")
}

// **省略は通ること** (上が「常に弾く」実装でも緑にならないようにする)。
func TestUpdate_OmittedVisibilityIsAccepted(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Title: "t"}
	c, rec := newReq(t, `{"pageId":"p1","title":"t2"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code, "visibility を省略した update は通ること")
}
