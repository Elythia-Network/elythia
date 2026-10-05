package users_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/users"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newExtraHandler(t *testing.T) (*users.Handler, *testutil.MockUserRepository, *testutil.MockNoteRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	followRequestRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	followingSvc := corefollowing.NewService(userRepo, followingRepo, followRequestRepo, idGen)
	h := users.NewHandler(userSvc, followingSvc, noteRepo, idGen)
	h.SetAbuseRepo(testutil.NewMockAbuseReportRepository())
	return h, userRepo, noteRepo
}

func postExtra(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = h(c)
	return rec
}

// --- Relation ---

func TestRelation_Success(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Relation, `{"userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var relArr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &relArr))
	require.Len(t, relArr, 1)
	resp := relArr[0]
	assert.Equal(t, "u2", resp["id"])
	assert.Equal(t, false, resp["isFollowing"])
}

func TestRelation_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Relation, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestRelation_NilViewer covers the unauthenticated branch — Relation returns
// 200 with all relation flags false (matches upstream behavior).
func TestRelation_NilViewer(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Relation, `{"userId":"u2"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var relArr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &relArr))
	require.Len(t, relArr, 1)
	resp := relArr[0]
	assert.Equal(t, false, resp["isFollowing"])
	assert.Equal(t, false, resp["isBlocking"])
}

// failingFollowingRepo always returns a non-NotFound error so the warn path
// (#984) can be exercised — `users/relation` should silently fall back to
// false and emit slog.Warn instead of leaking the error to the response.
type failingFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFollowingRepo) FindByPair(_, _ string) (*model.Following, error) {
	return nil, errors.New("simulated transient DB error")
}

