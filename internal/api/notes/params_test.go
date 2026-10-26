package notes

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/translate"
	"github.com/elythia-network/elythia/internal/model"
)

// TestHandlers_IntegerParamsAcceptFractionForms pins that the integer params
// of notes/* accept every JSON number with an integer value (10.0, 1e1), as
// upstream's ajv "type": "integer" does (#3847).
func TestHandlers_IntegerParamsAcceptFractionForms(t *testing.T) {
	h, _ := newTestHandler(t)
	me := &model.User{ID: "u1", Username: "u1"}
	const cursor = `"limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0`
	cases := []struct {
		name string
		fn   func(echo.Context) error
		body string
	}{
		{"notes", h.BulkShow, `{` + cursor + `}`},
		{"notes/children", h.Children, `{"noteId":"n1",` + cursor + `}`},
		{"notes/replies", h.Replies, `{"noteId":"n1",` + cursor + `}`},
		{"notes/renotes", h.Renotes, `{"noteId":"n1",` + cursor + `}`},
		{"notes/conversation", h.Conversation, `{"noteId":"n1","limit":10.0,"offset":1e0}`},
		{"notes/featured", h.Featured, `{"limit":10.0}`},
		{"notes/timeline", h.Timeline, `{` + cursor + `}`},
		{"notes/local-timeline", h.LocalTimeline, `{` + cursor + `}`},
		{"notes/global-timeline", h.GlobalTimeline, `{` + cursor + `}`},
		{"notes/hybrid-timeline", h.HybridTimeline, `{` + cursor + `}`},
		{"notes/mentions", h.Mentions, `{` + cursor + `}`},
		{"notes/user-list-timeline", h.UserListTimeline, `{"listId":"l1",` + cursor + `}`},
		{"notes/search-by-tag", h.SearchByTag, `{"tag":"x",` + cursor + `}`},
		{"notes/search", h.Search, `{"query":"x","offset":1e0,"rangeStartAt":0.0,"rangeEndAt":4102444800000.0,` + cursor + `}`},
		{"notes/reactions", h.Reactions, `{"noteId":"n1",` + cursor + `}`},
		{"notes/polls/recommendation", h.PollsRecommendation, `{"limit":10.0,"offset":0.0}`},
		{"notes/polls/vote", h.PollsVote, `{"noteId":"n1","choice":1.0}`},
		{"notes/drafts/list", h.DraftsList, `{` + cursor + `}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postRecovering(tc.fn, tc.body, me)
			assert.NotContains(t, rec.Body.String(), "INVALID_PARAM", rec.Body.String())
		})
	}
}

// postRecovering posts body to h like postExtra, and turns a panic of a
// handler whose dependencies the test leaves nil into a 500.
func postRecovering(h func(echo.Context) error, body string, user *model.User) (rec *httptest.ResponseRecorder) {
	defer func() {
		if r := recover(); r != nil {
			rec = httptest.NewRecorder()
			rec.WriteHeader(http.StatusInternalServerError)
		}
	}()
	return postExtra(h, body, user)
}

// notes/featured は本家も allowGet で、GET の query の整数も JSON.parse で読む
// (1.0 や 1e0 も 1 つの整数)。小数の値は今までどおり 400。本家の paramDef に無い
// offset・untilDate は、型によらず読まない (#3847)。
func TestFeatured_IntegerParamsLikeUpstream(t *testing.T) {
	h, noteRepo, _ := newExtraHandler(t)
	chID := "ch1"
	for _, id := range []string{"n_ch1", "n_ch2"} {
		noteRepo.Notes[id] = &model.Note{ID: id, UserID: "u1", Visibility: "public", ChannelID: &chID, User: &model.User{ID: "u1"}}
	}
	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		_ = h.Featured(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/notes/featured?"+query, nil), rec))
		return rec
	}
	count := func(rec *httptest.ResponseRecorder, name string) int {
		require.Equal(t, http.StatusOK, rec.Code, name+": "+rec.Body.String())
		var resp []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return len(resp)
	}
	for _, q := range []string{"channelId=ch1&limit=1.0", "channelId=ch1&limit=1e0"} {
		assert.Equal(t, 1, count(get(q), q))
	}
	for _, q := range []string{"channelId=ch1&offset=1", "channelId=ch1&offset=x&untilDate=x"} {
		assert.Equal(t, 2, count(get(q), q))
	}
	for _, body := range []string{`{"channelId":"ch1","offset":1}`, `{"channelId":"ch1","offset":"x","untilDate":"x"}`} {
		assert.Equal(t, 2, count(postExtra(h.Featured, body, nil), body))
	}
	assert.Equal(t, http.StatusBadRequest, get("limit=1.5").Code)
}

// drafts/create・drafts/update の scheduledAt と poll の expiresAt・expiredAfter も、
// 整数の値を持つ小数の形で受け、その値を保存する (#3847)。
func TestDrafts_IntegerParamsAcceptFractionForms(t *testing.T) {
	const at = int64(4102444800000)
	me := &model.User{ID: "u1"}
	h, repo := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsCreate, `{"text":"x","scheduledAt":4102444800000.0,"poll":{"choices":["a","b"],"expiresAt":4.1024448e12}}`, me)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, repo.drafts, 1)
	for _, d := range repo.drafts {
		require.NotNil(t, d.ScheduledAt)
		assert.Equal(t, at, d.ScheduledAt.UnixMilli())
		require.NotNil(t, d.PollExpiresAt)
		assert.Equal(t, at, d.PollExpiresAt.UnixMilli())
	}

	h, repo = newDraftHandlerWithRepo()
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Visibility: "public"}
	rec = postDraft(h.DraftsUpdate, `{"draftId":"d1","scheduledAt":4102444800000.0,"poll":{"choices":["a","b"],"expiredAfter":6e4}}`, me)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	d := repo.drafts["d1"]
	require.NotNil(t, d.ScheduledAt)
	assert.Equal(t, at, d.ScheduledAt.UnixMilli())
	require.NotNil(t, d.PollExpiredAfter)
	assert.Equal(t, int64(60000), *d.PollExpiredAfter)

	rec = postDraft(h.DraftsUpdate, `{"draftId":"d1","scheduledAt":1.5}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

// 本家の paramDef は targetLang を string とするだけで空文字も通し、ノートの照合・
// 可視性・空の本文の判定を先に行う。Elythia も同じ順で判定し、翻訳に回す直前で
// 空の言語を 400 にする (#3847)。キーが無ければ今までどおり 400。
func TestTranslate_EmptyTargetLangAfterTheNoteChecks(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	viewer := &model.User{ID: "viewer"}
	author := &model.User{ID: "author"}
	nid := f.seed(author, time.Hour)
	post := func(body string) *httptest.ResponseRecorder { return postExtra(f.h.Translate, body, viewer) }

	rec := post(`{"noteId":"ghost","targetLang":""}`)
	code, _ := translateBody(t, rec)
	assert.Equal(t, "NO_SUCH_NOTE", code)

	f.notes.Notes["secret"] = &model.Note{ID: "secret", UserID: author.ID, Visibility: model.NoteVisibilitySpecified, Text: f.notes.Notes[nid].Text}
	assertInvisibleTranslate(t, f, post(`{"noteId":"secret","targetLang":""}`))

	empty := " "
	f.notes.Notes["blank"] = &model.Note{ID: "blank", UserID: author.ID, Visibility: model.NoteVisibilityPublic, Text: &empty}
	assert.Equal(t, http.StatusNoContent, post(`{"noteId":"blank","targetLang":""}`).Code)

	rec = post(`{"noteId":"` + nid + `","targetLang":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, idStr := translateBody(t, rec)
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, apierr.UUIDInvalidParam, idStr)
	assert.False(t, *f.called, "空の言語を DeepL へ送ってはいけない")

	rec = post(`{"noteId":"` + nid + `"}`)
	code, _ = translateBody(t, rec)
	assert.Equal(t, "INVALID_PARAM", code)
	assert.False(t, *f.called)
}

// 空の targetLang は、本家と同じく翻訳の設定を見た後で判定する。設定が無ければ
// UNAVAILABLE、あれば 400 で、どちらも DeepL へは送らない。設定を読めなければ
// 500 (#3847)。
func TestTranslate_EmptyTargetLangAfterTheConfiguredCheck(t *testing.T) {
	f := newTranslateHiddenFixture(t)
	viewer := &model.User{ID: "viewer"}
	nid := f.seed(&model.User{ID: "author"}, time.Hour)
	body := `{"noteId":"` + nid + `","targetLang":""}`
	// MetaDeepL は本物の DeepL の URL へ送るので、送ったら落ちる transport で試す。
	// 判定を外す変異でも、外へは出ない。
	called := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("no network in this test: " + r.URL.Host)
	})}

	key, settingsErr := "", error(nil)
	f.h.SetTranslator(translate.NewMetaDeepL(func() (string, bool, error) { return key, false, settingsErr }, client))

	code, _ := translateBody(t, postExtra(f.h.Translate, body, viewer))
	assert.Equal(t, "UNAVAILABLE", code)

	key = "configured"
	rec := postExtra(f.h.Translate, body, viewer)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, _ = translateBody(t, rec)
	assert.Equal(t, "INVALID_PARAM", code)

	settingsErr = errors.New("meta unavailable")
	assert.Equal(t, http.StatusInternalServerError, postExtra(f.h.Translate, body, viewer).Code)
	assert.False(t, called, "空の言語を DeepL へ送ってはいけない")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
