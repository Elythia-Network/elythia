package notes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/api/apierr"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/search"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newQueryHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	searchSvc := search.NewService(search.NewSQLLikeProvider(noteRepo, nil))
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, searchSvc, idGen)
	return h, noteRepo
}

func newJSONRequest(t *testing.T, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// putUserOnNote stores a populated user on the note so PackNote serializes correctly.
func putUserOnNote(n *model.Note) {
	n.User = &model.User{
		ID:                n.UserID,
		Username:          "u",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
}

func seedPublicNote(repo *testutil.MockNoteRepository, id string) *model.Note {
	n := &model.Note{
		ID:         id,
		UserID:     "author",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	putUserOnNote(n)
	repo.Notes[id] = n
	return n
}

func TestRenotes_OK(t *testing.T) {
	h, repo := newQueryHandler(t)
	parent := seedPublicNote(repo, "parent")
	pid := parent.ID
	r := seedPublicNote(repo, "r1")
	r.RenoteID = &pid

	c, rec := newJSONRequest(t, "/api/notes/renotes", `{"noteId":"parent"}`)
	require.NoError(t, h.Renotes(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	assert.Equal(t, "r1", resp[0]["id"])
	shapetest.Assert(t, "Note", resp[0]) // L3 (#1316)
}

// #1554 renotes は viewer を block している author の note を除外する
// (upstream generateBaseNoteFilteringQuery 相当)。
func TestRenotes_FiltersBlockingAuthor(t *testing.T) {
	h, repo := newQueryHandler(t)
	parent := seedPublicNote(repo, "parent")
	pid := parent.ID
	// blocker が parent を renote。blocker は viewer(alice) を block している。
	r := seedPublicNote(repo, "r1")
	r.UserID = "blocker"
	r.RenoteID = &pid
	r.User = &model.User{ID: "blocker", Username: "blocker", AvatarDecorations: datatypes.JSON([]byte("[]"))}

	mutingRepo := testutil.NewMockMutingRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "alice"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}
	h.SetMutingRepo(mutingRepo)
	h.SetBlockingRepo(blockingRepo)
	h.SetUserRepo(userRepo)

	c, rec := newJSONRequest(t, "/api/notes/renotes", `{"noteId":"parent"}`)
	setAuthUser(c, &model.User{ID: "alice"})
	require.NoError(t, h.Renotes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// blocker の renote は除外される。
	assert.Empty(t, resp)
}

// #1554 replies は viewer が mute した author の note を除外する。
func TestReplies_FiltersMutedAuthor(t *testing.T) {
	h, repo := newQueryHandler(t)
	parent := seedPublicNote(repo, "parent")
	pid := parent.ID
	reply := seedPublicNote(repo, "rep1")
	reply.UserID = "muted"
	reply.ReplyID = &pid
	reply.User = &model.User{ID: "muted", Username: "muted", AvatarDecorations: datatypes.JSON([]byte("[]"))}

	mutingRepo := testutil.NewMockMutingRepository()
	mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "alice", MuteeID: "muted"}
	blockingRepo := testutil.NewMockBlockingRepository()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}
	h.SetMutingRepo(mutingRepo)
	h.SetBlockingRepo(blockingRepo)
	h.SetUserRepo(userRepo)

	c, rec := newJSONRequest(t, "/api/notes/replies", `{"noteId":"parent"}`)
	setAuthUser(c, &model.User{ID: "alice"})
	require.NoError(t, h.Replies(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

func TestRenotes_NotFound(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/renotes", `{"noteId":"ghost"}`)
	require.NoError(t, h.Renotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRenotes_InvalidParam(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/renotes", `{}`)
	require.NoError(t, h.Renotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRenotes_InvalidJSON(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/renotes", `{invalid`)
	require.NoError(t, h.Renotes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReplies_OK(t *testing.T) {
	h, repo := newQueryHandler(t)
	parent := seedPublicNote(repo, "p")
	pid := parent.ID
	r := seedPublicNote(repo, "r")
	r.ReplyID = &pid

	c, rec := newJSONRequest(t, "/api/notes/replies", `{"noteId":"p"}`)
	require.NoError(t, h.Replies(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	shapetest.Assert(t, "Note", resp[0]) // L3 (#1316)
}

// #1929: 親 note が存在しない場合、upstream replies.ts は親チェックを行わず
// 200 空配列を返す (404 NO_SUCH_NOTE ではない)。
func TestReplies_ParentMissingReturnsEmpty(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/replies", `{"noteId":"ghost"}`)
	require.NoError(t, h.Replies(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

// #1929: children も親不在で 200 空配列。
func TestChildren_ParentMissingReturnsEmpty(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/children", `{"noteId":"ghost"}`)
	require.NoError(t, h.Children(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

func TestChildren_OK(t *testing.T) {
	h, repo := newQueryHandler(t)
	parent := seedPublicNote(repo, "p")
	pid := parent.ID
	r := seedPublicNote(repo, "child1")
	r.ReplyID = &pid
	q := seedPublicNote(repo, "child2")
	q.RenoteID = &pid
	// quote renote (text あり) は child として返る。pure renote 除外 (#1554) を
	// 避けるため本文を持たせる。
	quoteText := "quote"
	q.Text = &quoteText

	c, rec := newJSONRequest(t, "/api/notes/children", `{"noteId":"p","limit":50}`)
	require.NoError(t, h.Children(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
	shapetest.Assert(t, "Note", resp[0]) // L3 (#1316)
}

// TestChildren_PureRenoteExcluded verifies the handler path drops pure renotes
// (no text/file/poll) from children while keeping quote renotes and replies,
// matching upstream notes/children (children.ts:53-67). Regression for #1554.
func TestChildren_PureRenoteExcluded(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "p")
	pid := "p"
	reply := seedPublicNote(repo, "reply")
	reply.ReplyID = &pid
	pure := seedPublicNote(repo, "pure")
	pure.RenoteID = &pid
	quote := seedPublicNote(repo, "quote")
	quote.RenoteID = &pid
	quoteText := "quote"
	quote.Text = &quoteText

	c, rec := newJSONRequest(t, "/api/notes/children", `{"noteId":"p","limit":50}`)
	require.NoError(t, h.Children(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	idsSet := map[string]bool{}
	for _, r := range resp {
		idsSet[r["id"].(string)] = true
	}
	assert.True(t, idsSet["reply"], "reply は child として返る")
	assert.True(t, idsSet["quote"], "quote renote は child として返る")
	assert.False(t, idsSet["pure"], "pure renote は child に出さない")
}

func TestChildren_LimitOutOfRangeRejected(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "p")
	c, rec := newJSONRequest(t, "/api/notes/children", `{"noteId":"p","limit":1000}`)
	require.NoError(t, h.Children(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1500 audit follow-up: notes/renotes・replies・children の visibility は repo
// (mock) の push-down delegation に委ねており、handler 自体は viewerID を渡す
// だけ。followers child が「非フォロワーには見えず、follower には見える」ことを
// handler 経由で fix することで、handler が viewerIDOf(viewer) を落とした
// 場合に検出できる (#1491 audit 指摘 4)。
//
// ヘルパー: followers visibility の child note を seed (User も付ける)。
func seedFollowersChild(repo *testutil.MockNoteRepository, id, parentID string, isReply bool) *model.Note {
	pid := parentID
	n := &model.Note{
		ID:         id,
		UserID:     "fol_author",
		Visibility: model.NoteVisibilityFollowers,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	if isReply {
		n.ReplyID = &pid
	} else {
		n.RenoteID = &pid
	}
	n.User = &model.User{
		ID:                "fol_author",
		Username:          "folauthor",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Notes[id] = n
	return n
}

func TestRenotes_FollowersChildHiddenFromNonFollower(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "parent")
	pub := seedPublicNote(repo, "pub_r")
	pid := "parent"
	pub.RenoteID = &pid
	seedFollowersChild(repo, "fol_r", "parent", false)

	// 非フォロワー viewer は public renote のみ。
	c, rec := newJSONRequest(t, "/api/notes/renotes", `{"noteId":"parent"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.Renotes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "pub_r", resp[0]["id"], "非フォロワーには followers renote が見えない")
}

func TestRenotes_FollowersChildVisibleToFollower(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "parent")
	pub := seedPublicNote(repo, "pub_r")
	pid := "parent"
	pub.RenoteID = &pid
	seedFollowersChild(repo, "fol_r", "parent", false)
	repo.Following = map[string][]string{"viewer": {"fol_author"}}

	c, rec := newJSONRequest(t, "/api/notes/renotes", `{"noteId":"parent"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.Renotes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2, "follow すると followers renote も見える")
}

func TestReplies_FollowersChildHiddenFromAnonymous(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "p")
	pub := seedPublicNote(repo, "pubRe")
	pid := "p"
	pub.ReplyID = &pid
	seedFollowersChild(repo, "folRe", "p", true)

	// auth user 未 set = anonymous → handler は viewerID=""
	c, rec := newJSONRequest(t, "/api/notes/replies", `{"noteId":"p"}`)
	require.NoError(t, h.Replies(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "pubRe", resp[0]["id"], "anonymous には followers reply が見えない")
}

func TestReplies_FollowersChildVisibleToFollower(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "p")
	pub := seedPublicNote(repo, "pubRe")
	pid := "p"
	pub.ReplyID = &pid
	seedFollowersChild(repo, "folRe", "p", true)
	repo.Following = map[string][]string{"viewer": {"fol_author"}}

	c, rec := newJSONRequest(t, "/api/notes/replies", `{"noteId":"p"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.Replies(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
}

func TestChildren_FollowersChildHiddenFromNonFollower(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "p")
	// 1 件は public reply、1 件は public quote renote、1 件は followers reply、1 件は followers quote renote。
	pubRe := seedPublicNote(repo, "pubRe")
	pid := "p"
	pubRe.ReplyID = &pid
	pubQ := seedPublicNote(repo, "pubQ")
	pubQ.RenoteID = &pid
	// quote renote として扱うため text を持たせる (pure renote 除外 #1554 を回避)。
	quoteText := "quote"
	pubQ.Text = &quoteText
	seedFollowersChild(repo, "folRe", "p", true)
	seedFollowersChild(repo, "folQ", "p", false).Text = &quoteText

	c, rec := newJSONRequest(t, "/api/notes/children", `{"noteId":"p","limit":50}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.Children(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2, "非フォロワーは public 2 件のみ")
	idsSet := map[string]bool{}
	for _, r := range resp {
		idsSet[r["id"].(string)] = true
	}
	assert.True(t, idsSet["pubRe"])
	assert.True(t, idsSet["pubQ"])
	assert.False(t, idsSet["folRe"], "followers reply は非フォロワーに漏れない")
	assert.False(t, idsSet["folQ"], "followers quote renote も非フォロワーに漏れない")
}

func TestChildren_FollowersChildVisibleToFollower(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "p")
	pubRe := seedPublicNote(repo, "pubRe")
	pid := "p"
	pubRe.ReplyID = &pid
	pubQ := seedPublicNote(repo, "pubQ")
	pubQ.RenoteID = &pid
	// quote renote として扱うため text を持たせる (pure renote 除外 #1554 を回避)。
	quoteText := "quote"
	pubQ.Text = &quoteText
	seedFollowersChild(repo, "folRe", "p", true)
	seedFollowersChild(repo, "folQ", "p", false).Text = &quoteText
	repo.Following = map[string][]string{"viewer": {"fol_author"}}

	c, rec := newJSONRequest(t, "/api/notes/children", `{"noteId":"p","limit":50}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.Children(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 4, "follow すると followers reply + followers quote renote も見える")
}

func TestSearch_OK(t *testing.T) {
	h, repo := newQueryHandler(t)
	hello := "Hello world"
	n := seedPublicNote(repo, "n1")
	n.Text = &hello

	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"hello"}`)
	require.NoError(t, h.Search(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	shapetest.Assert(t, "Note", resp[0]) // L3 (#1316)
}

// #1938 F1: notes/search は suspended author の note を除外する (applyMuteBlock 経由、
// upstream generateBaseNoteFilteringQuery 相当)。匿名でも効く。
func TestSearch_ExcludesSuspendedAuthor(t *testing.T) {
	h, repo := newQueryHandler(t)
	hello := "hello suspended"
	n := seedPublicNote(repo, "n1")
	n.Text = &hello
	n.User.IsSuspended = true

	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"hello"}`)
	require.NoError(t, h.Search(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp, "suspended author の note は search 結果から除外")
}

// #1938 F5: userId 指定時は channelId を無視する (upstream の userId 優先排他)。
// author の note (channel 無し) は channelId="other" を指定しても userId 一致で返る。
func TestSearch_UserIDOverridesChannelID(t *testing.T) {
	h, repo := newQueryHandler(t)
	hello := "hello exclusive"
	n := seedPublicNote(repo, "n1") // UserID="author", channel 無し
	n.Text = &hello

	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"hello","userId":"author","channelId":"other"}`)
	require.NoError(t, h.Search(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1, "userId 一致なら channelId 不一致でも返る (channelId は無視)")
}

// #1783: canSearchNotes policy が false の viewer (匿名含む) は UNAVAILABLE。
func TestSearch_CanSearchNotesDenied(t *testing.T) {
	h, _ := newQueryHandler(t)
	h.SetPolicyProvider(&draftStubPolicyProvider{policies: map[string]any{"canSearchNotes": false}})
	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"x"}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"UNAVAILABLE"`)
	assert.Contains(t, rec.Body.String(), "0b44998d-77aa-4427-80d0-d2c9b8523011")
}

// canSearchNotes=true なら検索が通る。
func TestSearch_CanSearchNotesAllowed(t *testing.T) {
	h, repo := newQueryHandler(t)
	hello := "hello"
	n := seedPublicNote(repo, "n1")
	n.Text = &hello
	h.SetPolicyProvider(&draftStubPolicyProvider{policies: map[string]any{"canSearchNotes": true}})
	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"hello"}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestSearch_LimitOutOfRangeRejected(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"x","limit":1000}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSearch_EmptyQuery(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":""}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSearch_InvalidJSON(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/search", `{invalid`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestSearch_NoopProviderReturnsUnavailable verifies that NoopProvider
// (= fulltextSearch.provider="none") の ErrUnavailable は handler 層で
// 400 UNAVAILABLE に翻訳されること (#877)。upstream Misskey TS の
// notes/search 未配備時の error shape と一致。
func TestSearch_NoopProviderReturnsUnavailable(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	searchSvc := search.NewService(search.NewNoopProvider())
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, searchSvc, idGen)

	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"hello"}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"code":"UNAVAILABLE"`)
	assert.Contains(t, body, "0b44998d-77aa-4427-80d0-d2c9b8523011")
}

// TestSearch_NoSearchService verifies the early-out branch when search is
// not configured at all (e.g. test handlers built without injecting one).
func TestSearch_NoSearchService(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, createSvc, deleteSvc, querySvc, nil, nil, nil, nil, idGen)

	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"hello"}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestSearch_DateCursorsConvertToIDs exercises the sinceDate / untilDate
// fallback path that runs the id generator.
func TestSearch_DateCursorsConvertToIDs(t *testing.T) {
	h, repo := newQueryHandler(t)
	hello := "Hello world"
	n := seedPublicNote(repo, "n1")
	n.Text = &hello

	body := `{"query":"hello","sinceDate":1700000000000,"untilDate":1900000000000}`
	c, rec := newJSONRequest(t, "/api/notes/search", body)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestSearch_RichFilters covers the userId / channelId / host fields by
// confirming a search with extra opts still completes successfully.
func TestSearch_RichFilters(t *testing.T) {
	h, repo := newQueryHandler(t)
	hello := "Hello"
	n := seedPublicNote(repo, "n1")
	n.Text = &hello
	n.UserID = "u1"
	body := `{"query":"hello","userId":"u1","channelId":"","host":"."}`
	c, rec := newJSONRequest(t, "/api/notes/search", body)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestState_OK(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "n1")

	c, rec := newJSONRequest(t, "/api/notes/state", `{"noteId":"n1"}`)
	require.NoError(t, h.State(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]bool
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp["isFavorited"])
}

func TestState_NotFound(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/state", `{"noteId":"ghost"}`)
	require.NoError(t, h.State(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestState_InvalidParam(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/state", `{}`)
	require.NoError(t, h.State(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestConversation_OK(t *testing.T) {
	h, repo := newQueryHandler(t)
	root := seedPublicNote(repo, "root")
	rid := root.ID
	leaf := seedPublicNote(repo, "leaf")
	leaf.ReplyID = &rid

	c, rec := newJSONRequest(t, "/api/notes/conversation", `{"noteId":"leaf"}`)
	require.NoError(t, h.Conversation(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	shapetest.Assert(t, "Note", resp[0]) // L3 (#1316)
	assert.Equal(t, "root", resp[0]["id"])
}

// #1948-17: 非可視な祖先は walk を打ち切らず hideNote stub で返す (upstream
// packMany(_, me) の hideNote 挙動)。可視な祖先 (gp) は通常通り返る。
func TestConversation_HiddenAncestorStubbed(t *testing.T) {
	h, repo := newQueryHandler(t)
	// leaf(public) -> secret(followers, 匿名には不可視, text 有り) -> gp(public)。
	gp := seedPublicNote(repo, "gp")
	gpID := gp.ID
	secretTxt := "secret content"
	secret := &model.Note{ID: "secret", UserID: "author", Visibility: model.NoteVisibilityFollowers, ReplyID: &gpID, Text: &secretTxt, Reactions: datatypes.JSON([]byte("{}"))}
	putUserOnNote(secret)
	repo.Notes["secret"] = secret
	leaf := seedPublicNote(repo, "leaf")
	sID := "secret"
	leaf.ReplyID = &sID

	// 匿名 viewer: secret は followers-only なので不可視 → stub。
	c, rec := newJSONRequest(t, "/api/notes/conversation", `{"noteId":"leaf"}`)
	require.NoError(t, h.Conversation(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2, "不可視祖先で打ち切らず gp まで返る (#1948-17)")
	// nearest-first: [secret(stub), gp]。
	assert.Equal(t, "secret", resp[0]["id"])
	assert.Equal(t, true, resp[0]["isHidden"], "不可視祖先は isHidden:true で stub (#1948-17)")
	assert.Nil(t, resp[0]["text"], "stub は text を消す (#1948-17)")
	assert.NotContains(t, rec.Body.String(), "secret content", "本文が leak しない")
	// gp は public なので通常表示。
	assert.Equal(t, "gp", resp[1]["id"])
	assert.NotEqual(t, true, resp[1]["isHidden"])
}

func TestConversation_LimitOutOfRangeRejected(t *testing.T) {
	h, repo := newQueryHandler(t)
	seedPublicNote(repo, "n1")
	c, rec := newJSONRequest(t, "/api/notes/conversation", `{"noteId":"n1","limit":1000}`)
	require.NoError(t, h.Conversation(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestConversation_NotFound(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/conversation", `{"noteId":"ghost"}`)
	require.NoError(t, h.Conversation(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestConversation_InvalidParam(t *testing.T) {
	h, _ := newQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/conversation", `{}`)
	require.NoError(t, h.Conversation(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingQueryRepo causes ListRenotesOf to fail to verify the internalError path.
type failingQueryRepo struct {
	*testutil.MockNoteRepository
}

func (f *failingQueryRepo) ListRenotesOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingQueryRepo) ListRepliesOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingQueryRepo) ListChildrenOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}
func (f *failingQueryRepo) SearchByFilter(_ model.NoteSearchFilter) ([]*model.Note, error) {
	return nil, testutil.ErrNotFound
}

func newFailingQueryHandler(t *testing.T) *Handler {
	t.Helper()
	mock := testutil.NewMockNoteRepository()
	seedPublicNote(mock, "p")
	repo := &failingQueryRepo{MockNoteRepository: mock}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(repo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(repo)
	querySvc := corenote.NewQueryService(repo, nil)
	searchSvc := search.NewService(search.NewSQLLikeProvider(repo, nil))
	return NewHandler(repo, createSvc, deleteSvc, querySvc, nil, nil, nil, searchSvc, idGen)
}

func TestRenotes_RepoError(t *testing.T) {
	h := newFailingQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/renotes", `{"noteId":"p"}`)
	require.NoError(t, h.Renotes(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestReplies_RepoError(t *testing.T) {
	h := newFailingQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/replies", `{"noteId":"p"}`)
	require.NoError(t, h.Replies(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestChildren_RepoError(t *testing.T) {
	h := newFailingQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/children", `{"noteId":"p"}`)
	require.NoError(t, h.Children(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestSearch_RepoError(t *testing.T) {
	h := newFailingQueryHandler(t)
	c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"x"}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestCreate_ReplyTargetNotFound triggers the NO_SUCH_REPLY_TARGET branch in
// Create when the reply target does not exist.
func TestCreate_ReplyTargetNotFound(t *testing.T) {
	h, _ := newQueryHandler(t)
	user := &model.User{ID: "u", Username: "u"}

	body := `{"text":"hi","replyId":"ghost"}`
	c, rec := newJSONRequest(t, "/api/notes/create", body)
	setAuthUser(c, user)
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_REPLY_TARGET", errObj["code"])
	assert.Equal(t, apierr.UUIDNoSuchReplyTarget, errObj["id"])
}

// TestCreate_RenoteTargetInvisible triggers the CANNOT_RENOTE_DUE_TO_VISIBILITY
// branch when the renote target is not visible to the actor.
func TestCreate_RenoteTargetInvisible(t *testing.T) {
	h, repo := newQueryHandler(t)
	repo.Notes["secret"] = &model.Note{
		ID: "secret", UserID: "author", Visibility: model.NoteVisibilityFollowers,
	}
	user := &model.User{ID: "viewer", Username: "viewer"}

	body := `{"renoteId":"secret"}`
	c, rec := newJSONRequest(t, "/api/notes/create", body)
	setAuthUser(c, user)
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "CANNOT_RENOTE_DUE_TO_VISIBILITY", errObj["code"])
	assert.Equal(t, apierr.UUIDCannotRenoteDueToVisibility, errObj["id"])
}

// TestBulkShow_NoQueryServiceRejects verifies BulkShow returns an empty
// array when queryService is not wired, ensuring the visibility filter is
// never bypassed (#509, sibling of #445 / TestShow_NoQueryServiceRejects).
func TestBulkShow_NoQueryServiceRejects(t *testing.T) {
	// 過去はここで fallback 経路が走り、followers / specified visibility の
	// ノートが任意の閲覧者に漏洩する security regression risk があった。
	// fallback を消したので、queryService nil なら note 行が引けても
	// 空配列で返す。production でこの経路を踏むことは無いが、wiring 不整合
	// に対する defensive 措置。
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	h := NewHandler(noteRepo, createSvc, deleteSvc, nil, nil, nil, nil, nil, idGen)

	seedPublicNote(noteRepo, "n1")
	seedPublicNote(noteRepo, "n2")
	c, rec := newJSONRequest(t, "/api/notes", `{"noteIds":["n1","n2"]}`)
	require.NoError(t, h.BulkShow(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String(),
		"BulkShow must return [] when queryService is unwired so notes never bypass visibility filtering")
}

// TestShow_NoQueryServiceRejects verifies Show returns NO_SUCH_NOTE when
// queryService is not wired, ensuring the visibility check is never
// bypassed (#445).
func TestShow_NoQueryServiceRejects(t *testing.T) {
	// 過去はここで fallback 経路が走り、followers / specified visibility の
	// ノートが任意の閲覧者に漏洩する security regression risk があった。
	// fallback を消したので、queryService nil なら public note でも
	// NO_SUCH_NOTE で蹴られる。production でこの経路を踏むことは無いが、
	// wiring 不整合に対する防御として安全側に倒す。
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	h := NewHandler(noteRepo, createSvc, deleteSvc, nil, nil, nil, nil, nil, idGen)

	seedPublicNote(noteRepo, "n1")
	c, rec := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
}
