package notes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"

	"github.com/elythia-network/elythia/internal/api/apierr"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func postDraft(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = handler(c)
	return rec
}

func newDraftHandler() *Handler {
	idGen, _ := id.NewGenerator("aidx")
	return &Handler{idGen: idGen}
}

// --- Mock DraftRepo ---

// **not-found を模す。** 汎用 error だと #2792 の「DB 障害は 500」に引っかかる。
// repository は GORM の error をそのまま返すので、テストもそれに揃える。
var errDraftMock = repository.ErrNotFound

// errBoom is a generic failure for write paths. **not-found とは分けること** —
// 同じ値を使い回すと、将来 write path に `IsNotFound` の分岐が入ったときに
// これらのテストが黙って別の枝を通る (#2792)。
var errBoom = errors.New("db down")

type mockDraftRepo struct {
	drafts    map[string]*model.NoteDraft
	createErr error
}

func newMockDraftRepo() *mockDraftRepo {
	return &mockDraftRepo{drafts: make(map[string]*model.NoteDraft)}
}

func (m *mockDraftRepo) Create(d *model.NoteDraft) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.drafts[d.ID] = d
	return nil
}
func (m *mockDraftRepo) FindByIDAndUser(id, userID string) (*model.NoteDraft, error) {
	if d, ok := m.drafts[id]; ok && d.UserID == userID {
		return d, nil
	}
	return nil, errDraftMock
}
func (m *mockDraftRepo) ListByUser(userID, sinceID, untilID string, scheduled *bool, limit int) ([]*model.NoteDraft, error) {
	var result []*model.NoteDraft
	for _, d := range m.drafts {
		if d.UserID != userID {
			continue
		}
		if sinceID != "" && d.ID <= sinceID {
			continue
		}
		if untilID != "" && d.ID >= untilID {
			continue
		}
		if scheduled != nil && d.IsActuallyScheduled != *scheduled {
			continue
		}
		result = append(result, d)
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}
func (m *mockDraftRepo) Update(d *model.NoteDraft) error {
	m.drafts[d.ID] = d
	return nil
}
func (m *mockDraftRepo) Delete(id, _ string) (int64, error) {
	if _, ok := m.drafts[id]; !ok {
		return 0, nil
	}
	delete(m.drafts, id)
	return 1, nil
}
func (m *mockDraftRepo) CountByUser(userID string) (int64, error) {
	var count int64
	for _, d := range m.drafts {
		if d.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (m *mockDraftRepo) FindByID(id string) (*model.NoteDraft, error) {
	if d, ok := m.drafts[id]; ok {
		return d, nil
	}
	return nil, errors.New("note draft not found")
}

func (m *mockDraftRepo) CountScheduledByUser(userID string) (int64, error) {
	var count int64
	for _, d := range m.drafts {
		if d.UserID == userID && d.IsActuallyScheduled {
			count++
		}
	}
	return count, nil
}

func newDraftHandlerWithRepo() (*Handler, *mockDraftRepo) {
	h := newDraftHandler()
	repo := newMockDraftRepo()
	h.draftRepo = repo
	return h, repo
}

// --- DraftsList ---

func TestDraftsList_NilRepo(t *testing.T) {
	h := newDraftHandler()
	rec := postDraft(h.DraftsList, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestDraftsList_WithData(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	text := "hello"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Text: &text, Visibility: "public"}
	rec := postDraft(h.DraftsList, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

// #1948-19: packDraft は replyId/renoteId set 時に Note を resolve し、未設定なら
// key を省略する。channelId 同様。poll.expiresAt は nil で key 省略。
func TestPackDraft_ResolvesReplyRenoteChannelAndPoll(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["reply1"] = &model.Note{ID: "reply1", UserID: "author", Visibility: "public", Reactions: datatypes.JSON([]byte("{}")), User: &model.User{ID: "author", AvatarDecorations: datatypes.JSON([]byte("[]"))}}
	noteRepo.Notes["renote1"] = &model.Note{ID: "renote1", UserID: "author", Visibility: "public", Reactions: datatypes.JSON([]byte("{}")), User: &model.User{ID: "author", AvatarDecorations: datatypes.JSON([]byte("[]"))}}
	h.noteRepo = noteRepo
	// public note は viewer に可視なので CanSee=true。queryService を wire する。
	h.queryService = corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())
	chRepo := testutil.NewMockChannelRepository()
	owner := "u1"
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "general", Color: "#fff", IsSensitive: true, AllowRenoteToExternal: false, UserID: &owner}
	h.channelRepo = chRepo

	replyID, renoteID, channelID := "reply1", "renote1", "ch1"
	repo.drafts["d1"] = &model.NoteDraft{
		ID: "d1", UserID: "u1", Visibility: "public",
		ReplyID: &replyID, RenoteID: &renoteID, ChannelID: &channelID,
		HasPoll: true, PollChoices: model.StringArray{"a", "b"}, // PollExpiresAt nil
	}
	rec := postDraft(h.DraftsList, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	d := resp[0]
	// reply/renote が resolve される。
	reply, ok := d["reply"].(map[string]any)
	require.True(t, ok, "replyId set なら reply object を resolve (#1948-19)")
	assert.Equal(t, "reply1", reply["id"])
	renote, ok := d["renote"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "renote1", renote["id"])
	// channel summary。
	ch, ok := d["channel"].(map[string]any)
	require.True(t, ok, "channelId set なら channel summary を resolve (#1948-19)")
	assert.Equal(t, "general", ch["name"])
	assert.Equal(t, true, ch["isSensitive"])
	// poll.expiresAt は nil なので key 省略。
	poll := d["poll"].(map[string]any)
	_, hasExp := poll["expiresAt"]
	assert.False(t, hasExp, "poll.expiresAt nil は key 省略 (#1948-19)")
}

// #2017: DraftsCreate は reply/renote target の存在・可視性・pure-renote を検証し、
// drafts/create.ts meta.errors の code/id で 400 を返す。
func TestDraftsCreate_ReplyRenoteValidation(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	noteRepo := testutil.NewMockNoteRepository()
	mk := func(id, vis string) *model.Note {
		txt := "body"
		return &model.Note{ID: id, UserID: "author", Visibility: model.NoteVisibility(vis), Text: &txt,
			Reactions: datatypes.JSON([]byte("{}")), User: &model.User{ID: "author", AvatarDecorations: datatypes.JSON([]byte("[]"))}}
	}
	noteRepo.Notes["visible"] = mk("visible", "public")
	noteRepo.Notes["followers"] = mk("followers", "followers")
	h.noteRepo = noteRepo
	h.queryService = corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())
	u := &model.User{ID: "u1"}

	cases := []struct {
		name, body, wantCode string
		wantStatus           int
	}{
		{"no such reply", `{"text":"x","replyId":"missing"}`, "NO_SUCH_REPLY_TARGET", http.StatusBadRequest},
		{"invisible reply", `{"text":"x","replyId":"followers"}`, "CANNOT_REPLY_TO_AN_INVISIBLE_NOTE", http.StatusBadRequest},
		{"no such renote", `{"text":"x","renoteId":"missing"}`, "NO_SUCH_RENOTE_TARGET", http.StatusBadRequest},
		{"renote of others followers", `{"text":"x","renoteId":"followers"}`, "CANNOT_RENOTE_DUE_TO_VISIBILITY", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postDraft(h.DraftsCreate, tc.body, u)
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantCode, "#2017")
		})
	}

	// 可視 public note への reply は成功 (draft 作成 200)。
	rec := postDraft(h.DraftsCreate, `{"text":"x","replyId":"visible"}`, u)
	assert.Equal(t, http.StatusOK, rec.Code, "可視 reply target は通過 (#2017)")
}

// #2017: DraftsUpdate は (1) update endpoint 固有の error code/id を返す
// (NO_SUCH_REPLY ≠ create の NO_SUCH_REPLY_TARGET)、(2) reply/renote をリクエストで
// 明示指定したときのみ検証し、既存 draft の値を再検証しない (over-rejection 回避)。
func TestDraftsUpdate_ReplyRenoteValidation(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	noteRepo := testutil.NewMockNoteRepository()
	secret := "s"
	noteRepo.Notes["followers"] = &model.Note{ID: "followers", UserID: "author", Visibility: model.NoteVisibilityFollowers, Text: &secret,
		Reactions: datatypes.JSON([]byte("{}")), User: &model.User{ID: "author", AvatarDecorations: datatypes.JSON([]byte("[]"))}}
	h.noteRepo = noteRepo
	h.queryService = corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())
	u := &model.User{ID: "u1"}

	// (1) update endpoint 固有 code: NO_SUCH_REPLY / c4721841 (create の TARGET 系ではない)。
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Visibility: "public"}
	rec := postDraft(h.DraftsUpdate, `{"draftId":"d1","replyId":"missing"}`, u)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var er struct {
		Error struct{ Code, ID string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &er))
	assert.Equal(t, "NO_SUCH_REPLY", er.Error.Code, "update は NO_SUCH_REPLY (#2017)")
	assert.Equal(t, "c4721841-22fc-4bb7-ad3d-897ef1d375b5", er.Error.ID)

	// (2) over-rejection 回避: 既存 draft の replyId が invisible でも、replyId を
	// 指定しない update は再検証されず成功する。
	replyID := "followers"
	repo.drafts["d2"] = &model.NoteDraft{ID: "d2", UserID: "u1", Visibility: "public", ReplyID: &replyID}
	rec = postDraft(h.DraftsUpdate, `{"draftId":"d2","text":"new"}`, u)
	assert.Equal(t, http.StatusOK, rec.Code, "replyId 未指定の update は既存値を再検証しない (#2017 HIGH-2)")
}