// TestRelation_TransientDBErrorFallsBackToFalse: non-NotFound error from
// repo should not propagate; relation field stays false. Smoke test the
// warnRelErr path.
func TestRelation_TransientDBErrorFallsBackToFalse(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	repo := &failingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository()}
	h.SetFollowingRepo(repo)

	rec := postExtra(h.Relation, `{"userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var relArr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &relArr))
	require.Len(t, relArr, 1)
	resp := relArr[0]
	assert.Equal(t, false, resp["isFollowing"])
	assert.Equal(t, false, resp["isFollowed"])
}

// TestRelation_PartialDirection covers the case where only one direction of
// a follow exists (u1 → u2 follow, no reverse). Guards against cross-talk
// regressions where bidirectional fields are mistakenly tied together.
func TestRelation_PartialDirection(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	followingRepo := testutil.NewMockFollowingRepository()
	h.SetFollowingRepo(followingRepo)
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FollowerID: "u1", FolloweeID: "u2"}))

	rec := postExtra(h.Relation, `{"userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var relArr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &relArr))
	require.Len(t, relArr, 1)
	resp := relArr[0]
	// u1 → u2 follow のみ。逆方向 (= u2 → u1) は存在しない。
	assert.Equal(t, true, resp["isFollowing"])
	assert.Equal(t, false, resp["isFollowed"])
	// 他 relation も全 false に保たれること (cross-talk regression guard)。
	assert.Equal(t, false, resp["hasPendingFollowRequestFromYou"])
	assert.Equal(t, false, resp["hasPendingFollowRequestToYou"])
	assert.Equal(t, false, resp["isBlocking"])
	assert.Equal(t, false, resp["isBlocked"])
	assert.Equal(t, false, resp["isMuted"])
	assert.Equal(t, false, resp["isRenoteMuted"])
}

// TestRelation_PopulatedRelations wires the 5 repos and seeds rows so every
// bool field flips to true. Validates #984 drift fix: handler returns real
// DB-backed state instead of hardcoded false.
func TestRelation_PopulatedRelations(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	followingRepo := testutil.NewMockFollowingRepository()
	followRequestRepo := testutil.NewMockFollowRequestRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	mutingRepo := testutil.NewMockMutingRepository()
	renoteMutingRepo := testutil.NewMockRenoteMutingRepository()
	h.SetFollowingRepo(followingRepo)
	h.SetFollowRequestRepo(followRequestRepo)
	h.SetBlockingRepo(blockingRepo)
	h.SetMutingRepo(mutingRepo)
	h.SetRenoteMutingRepo(renoteMutingRepo)

	// 双方向の follow / request / block + viewer→target の mute / renote-mute
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FollowerID: "u1", FolloweeID: "u2"}))
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f2", FollowerID: "u2", FolloweeID: "u1"}))
	require.NoError(t, followRequestRepo.Create(&model.FollowRequest{ID: "fr1", FollowerID: "u1", FolloweeID: "u2"}))
	require.NoError(t, followRequestRepo.Create(&model.FollowRequest{ID: "fr2", FollowerID: "u2", FolloweeID: "u1"}))
	require.NoError(t, blockingRepo.Create(&model.Blocking{ID: "b1", BlockerID: "u1", BlockeeID: "u2"}))
	require.NoError(t, blockingRepo.Create(&model.Blocking{ID: "b2", BlockerID: "u2", BlockeeID: "u1"}))
	require.NoError(t, mutingRepo.Create(&model.Muting{ID: "m1", MuterID: "u1", MuteeID: "u2"}))
	require.NoError(t, renoteMutingRepo.Create(&model.RenoteMuting{ID: "rm1", MuterID: "u1", MuteeID: "u2"}))

	rec := postExtra(h.Relation, `{"userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var relArr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &relArr))
	require.Len(t, relArr, 1)
	resp := relArr[0]
	assert.Equal(t, "u2", resp["id"])
	assert.Equal(t, true, resp["isFollowing"])
	assert.Equal(t, true, resp["isFollowed"])
	assert.Equal(t, true, resp["hasPendingFollowRequestFromYou"])
	assert.Equal(t, true, resp["hasPendingFollowRequestToYou"])
	assert.Equal(t, true, resp["isBlocking"])
	assert.Equal(t, true, resp["isBlocked"])
	assert.Equal(t, true, resp["isMuted"])
	assert.Equal(t, true, resp["isRenoteMuted"])
}

// --- ReportAbuse ---

func TestReportAbuse_Success(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestReportAbuse_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.ReportAbuse, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReportAbuse_NilRepo(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	h := users.NewHandler(userSvc, nil, noteRepo, idGen)
	// abuseRepo not set
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// --- Reactions ---

func TestReactions_Success(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Reactions, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestReactions_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.Reactions, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 以下 #821 PR-D で stub から本実装に書き換えた users/reactions の各 path
// を cover する。userRepo / noteReactionRepo を wire するので
// newExtraHandler とは別の構築 helper を使う。

func newReactionsHandler(t *testing.T) (*users.Handler, *testutil.MockUserRepository, *testutil.MockNoteReactionRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	h := users.NewHandler(userSvc, nil, noteRepo, idGen)
	h.SetUserRepo(userRepo)
	rxRepo := testutil.NewMockNoteReactionRepository()
	h.SetNoteReactionRepo(rxRepo)
	return h, userRepo, rxRepo
}

// target user が存在しない → 404 NO_SUCH_USER。
func TestReactions_NoSuchUser(t *testing.T) {
	h, _, _ := newReactionsHandler(t)
	rec := postExtra(h.Reactions, `{"userId":"missing"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// target が remote user (host が non-nil) → 400 IS_REMOTE_USER。
// upstream TS は `host !== null` で判定するので、host=="" の row も remote
// 扱いになる (= mk-go も nil 判定のみ)。
func TestReactions_RemoteUser(t *testing.T) {
	h, userRepo, _ := newReactionsHandler(t)
	host := "remote.example"
	userRepo.Users["u_remote"] = &model.User{ID: "u_remote", Host: &host}
	rec := postExtra(h.Reactions, `{"userId":"u_remote"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// publicReactions=false で viewer != target → 403 REACTIONS_NOT_PUBLIC。
func TestReactions_NotPublic(t *testing.T) {
	h, userRepo, _ := newReactionsHandler(t)
	userRepo.Users["u_target"] = &model.User{ID: "u_target"}
	userRepo.Profiles["u_target"] = &model.UserProfile{UserID: "u_target", PublicReactions: false}
	rec := postExtra(h.Reactions, `{"userId":"u_target"}`, &model.User{ID: "u_viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// self view (viewer == target) なら publicReactions=false でも取得可能。
// あわせて Preload された User / Note が出力 entry に詰まること、
// idGen.ParseTime で createdAt が ISO8601 形式に整形されることも check。
func TestReactions_SelfViewBypassesPublicReactions(t *testing.T) {
	h, userRepo, rxRepo := newReactionsHandler(t)
	userRepo.Users["u_self"] = &model.User{ID: "u_self"}
	userRepo.Profiles["u_self"] = &model.UserProfile{UserID: "u_self", PublicReactions: false}
	// createdAt の format check のため real aidx を使う (= ParseTime が成功)。
	idGen, _ := id.NewGenerator("aidx")
	rxID := idGen.Generate(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	noteID := idGen.Generate(time.Date(2026, 5, 7, 11, 0, 0, 0, time.UTC))
	noteText := "hi"
	rxRepo.Reactions[rxID] = &model.NoteReaction{
		ID:       rxID,
		UserID:   "u_self",
		NoteID:   noteID,
		Reaction: "👍",
		User:     &model.User{ID: "u_self", Username: "self"},
		// Note.User を preload 済みにする (production の Preload("Note.User") 相当)。
		Note: &model.Note{
			ID: noteID, UserID: "u_other", Text: &noteText,
			Visibility: "public",
			User:       &model.User{ID: "u_other", Username: "other"},
		},
	}
	rec := postExtra(h.Reactions, `{"userId":"u_self","limit":100}`, &model.User{ID: "u_self"})
	require.Equal(t, http.StatusOK, rec.Code)

	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	entry := list[0]
	assert.Equal(t, rxID, entry["id"])
	assert.Equal(t, "👍", entry["type"])
	require.NotNil(t, entry["user"])
	require.NotNil(t, entry["note"])
	note, _ := entry["note"].(map[string]any)
	assert.Equal(t, noteID, note["id"])
	assert.Equal(t, "u_other", note["userId"])
	assert.Equal(t, "hi", note["text"])
	// note は完全 shape で返ること。misskey_dart の Note.fromJson が非null必須と
	// する createdAt / visibility / user が欠落していると落ちる (#1227 回帰防止)。
	noteCreatedAt, ok := note["createdAt"].(string)
	assert.True(t, ok, "note.createdAt must be a non-null string")
	assert.NotEmpty(t, noteCreatedAt)
	assert.Equal(t, "public", note["visibility"])
	noteUser, ok := note["user"].(map[string]any)
	require.True(t, ok, "note.user must be a non-null object")
	assert.Equal(t, "u_other", noteUser["id"])
	// createdAt は ISO8601 ms 精度で出る (= 2026-05-07T12:00:00.000Z)。
	createdAt, ok := entry["createdAt"].(string)
	require.True(t, ok, "createdAt should be a string")
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", createdAt)
	require.NoError(t, err, "createdAt=%q should be ISO8601 ms", createdAt)
	assert.Equal(t, 2026, parsed.Year())
}

// publicReactions=true (default) で viewer != target → list を返す。
func TestReactions_PublicReactions(t *testing.T) {
	h, userRepo, rxRepo := newReactionsHandler(t)
	userRepo.Users["u_pub"] = &model.User{ID: "u_pub"}
	userRepo.Profiles["u_pub"] = &model.UserProfile{UserID: "u_pub", PublicReactions: true}
	rxRepo.Reactions["rx2"] = &model.NoteReaction{
		ID:       "rx2",
		UserID:   "u_pub",
		NoteID:   "n2",
		Reaction: "❤",
		// real repo は FK + Preload で note が必ず存在するので public note を seed
		// する (visibility filter で除外されないことの確認も兼ねる)。
		Note: &model.Note{ID: "n2", UserID: "u_author", Visibility: "public"},
	}
	rec := postExtra(h.Reactions, `{"userId":"u_pub"}`, &model.User{ID: "u_viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
}

// noteReactionRepo が wire されていない (= test stub 構成) なら空配列を
// 返す互換 path を維持する。
func TestReactions_NoteReactionRepoNotWired(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	h := users.NewHandler(userSvc, nil, noteRepo, idGen)
	h.SetUserRepo(userRepo)
	userRepo.Users["u_x"] = &model.User{ID: "u_x"}
	userRepo.Profiles["u_x"] = &model.UserProfile{UserID: "u_x", PublicReactions: true}
	// noteReactionRepo を SetNoteReactionRepo していない状態。
	rec := postExtra(h.Reactions, `{"userId":"u_x"}`, &model.User{ID: "u_x"})
	require.Equal(t, http.StatusOK, rec.Code)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list, 0)
}

// noteReactionRepo.ListByUserID がエラー → 500 INTERNAL_ERROR。
type failingListByUserIDRepo struct {
	*testutil.MockNoteReactionRepository
}

func (f *failingListByUserIDRepo) ListByUserID(_, _, _, _ string, _ int) ([]*model.NoteReaction, error) {
	return nil, assert.AnError
}

func TestReactions_ListError(t *testing.T) {
	h, userRepo, _ := newReactionsHandler(t)
	userRepo.Users["u_e"] = &model.User{ID: "u_e"}
	userRepo.Profiles["u_e"] = &model.UserProfile{UserID: "u_e", PublicReactions: true}
	h.SetNoteReactionRepo(&failingListByUserIDRepo{testutil.NewMockNoteReactionRepository()})
	rec := postExtra(h.Reactions, `{"userId":"u_e"}`, &model.User{ID: "u_e"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- FeaturedNotes ---

func TestFeaturedNotes_Success(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFeaturedNotes_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.FeaturedNotes, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// anonymous viewer に対して followers / specified visibility の
// ノートが除外されることを guard する。
func TestFeaturedNotes_AnonymousExcludesNonPublicVisibility(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())
	noteRepo.Notes["fn_pub"] = &model.Note{ID: "fn_pub", UserID: "u1", Visibility: "public", User: &model.User{ID: "u1"}}
	noteRepo.Notes["fn_fol"] = &model.Note{ID: "fn_fol", UserID: "u1", Visibility: "followers", User: &model.User{ID: "u1"}}
	noteRepo.Notes["fn_spec"] = &model.Note{ID: "fn_spec", UserID: "u1", Visibility: "specified", VisibleUserIDs: []string{"other"}, User: &model.User{ID: "u1"}}

	rec := postExtra(h.FeaturedNotes, `{"userId":"u1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := map[string]bool{}
	for _, n := range out {
		ids[n["id"].(string)] = true
	}
	assert.True(t, ids["fn_pub"])
	assert.False(t, ids["fn_fol"], "followers は anonymous に漏らさない")
	assert.False(t, ids["fn_spec"], "specified は対象外 viewer に漏らさない")
}

// #1487 Option B / #1491 review: upstream featured-notes.ts は engagement で
// 上位を選抜したあと id DESC で表示する。mk-go も同じ 2 段で揃えるため、
// engagement の高低と id の大小が一致しない fixture で表示順が id DESC に
// なることを覆う (engagement DESC のままだと untilId cursor とソート順が
// ずれてページングで重複/欠落するため逆の挙動を見せたい)。
func TestFeaturedNotes_OrderedByIDDescAfterEngagementSelection(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())
	// engagement と id の並びが逆になる fixture:
	//   id DESC:        fn_x3, fn_x2, fn_x1
	//   engagement DESC: fn_x1 (10), fn_x2 (5), fn_x3 (1)
	noteRepo.Notes["fn_x1"] = &model.Note{ID: "fn_x1", UserID: "u1", Visibility: "public", RenoteCount: 10, User: &model.User{ID: "u1"}}
	noteRepo.Notes["fn_x2"] = &model.Note{ID: "fn_x2", UserID: "u1", Visibility: "public", RenoteCount: 5, User: &model.User{ID: "u1"}}
	noteRepo.Notes["fn_x3"] = &model.Note{ID: "fn_x3", UserID: "u1", Visibility: "public", RenoteCount: 1, User: &model.User{ID: "u1"}}

	rec := postExtra(h.FeaturedNotes, `{"userId":"u1","limit":10}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 3)
	assert.Equal(t, "fn_x3", out[0]["id"], "display は id DESC")
	assert.Equal(t, "fn_x2", out[1]["id"])
	assert.Equal(t, "fn_x1", out[2]["id"])
}

// #1491 review: untilId は display 段 (id DESC + id < untilId) で適用され、
// engagement DESC で発生する重複/欠落なしにページングできることを覆う。
func TestFeaturedNotes_UntilIDPaginatesByIDDesc(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())
	noteRepo.Notes["fn_x1"] = &model.Note{ID: "fn_x1", UserID: "u1", Visibility: "public", RenoteCount: 10, User: &model.User{ID: "u1"}}
	noteRepo.Notes["fn_x2"] = &model.Note{ID: "fn_x2", UserID: "u1", Visibility: "public", RenoteCount: 5, User: &model.User{ID: "u1"}}
	noteRepo.Notes["fn_x3"] = &model.Note{ID: "fn_x3", UserID: "u1", Visibility: "public", RenoteCount: 1, User: &model.User{ID: "u1"}}

	// untilId="fn_x2" → id < fn_x2 = fn_x1 のみ。fn_x3 は除外され、fn_x2 自身も除外。
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1","limit":10,"untilId":"fn_x2"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "fn_x1", out[0]["id"])
}

// #1491 audit 指摘: TestFeaturedNotes_* は 3-10 件の fixture しか使っておらず、
// mock 側の pool cap (mockFeaturedNotesPerUserPoolSize=50) を踏まない。51 件
// fixture で「engagement=0 最大id の note は pool 外に落ちて display に現れず、
// engagement=10 の 50 件のみ id DESC で返る」ことを mock 経由でも fix する。
// repo 側の TestNoteRepository_ListFeaturedByUser_EngagementPoolCap の handler
// レイヤー版で、mock の pool cap が silently 別値になった瞬間に落ちる。
func TestFeaturedNotes_EngagementPoolCap_DropsLowEngagementBelow50(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())

	// engagement=10 を 50 件 (id: fn_cap_pool_01 .. fn_cap_pool_50)。
	for i := 1; i <= 50; i++ {
		id := fmt.Sprintf("fn_cap_pool_%02d", i)
		noteRepo.Notes[id] = &model.Note{
			ID: id, UserID: "u1", Visibility: "public", RenoteCount: 10, User: &model.User{ID: "u1"},
		}
	}
	// engagement=0 + 最大 id (lex order で全 pool note より後ろ)。
	const lowID = "fn_cap_zzz_low"
	noteRepo.Notes[lowID] = &model.Note{
		ID: lowID, UserID: "u1", Visibility: "public", RenoteCount: 0, User: &model.User{ID: "u1"},
	}

	rec := postExtra(h.FeaturedNotes, `{"userId":"u1","limit":100}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 50, "mock pool cap=50 が踏まれて 50 件に絞られる")
	for _, n := range out {
		gotID := n["id"].(string)
		assert.NotEqual(t, lowID, gotID, "engagement 最下位 (=0) は pool 外に落ちる")
		assert.True(t, strings.HasPrefix(gotID, "fn_cap_pool_"),
			"engagement DESC 上位 50 件 (fn_cap_pool_*) のみが選抜される")
	}
	// display は id DESC: fn_cap_pool_50 が先頭、fn_cap_pool_01 が末尾。
	assert.Equal(t, "fn_cap_pool_50", out[0]["id"])
	assert.Equal(t, "fn_cap_pool_01", out[49]["id"])
}

// #1487: post-fetch FilterVisible → SQL push-down に変更されたことで、limit に
// 達するまでに非表示 note が間引かれないことを覆う (under-fill 回帰防止)。
func TestFeaturedNotes_LimitNotUnderfilled(t *testing.T) {
	h, _, noteRepo := newExtraHandler(t)
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())
	// public 5 件 + non-visible (followers) 5 件、limit=5 で public のみ 5 件返る。
	for i := 0; i < 5; i++ {
		id := "fn_p" + string(rune('0'+i))
		noteRepo.Notes[id] = &model.Note{ID: id, UserID: "u1", Visibility: "public", RenoteCount: int16(10 - i), User: &model.User{ID: "u1"}}
	}
	for i := 0; i < 5; i++ {
		id := "fn_f" + string(rune('0'+i))
		noteRepo.Notes[id] = &model.Note{ID: id, UserID: "u1", Visibility: "followers", RenoteCount: int16(100), User: &model.User{ID: "u1"}}
	}
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1","limit":5}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 5, "push-down で limit ぶん必ず埋まる")
	for _, n := range out {
		// followers は出ない。
		assert.NotContains(t, n["id"].(string), "fn_f")
	}
}

// --- SearchByUsernameAndHost ---

func TestSearchByUsernameAndHost_Success(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	rec := postExtra(h.SearchByUsernameAndHost, `{"username":"alice"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	shapetest.Assert(t, "UserLite", out[0]) // L3 (#1314)
}

func TestSearchByUsernameAndHost_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.SearchByUsernameAndHost, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #766 / #1064: host filter の 3-state semantics を handler 経由で覆う。
//   - host 未指定 / 空文字 → host filter なし (= local + remote)
//   - host 指定 → 当該 host の prefix match (case-insensitive)
func TestSearchByUsernameAndHost_FiltersByHost(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	remote := "remote.example"
	other := "other.example"
	userRepo.Users["u_local"] = &model.User{ID: "u_local", Username: "alice", UsernameLower: "alice"}
	userRepo.Users["u_remote"] = &model.User{ID: "u_remote", Username: "alice", UsernameLower: "alice", Host: &remote}
	userRepo.Users["u_other"] = &model.User{ID: "u_other", Username: "alice", UsernameLower: "alice", Host: &other}

	// #1064: host 未指定 / 空文字 → local + remote 両方返す。
	// 旧来は local 限定だったが、frontend MkAutocomplete が `@alice` だけ
	// 入力した状態で host=undefined を送るので、それでは remote candidate
	// が一切出ない drop-in 互換性違反だった。
	for _, body := range []string{`{"username":"alice"}`, `{"username":"alice","host":""}`} {
		rec := postExtra(h.SearchByUsernameAndHost, body, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var got []entity.UserLite
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		ids := make(map[string]bool, len(got))
		for _, u := range got {
			ids[u.ID] = true
		}
		assert.True(t, ids["u_local"], "body=%s should include local user", body)
		assert.True(t, ids["u_remote"], "body=%s should include remote user", body)
		assert.True(t, ids["u_other"], "body=%s should include other-host user", body)
	}

	// host 指定 → 当該 host の prefix match user のみ (local や別 host の user は除外)
	rec := postExtra(h.SearchByUsernameAndHost, `{"username":"alice","host":"remote.example"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []entity.UserLite
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "u_remote", got[0].ID)

	// case-insensitive な host 比較
	rec = postExtra(h.SearchByUsernameAndHost, `{"username":"alice","host":"REMOTE.EXAMPLE"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "u_remote", got[0].ID)

	// #1064: host="." は local 限定 (upstream の shortcut)。
	rec = postExtra(h.SearchByUsernameAndHost, `{"username":"alice","host":"."}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "u_local", got[0].ID)
}

// --- UpdateMemo ---

func TestUpdateMemo_Success(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	userRepo.Users["u2"] = &model.User{ID: "u2"}
	rec := postExtra(h.UpdateMemo, `{"userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestUpdateMemo_NoSuchUser: target が存在しなければ NO_SUCH_USER (#1547)。
func TestUpdateMemo_NoSuchUser(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.UpdateMemo, `{"userId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "6fef56f3-e765-4957-88e5-c6f65329b8a5")
}

func TestUpdateMemo_InvalidParam(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	rec := postExtra(h.UpdateMemo, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Failing repo ---

type failingAbuseCreateRepo struct {
	*testutil.MockAbuseReportRepository
}

func (f *failingAbuseCreateRepo) Create(_ *model.AbuseUserReport) error {
	return assert.AnError
}

func TestReportAbuse_CreateError(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	h.SetAbuseRepo(&failingAbuseCreateRepo{testutil.NewMockAbuseReportRepository()})
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// FeaturedNotes は #1487 で ListFeaturedByUser に切り替わったため、500 経路は
// 当該メソッドが err を返す stub で覆う。
type failingListFeaturedByUserRepo struct {
	*testutil.MockNoteRepository
}

func (f *failingListFeaturedByUserRepo) ListFeaturedByUser(_, _, _ string, _ int) ([]*model.Note, error) {
	return nil, assert.AnError
}

func TestFeaturedNotes_Error(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := &failingListFeaturedByUserRepo{testutil.NewMockNoteRepository()}
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	h := users.NewHandler(userSvc, nil, noteRepo, idGen)
	rec := postExtra(h.FeaturedNotes, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingSearchRepo struct {
	*testutil.MockUserRepository
}

func (f *failingSearchRepo) SearchByUsernameAndHost(_ string, _ *string, _ bool, _ int) ([]*model.User, error) {
	return nil, assert.AnError
}

func TestSearchByUsernameAndHost_Error(t *testing.T) {
	userRepo := &failingSearchRepo{testutil.NewMockUserRepository()}
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	h := users.NewHandler(userSvc, nil, noteRepo, idGen)
	rec := postExtra(h.SearchByUsernameAndHost, `{"username":"x"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// stubModeratorChecker は指定 userID を moderator とみなす test stub。
// adminID を指定すると IsAdministrator が true を返す。
type stubModeratorChecker struct {
	modID   string
	adminID string
}

func (s stubModeratorChecker) IsModerator(userID string) bool     { return userID == s.modID }
func (s stubModeratorChecker) IsAdministrator(userID string) bool { return userID == s.adminID }

// moderator viewer は remote user の reaction も閲覧できる (upstream:
// "Moderators can see reactions of all users")。IS_REMOTE_USER を返さず
// reaction list (空でも 200) を返す。リモートユーザーのリアクションが
// moderator で見られない回帰の防止。
func TestReactions_ModeratorSeesRemoteUser(t *testing.T) {
	h, userRepo, _ := newReactionsHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{modID: "u_mod"})
	host := "remote.example"
	userRepo.Users["u_remote"] = &model.User{ID: "u_remote", Host: &host}
	rec := postExtra(h.Reactions, `{"userId":"u_remote"}`, &model.User{ID: "u_mod"})
	assert.Equal(t, http.StatusOK, rec.Code) // IS_REMOTE_USER ではなく list (空) を返す
}

// moderator viewer は publicReactions=false の user の reaction も閲覧できる。
func TestReactions_ModeratorSeesNonPublic(t *testing.T) {
	h, userRepo, _ := newReactionsHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{modID: "u_mod"})
	userRepo.Users["u_target"] = &model.User{ID: "u_target"}
	userRepo.Profiles["u_target"] = &model.UserProfile{UserID: "u_target", PublicReactions: false}
	rec := postExtra(h.Reactions, `{"userId":"u_target"}`, &model.User{ID: "u_mod"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// 非 moderator viewer では remote user は従来どおり IS_REMOTE_USER のまま。
func TestReactions_NonModeratorRemoteStillBlocked(t *testing.T) {
	h, userRepo, _ := newReactionsHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{modID: "u_mod"})
	host := "remote.example"
	userRepo.Users["u_remote"] = &model.User{ID: "u_remote", Host: &host}
	rec := postExtra(h.Reactions, `{"userId":"u_remote"}`, &model.User{ID: "u_other"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// seedVisibilityReactions は public note と followers-only note それぞれへの
// reaction を target に seed する。visibility filter の検証用 helper。
func seedVisibilityReactions(t *testing.T, rxRepo *testutil.MockNoteReactionRepository, targetID string) (publicRxID, followersRxID string) {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	publicRxID = idGen.Generate(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	followersRxID = idGen.Generate(time.Date(2026, 5, 10, 13, 0, 0, 0, time.UTC))
	pubNoteID := idGen.Generate(time.Date(2026, 5, 10, 11, 0, 0, 0, time.UTC))
	folNoteID := idGen.Generate(time.Date(2026, 5, 10, 11, 30, 0, 0, time.UTC))
	pubText, folText := "public note", "followers only note"
	rxRepo.Reactions[publicRxID] = &model.NoteReaction{
		ID: publicRxID, UserID: targetID, NoteID: pubNoteID, Reaction: "👍",
		User: &model.User{ID: targetID, Username: "target"},
		Note: &model.Note{
			ID: pubNoteID, UserID: "u_author", Text: &pubText,
			Visibility: "public", User: &model.User{ID: "u_author", Username: "author"},
		},
	}
	rxRepo.Reactions[followersRxID] = &model.NoteReaction{
		ID: followersRxID, UserID: targetID, NoteID: folNoteID, Reaction: "❤",
		User: &model.User{ID: targetID, Username: "target"},
		Note: &model.Note{
			ID: folNoteID, UserID: "u_author", Text: &folText,
			Visibility: "followers", User: &model.User{ID: "u_author", Username: "author"},
		},
	}
	return publicRxID, followersRxID
}

// upstream の generateVisibilityQuery(query, me) 相当: viewer が閲覧できない
// note (followers-only で follower でない) の reaction は除外され、public note
// の reaction のみ返る。mock の Following を設定しないので followers note は
// 非 follower viewer 扱いで除外される。
func TestReactions_FiltersInvisibleNotes(t *testing.T) {
	h, userRepo, rxRepo := newReactionsHandler(t)
	userRepo.Users["u_target"] = &model.User{ID: "u_target"}
	userRepo.Profiles["u_target"] = &model.UserProfile{UserID: "u_target", PublicReactions: true}
	publicRxID, followersRxID := seedVisibilityReactions(t, rxRepo, "u_target")

	rec := postExtra(h.Reactions, `{"userId":"u_target","limit":100}`, &model.User{ID: "u_viewer"})
	require.Equal(t, http.StatusOK, rec.Code)

	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1, "followers-only note reaction must be filtered out")
	assert.Equal(t, publicRxID, list[0]["id"])
	assert.NotEqual(t, followersRxID, list[0]["id"])
}

// follower viewer は followers-only note の reaction も閲覧できる
// (mock の Following に viewer -> author を登録)。public とあわせ 2 件返る。
func TestReactions_FollowerSeesFollowersOnlyNote(t *testing.T) {
	h, userRepo, rxRepo := newReactionsHandler(t)
	userRepo.Users["u_target"] = &model.User{ID: "u_target"}
	userRepo.Profiles["u_target"] = &model.UserProfile{UserID: "u_target", PublicReactions: true}
	seedVisibilityReactions(t, rxRepo, "u_target")
	// u_follower が note author (u_author) を follow している。
	rxRepo.Following = map[string][]string{"u_follower": {"u_author"}}

	rec := postExtra(h.Reactions, `{"userId":"u_target","limit":100}`, &model.User{ID: "u_follower"})
	require.Equal(t, http.StatusOK, rec.Code)

	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 2, "follower must see both public and followers-only note reactions")
}

// specified note は visibleUserIds に含まれる viewer のみ閲覧でき、含まれない
// viewer からは除外される (CanSeeNote の specified 分岐相当)。
func TestReactions_SpecifiedNoteVisibility(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	rxID := idGen.Generate(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC))
	noteID := idGen.Generate(time.Date(2026, 5, 11, 11, 0, 0, 0, time.UTC))
	text := "specified note"
	seed := func(rxRepo *testutil.MockNoteReactionRepository) {
		rxRepo.Reactions[rxID] = &model.NoteReaction{
			ID: rxID, UserID: "u_target", NoteID: noteID, Reaction: "👀",
			User: &model.User{ID: "u_target", Username: "target"},
			Note: &model.Note{
				ID: noteID, UserID: "u_author", Text: &text,
				Visibility: "specified", VisibleUserIDs: []string{"u_allowed"},
				User: &model.User{ID: "u_author", Username: "author"},
			},
		}
	}

	// visibleUserIds に含まれる viewer は閲覧可。
	h, userRepo, rxRepo := newReactionsHandler(t)
	userRepo.Users["u_target"] = &model.User{ID: "u_target"}
	userRepo.Profiles["u_target"] = &model.UserProfile{UserID: "u_target", PublicReactions: true}
	seed(rxRepo)
	rec := postExtra(h.Reactions, `{"userId":"u_target","limit":100}`, &model.User{ID: "u_allowed"})
	require.Equal(t, http.StatusOK, rec.Code)
	var allowed []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &allowed))
	require.Len(t, allowed, 1, "viewer in visibleUserIds must see the specified note reaction")

	// 含まれない viewer からは除外。
	h2, userRepo2, rxRepo2 := newReactionsHandler(t)
	userRepo2.Users["u_target"] = &model.User{ID: "u_target"}
	userRepo2.Profiles["u_target"] = &model.UserProfile{UserID: "u_target", PublicReactions: true}
	seed(rxRepo2)
	rec2 := postExtra(h2.Reactions, `{"userId":"u_target","limit":100}`, &model.User{ID: "u_other"})
	require.Equal(t, http.StatusOK, rec2.Code)
	var other []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &other))
	assert.Len(t, other, 0, "viewer not in visibleUserIds must not see the specified note reaction")
}

// moderator viewer でも visibility filter は適用される (upstream は
// generateVisibilityQuery を moderator 含む全 viewer に無条件適用)。
// moderator は remote / publicReactions gate を bypass するが、follower で
// ない followers-only note の reaction は除外される。
func TestReactions_ModeratorStillFiltersInvisibleNotes(t *testing.T) {
	h, userRepo, rxRepo := newReactionsHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{modID: "u_mod"})
	userRepo.Users["u_target"] = &model.User{ID: "u_target"}
	userRepo.Profiles["u_target"] = &model.UserProfile{UserID: "u_target", PublicReactions: false}
	publicRxID, _ := seedVisibilityReactions(t, rxRepo, "u_target")

	rec := postExtra(h.Reactions, `{"userId":"u_target","limit":100}`, &model.User{ID: "u_mod"})
	require.Equal(t, http.StatusOK, rec.Code)

	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1, "moderator must not see followers-only note reaction")
	assert.Equal(t, publicRxID, list[0]["id"])
}

// --- #H-PR8b: report-abuse validation ---

func TestReportAbuse_NoSuchUser(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	h.SetUserRepo(userRepo) // target u2 は存在しない
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "1acefcb5-0959-43fd-9685-b48305736cb5")
}

func TestReportAbuse_CannotReportYourself(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "self"}
	h.SetUserRepo(userRepo)
	rec := postExtra(h.ReportAbuse, `{"userId":"u1","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CANNOT_REPORT_YOURSELF")
}

func TestReportAbuse_CannotReportAdmin(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "boss"}
	h.SetUserRepo(userRepo)
	h.SetModeratorChecker(stubModeratorChecker{adminID: "u2"})
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CANNOT_REPORT_THE_ADMIN")
}

func TestReportAbuse_CommentTooLong(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	long := strings.Repeat("a", 2049)
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"`+long+`"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// maxLength は rune 数で判定する。多バイト文字 2048 個 (byte では超過) は通り、
// 2049 個は弾く (ajv の文字数基準と一致)。
func TestReportAbuse_CommentRuneLength(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "x"}
	h.SetUserRepo(userRepo)

	ok := strings.Repeat("あ", 2048) // 6144 byte だが 2048 文字なので通る
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"`+ok+`"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)

	tooLong := strings.Repeat("あ", 2049)
	rec = postExtra(h.ReportAbuse, `{"userId":"u2","comment":"`+tooLong+`"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReportAbuse_SavesTargetUserHost(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	host := "remote.example"
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "remoteuser", Host: &host}
	h.SetUserRepo(userRepo)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	h.SetAbuseRepo(abuseRepo)
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, abuseRepo.Reports, 1)
	for _, r := range abuseRepo.Reports {
		require.NotNil(t, r.TargetUserHost)
		assert.Equal(t, "remote.example", *r.TargetUserHost)
	}
}

// #1996: public /users の state enum は ['all','alive'] (空は default 'all')。
// 範囲外 (moderator/admin 等) は reject させる (= ValidListState=false → handler 400)。
func TestValidListState(t *testing.T) {
	for _, ok := range []string{"", "all", "alive"} {
		assert.True(t, users.ValidListState(ok), "state=%q は許容 (#1996)", ok)
	}
	for _, bad := range []string{"moderator", "admin", "available", "suspended", "ALL"} {
		assert.False(t, users.ValidListState(bad), "state=%q は範囲外で reject (#1996)", bad)
	}
}

// #2106 L8: viewer が block されていても remote target には block 早期 return より先に
// IS_REMOTE_USER を返す (upstream reactions.ts の評価順)。
func TestReactions_BlockedAndRemote_ReturnsRemoteError(t *testing.T) {
	h, userRepo, _ := newReactionsHandler(t)
	host := "remote.example"
	userRepo.Users["u_remote"] = &model.User{ID: "u_remote", Host: &host}
	blockRepo := testutil.NewMockBlockingRepository()
	_ = blockRepo.Create(&model.Blocking{ID: "b1", BlockerID: "u_remote", BlockeeID: "u_viewer"})
	h.SetBlockingRepo(blockRepo)
	rec := postExtra(h.Reactions, `{"userId":"u_remote"}`, &model.User{ID: "u_viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "IS_REMOTE_USER")
}
