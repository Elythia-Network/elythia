package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/core/ugcvisibility"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubFeedUsers struct{ users map[string]*model.User }

func (s stubFeedUsers) FindLocalByUsername(username string) (*model.User, error) {
	u, ok := s.users[username]
	if !ok {
		// **repository の sentinel を返す。** `echo.ErrNotFound` は HTTP の
		// error で、`repository.IsNotFound` では判定できない (#2792)。
		return nil, repository.ErrNotFound
	}
	return u, nil
}

type stubFeedNotes struct{ notes []*model.Note }

func (s stubFeedNotes) ListPublicNotesForFeed(_ string, limit int) ([]*model.Note, error) {
	if len(s.notes) > limit {
		return s.notes[:limit], nil
	}
	return s.notes, nil
}

func strp(s string) *string { return &s }

func newFeedTestHandler(notes []*model.Note) *feedHandler {
	name := "Alice"
	return &feedHandler{
		baseURL: "https://example.test",
		host:    "example.test",
		users: stubFeedUsers{users: map[string]*model.User{
			"alice": {ID: "u1", Username: "alice", Name: &name, NotesCount: 5, FollowingCount: 2, FollowersCount: 3},
		}},
		notes:     stubFeedNotes{notes: notes},
		parseTime: func(string) (time.Time, error) { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), nil },
		profiles: func(string) *model.UserProfile {
			return &model.UserProfile{FollowingVisibility: "public", FollowersVisibility: "public"}
		},
		avatarURL: func(*model.User) string { return "https://example.test/avatar.png" },
		toHTML:    func(text, _ string) string { return "<p>" + text + "</p>" },
	}
}

func doFeedReq(t *testing.T, h func(echo.Context, string) error, user string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := h(c, user); err != nil {
		var he *echo.HTTPError
		if ok := asHTTPError(err, &he); ok {
			rec.Code = he.Code
			return rec
		}
		t.Fatal(err)
	}
	return rec
}

func asHTTPError(err error, out **echo.HTTPError) bool {
	he, ok := err.(*echo.HTTPError)
	if ok {
		*out = he
	}
	return ok
}

func sampleFeedNotes() []*model.Note {
	return []*model.Note{
		{ID: "n1", UserID: "u1", Text: strp("hello"), Visibility: model.NoteVisibilityPublic},
	}
}

func TestFeed_RSS(t *testing.T) {
	h := newFeedTestHandler(sampleFeedNotes())
	rec := doFeedReq(t, h.RSS, "alice")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/rss+xml; charset=utf-8", rec.Header().Get(echo.HeaderContentType))
	body := rec.Body.String()
	assert.Contains(t, body, "<rss version=\"2.0\">")
	assert.Contains(t, body, "<title>Alice (@alice@example.test)</title>")
	assert.Contains(t, body, "https://example.test/notes/n1")
	assert.Contains(t, body, "5 Notes, 2 Following, 3 Followers")
}

func TestFeed_Atom(t *testing.T) {
	h := newFeedTestHandler(sampleFeedNotes())
	rec := doFeedReq(t, h.Atom, "alice")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/atom+xml; charset=utf-8", rec.Header().Get(echo.HeaderContentType))
	body := rec.Body.String()
	assert.Contains(t, body, "http://www.w3.org/2005/Atom")
	assert.Contains(t, body, "<id>https://example.test/@alice</id>")
	assert.Contains(t, body, "2026-01-02T03:04:05Z")
}

func TestFeed_JSON(t *testing.T) {
	h := newFeedTestHandler(sampleFeedNotes())
	rec := doFeedReq(t, h.JSON, "alice")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get(echo.HeaderContentType))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	assert.Equal(t, "https://jsonfeed.org/version/1.1", doc["version"])
	assert.Equal(t, "https://example.test/@alice", doc["home_page_url"])
	items, _ := doc["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	assert.Equal(t, "https://example.test/notes/n1", item["url"])
	assert.Equal(t, "<p>hello</p>", item["content_html"])
}

// TestFeed_PassesMentionedRemoteUsers checks that the note's
// mentionedRemoteUsers column reaches toHTML, as upstream FeedService passes
// it to MfmService.toHtml.
func TestFeed_PassesMentionedRemoteUsers(t *testing.T) {
	notes := sampleFeedNotes()
	notes[0].MentionedRemoteUsers = `[{"uri":"https://remote.example/users/1","username":"bob","host":"remote.example"}]`
	h := newFeedTestHandler(notes)
	var got string
	h.toHTML = func(_, mentionedRemoteUsers string) string {
		got = mentionedRemoteUsers
		return ""
	}
	rec := doFeedReq(t, h.JSON, "alice")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, notes[0].MentionedRemoteUsers, got)
}