// #2039: DraftsCreate は reply/renote 作者の block / channel renote-external /
// 自 channelId 存在を検証する (upstream NoteDraftService の blocked+channel block)。
func TestDraftsCreate_BlockedAndChannelValidation(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	noteRepo := testutil.NewMockNoteRepository()
	txt := "x"
	mk := func(id, author string, ch *string) *model.Note {
		return &model.Note{ID: id, UserID: author, Visibility: "public", Text: &txt, ChannelID: ch,
			Reactions: datatypes.JSON([]byte("{}")), User: &model.User{ID: author, AvatarDecorations: datatypes.JSON([]byte("[]"))}}
	}
	chID, openID, ghostID := "ch1", "chopen", "ghostch"
	noteRepo.Notes["byblocker"] = mk("byblocker", "blocker", nil)
	noteRepo.Notes["chnote"] = mk("chnote", "author", &chID)
	noteRepo.Notes["opennote"] = mk("opennote", "author", &openID)
	noteRepo.Notes["ghostnote"] = mk("ghostnote", "author", &ghostID)
	h.noteRepo = noteRepo
	h.queryService = corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())
	blk := testutil.NewMockBlockingRepository()
	blk.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "u1"}
	h.SetBlockingRepo(blk)
	chRepo := testutil.NewMockChannelRepository()
	owner := "owner"
	chRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "c", UserID: &owner, AllowRenoteToExternal: false}
	chRepo.Channels["chopen"] = &model.Channel{ID: "chopen", Name: "o", UserID: &owner, AllowRenoteToExternal: true}
	h.SetChannelRepo(chRepo)
	u := &model.User{ID: "u1"}

	// 400 を返すケース。
	rejectCases := []struct{ name, body, wantCode string }{
		{"blocked renote", `{"text":"x","renoteId":"byblocker"}`, "YOU_HAVE_BEEN_BLOCKED"},
		{"renote channel disallows external", `{"text":"x","renoteId":"chnote"}`, "CANNOT_RENOTE_TO_EXTERNAL"},
		{"renote channel not found", `{"text":"x","renoteId":"ghostnote"}`, "NO_SUCH_CHANNEL"},
		{"draft channelId not found", `{"text":"x","channelId":"missing"}`, "NO_SUCH_CHANNEL"},
	}
	for _, tc := range rejectCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postDraft(h.DraftsCreate, tc.body, u)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantCode, "#2039")
		})
	}

	// 通過するケース (200)。
	okCases := []struct{ name, body string }{
		{"renote channel allows external", `{"text":"x","renoteId":"opennote"}`},
		{"same-channel renote", `{"text":"x","renoteId":"chnote","channelId":"ch1"}`}, // 同一 channel は external check skip
	}
	for _, tc := range okCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postDraft(h.DraftsCreate, tc.body, u)
			assert.Equal(t, http.StatusOK, rec.Code, "#2039 通過 (%s)", tc.name)
		})
	}
}

