package antennas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testRedis *testutil.TestRedis
	idGen     id.Generator
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("redis setup failed: %v", err)
	}
	testRedis = tr
	idGen, _ = id.NewGenerator("aidx")
	code := m.Run()
	testRedis.Teardown(ctx)
	os.Exit(code)
}

func newHandler(t *testing.T) (*Handler, *testutil.MockAntennaRepository, *testutil.MockNoteRepository) {
	t.Helper()
	testRedis.FlushAll(context.Background())
	repo := testutil.NewMockAntennaRepository()
	noteRepo := testutil.NewMockNoteRepository()
	svc := coreantenna.NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	return NewHandler(svc, noteRepo, idGen), repo, noteRepo
}

func newReq(t *testing.T, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func setUser(c echo.Context, userID string) {
	c.Set(string(middleware.UserContextKey), &model.User{ID: userID})
}

// --- Create ----------------------------------------------------------------

func TestCreate_Success(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha","src":"all","keywords":[["foo"]],"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestCreate_NoUserIdInResponse は #904 で fix した shape drift の
// regression guard。upstream Misskey TS の packAntenna は userId を
// embed しないので、mk-go も同 shape (= userId field なし) で揃える。
func TestCreate_NoUserIdInResponse(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha","src":"all","keywords":[["foo"]],"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	_, ok := body["userId"]
	assert.False(t, ok, "antennaToMap response must not include userId (#904)")
	// 必須 field の existence は念のため touch しておく (drift で吸い込ま
	// れないように reverse の guard)。
	assert.Equal(t, "alpha", body["name"])
	// misskey_dart の Antenna.fromJson は createdAt (非null String) /
	// hasUnreadNote (非null bool) を要求する (#1244)。
	createdAt, ok := body["createdAt"].(string)
	assert.True(t, ok, "createdAt must be a non-null string")
	assert.NotEmpty(t, createdAt)
	hasUnread, ok := body["hasUnreadNote"].(bool)
	assert.True(t, ok, "hasUnreadNote must be a non-null bool")
	assert.False(t, hasUnread)
	// L3 (#1270): antennas/create の実レスポンスを golden Antenna に突合する
	// (上の #1244 手動チェックを gate 化して全 field をカバー)。
	shapetest.Assert(t, "Antenna", body)
}

func TestCreate_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_NameRequired(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_InvalidSource(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha","src":"bogus","keywords":[["foo"]],"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingAntennaRepo returns errors on Create.
type failingAntennaRepo struct {
	*testutil.MockAntennaRepository
}

func (r *failingAntennaRepo) Create(_ *model.Antenna) error { return errors.New("boom") }

func TestCreate_RepoError(t *testing.T) {
	mock := testutil.NewMockAntennaRepository()
	repo := &failingAntennaRepo{MockAntennaRepository: mock}
	svc := coreantenna.NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	h := NewHandler(svc, testutil.NewMockNoteRepository(), idGen)
	c, rec := newReq(t, `{"name":"alpha","src":"all","keywords":[["foo"]],"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// newHandlerWithUserList は userListRepo を配線した handler を返す
// (NO_SUCH_USER_LIST 検証用)。
func newHandlerWithUserList(t *testing.T) (*Handler, *testutil.MockAntennaRepository, *testutil.MockUserListRepository) {
	t.Helper()
	testRedis.FlushAll(context.Background())
	repo := testutil.NewMockAntennaRepository()
	lists := testutil.NewMockUserListRepository()
	svc := coreantenna.NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	svc.SetUserListRepo(lists)
	return NewHandler(svc, testutil.NewMockNoteRepository(), idGen), repo, lists
}

// upstream create.ts: keywords と excludeKeywords が両方空なら EMPTY_KEYWORD (#1544)。
func TestCreate_EmptyKeyword(t *testing.T) {
	h, _, _ := newHandler(t)
	// keywords/excludeKeywords 未指定 → 両方空扱いで EMPTY_KEYWORD。
	c, rec := newReq(t, `{"name":"alpha","src":"all","excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false,"keywords":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EMPTY_KEYWORD")

	// 空文字のみの DNF も空扱い。
	c, rec = newReq(t, `{"name":"alpha","src":"all","keywords":[[""]],"excludeKeywords":[[""]],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// excludeKeywords のみ指定なら EMPTY_KEYWORD にはならない。
func TestCreate_ExcludeKeywordOnlySatisfies(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha","src":"all","excludeKeywords":[["spam"]],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false,"keywords":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// upstream create.ts: src=list で userListId が自分の list でないなら NO_SUCH_USER_LIST。
func TestCreate_NoSuchUserList(t *testing.T) {
	h, _, lists := newHandlerWithUserList(t)
	// 他人 (bob) 所有の list を alice が参照 → NO_SUCH_USER_LIST。
	require.NoError(t, lists.Create(&model.UserList{ID: "ul1", UserID: "bob", Name: "theirs"}))
	c, rec := newReq(t, `{"name":"alpha","src":"list","userListId":"ul1","keywords":[["foo"]],"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER_LIST")

	// 存在しない list も同様。
	c, rec = newReq(t, `{"name":"alpha","src":"list","userListId":"ghost","keywords":[["foo"]],"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 自分の list を参照する src=list は成功する。
func TestCreate_OwnedUserListAccepted(t *testing.T) {
	h, _, lists := newHandlerWithUserList(t)
	require.NoError(t, lists.Create(&model.UserList{ID: "ul1", UserID: "alice", Name: "mine"}))
	c, rec := newReq(t, `{"name":"alpha","src":"list","userListId":"ul1","keywords":[["foo"]],"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// excludeNotesInSensitiveChannel が永続化されレスポンスに反映される (#1544)。
func TestCreate_ExcludeNotesInSensitiveChannelPersisted(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha","src":"all","keywords":[["foo"]],"excludeNotesInSensitiveChannel":true,"excludeKeywords":[],"users":[],"caseSensitive":false,"withReplies":false,"withFile":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["excludeNotesInSensitiveChannel"])
}

// --- Show ------------------------------------------------------------------

func TestShow_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "Antenna", resp) // L3 (#1318)
}

func TestShow_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"antennaId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_AccessDenied(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "bob"}
	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Update ----------------------------------------------------------------

func TestUpdate_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{"antennaId":"a1","name":"alpha-v2"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"antennaId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_AccessDenied(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "bob"}
	c, rec := newReq(t, `{"antennaId":"a1","name":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_NameEmpty(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	c, rec := newReq(t, `{"antennaId":"a1","name":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_InvalidSource(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	c, rec := newReq(t, `{"antennaId":"a1","src":"bogus"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingUpdateRepo causes UpdateFields to fail.
type failingUpdateRepo struct {
	*testutil.MockAntennaRepository
}

func (r *failingUpdateRepo) UpdateFields(_ string, _ map[string]any) error {
	return errors.New("boom")
}

func TestUpdate_RepoError(t *testing.T) {
	mock := testutil.NewMockAntennaRepository()
	mock.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	repo := &failingUpdateRepo{MockAntennaRepository: mock}
	svc := coreantenna.NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	h := NewHandler(svc, testutil.NewMockNoteRepository(), idGen)
	c, rec := newReq(t, `{"antennaId":"a1","name":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// upstream update.ts: keywords/excludeKeywords が両方指定され両方空なら EMPTY_KEYWORD (#1544)。
func TestUpdate_EmptyKeyword(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{"antennaId":"a1","keywords":[[""]],"excludeKeywords":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EMPTY_KEYWORD")
}

// keywords のみ指定 (excludeKeywords 未指定) なら EMPTY_KEYWORD 検査しない。
func TestUpdate_OneSidedKeywordsNotChecked(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{"antennaId":"a1","keywords":[[""]]}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// upstream update.ts: 既存 src=list で userListId が自分の list でないなら NO_SUCH_USER_LIST。
func TestUpdate_NoSuchUserList(t *testing.T) {
	h, repo, lists := newHandlerWithUserList(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice", Name: "alpha", Src: model.AntennaSourceList}
	require.NoError(t, lists.Create(&model.UserList{ID: "ul1", UserID: "bob", Name: "theirs"}))
	c, rec := newReq(t, `{"antennaId":"a1","userListId":"ul1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER_LIST")
}

// excludeNotesInSensitiveChannel を update で更新できる (#1544)。
func TestUpdate_ExcludeNotesInSensitiveChannelPersisted(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{"antennaId":"a1","excludeNotesInSensitiveChannel":true}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["excludeNotesInSensitiveChannel"])
}

// --- Delete ----------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestDelete_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"antennaId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_AccessDenied(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "bob"}
	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingDeleteRepo causes Delete to fail.
type failingDeleteRepo struct {
	*testutil.MockAntennaRepository
}

func (r *failingDeleteRepo) Delete(_ *model.Antenna) error { return errors.New("boom") }

func TestDelete_RepoError(t *testing.T) {
	mock := testutil.NewMockAntennaRepository()
	mock.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	repo := &failingDeleteRepo{MockAntennaRepository: mock}
	svc := coreantenna.NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	h := NewHandler(svc, testutil.NewMockNoteRepository(), idGen)
	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- List ------------------------------------------------------------------

func TestList_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha")

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	shapetest.Assert(t, "Antenna", resp[0]) // L3 (#1318)
}

// listFailRepo causes ListByUser to fail.
type listFailRepo struct {
	*testutil.MockAntennaRepository
}

func (r *listFailRepo) ListByUser(_ string) ([]*model.Antenna, error) {
	return nil, errors.New("boom")
}

func TestList_RepoError(t *testing.T) {
	repo := &listFailRepo{MockAntennaRepository: testutil.NewMockAntennaRepository()}
	svc := coreantenna.NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	h := NewHandler(svc, testutil.NewMockNoteRepository(), idGen)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Notes -----------------------------------------------------------------

func TestNotes_Success(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice"}

	// Push a note id directly into the antenna timeline so Notes returns it.
	seedAntennaNote(t, "antennaTimeline:a1", "n1")

	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "n1")
}

func TestNotes_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotes_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"antennaId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotes_AccessDenied(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "bob"}
	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotes_RedisError(t *testing.T) {
	repo := testutil.NewMockAntennaRepository()
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	closed := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = closed.Close()
	svc := coreantenna.NewService(repo, testutil.NewMockUserRepository(), closed, idGen)
	h := NewHandler(svc, testutil.NewMockNoteRepository(), idGen)
	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// failingNoteRepo causes FindManyByIDsWithUser to fail.
type failingNoteRepo struct {
	*testutil.MockNoteRepository
}

func (r *failingNoteRepo) FindManyByIDsWithUser(_ []string) ([]*model.Note, error) {
	return nil, errors.New("boom")
}

// #693: handler が untilId / sinceId を service 層に渡し、結果として
// stream の paging が効くこと。strictly older / newer な区切りで挙動が変わる
// ことを e2e (redis 経由) で確認する。
func TestNotes_PagingUntilId(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	// stream ID 衝突を避けるため antenna ID を test 専用に分ける。
	repo.Antennas["a693u"] = &model.Antenna{ID: "a693u", UserID: "alice"}

	t1 := time.Now()
	idA := idGen.Generate(t1)
	idB := idGen.Generate(t1.Add(time.Millisecond))
	idC := idGen.Generate(t1.Add(2 * time.Millisecond))
	entries := []struct {
		noteID string
		ms     int64
	}{
		{idA, t1.UnixMilli()},
		{idB, t1.Add(time.Millisecond).UnixMilli()},
		{idC, t1.Add(2 * time.Millisecond).UnixMilli()},
	}
	for _, e := range entries {
		seedAntennaNote(t, "antennaTimeline:a693u", e.noteID)
		noteRepo.Notes[e.noteID] = &model.Note{ID: e.noteID, UserID: "alice"}
	}

	// untilId = idC → strictly older
	body := fmt.Sprintf(`{"antennaId":"a693u","untilId":%q}`, idC)
	c, rec := newReq(t, body)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), idA)
	assert.Contains(t, rec.Body.String(), idB)
	assert.NotContains(t, rec.Body.String(), idC, "untilId 自身は含まれてはいけない")
}

func TestNotes_NoteRepoError(t *testing.T) {
	repo := testutil.NewMockAntennaRepository()
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	svc := coreantenna.NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	noteRepo := &failingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	h := NewHandler(svc, noteRepo, idGen)
	c, rec := newReq(t, `{"antennaId":"a1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// #1464: queryService 配線時、stream に残留した followers note は handler の
// FilterVisible で落ちる (defense-in-depth)。push 段で gate 済の前提だが、
// 過去 entry 用フォールバックが動くことを confirm する。
func TestNotes_FollowersNote_FilteredByQueryService(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a-vis"] = &model.Antenna{ID: "a-vis", UserID: "alice"}

	// followingRepo は空 = alice は author を follow していない。
	followingRepo := testutil.NewMockFollowingRepository()
	h.SetQueryService(corenote.NewQueryService(noteRepo, followingRepo))

	// stream には残留した「alice にとって不可視な followers note」が積まれている。
	noteRepo.Notes["n-leak"] = &model.Note{
		ID: "n-leak", UserID: "author", Visibility: model.NoteVisibilityFollowers,
	}
	seedAntennaNote(t, "antennaTimeline:a-vis", "n-leak")

	c, rec := newReq(t, `{"antennaId":"a-vis"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "n-leak",
		"defense-in-depth filter should drop a residual followers note from a non-follower")
}

// #1467 review: stream の最新側に大量の followers/specified residual entry が
// 残っていても、handler が over-fetch して filter 後に req.Limit 件を返せること
// を assert する (ページ欠け対策)。
//
// 仕込み: stream に 15 件 push。最新 5 件は alice にとって不可視な followers note、
// 続く 10 件は public note。req.Limit (デフォルト 10) で叩くと、over-fetch せず
// に fetch 10 すると最新 5 件が filter で落ちて 5 件しか返らないが、over-fetch
// (limit*2=20) なら 15 件全部拾って followers 5 件を落として残り 10 件を返せる。
func TestNotes_OverFetchTrim_PreservesLimitAfterFilterVisible(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a-trim"] = &model.Antenna{ID: "a-trim", UserID: "alice"}

	// alice は author を follow していない (= followers note は不可視)
	followingRepo := testutil.NewMockFollowingRepository()
	h.SetQueryService(corenote.NewQueryService(noteRepo, followingRepo))

	// 古い側: public note 10 件
	publicIDs := make([]string, 10)
	for i := 0; i < 10; i++ {
		nid := fmt.Sprintf("n-pub-%02d", i)
		publicIDs[i] = nid
		noteRepo.Notes[nid] = &model.Note{
			ID: nid, UserID: "author", Visibility: model.NoteVisibilityPublic,
		}
		seedAntennaNote(t, "antennaTimeline:a-trim", nid)
	}
	// 新しい側: followers note 5 件 (ms = 200..204) — filter で全て落ちる
	for i := 0; i < 5; i++ {
		nid := fmt.Sprintf("n-fol-%02d", i)
		noteRepo.Notes[nid] = &model.Note{
			ID: nid, UserID: "author", Visibility: model.NoteVisibilityFollowers,
		}
		seedAntennaNote(t, "antennaTimeline:a-trim", nid)
	}

	c, rec := newReq(t, `{"antennaId":"a-trim","limit":10}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// over-fetch trim 後、newest-first で public 10 件が返るはず。
	assert.Len(t, resp, 10, "handler should return req.Limit notes by over-fetching past the residual followers entries")
	// followers note は 1 件も含まれない (defense-in-depth filter)
	body := rec.Body.String()
	for i := 0; i < 5; i++ {
		assert.NotContains(t, body, fmt.Sprintf("n-fol-%02d", i),
			"residual followers note should be filtered out")
	}
}

// #1467 review: queryService 未配線時は filter skip = 旧挙動互換 で req.Limit を
// そのまま渡す (over-fetch しても trim 後の件数は req.Limit に揃う)。
func TestNotes_OverFetchTrim_QueryServiceUnwired_DefaultLimit(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a-default"] = &model.Antenna{ID: "a-default", UserID: "alice"}

	// 30 件 push して default limit (10) で叩く → trim 後 10 件
	for i := 0; i < 30; i++ {
		nid := fmt.Sprintf("n-%02d", i)
		noteRepo.Notes[nid] = &model.Note{
			ID: nid, UserID: "alice", Visibility: model.NoteVisibilityPublic,
		}
		seedAntennaNote(t, "antennaTimeline:a-default", nid)
	}

	c, rec := newReq(t, `{"antennaId":"a-default"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 10, "over-fetch should trim back to default limit (10) when no filter drops apply")
}

// #1544: viewer が mute した user / viewer を block した user の note と、
// viewer が mute した channel の note が antennas/notes から除外されること。
func TestNotes_MuteBlockChannelFiltered(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a-mb"] = &model.Antenna{ID: "a-mb", UserID: "alice"}

	muting := testutil.NewMockMutingRepository()
	require.NoError(t, muting.Create(&model.Muting{ID: "m1", MuterID: "alice", MuteeID: "muted"}))
	blocking := testutil.NewMockBlockingRepository()
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "alice"}))
	channelMuting := testutil.NewMockChannelMutingRepository()
	require.NoError(t, channelMuting.Create(&model.ChannelMuting{UserID: "alice", ChannelID: "ch-muted"}))
	h.SetMuteBlockRepos(muting, blocking, channelMuting)

	mutedCh := "ch-muted"
	notes := map[string]*model.Note{
		"keep":          {ID: "keep", UserID: "author", Visibility: model.NoteVisibilityPublic},
		"by-muted":      {ID: "by-muted", UserID: "muted", Visibility: model.NoteVisibilityPublic},
		"by-blocker":    {ID: "by-blocker", UserID: "blocker", Visibility: model.NoteVisibilityPublic},
		"muted-channel": {ID: "muted-channel", UserID: "author", Visibility: model.NoteVisibilityPublic, ChannelID: &mutedCh},
	}
	order := []string{"keep", "by-muted", "by-blocker", "muted-channel"}
	for _, nid := range order {
		noteRepo.Notes[nid] = notes[nid]
		seedAntennaNote(t, "antennaTimeline:a-mb", nid)
	}

	c, rec := newReq(t, `{"antennaId":"a-mb"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "keep")
	assert.NotContains(t, body, "by-muted", "note authored by a muted user must be dropped")
	assert.NotContains(t, body, "by-blocker", "note authored by a user who blocked the viewer must be dropped")
	assert.NotContains(t, body, "muted-channel", "note in a muted channel must be dropped")
}

// #1544 fail-closed: mute/block/channel set のロードでリポジトリエラーが出たら
// silently note を漏らさず 500 を返すこと。
func TestNotes_MuteBlockLoadError(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a-err"] = &model.Antenna{ID: "a-err", UserID: "alice"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityPublic}
	seedAntennaNote(t, "antennaTimeline:a-err", "n1")

	blocking := testutil.NewMockBlockingRepository()
	blocking.ExistsErr = errors.New("block boom") // ListBlockerIDs もこのエラーで失敗する
	h.SetMuteBlockRepos(testutil.NewMockMutingRepository(), blocking, testutil.NewMockChannelMutingRepository())

	c, rec := newReq(t, `{"antennaId":"a-err"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// #1464: queryService 配線時、follow 関係があれば followers note は引き続き
// 表示される (gate が過剰に絞っていないことの guard)。
func TestNotes_FollowersNote_VisibleToFollower(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a-ok"] = &model.Antenna{ID: "a-ok", UserID: "alice"}

	followingRepo := testutil.NewMockFollowingRepository()
	require.NoError(t, followingRepo.Create(&model.Following{
		ID: "f1", FollowerID: "alice", FolloweeID: "author",
	}))
	h.SetQueryService(corenote.NewQueryService(noteRepo, followingRepo))

	noteRepo.Notes["n-ok"] = &model.Note{
		ID: "n-ok", UserID: "author", Visibility: model.NoteVisibilityFollowers,
	}
	seedAntennaNote(t, "antennaTimeline:a-ok", "n-ok")

	c, rec := newReq(t, `{"antennaId":"a-ok"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "n-ok")
}

// #2069 (upstream #17463): antennas/remove-note の handler。
func TestRemoveNote_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	c, rec := newReq(t, `{"antennaId":"a1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code, "存在しないノート除去も 204 (no-op)")
}

func TestRemoveNote_NoSuchAntenna(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "bob"} // 他人所有
	// not-owner → NO_SUCH_ANTENNA。
	c, rec := newReq(t, `{"antennaId":"a1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_ANTENNA")
	assert.Contains(t, rec.Body.String(), "850926e0-fd3b-49b6-b69a-b28a5dbd82fe")
	// 未存在 antenna も NO_SUCH_ANTENNA。
	c2, rec2 := newReq(t, `{"antennaId":"nope","noteId":"n1"}`)
	setUser(c2, "alice")
	require.NoError(t, h.RemoveNote(c2))
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
}

func TestRemoveNote_BadParam(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"antennaId":"a1"}`) // noteId 欠落
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #2106 N5: antennas/notes は admin の blockedHosts でブロックした remote host の note と
// suspended author の note を除外する (upstream generateBaseNoteFilteringQuery 相当)。
func TestNotes_FiltersBlockedHostAndSuspended(t *testing.T) {
	h, repo, noteRepo := newHandler(t)
	repo.Antennas["a-n5"] = &model.Antenna{ID: "a-n5", UserID: "alice"}

	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"blocked.example"}}
	h.SetMetaRepo(meta)

	host := "blocked.example"
	noteRepo.Notes["n-blocked"] = &model.Note{ID: "n-blocked", UserID: "remote1", UserHost: &host}
	noteRepo.Notes["n-suspended"] = &model.Note{ID: "n-suspended", UserID: "susp", User: &model.User{ID: "susp", IsSuspended: true}}
	noteRepo.Notes["n-ok"] = &model.Note{ID: "n-ok", UserID: "alice"}

	for _, id := range []string{"n-blocked", "n-suspended", "n-ok"} {
		seedAntennaNote(t, "antennaTimeline:a-n5", id)
	}

	c, rec := newReq(t, `{"antennaId":"a-n5"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "n-blocked", "blockedHosts の note を除外")
	assert.NotContains(t, body, "n-suspended", "suspended author の note を除外")
	assert.Contains(t, body, "n-ok", "通常 note は残る")
}

// upstream paramDef の required
// (`['name','src','keywords','excludeKeywords','users','caseSensitive','withReplies','withFile']`)
// を欠く payload は 400 INVALID_PARAM で弾く (#2284)。mk-go は name しか
// 見ておらず、TS backend では 400 になる payload を 200 で受けていた。
func TestCreate_MissingRequiredParams(t *testing.T) {
	full := map[string]string{
		"name":            `"alpha"`,
		"src":             `"all"`,
		"keywords":        `[["foo"]]`,
		"excludeKeywords": `[]`,
		"users":           `[]`,
		"caseSensitive":   `false`,
		"withReplies":     `false`,
		"withFile":        `false`,
	}
	build := func(omit string) string {
		parts := make([]string, 0, len(full))
		for k, v := range full {
			if k == omit {
				continue
			}
			parts = append(parts, `"`+k+`":`+v)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}

	// 完全な payload は 200。
	c, rec := newReq(t, build(""))
	setUser(c, "alice")
	h, _, _ := newHandler(t)
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code, "全 required が揃っていれば通る")

	for k := range full {
		t.Run("omit_"+k, func(t *testing.T) {
			h, _, _ := newHandler(t)
			c, rec := newReq(t, build(k))
			setUser(c, "alice")
			require.NoError(t, h.Create(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s を欠くと 400", k)
		})
	}
}

// seedAntennaNote pushes a note id into the antenna timeline the way the
// service does (#2465 で Stream から ZSET へ変更)。
//
// テストが Redis の構造を直接組み立てているので、実装と同じ形にしておかないと
// 「テストは通るが本番は動かない」になる。score は実装と同じく 0 で揃える
// (ZRANGEBYLEX は score が揃っている前提)。
func seedAntennaNote(t *testing.T, key, noteID string) {
	t.Helper()
	require.NoError(t, testRedis.Client.ZAdd(context.Background(), key, redis.Z{Score: 0, Member: noteID}).Err())
}