// TestFeedNoteHTML checks that the feed's HTML links a remote mention to the
// url in the mentionedRemoteUsers column and an unknown one to this server.
func TestFeedNoteHTML(t *testing.T) {
	toHTML := feedNoteHTML("example.test")
	got := toHTML("@bob@remote.example @carol@other.example",
		`[{"uri":"https://remote.example/users/1","url":"https://remote.example/@bob","username":"bob","host":"remote.example"}]`)
	assert.Equal(t,
		`<a href="https://remote.example/@bob" class="u-url mention">@bob@remote.example</a> `+
			`<a href="https://example.test/@carol@other.example" class="u-url mention">@carol@other.example</a>`,
		got)
}

// 存在しないユーザーは 404。upstream も同じ。
func TestFeed_UnknownUserIs404(t *testing.T) {
	h := newFeedTestHandler(nil)
	for _, fn := range []func(echo.Context, string) error{h.RSS, h.Atom, h.JSON} {
		rec := doFeedReq(t, fn, "nobody")
		assert.Equal(t, http.StatusNotFound, rec.Code)
	}
}

// ノートが 1 件も無くても 200 で空のフィードを返す (upstream 同様)。
func TestFeed_EmptyNotesStillServes(t *testing.T) {
	h := newFeedTestHandler(nil)
	rec := doFeedReq(t, h.RSS, "alice")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<rss version=\"2.0\">")
}

// フォロー数の可視性が public でなければ数を伏せる。
func TestFeed_HidesCountsWhenNotPublic(t *testing.T) {
	h := newFeedTestHandler(sampleFeedNotes())
	h.profiles = func(string) *model.UserProfile {
		return &model.UserProfile{FollowingVisibility: "private", FollowersVisibility: "followers"}
	}
	rec := doFeedReq(t, h.RSS, "alice")
	assert.Contains(t, rec.Body.String(), "5 Notes, ? Following, ? Followers")
}

// CW 付きノートは summary に出す。
func TestFeed_CWBecomesSummary(t *testing.T) {
	notes := sampleFeedNotes()
	notes[0].CW = strp("注意")
	h := newFeedTestHandler(notes)
	rec := doFeedReq(t, h.Atom, "alice")
	assert.Contains(t, rec.Body.String(), "<summary>注意</summary>")
}

// Echo のルータはセグメント内リテラルを解釈できないので、/@<acct> で受けて
// 拡張子で振り分ける。拡張子が無ければフィードとして扱わない。
func TestFeed_TryServe(t *testing.T) {
	h := newFeedTestHandler(sampleFeedNotes())
	e := echo.New()

	for _, tc := range []struct {
		acct    string
		handled bool
		ctype   string
	}{
		{"alice.rss", true, "application/rss+xml; charset=utf-8"},
		{"alice.atom", true, "application/atom+xml; charset=utf-8"},
		{"alice.json", true, "application/json; charset=utf-8"},
		{"alice", false, ""},
		{"alice.png", false, ""},
	} {
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		handled, err := h.TryServe(c, tc.acct)
		require.NoError(t, err, tc.acct)
		assert.Equal(t, tc.handled, handled, tc.acct)
		if tc.handled {
			assert.Equal(t, tc.ctype, rec.Header().Get(echo.HeaderContentType), tc.acct)
		}
	}
}

// 凍結済みの利用者はフィードを配らない。upstream getFeed は
// `isSuspended: false` を lookup 条件に入れており、外れると 404 になる。
func TestFeed_SuspendedUserIs404(t *testing.T) {
	h := newFeedTestHandler(sampleFeedNotes())
	h.users.(stubFeedUsers).users["alice"].IsSuspended = true
	for _, fn := range []func(echo.Context, string) error{h.RSS, h.Atom, h.JSON} {
		rec := doFeedReq(t, fn, "alice")
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.NotContains(t, rec.Body.String(), "hello")
	}
}