// #2016: draft の reply は upstream で detail:false (clippedCount/poll/myReaction/
// nested embed を省く)、renote は detail:true。clippedCount は detail:true で常に
// 出力 (&n.ClippedCount)、detail:false で省略されるので、reply は省略・renote は
// 出力されることを確認する。
func TestPackDraft_ReplyDetailFalse_RenoteDetailTrue(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	noteRepo := testutil.NewMockNoteRepository()
	mkNote := func(id string) *model.Note {
		return &model.Note{ID: id, UserID: "author", Visibility: "public",
			Reactions: datatypes.JSON([]byte("{}")), User: &model.User{ID: "author", AvatarDecorations: datatypes.JSON([]byte("[]"))}}
	}
	noteRepo.Notes["reply1"] = mkNote("reply1")
	noteRepo.Notes["renote1"] = mkNote("renote1")
	h.noteRepo = noteRepo
	h.queryService = corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())

	replyID, renoteID := "reply1", "renote1"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Visibility: "public", ReplyID: &replyID, RenoteID: &renoteID}
	rec := postDraft(h.DraftsList, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	d := resp[0]

	reply := d["reply"].(map[string]any)
	_, hasClipped := reply["clippedCount"]
	assert.False(t, hasClipped, "reply は detail:false なので clippedCount 省略 (#2016)")

	renote := d["renote"].(map[string]any)
	_, rHasClipped := renote["clippedCount"]
	assert.True(t, rHasClipped, "renote は detail:true なので clippedCount 出力 (#2016)")
}

// #1948-19 (security): draft owner が閲覧不可な note を replyId に入れても、その
// 本文は漏れず hideNote stub される (followers/specified の intrinsic hide)。
func TestPackDraft_ReplyToInvisibleNote_Hidden(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	noteRepo := testutil.NewMockNoteRepository()
	secret := "TOP SECRET BODY"
	// followers-only note by author; viewer (u1) は author を follow していない。
	noteRepo.Notes["secret1"] = &model.Note{ID: "secret1", UserID: "author", Visibility: "followers", Text: &secret, Reactions: datatypes.JSON([]byte("{}")), User: &model.User{ID: "author", AvatarDecorations: datatypes.JSON([]byte("[]"))}}
	h.noteRepo = noteRepo
	h.queryService = corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())

	replyID := "secret1"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Visibility: "public", ReplyID: &replyID}
	rec := postDraft(h.DraftsList, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "TOP SECRET BODY", "非可視 reply note の本文が漏れない (#1948-19)")
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	reply := resp[0]["reply"].(map[string]any)
	assert.Equal(t, true, reply["isHidden"], "非可視 reply は isHidden:true で stub (#1948-19)")
	assert.Nil(t, reply["text"], "stub は text を消す")
}

// #1948-19: replyId/renoteId/channelId 未設定なら reply/renote/channel key を省略する。
func TestPackDraft_OmitsKeysWhenUnset(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	text := "plain"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Text: &text, Visibility: "public"}
	rec := postDraft(h.DraftsList, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	d := resp[0]
	for _, k := range []string{"reply", "renote", "channel"} {
		_, has := d[k]
		assert.False(t, has, k+" は未設定時に key 省略 (#1948-19)")
	}
}

// #1948-19: drafts/list は sinceDate/untilDate を id cursor に正規化する。
func TestDraftsList_DateCursor(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	idGen, _ := id.NewGenerator("aidx")
	// 2 件: 古い (2020) と新しい (2026)。
	oldID := idGen.Generate(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	newID := idGen.Generate(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	repo.drafts[oldID] = &model.NoteDraft{ID: oldID, UserID: "u1", Visibility: "public"}
	repo.drafts[newID] = &model.NoteDraft{ID: newID, UserID: "u1", Visibility: "public"}
	// sinceDate = 2023 → 2026 の draft のみ (id > sinceID)。
	body := `{"sinceDate":` + itoaMillis(time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)) + `}`
	rec := postDraft(h.DraftsList, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "sinceDate cursor で 2023 以降の draft のみ (#1948-19)")
	assert.Equal(t, newID, resp[0]["id"])
}

func itoaMillis(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// --- DraftsCreate ---

func TestDraftsCreate_NilRepo(t *testing.T) {
	h := newDraftHandler()
	rec := postDraft(h.DraftsCreate, `{"text":"hello"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestDraftsCreate_Success(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsCreate, `{"text":"draft text"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.drafts, 1)
}

func TestDraftsCreate_DefaultVisibility(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsCreate, `{"text":"test"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1934: drafts/create も visibility / reactionAcceptance enum を検証する。
func TestDraftsCreate_InvalidVisibility(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsCreate, `{"text":"x","visibility":"foo"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	assert.Empty(t, repo.drafts, "不正値の draft は保存しない")
}

func TestDraftsCreate_InvalidReactionAcceptance(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsCreate, `{"text":"x","reactionAcceptance":"bogus"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	assert.Empty(t, repo.drafts)
}

// create が replyId/renoteId/channelId/visibleUserIds/reactionAcceptance/
// localOnly/hashtag/poll を永続化し、{createdDraft} で full shape を返す。
func TestDraftsCreate_PersistsAllFieldsAndWraps(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	body := `{"text":"t","visibility":"specified","visibleUserIds":["v1"],"localOnly":true,` +
		`"reactionAcceptance":"likeOnly","replyId":"r1","renoteId":"rn1","channelId":"ch1",` +
		`"hashtag":"tag","poll":{"choices":["a","b"],"multiple":true,"expiredAfter":3600000}}`
	rec := postDraft(h.DraftsCreate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)

	// 永続化検証。
	require.Len(t, repo.drafts, 1)
	var d *model.NoteDraft
	for _, v := range repo.drafts {
		d = v
	}
	require.NotNil(t, d.ReplyID)
	assert.Equal(t, "r1", *d.ReplyID)
	require.NotNil(t, d.RenoteID)
	assert.Equal(t, "rn1", *d.RenoteID)
	require.NotNil(t, d.ChannelID)
	assert.Equal(t, "ch1", *d.ChannelID)
	assert.Equal(t, []string{"v1"}, []string(d.VisibleUserIDs))
	assert.True(t, d.LocalOnly)
	require.NotNil(t, d.ReactionAcceptance)
	assert.Equal(t, "likeOnly", *d.ReactionAcceptance)
	require.NotNil(t, d.Hashtag)
	assert.Equal(t, "tag", *d.Hashtag)
	assert.True(t, d.HasPoll)
	assert.Equal(t, []string{"a", "b"}, []string(d.PollChoices))
	assert.True(t, d.PollMultiple)

	// レスポンスは {createdDraft: NoteDraft} で full shape。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	cd, ok := resp["createdDraft"].(map[string]any)
	require.True(t, ok, "createdDraft で包む")
	assert.Equal(t, "r1", cd["replyId"])
	assert.Equal(t, "ch1", cd["channelId"])
	assert.Equal(t, []any{"v1"}, cd["visibleUserIds"])
	assert.Equal(t, "likeOnly", cd["reactionAcceptance"])
	poll, ok := cd["poll"].(map[string]any)
	require.True(t, ok, "poll object が出る")
	assert.Equal(t, []any{"a", "b"}, poll["choices"])
	assert.Equal(t, true, poll["multiple"])
	// user (UserLite) が埋まる。
	_, hasUser := cd["user"]
	assert.True(t, hasUser)
}

func TestDraftsCreate_InvalidJSON(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsCreate, `invalid`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDraftsCreate_Error(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	repo.createErr = errBoom
	rec := postDraft(h.DraftsCreate, `{"text":"x"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// #1029: noteDraftLimit role policy gate test。
type draftStubPolicyProvider struct {
	policies map[string]any
}

func (s *draftStubPolicyProvider) GetUserPolicies(_ string) map[string]any {
	return s.policies
}

func TestDraftsCreate_NoteDraftLimitExceeded(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1"}
	repo.drafts["d2"] = &model.NoteDraft{ID: "d2", UserID: "u1"}
	h.SetPolicyProvider(&draftStubPolicyProvider{policies: map[string]any{
		"noteDraftLimit": 2,
	}})
	rec := postDraft(h.DraftsCreate, `{"text":"third"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_DRAFTS")
}

func TestDraftsCreate_NoteDraftLimit_PassesUnderLimit(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	h.SetPolicyProvider(&draftStubPolicyProvider{policies: map[string]any{
		"noteDraftLimit": 10,
	}})
	rec := postDraft(h.DraftsCreate, `{"text":"draft"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- DraftsUpdate ---

func TestDraftsUpdate_NilRepo(t *testing.T) {
	h := newDraftHandler()
	rec := postDraft(h.DraftsUpdate, `{"draftId":"d1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestDraftsUpdate_Success(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	text := "old"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Text: &text, Visibility: "public"}
	rec := postDraft(h.DraftsUpdate, `{"draftId":"d1","text":"new","visibility":"home"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "new", *repo.drafts["d1"].Text)
}

// update が新 param を反映し {updatedDraft} で包んで返す。
func TestDraftsUpdate_PersistsParamsAndWraps(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	text := "old"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Text: &text, Visibility: "public"}
	body := `{"draftId":"d1","visibleUserIds":["v2"],"reactionAcceptance":"likeOnly","replyId":"rp","poll":{"choices":["x"],"multiple":false}}`
	rec := postDraft(h.DraftsUpdate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)

	d := repo.drafts["d1"]
	assert.Equal(t, []string{"v2"}, []string(d.VisibleUserIDs))
	require.NotNil(t, d.ReplyID)
	assert.Equal(t, "rp", *d.ReplyID)
	assert.True(t, d.HasPoll)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ud, ok := resp["updatedDraft"].(map[string]any)
	require.True(t, ok, "updatedDraft で包む")
	assert.Equal(t, "rp", ud["replyId"])
	assert.Equal(t, []any{"v2"}, ud["visibleUserIds"])
}

// seedDraftFile registers a drive file owned by ownerID in the mock repo.
func seedDraftFile(repo *testutil.MockDriveFileRepository, fid, ownerID string) {
	uid := ownerID
	repo.Files[fid] = &model.DriveFile{ID: fid, UserID: &uid, Name: fid + ".png", Type: "image/png"}
}

// scheduledAt はレスポンス上 number (ms epoch) でなければならない (ISO 文字列で
// 返すと misskey-js の Date 変換が壊れる)。F1 回帰固定。
func TestDraftsCreate_ScheduledAtIsNumber(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	at := futureMs(time.Hour)
	body := fmt.Sprintf(`{"text":"hi","scheduledAt":%d,"isActuallyScheduled":true}`, at)
	rec := postDraft(h.DraftsCreate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	cd := resp["createdDraft"].(map[string]any)
	// JSON number は encoding/json で float64 に decode される。
	v, ok := cd["scheduledAt"].(float64)
	require.True(t, ok, "scheduledAt は number で返る")
	assert.Equal(t, at, int64(v))
}

// poll.expiresAt が過去の場合は create を CANNOT_CREATE_ALREADY_EXPIRED_POLL で
// 弾く (upstream NoteDraftService と同 validation)。F4。
func TestDraftsCreate_ExpiredPollRejected(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	past := time.Now().Add(-time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"text":"t","poll":{"choices":["a","b"],"expiresAt":%d}}`, past)
	rec := postDraft(h.DraftsCreate, body, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CANNOT_CREATE_ALREADY_EXPIRED_POLL")
	assert.Contains(t, rec.Body.String(), "04da457d-b083-4055-9082-955525eda5a5")
	assert.Empty(t, repo.drafts, "期限切れ poll では永続化しない")
}

// update でも過去 expiresAt の poll は弾く。F4。
func TestDraftsUpdate_ExpiredPollRejected(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Visibility: "public"}
	past := time.Now().Add(-time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"draftId":"d1","poll":{"choices":["a"],"expiresAt":%d}}`, past)
	rec := postDraft(h.DraftsUpdate, body, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CANNOT_CREATE_ALREADY_EXPIRED_POLL")
	assert.False(t, repo.drafts["d1"].HasPoll, "期限切れ poll は適用しない")
}

// 所有していない fileId を含む create は NO_SUCH_FILE で弾く。F5。
func TestDraftsCreate_UnownedFileRejected(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	fileRepo := testutil.NewMockDriveFileRepository()
	seedDraftFile(fileRepo, "f1", "u1")
	h.SetDriveFileRepo(fileRepo)
	// f2 は存在しない (= 所有検証で落ちる)。
	rec := postDraft(h.DraftsCreate, `{"text":"t","fileIds":["f1","f2"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
	assert.Contains(t, rec.Body.String(), "b6992544-63e7-67f0-fa7f-32444b1b5306")
	assert.Empty(t, repo.drafts)
}

// 他人所有の fileId も NO_SUCH_FILE。F5 (所有権)。
func TestDraftsCreate_OtherUserFileRejected(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	fileRepo := testutil.NewMockDriveFileRepository()
	seedDraftFile(fileRepo, "f1", "someone-else")
	h.SetDriveFileRepo(fileRepo)
	rec := postDraft(h.DraftsCreate, `{"text":"t","fileIds":["f1"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
}

// update で所有していない fileId を渡すと NO_SUCH_FILE。F5。
func TestDraftsUpdate_UnownedFileRejected(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Visibility: "public"}
	fileRepo := testutil.NewMockDriveFileRepository()
	h.SetDriveFileRepo(fileRepo)
	rec := postDraft(h.DraftsUpdate, `{"draftId":"d1","fileIds":["ghost"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
}

// 所有 file は packDraft で fileIds の順序どおり files に解決される
// (resolveDraftFileMap が map でも順序は fileIds に従う)。
func TestDraftsCreate_OwnedFilesResolvedInOrder(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	fileRepo := testutil.NewMockDriveFileRepository()
	seedDraftFile(fileRepo, "f1", "u1")
	seedDraftFile(fileRepo, "f2", "u1")
	h.SetDriveFileRepo(fileRepo)
	// fileIds を逆順 [f2,f1] で渡し、その順で files が並ぶことを確認。
	rec := postDraft(h.DraftsCreate, `{"text":"t","fileIds":["f2","f1"]}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	cd := resp["createdDraft"].(map[string]any)
	files := cd["files"].([]any)
	require.Len(t, files, 2)
	assert.Equal(t, "f2", files[0].(map[string]any)["id"])
	assert.Equal(t, "f1", files[1].(map[string]any)["id"])
	assert.Equal(t, []any{"f2", "f1"}, cd["fileIds"])
}

// 省略した field は update で維持される (partial update)。
func TestDraftsUpdate_PartialUpdateMaintained(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	oldText := "old"
	cw := "warn"
	repo.drafts["d1"] = &model.NoteDraft{
		ID: "d1", UserID: "u1", Text: &oldText, CW: &cw, Visibility: "home",
		LocalOnly: true, VisibleUserIDs: []string{"v1"},
	}
	// text だけ更新。他 field は維持される。
	rec := postDraft(h.DraftsUpdate, `{"draftId":"d1","text":"new"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	d := repo.drafts["d1"]
	assert.Equal(t, "new", *d.Text)
	require.NotNil(t, d.CW)
	assert.Equal(t, "warn", *d.CW, "cw は維持")
	assert.Equal(t, "home", d.Visibility, "visibility は維持")
	assert.True(t, d.LocalOnly, "localOnly は維持")
	assert.Equal(t, []string{"v1"}, []string(d.VisibleUserIDs), "visibleUserIds は維持")
}

func TestDraftsUpdate_NotFound(t *testing.T) {
	// #688: 存在しない draftId 指定時に upstream notes/drafts/update 互換の
	// NO_SUCH_NOTE_DRAFT (UUID 49cd6b9d-...) を返すこと。
	h, _ := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsUpdate, `{"draftId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_NOTE_DRAFT", errObj["code"])
	assert.Equal(t, apierr.UUIDNoSuchNoteDraft, errObj["id"])
}

func TestDraftsUpdate_InvalidParam(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsUpdate, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1934: drafts/update も visibility / reactionAcceptance enum を検証する。
// 不正 enum は draft 不在 (404) より先に 400 で弾く (paramDef 相当)。
func TestDraftsUpdate_InvalidVisibility(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	text := "old"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Text: &text, Visibility: "public"}
	rec := postDraft(h.DraftsUpdate, `{"draftId":"d1","visibility":"foo"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	assert.Equal(t, "public", repo.drafts["d1"].Visibility, "不正値で draft は変更されない")
}

func TestDraftsUpdate_InvalidReactionAcceptance(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	text := "old"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Text: &text, Visibility: "public"}
	rec := postDraft(h.DraftsUpdate, `{"draftId":"d1","reactionAcceptance":"bogus"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

// #1421: 他人 draftID を投げると NO_SUCH_NOTE_DRAFT 404 で reject される
// (IDOR regression guard)。現行 handler は repo.FindByIDAndUser に owner filter
// を SQL push-down しているので構造的に他人 draft を書き換えられないが、
// refactor で FindByID に入れ替わると owner check が抜ける。その回帰を
// handler 層 negative test で固定する。
func TestDraftsUpdate_OtherUserDraft(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	text := "owner-only"
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", Text: &text, Visibility: "public"}
	rec := postDraft(h.DraftsUpdate, `{"draftId":"d1","text":"pwned"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_NOTE_DRAFT", errObj["code"])
	assert.Equal(t, apierr.UUIDNoSuchNoteDraft, errObj["id"])
	// 他人 draft の text は変更されない
	assert.Equal(t, "owner-only", *repo.drafts["d1"].Text)
}

// --- DraftsDelete ---

func TestDraftsDelete_NilRepo(t *testing.T) {
	h := newDraftHandler()
	rec := postDraft(h.DraftsDelete, `{"draftId":"d1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestDraftsDelete_Success(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1"}
	rec := postDraft(h.DraftsDelete, `{"draftId":"d1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.drafts)
}

func TestDraftsDelete_InvalidParam(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsDelete, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDraftsDelete_NotFound(t *testing.T) {
	// #688 review follow-up: 存在しない draftId で削除しても upstream
	// notes/drafts/delete.ts と同じく `NO_SUCH_NOTE_DRAFT` を返す。silent
	// 204 だと frontend が「該当 draft が無い」を観測できないので 404 が
	// 期待挙動。
	h, _ := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsDelete, `{"draftId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_NOTE_DRAFT", errObj["code"])
	assert.Equal(t, apierr.UUIDNoSuchNoteDraft, errObj["id"])
}

// --- DraftsCount ---

func TestDraftsCount_NilRepo(t *testing.T) {
	h := newDraftHandler()
	rec := postDraft(h.DraftsCount, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	// upstream drafts/count は res type:'number' で裸の数値 (#1538)。
	assert.JSONEq(t, `0`, rec.Body.String())
}

func TestDraftsCount_WithData(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1"}
	rec := postDraft(h.DraftsCount, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `1`, rec.Body.String())
}

// --- ThreadMuting ---

func TestThreadMutingCreate_Success(t *testing.T) {
	h, noteRepo, _ := newThreadMuteHandler()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	rec := postDraft(h.ThreadMutingCreate, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestThreadMutingCreate_InvalidParam(t *testing.T) {
	h := newDraftHandler()
	rec := postDraft(h.ThreadMutingCreate, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestThreadMutingDelete_Success(t *testing.T) {
	h, noteRepo, _ := newThreadMuteHandler()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	rec := postDraft(h.ThreadMutingDelete, `{"noteId":"n1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestThreadMutingDelete_InvalidParam(t *testing.T) {
	h := newDraftHandler()
	rec := postDraft(h.ThreadMutingDelete, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- PollsRecommendation ---

func TestPollsRecommendation(t *testing.T) {
	h := newDraftHandler()
	rec := postDraft(h.PollsRecommendation, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// --- #1040: scheduled note (drafts/create with scheduledAt) ---

// scheduledEnqueueStub captures EnqueuePostScheduledNote calls for assertion.
type scheduledEnqueueStub struct {
	calls   []queue.PostScheduledNotePayload
	cleared []string
	err     error
}

func (s *scheduledEnqueueStub) EnqueuePostScheduledNote(p queue.PostScheduledNotePayload, _ ...driver.EnqueueOption) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, p)
	return nil
}

func (s *scheduledEnqueueStub) ClearScheduledNote(draftID string) error {
	s.cleared = append(s.cleared, draftID)
	return nil
}

func futureMs(d time.Duration) int64 {
	return time.Now().Add(d).UnixMilli()
}

func TestDraftsCreate_ScheduledHappy(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	body := fmt.Sprintf(`{"text":"hi","scheduledAt":%d,"isActuallyScheduled":true}`, futureMs(time.Hour))
	rec := postDraft(h.DraftsCreate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.drafts, 1)
	require.Len(t, enq.calls, 1, "scheduled draft 1 件で enqueue が 1 度走る")
	// draft.IsActuallyScheduled=true で保存される
	for _, d := range repo.drafts {
		assert.True(t, d.IsActuallyScheduled)
		require.NotNil(t, d.ScheduledAt)
	}
}

func TestDraftsCreate_ScheduledAtRequired(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	rec := postDraft(h.DraftsCreate, `{"text":"hi","isActuallyScheduled":true}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "SCHEDULED_AT_REQUIRED")
	assert.Contains(t, rec.Body.String(), "15e28a55-e74c-4d65-89b7-8880cdaaa87d")
}

func TestDraftsCreate_ScheduledAtMustBeInFuture(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	past := time.Now().Add(-time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"text":"hi","scheduledAt":%d,"isActuallyScheduled":true}`, past)
	rec := postDraft(h.DraftsCreate, body, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "SCHEDULED_AT_MUST_BE_IN_FUTURE")
	assert.Contains(t, rec.Body.String(), "e4bed6c9-017e-4934-aed0-01c22cc60ec1")
}

func TestDraftsCreate_ScheduledNoteLimitExceeded(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1", IsActuallyScheduled: true}
	repo.drafts["d2"] = &model.NoteDraft{ID: "d2", UserID: "u1", IsActuallyScheduled: true}
	h.SetPolicyProvider(&draftStubPolicyProvider{policies: map[string]any{
		"scheduledNoteLimit": 2,
	}})
	body := fmt.Sprintf(`{"text":"third","scheduledAt":%d,"isActuallyScheduled":true}`, futureMs(time.Hour))
	rec := postDraft(h.DraftsCreate, body, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_SCHEDULED_NOTES")
	assert.Contains(t, rec.Body.String(), "22ae69eb-09e3-4541-a850-773cfa45e693")
}

// isActuallyScheduled=false で scheduledAt が指定されても enqueue は走らない
// (= 単なる「下書きとしての希望時刻」)。
func TestDraftsCreate_ScheduledAtWithoutActuallyScheduled(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	body := fmt.Sprintf(`{"text":"hi","scheduledAt":%d}`, futureMs(time.Hour))
	rec := postDraft(h.DraftsCreate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, enq.calls, "isActuallyScheduled=false なら enqueue されない")
}

// **予約投稿は driver の都合で無効化されない (#2985)。** 以前は asynq driver で
// clearSchedule が確実に効かないため `TOO_MANY_SCHEDULED_NOTES` で門前払いして
// いた。その分岐を消したので、上限とは無関係に 400 を返していないことを固定
// する (回帰すると、利用者には「予約上限に達した」と嘘の理由が出る)。
func TestDraftsCreate_ScheduledNotRejectedAsQuota(t *testing.T) {
	h, _ := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	body := fmt.Sprintf(`{"text":"hi","scheduledAt":%d,"isActuallyScheduled":true}`, futureMs(time.Hour))
	rec := postDraft(h.DraftsCreate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "TOO_MANY_SCHEDULED_NOTES")
	assert.Len(t, enq.calls, 1, "enqueue まで到達する")
}

// --- #1045 Phase 2-C: DraftsUpdate / DraftsDelete scheduled note 経路 ---

// scheduledAt を変更すると旧 delayed task を clear して新時刻で re-enqueue。
func TestDraftsUpdate_RescheduleClearsAndReenqueues(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	oldT := time.Now().Add(time.Hour)
	repo.drafts["d1"] = &model.NoteDraft{
		ID: "d1", UserID: "u1", IsActuallyScheduled: true, ScheduledAt: &oldT,
	}
	body := fmt.Sprintf(`{"draftId":"d1","scheduledAt":%d,"isActuallyScheduled":true}`, futureMs(2*time.Hour))
	rec := postDraft(h.DraftsUpdate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"d1"}, enq.cleared, "旧 task を clear")
	require.Len(t, enq.calls, 1, "新 task を 1 度 enqueue")
	assert.Equal(t, "d1", enq.calls[0].NoteDraftID)
}

// isActuallyScheduled=false への切り替えは clear のみ (= 再 enqueue しない)。
func TestDraftsUpdate_UnscheduleClearsOnly(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	t1 := time.Now().Add(time.Hour)
	repo.drafts["d1"] = &model.NoteDraft{
		ID: "d1", UserID: "u1", IsActuallyScheduled: true, ScheduledAt: &t1,
	}
	body := `{"draftId":"d1","isActuallyScheduled":false}`
	rec := postDraft(h.DraftsUpdate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"d1"}, enq.cleared, "clear は呼ばれる")
	assert.Empty(t, enq.calls, "isActuallyScheduled=false で再 enqueue は走らない")
}

// scheduled 関連の field が無い update では clear / enqueue 経路は走らない
// (= 旧挙動互換、scheduledAt 既存 draft への text 編集等を阻害しない)。
func TestDraftsUpdate_NonScheduledChangeDoesNotTouchQueue(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	t1 := time.Now().Add(time.Hour)
	repo.drafts["d1"] = &model.NoteDraft{
		ID: "d1", UserID: "u1", IsActuallyScheduled: true, ScheduledAt: &t1,
	}
	body := `{"draftId":"d1","text":"updated"}`
	rec := postDraft(h.DraftsUpdate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, enq.cleared)
	assert.Empty(t, enq.calls)
}

// update 経由の scheduled 切替も同じ (#2985)。こちらは create とは別の
// エラー id を持っていたので、分岐が片方だけ残る形を塞ぐために両方見る。
func TestDraftsUpdate_ScheduledNotRejectedAsQuota(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1"}
	body := fmt.Sprintf(`{"draftId":"d1","scheduledAt":%d,"isActuallyScheduled":true}`, futureMs(time.Hour))
	rec := postDraft(h.DraftsUpdate, body, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "TOO_MANY_SCHEDULED_NOTES")
	assert.Len(t, enq.calls, 1, "enqueue まで到達する")
}

// Delete 経由でも旧 delayed task を clear する (= unschedule された draft の
// processor 早期 fire を防ぐ best-effort)。
func TestDraftsDelete_ClearsScheduledNote(t *testing.T) {
	h, repo := newDraftHandlerWithRepo()
	enq := &scheduledEnqueueStub{}
	h.SetScheduledNoteEnqueuer(enq)
	repo.drafts["d1"] = &model.NoteDraft{ID: "d1", UserID: "u1"}
	rec := postDraft(h.DraftsDelete, `{"draftId":"d1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"d1"}, enq.cleared)
}

// --- #1538: polls/recommendation ---

func TestPollsRecommendation_NilRepoEmpty(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := &Handler{idGen: idGen}
	rec := postDraft(h.PollsRecommendation, `{}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestPollsRecommendation_ReturnsNotes(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	pollRepo := testutil.NewMockPollRepository()
	noteRepo := testutil.NewMockNoteRepository()
	for _, nid := range []string{"np1", "np2"} {
		noteRepo.Notes[nid] = &model.Note{ID: nid, UserID: "author", Reactions: datatypes.JSON([]byte("{}"))}
		pollRepo.Polls[nid] = &model.Poll{NoteID: nid, UserID: "author", NoteVisibility: model.NoteVisibilityPublic}
	}
	h := &Handler{idGen: idGen, pollRepo: pollRepo, noteRepo: noteRepo}
	rec := postDraft(h.PollsRecommendation, `{"limit":10}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 2)
}

func TestPollsRecommendation_ExcludesSelfAndVoted(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	pollRepo := testutil.NewMockPollRepository()
	noteRepo := testutil.NewMockNoteRepository()
	// good poll, self poll, voted poll.
	noteRepo.Notes["good"] = &model.Note{ID: "good", UserID: "author", Reactions: datatypes.JSON([]byte("{}"))}
	pollRepo.Polls["good"] = &model.Poll{NoteID: "good", UserID: "author", NoteVisibility: model.NoteVisibilityPublic}
	pollRepo.Polls["self"] = &model.Poll{NoteID: "self", UserID: "viewer", NoteVisibility: model.NoteVisibilityPublic}
	pollRepo.Polls["voted"] = &model.Poll{NoteID: "voted", UserID: "author", NoteVisibility: model.NoteVisibilityPublic}
	testutil.MockPollVoted("viewer", "voted")

	h := &Handler{idGen: idGen, pollRepo: pollRepo, noteRepo: noteRepo}
	rec := postDraft(h.PollsRecommendation, `{}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "good", out[0]["id"])
}

// --- #1538: thread-muting write side ---

func newThreadMuteHandler() (*Handler, *testutil.MockNoteRepository, *testutil.MockNoteThreadMutingRepository) {
	idGen, _ := id.NewGenerator("aidx")
	noteRepo := testutil.NewMockNoteRepository()
	tmRepo := testutil.NewMockNoteThreadMutingRepository()
	return &Handler{idGen: idGen, noteRepo: noteRepo, threadMutingRepo: tmRepo}, noteRepo, tmRepo
}

// upstream 1853898d71: 2 回目は 400 ALREADY_MUTING。#1538 までの mk-go は 204 を
// 返す冪等実装で、同じ入力に対する wire 上のレスポンスが upstream と分岐していた。
func TestThreadMutingCreate_PersistsAndRejectsDuplicate(t *testing.T) {
	h, noteRepo, tmRepo := newThreadMuteHandler()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	user := &model.User{ID: "viewer"}

	rec := postDraft(h.ThreadMutingCreate, `{"noteId":"n1"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)
	ex, _ := tmRepo.Exists("viewer", "n1")
	assert.True(t, ex, "thread muting row must be persisted")

	rec2 := postDraft(h.ThreadMutingCreate, `{"noteId":"n1"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec2.Code, "重複は upstream と同じ 400")
	assert.Contains(t, rec2.Body.String(), "ALREADY_MUTING")
	assert.Contains(t, rec2.Body.String(), "c146e22d-1141-4b31-b28d-176371014d18")
	assert.Len(t, tmRepo.Mutings, 1, "重複行は作らない")
}

func TestThreadMutingCreate_UsesThreadID(t *testing.T) {
	h, noteRepo, tmRepo := newThreadMuteHandler()
	thread := "root"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", ThreadID: &thread}
	postDraft(h.ThreadMutingCreate, `{"noteId":"n1"}`, &model.User{ID: "viewer"})
	ex, _ := tmRepo.Exists("viewer", "root")
	assert.True(t, ex, "mute must key on threadId when the note has one")
}

func TestThreadMutingCreate_NoSuchNote(t *testing.T) {
	h, _, _ := newThreadMuteHandler()
	rec := postDraft(h.ThreadMutingCreate, `{"noteId":"missing"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
}

func TestThreadMutingCreate_MissingNoteID(t *testing.T) {
	h, _, _ := newThreadMuteHandler()
	rec := postDraft(h.ThreadMutingCreate, `{}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestThreadMutingDelete_RemovesRowIdempotently(t *testing.T) {
	h, noteRepo, tmRepo := newThreadMuteHandler()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	_ = tmRepo.Create(&model.NoteThreadMuting{ID: "m1", UserID: "viewer", ThreadID: "n1"})

	rec := postDraft(h.ThreadMutingDelete, `{"noteId":"n1"}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	ex, _ := tmRepo.Exists("viewer", "n1")
	assert.False(t, ex)

	// idempotent: deleting again is still 204.
	rec2 := postDraft(h.ThreadMutingDelete, `{"noteId":"n1"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusNoContent, rec2.Code)
}

func TestThreadMutingDelete_NoSuchNote(t *testing.T) {
	h, _, _ := newThreadMuteHandler()
	rec := postDraft(h.ThreadMutingDelete, `{"noteId":"missing"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestThreadMuting_NilRepoFailsClosed(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := &Handler{idGen: idGen} // repos unwired
	rec := postDraft(h.ThreadMutingCreate, `{"noteId":"n1"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	rec = postDraft(h.ThreadMutingDelete, `{"noteId":"n1"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// failingThreadMutingRepo は Exists だけを DB 障害にする。
type failingThreadMutingRepo struct {
	*testutil.MockNoteThreadMutingRepository
	err error
}

func (r *failingThreadMutingRepo) Exists(string, string) (bool, error) { return false, r.err }

// **DB 障害を「既にミュート済み」にも成功にも丸めない** (#2792)。Exists の error を
// 捨てると、接続断が 400 ALREADY_MUTING か 204 に化けて監視で 5xx が立たない。
func TestThreadMutingCreate_ExistsDBFailureIsNot4xx(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	h := &Handler{idGen: idGen, noteRepo: noteRepo, threadMutingRepo: &failingThreadMutingRepo{
		MockNoteThreadMutingRepository: testutil.NewMockNoteThreadMutingRepository(),
		err:                            errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	}}
	rec := postDraft(h.ThreadMutingCreate, `{"noteId":"n1"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "DB 障害が 4xx に化けている (#2792)")
	// body も見る。status だけだと Create 失敗側 (kind:client) と取り違えても通る。
	assert.Contains(t, rec.Body.String(), "INTERNAL_ERROR")
	assert.Contains(t, rec.Body.String(), `"kind":"server"`)
}