// 「ログインしていないユーザーにコンテンツを見せない」設定の利用者も同様に
// 404。フィードは常に未認証で読めるので、ここが唯一のゲートになる。
func TestFeed_RequireSigninToViewContentsIs404(t *testing.T) {
	h := newFeedTestHandler(sampleFeedNotes())
	h.users.(stubFeedUsers).users["alice"].RequireSigninToViewContents = true
	for _, fn := range []func(echo.Context, string) error{h.RSS, h.Atom, h.JSON} {
		rec := doFeedReq(t, fn, "alice")
		assert.Equal(t, http.StatusNotFound, rec.Code)
		// 本文 (MFM→HTML 変換済み) も著者名も 1 文字も出さない。
		body := rec.Body.String()
		assert.NotContains(t, body, "hello")
		assert.NotContains(t, body, "Alice")
	}
}

// TryServe 経由 (= 実際のルーティング経路) でも同じゲートが効く。
// serve() を直接叩くテストだけだと、振り分け側にバイパスがあっても気付けない。
func TestFeed_TryServeAppliesVisibilityGate(t *testing.T) {
	e := echo.New()
	for _, tc := range []struct {
		name  string
		apply func(*model.User)
	}{
		{"suspended", func(u *model.User) { u.IsSuspended = true }},
		{"requireSignin", func(u *model.User) { u.RequireSigninToViewContents = true }},
	} {
		for _, acct := range []string{"alice.rss", "alice.atom", "alice.json"} {
			h := newFeedTestHandler(sampleFeedNotes())
			tc.apply(h.users.(stubFeedUsers).users["alice"])
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
			handled, err := h.TryServe(c, acct)
			assert.True(t, handled, "%s/%s", tc.name, acct)
			var he *echo.HTTPError
			require.True(t, asHTTPError(err, &he), "%s/%s: expected an echo.HTTPError", tc.name, acct)
			assert.Equal(t, http.StatusNotFound, he.Code, "%s/%s", tc.name, acct)
			assert.NotContains(t, rec.Body.String(), "hello", "%s/%s", tc.name, acct)
		}
	}
}

// ゲートを足しても通常の利用者は 200 のまま (= ゲートが広すぎないこと)。
func TestFeed_NormalUserStillServed(t *testing.T) {
	h := newFeedTestHandler(sampleFeedNotes())
	for _, fn := range []func(echo.Context, string) error{h.RSS, h.Atom, h.JSON} {
		rec := doFeedReq(t, fn, "alice")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "hello")
	}
}

// upstream getFeed と同じく、ugcVisibilityForVisitor が 'none' のときは feed を
// 返さない。'local' / 'all' ではローカル利用者の feed をそのまま返す。
// TryServe (実際のルーティング経路) で 3 形式すべてを見る。
func TestFeed_UGCVisibilityForVisitor(t *testing.T) {
	e := echo.New()
	for _, tc := range []struct {
		policy string
		want   int
	}{
		{ugcvisibility.None, http.StatusNotFound},
		{ugcvisibility.Local, http.StatusOK},
		{ugcvisibility.All, http.StatusOK},
	} {
		for _, acct := range []string{"alice.rss", "alice.atom", "alice.json"} {
			h := newFeedTestHandler(sampleFeedNotes())
			policy := tc.policy
			h.ugcVisibility = func() string { return policy }
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
			handled, err := h.TryServe(c, acct)
			require.True(t, handled, "%s/%s", tc.policy, acct)
			code := rec.Code
			var he *echo.HTTPError
			if asHTTPError(err, &he) {
				code = he.Code
			} else {
				require.NoError(t, err, "%s/%s", tc.policy, acct)
			}
			assert.Equal(t, tc.want, code, "%s/%s", tc.policy, acct)
			if tc.want == http.StatusNotFound {
				assert.NotContains(t, rec.Body.String(), "hello", "%s/%s", tc.policy, acct)
			} else {
				assert.Contains(t, rec.Body.String(), "hello", "%s/%s", tc.policy, acct)
			}
		}
	}
}

// HasUGCVisibility backs the feed.ugcVisibility startup wiring check: an
// unwired lookup silently falls back to `local`, so `none` would not 404.
func TestFeedHandler_HasUGCVisibility(t *testing.T) {
	assert.False(t, (&feedHandler{}).HasUGCVisibility(), "unwired lookup must be reported")
	assert.True(t, (&feedHandler{ugcVisibility: func() string { return "none" }}).HasUGCVisibility())
}
