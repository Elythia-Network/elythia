package antenna

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
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

func newSvc(t *testing.T) (*Service, *testutil.MockAntennaRepository) {
	t.Helper()
	testRedis.FlushAll(context.Background())
	repo := testutil.NewMockAntennaRepository()
	return NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen), repo
}

func closedClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = c.Close()
	return c
}

// --- Create ----------------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	svc, repo := newSvc(t)
	a, err := svc.Create(CreateInput{
		OwnerID:  "u1",
		Name:     "alpha",
		Src:      model.AntennaSourceAll,
		Keywords: [][]string{{"misskey"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "alpha", a.Name)
	assert.Equal(t, model.AntennaSourceAll, a.Src)
	assert.Len(t, repo.Antennas, 1)
}

func TestCreate_NameRequired(t *testing.T) {
	svc, _ := newSvc(t)
	_, err := svc.Create(CreateInput{OwnerID: "u1", Src: model.AntennaSourceAll})
	assert.ErrorIs(t, err, ErrAntennaNameRequired)
}

func TestCreate_OwnerRequired(t *testing.T) {
	svc, _ := newSvc(t)
	_, err := svc.Create(CreateInput{Name: "alpha", Src: model.AntennaSourceAll})
	assert.Error(t, err)
}

func TestCreate_InvalidSource(t *testing.T) {
	svc, _ := newSvc(t)
	_, err := svc.Create(CreateInput{OwnerID: "u1", Name: "alpha", Src: "bogus"})
	assert.ErrorIs(t, err, ErrInvalidSource)
}

func TestCreate_ListSourceAccepted(t *testing.T) {
	// #170 以降 list ソースも正規値として扱う (validSource に追加済み)。
	svc, _ := newSvc(t)
	listID := "ul1"
	_, err := svc.Create(CreateInput{OwnerID: "u1", Name: "alpha", Src: model.AntennaSourceList, UserListID: &listID})
	require.NoError(t, err)
}

func TestAllKeywordsEmpty(t *testing.T) {
	assert.True(t, AllKeywordsEmpty(nil))
	assert.True(t, AllKeywordsEmpty([][]string{}))
	assert.True(t, AllKeywordsEmpty([][]string{{""}, {"", ""}}))
	assert.False(t, AllKeywordsEmpty([][]string{{""}, {"foo"}}))
	assert.False(t, AllKeywordsEmpty([][]string{{"bar"}}))
}

// src=list で他人所有 / 不在の list を参照すると ErrNoSuchUserList。
func TestCreate_ListSource_NoSuchUserList(t *testing.T) {
	svc, _ := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)
	require.NoError(t, lists.Create(&model.UserList{ID: "ul1", UserID: "other", Name: "theirs"}))

	// 他人所有の list。
	notMine := "ul1"
	_, err := svc.Create(CreateInput{OwnerID: "u1", Name: "alpha", Src: model.AntennaSourceList, UserListID: &notMine})
	assert.ErrorIs(t, err, ErrNoSuchUserList)

	// 存在しない list。
	ghost := "ghost"
	_, err = svc.Create(CreateInput{OwnerID: "u1", Name: "alpha", Src: model.AntennaSourceList, UserListID: &ghost})
	assert.ErrorIs(t, err, ErrNoSuchUserList)
}

func TestCreate_RepoError(t *testing.T) {
	svc, repo := newSvc(t)
	repo.CreateErr = errors.New("boom")
	_, err := svc.Create(CreateInput{OwnerID: "u1", Name: "alpha", Src: model.AntennaSourceAll})
	assert.Error(t, err)
}

// --- Show ------------------------------------------------------------------

func TestShow_HappyPath(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", Name: "alpha"}
	got, err := svc.Show("u1", "a1")
	require.NoError(t, err)
	assert.Equal(t, "alpha", got.Name)
}

func TestShow_NotFound(t *testing.T) {
	svc, _ := newSvc(t)
	_, err := svc.Show("u1", "missing")
	assert.ErrorIs(t, err, ErrAntennaNotFound)
}

func TestShow_AccessDenied(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	_, err := svc.Show("u2", "a1")
	assert.ErrorIs(t, err, ErrAccessDenied)
}

func TestShow_DoesNotBumpLastUsedAt(t *testing.T) {
	svc, repo := newSvc(t)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", LastUsedAt: old, IsActive: false}
	// 本家 show.ts は lastUsedAt を bump しない (#1604)。
	_, err := svc.Show("u1", "a1")
	require.NoError(t, err)
	assert.Equal(t, old, repo.Antennas["a1"].LastUsedAt, "Show は lastUsedAt を変えない")
	assert.False(t, repo.Antennas["a1"].IsActive, "Show は再活性化しない")
}

// --- Update ----------------------------------------------------------------

func TestUpdate_HappyPath(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{
		ID: "a1", UserID: "u1", Name: "alpha", Src: model.AntennaSourceAll,
		Keywords: datatypes.JSON([]byte("[]")), ExcludeKeywords: datatypes.JSON([]byte("[]")),
	}
	newName := "alpha-v2"
	src := model.AntennaSourceUsers
	users := []string{"alice"}
	keywords := [][]string{{"foo"}}
	exclude := [][]string{{"bar"}}
	caseS := true
	exB := true
	wR := true
	wF := true
	lo := true
	active := false
	got, err := svc.Update("u1", "a1", UpdateInput{
		Name:            &newName,
		Src:             &src,
		Users:           &users,
		Keywords:        &keywords,
		ExcludeKeywords: &exclude,
		CaseSensitive:   &caseS,
		ExcludeBots:     &exB,
		WithReplies:     &wR,
		WithFile:        &wF,
		LocalOnly:       &lo,
		IsActive:        &active,
	})
	require.NoError(t, err)
	assert.Equal(t, "alpha-v2", got.Name)
}

func TestUpdate_BumpsLastUsedAt(t *testing.T) {
	svc, repo := newSvc(t)
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	repo.Antennas["a1"] = &model.Antenna{
		ID: "a1", UserID: "u1", Name: "alpha", Src: model.AntennaSourceAll, LastUsedAt: old,
		Keywords: datatypes.JSON([]byte("[]")), ExcludeKeywords: datatypes.JSON([]byte("[]")),
	}
	name := "alpha-v2"
	repo.Antennas["a1"].IsActive = false
	// 本家 update.ts と同じく更新で lastUsedAt が now に bump され、isActive を明示
	// 指定しない編集では isActive=true へ復帰する (#1604)。
	_, err := svc.Update("u1", "a1", UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, fixed, repo.Antennas["a1"].LastUsedAt)
	assert.True(t, repo.Antennas["a1"].IsActive, "isActive 未指定の編集は再活性化")
}

func TestUpdate_RespectsExplicitIsActiveFalse(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{
		ID: "a1", UserID: "u1", Name: "alpha", Src: model.AntennaSourceAll, IsActive: true,
		Keywords: datatypes.JSON([]byte("[]")), ExcludeKeywords: datatypes.JSON([]byte("[]")),
	}
	off := false
	// 明示的な isActive=false は尊重する (mk-go の isActive param 拡張)。
	_, err := svc.Update("u1", "a1", UpdateInput{IsActive: &off})
	require.NoError(t, err)
	assert.False(t, repo.Antennas["a1"].IsActive, "明示 isActive=false は尊重")
}

func TestUpdate_NotFound(t *testing.T) {
	svc, _ := newSvc(t)
	_, err := svc.Update("u1", "missing", UpdateInput{})
	assert.ErrorIs(t, err, ErrAntennaNotFound)
}

func TestUpdate_AccessDenied(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "other"}
	_, err := svc.Update("u1", "a1", UpdateInput{})
	assert.ErrorIs(t, err, ErrAccessDenied)
}

func TestUpdate_NameEmpty(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	empty := ""
	_, err := svc.Update("u1", "a1", UpdateInput{Name: &empty})
	assert.ErrorIs(t, err, ErrAntennaNameRequired)
}

func TestUpdate_InvalidSource(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	bogus := model.AntennaSource("bogus")
	_, err := svc.Update("u1", "a1", UpdateInput{Src: &bogus})
	assert.ErrorIs(t, err, ErrInvalidSource)
}

// Update で empty Users を渡しても、UpdateFields に model.StringArray
// として渡されることを担保する (#896 と同 pattern)。plain []string で
// 渡すと production の GORM 経由で NULL 化して NOT NULL 制約違反になる。
func TestUpdate_EmptyUsersWrappedAsStringArray(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	emptyUsers := []string{}
	_, err := svc.Update("u1", "a1", UpdateInput{Users: &emptyUsers})
	require.NoError(t, err)
	require.Contains(t, repo.LastUpdates, "users")
	v, ok := repo.LastUpdates["users"].(model.StringArray)
	require.True(t, ok, "users should be model.StringArray, got %T", repo.LastUpdates["users"])
	assert.Empty(t, v)
}

func TestUpdate_RepoError(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	repo.UpdateErr = errors.New("boom")
	name := "x"
	_, err := svc.Update("u1", "a1", UpdateInput{Name: &name})
	assert.Error(t, err)
}

// --- Delete ----------------------------------------------------------------

func TestDelete_HappyPath(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	require.NoError(t, svc.Delete("u1", "a1"))
	assert.Empty(t, repo.Antennas)
}

func TestDelete_NotFound(t *testing.T) {
	svc, _ := newSvc(t)
	err := svc.Delete("u1", "missing")
	assert.ErrorIs(t, err, ErrAntennaNotFound)
}

func TestDelete_AccessDenied(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "other"}
	err := svc.Delete("u1", "a1")
	assert.ErrorIs(t, err, ErrAccessDenied)
}

// failingDeleteRepo causes Delete to fail.
type failingDeleteRepo struct {
	*testutil.MockAntennaRepository
}

func (r *failingDeleteRepo) Delete(_ *model.Antenna) error { return errors.New("boom") }

func TestDelete_RepoError(t *testing.T) {
	mock := testutil.NewMockAntennaRepository()
	mock.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	repo := &failingDeleteRepo{MockAntennaRepository: mock}
	svc := NewService(repo, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	err := svc.Delete("u1", "a1")
	assert.Error(t, err)
}

// --- ListByUser ------------------------------------------------------------

func TestListByUser(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	repo.Antennas["a2"] = &model.Antenna{ID: "a2", UserID: "u1"}
	rows, err := svc.ListByUser("u1")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

// --- Notes -----------------------------------------------------------------

func TestNotes_HappyPath(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	require.NoError(t, svc.pushNote(context.Background(), "a1", "n1", time.Now()))
	require.NoError(t, svc.pushNote(context.Background(), "a1", "n2", time.Now().Add(time.Millisecond)))
	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestNotes_BumpsLastUsedAtAndReactivates(t *testing.T) {
	svc, repo := newSvc(t)
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	// 非アクティブ + 古い lastUsedAt の antenna を notes 取得すると、本家
	// antennas/notes.ts と同じく isActive=true + lastUsedAt=now に bump される (#1604)。
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", IsActive: false, LastUsedAt: old}
	_, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.True(t, repo.Antennas["a1"].IsActive, "notes 取得で再活性化")
	assert.Equal(t, fixed, repo.Antennas["a1"].LastUsedAt, "notes 取得で lastUsedAt が now に bump")
}

// #2752: **すでにアクティブな antenna では `isActive` を書かない。**
//
// 書き込みは CachedAntennaRepository の invalidate を撃つので、無条件に書くと
// antenna タイムラインを開くたびに全 worker の cache が落ちる (= cache が最も
// 要る構成でこそ効かない)。upstream も `needPublishEvent = !antenna.isActive` の
// 条件付きでしか antennaUpdated を publish しない (antennas/notes.ts:90,96-98)。
//
// **lastUsedAt は毎回書く。** 書かないと #1604 の clean cron が使用中の antenna
// を deactivate する。
func TestNotes_ActiveAntennaBumpsOnlyLastUsedAt(t *testing.T) {
	svc, repo := newSvc(t)
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", IsActive: true, LastUsedAt: old}

	_, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)

	require.NotNil(t, repo.LastUpdates, "lastUsedAt は毎回 bump する (clean cron 対策)")
	assert.Equal(t, fixed, repo.Antennas["a1"].LastUsedAt)
	_, hasIsActive := repo.LastUpdates["isActive"]
	assert.False(t, hasIsActive, "既にアクティブなら isActive は書かない (cache を落とさない)")
	assert.Len(t, repo.LastUpdates, 1, "書くのは lastUsedAt だけ")
}

// 非アクティブだったときは isActive も書く (= cache を落とす経路)。
func TestNotes_InactiveAntennaAlsoWritesIsActive(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", IsActive: false}

	_, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)

	require.NotNil(t, repo.LastUpdates)
	assert.Equal(t, true, repo.LastUpdates["isActive"], "再活性化は isActive を書いて cache を落とす")
}

func TestNotes_LimitClamping(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	rows, err := svc.Notes(context.Background(), "u1", "a1", -1, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows)
	rows, err = svc.Notes(context.Background(), "u1", "a1", 9999, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestNotes_NotFound(t *testing.T) {
	svc, _ := newSvc(t)
	_, err := svc.Notes(context.Background(), "u1", "missing", 10, "", "")
	assert.ErrorIs(t, err, ErrAntennaNotFound)
}

func TestNotes_RedisError(t *testing.T) {
	repo := testutil.NewMockAntennaRepository()
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	svc := NewService(repo, testutil.NewMockUserRepository(), closedClient(t), idGen)
	_, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	assert.Error(t, err)
}

// #693: untilID で渡した noteID より strictly old なエントリだけ返ること。
// FE の無限スクロールが「次のページ」を取りに来た時にこれが効かないと、
// 同じ最新 N 件を毎回返してしまう。
func TestNotes_PagingUntilID(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	// 3 件: 古→新の順 (idGen.Generate は時間 monotonic)
	t1 := time.Now()
	id1 := idGen.Generate(t1)
	id2 := idGen.Generate(t1.Add(time.Millisecond))
	id3 := idGen.Generate(t1.Add(2 * time.Millisecond))
	require.NoError(t, svc.pushNote(context.Background(), "a1", id1, t1))
	require.NoError(t, svc.pushNote(context.Background(), "a1", id2, t1.Add(time.Millisecond)))
	require.NoError(t, svc.pushNote(context.Background(), "a1", id3, t1.Add(2*time.Millisecond)))

	// untilID = id3 → strictly older だけ → id2, id1 (newest first)
	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", id3)
	require.NoError(t, err)
	assert.Equal(t, []string{id2, id1}, rows)

	// untilID = id1 (= 一番古い) → さらに古いものは無いので空
	rows, err = svc.Notes(context.Background(), "u1", "a1", 10, "", id1)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// #693: sinceID で渡した noteID より strictly new なエントリだけ返ること。
func TestNotes_PagingSinceID(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	t1 := time.Now()
	id1 := idGen.Generate(t1)
	id2 := idGen.Generate(t1.Add(time.Millisecond))
	id3 := idGen.Generate(t1.Add(2 * time.Millisecond))
	require.NoError(t, svc.pushNote(context.Background(), "a1", id1, t1))
	require.NoError(t, svc.pushNote(context.Background(), "a1", id2, t1.Add(time.Millisecond)))
	require.NoError(t, svc.pushNote(context.Background(), "a1", id3, t1.Add(2*time.Millisecond)))

	// #1778: sinceID 単独 (fetch-newer) は昇順 (oldest-first) で返す
	// (upstream notes.ts の sinceId-only ascending sort)。strictly newer の
	// 最古から id2, id3 の順。
	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, id1, "")
	require.NoError(t, err)
	assert.Equal(t, []string{id2, id3}, rows)

	// limit を絞ると sinceID 直後の最古 limit 件 (id2) を返す (newest ではなく)。
	rows, err = svc.Notes(context.Background(), "u1", "a1", 1, id1, "")
	require.NoError(t, err)
	assert.Equal(t, []string{id2}, rows)
}

// #693 review #1: 同一 ms に同じアンテナへ複数回 pushNote しても XADD が
// 失敗せず、両方が stream に格納されて一覧取得できる (旧 `<ms>-0` 固定実装は
// monotonic 違反で 2 件目を drop していた)。
func TestPushNote_SameMsDoesNotCollide(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	now := time.Now()
	require.NoError(t, svc.pushNote(context.Background(), "a1", "n1", now))
	require.NoError(t, svc.pushNote(context.Background(), "a1", "n2", now), "同 ms でも 2 件目が成功すること")
	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n1", "n2"}, rows)
}

// #693: 不正な ID (ParseTime 失敗) は無視されて全範囲が返る (安全側 fallback)。
func TestNotes_PagingInvalidID(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	require.NoError(t, svc.pushNote(context.Background(), "a1", "n1", time.Now()))
	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "not-an-id")
	require.NoError(t, err)
	assert.Len(t, rows, 1, "ParseTime 失敗時は全 range にフォールバックして既存挙動を維持")
}

func TestNotes_SkipsBadValue(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	// noteId フィールドが無いエントリを混ぜる
	require.NoError(t, testRedis.Client.XAdd(context.Background(), &redis.XAddArgs{
		Stream: streamKey("a1"),
		ID:     "1-0",
		Values: map[string]any{"other": "x"},
	}).Err())
	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// --- OnNoteCreated + matchNote --------------------------------------------

func makeAntenna(t *testing.T, id, userID string, kw [][]string, mods ...func(*model.Antenna)) *model.Antenna {
	t.Helper()
	keywordsJSON := []byte("[]")
	if len(kw) > 0 {
		raw, err := json.Marshal(kw)
		require.NoError(t, err)
		keywordsJSON = raw
	}
	a := &model.Antenna{
		ID:              id,
		UserID:          userID,
		Name:            id,
		Src:             model.AntennaSourceAll,
		Keywords:        datatypes.JSON(keywordsJSON),
		ExcludeKeywords: datatypes.JSON([]byte("[]")),
		IsActive:        true,
	}
	for _, m := range mods {
		m(a)
	}
	return a
}

func TestOnNoteCreated_HappyPath(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"misskey"}})

	text := "hello misskey world"
	n := &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "author", Username: "alice"}
	svc.OnNoteCreated(n, author)

	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, rows)
}

func TestOnNoteCreated_InsertsUnreadRow(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "owner", [][]string{{"hit"}})
	unread := testutil.NewMockAntennaNoteUnreadRepository()
	svc.SetUnreadRepo(unread)

	text := "hit this"
	svc.OnNoteCreated(
		&model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "author", Username: "a"},
	)
	// 1 row should be inserted for antenna owner (not the author)
	require.Len(t, unread.Rows, 1)
	assert.Equal(t, "owner", unread.Rows[0].UserID)
	assert.Equal(t, "a1", unread.Rows[0].AntennaID)
	assert.Equal(t, "n1", unread.Rows[0].NoteID)
}

func TestOnNoteCreated_SelfAuthoredSkipsUnread(t *testing.T) {
	svc, repo := newSvc(t)
	// antenna owner == note author → unread を出さない
	repo.Antennas["a1"] = makeAntenna(t, "a1", "author", [][]string{{"hit"}})
	unread := testutil.NewMockAntennaNoteUnreadRepository()
	svc.SetUnreadRepo(unread)

	text := "hit this"
	svc.OnNoteCreated(
		&model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "author", Username: "a"},
	)
	assert.Empty(t, unread.Rows)
}

func TestOnNoteCreated_NilArgsAreNoOp(t *testing.T) {
	svc, _ := newSvc(t)
	svc.OnNoteCreated(nil, &model.User{})
	svc.OnNoteCreated(&model.Note{Visibility: model.NoteVisibilityPublic}, nil)
}

func TestOnNoteCreated_RepoErrorIsNoOp(t *testing.T) {
	repo := testutil.NewMockAntennaRepository()
	svc := NewService(&listFailRepo{repo}, testutil.NewMockUserRepository(), testRedis.Client, idGen)
	text := "hi"
	svc.OnNoteCreated(&model.Note{ID: "n1", Text: &text, Visibility: model.NoteVisibilityPublic}, &model.User{ID: "u1"})
}

// listFailRepo causes ListAllActive to fail.
type listFailRepo struct {
	*testutil.MockAntennaRepository
}

func (r *listFailRepo) ListAllActive() ([]*model.Antenna, error) {
	return nil, errors.New("boom")
}

func TestOnNoteCreated_NoMatchSkipped(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"missing"}})

	text := "hello world"
	svc.OnNoteCreated(
		&model.Note{ID: "n1", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "author", Username: "alice"},
	)

	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// --- matchNote each filter ------------------------------------------------

func TestMatchNote_LocalOnlyRejectsRemote(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.LocalOnly = true
	})
	host := "remote.example"
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{Host: &host}, nil))
}

func TestMatchNote_ExcludeBotsRejectsBot(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.ExcludeBots = true
	})
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{IsBot: true}, nil))
}

func TestMatchNote_WithFileRequiresAttachment(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.WithFile = true
	})
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{FileIDs: []string{"f1"}, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_WithRepliesFalseRejectsReplies(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil)
	parent := "p1"
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{ReplyID: &parent, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_WithRepliesTrueAllowsReplies(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.WithReplies = true
	})
	parent := "p1"
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{ReplyID: &parent, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_UsersSourceWhitelist(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.Src = model.AntennaSourceUsers
		a.Users = []string{"alice"}
	})
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{Username: "alice"}, nil))
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{Username: "bob"}, nil))
}

func TestMatchNote_UsersBlacklist(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.Src = model.AntennaSourceUsersBlacklist
		a.Users = []string{"alice"}
	})
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{Username: "alice"}, nil))
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{Username: "bob"}, nil))
}

func TestMatchNote_KeywordsCaseSensitiveMiss(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"Misskey"}}, func(a *model.Antenna) {
		a.CaseSensitive = true
	})
	text := "this is misskey"
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &text, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_KeywordsCaseSensitiveHit(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"Misskey"}}, func(a *model.Antenna) {
		a.CaseSensitive = true
	})
	text := "this is Misskey"
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &text, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_KeywordsAndOr(t *testing.T) {
	svc, repo := newSvc(t)
	// (foo AND bar) OR baz
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"foo", "bar"}, {"baz"}})
	hit1 := "foo bar"
	hit2 := "baz only"
	miss := "foo only"
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &hit1, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &hit2, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &miss, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_ExcludeKeywords(t *testing.T) {
	svc, repo := newSvc(t)
	exclude := []byte(`[["spam"]]`)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.ExcludeKeywords = datatypes.JSON(exclude)
	})
	dirty := "this is spam"
	clean := "this is fine"
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &dirty, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &clean, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_BadKeywordsJSONTreatedAsEmpty(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.Keywords = datatypes.JSON([]byte("{not json"))
	})
	// 不正 JSON は emptyMatches=true として扱うので keyword フィルタは pass
	// (matchKeywords が true を返す)
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_NilKeywordsTreatedAsEmpty(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.Keywords = nil // empty raw → emptyMatches=true
	})
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestMatchNote_EmptyInnerGroupSkipped(t *testing.T) {
	svc, repo := newSvc(t)
	// 外側 OR の中に空 group + 実 group。空 group は skip され実 group が
	// マッチしなければ false
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", nil, func(a *model.Antenna) {
		a.Keywords = datatypes.JSON([]byte(`[[],["foo"]]`))
	})
	miss := "no match here"
	require.False(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &miss, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
	hit := "foo here"
	require.True(t, svc.matchNote(repo.Antennas["a1"], &model.Note{Text: &hit, Visibility: model.NoteVisibilityPublic}, &model.User{}, nil))
}

func TestNoteText_CWAndText(t *testing.T) {
	cw := "warning"
	text := "body"
	n := &model.Note{CW: &cw, Text: &text, Visibility: model.NoteVisibilityPublic}
	got := noteText(n)
	assert.Contains(t, got, "warning")
	assert.Contains(t, got, "body")
}

// --- SetClock --------------------------------------------------------------

func TestSetClock(t *testing.T) {
	svc, _ := newSvc(t)
	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	svc.SetClock(nil)
	a, err := svc.Create(CreateInput{OwnerID: "u1", Name: "alpha", Src: model.AntennaSourceAll})
	require.NoError(t, err)
	assert.Equal(t, fixed, a.LastUsedAt)
}

// Home source: owner follows author → match, otherwise miss.
func TestMatchNote_HomeSource_Follows(t *testing.T) {
	svc, _ := newSvc(t)
	follows := testutil.NewMockFollowingRepository()
	svc.SetFollowingRepo(follows)
	// u1 → alice をフォロー
	require.NoError(t, follows.Create(&model.Following{ID: "f1", FollowerID: "u1", FolloweeID: "alice"}))
	a, err := svc.Create(CreateInput{OwnerID: "u1", Name: "homeA", Src: model.AntennaSourceHome})
	require.NoError(t, err)
	text := "hi"
	n := &model.Note{ID: "n1", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic}
	assert.True(t, svc.matchNote(a, n, &model.User{ID: "alice", Username: "alice"}, nil))
}

func TestMatchNote_HomeSource_NotFollowing(t *testing.T) {
	svc, _ := newSvc(t)
	svc.SetFollowingRepo(testutil.NewMockFollowingRepository())
	a, err := svc.Create(CreateInput{OwnerID: "u1", Name: "homeB", Src: model.AntennaSourceHome})
	require.NoError(t, err)
	text := "hi"
	n := &model.Note{ID: "n1", UserID: "bob", Text: &text, Visibility: model.NoteVisibilityPublic}
	assert.False(t, svc.matchNote(a, n, &model.User{ID: "bob", Username: "bob"}, nil))
}

func TestMatchNote_HomeSource_NilRepoMisses(t *testing.T) {
	// followingRepo 未注入なら home source は必ず miss (設定ミスを気付かせる)
	svc, _ := newSvc(t)
	a, err := svc.Create(CreateInput{OwnerID: "u1", Name: "homeC", Src: model.AntennaSourceHome})
	require.NoError(t, err)
	text := "hi"
	n := &model.Note{ID: "n1", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic}
	assert.False(t, svc.matchNote(a, n, &model.User{ID: "alice", Username: "alice"}, nil))
}

// List source: author が UserList に含まれていれば match。
func TestMatchNote_ListSource_MemberMatches(t *testing.T) {
	svc, _ := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)
	listID := "ul1"
	require.NoError(t, lists.Create(&model.UserList{ID: listID, UserID: "u1", Name: "friends"}))
	require.NoError(t, lists.AddMember(&model.UserListMembership{UserListID: listID, UserID: "alice"}))
	a, err := svc.Create(CreateInput{OwnerID: "u1", Name: "listA", Src: model.AntennaSourceList, UserListID: &listID})
	require.NoError(t, err)
	text := "hi"
	n := &model.Note{ID: "n1", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic}
	assert.True(t, svc.matchNote(a, n, &model.User{ID: "alice", Username: "alice"}, nil))
}

func TestMatchNote_ListSource_NonMember(t *testing.T) {
	svc, _ := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)
	listID := "ul1"
	require.NoError(t, lists.Create(&model.UserList{ID: listID, UserID: "u1", Name: "friends"}))
	a, err := svc.Create(CreateInput{OwnerID: "u1", Name: "listB", Src: model.AntennaSourceList, UserListID: &listID})
	require.NoError(t, err)
	text := "hi"
	n := &model.Note{ID: "n1", UserID: "bob", Text: &text, Visibility: model.NoteVisibilityPublic}
	assert.False(t, svc.matchNote(a, n, &model.User{ID: "bob", Username: "bob"}, nil))
}

// --- List source: end-to-end through OnNoteCreated → Notes -----------------

// リストメンバーのノートだけがアンテナに届き、非メンバーのノートは除外される
// ことを OnNoteCreated → Notes の完全フローで検証する。
func TestOnNoteCreated_ListSource_OnlyMemberNotesAppear(t *testing.T) {
	svc, repo := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)

	// pushNoteはnow.UnixMilliからRedisストリームIDを生成するため、同一msで
	// 2件以上ENTRYするとXADDが重複IDで失敗する。テスト決定性のため単調増加の
	// 疑似クロックを注入する。
	start := time.Unix(1_700_000_000, 0)
	var tick int64
	svc.SetClock(func() time.Time {
		tick++
		return start.Add(time.Duration(tick) * time.Millisecond)
	})

	// ユーザーリストを作成し、alice と carol をメンバーに追加
	listID := "ul1"
	require.NoError(t, lists.Create(&model.UserList{ID: listID, UserID: "owner", Name: "favorites"}))
	require.NoError(t, lists.AddMember(&model.UserListMembership{UserListID: listID, UserID: "alice"}))
	require.NoError(t, lists.AddMember(&model.UserListMembership{UserListID: listID, UserID: "carol"}))

	// list ソースのアンテナを作成
	a := makeAntenna(t, "ant1", "owner", nil, func(a *model.Antenna) {
		a.Src = model.AntennaSourceList
		a.UserListID = &listID
	})
	repo.Antennas["ant1"] = a

	// リストメンバー alice のノート → アンテナに届くはず
	text1 := "hello from alice"
	n1 := &model.Note{ID: "n1", UserID: "alice", Text: &text1, Visibility: model.NoteVisibilityPublic}
	svc.OnNoteCreated(n1, &model.User{ID: "alice", Username: "alice"})

	// 非メンバー bob のノート → アンテナに届かないはず
	text2 := "hello from bob"
	n2 := &model.Note{ID: "n2", UserID: "bob", Text: &text2, Visibility: model.NoteVisibilityPublic}
	svc.OnNoteCreated(n2, &model.User{ID: "bob", Username: "bob"})

	// リストメンバー carol のノート → アンテナに届くはず
	text3 := "hello from carol"
	n3 := &model.Note{ID: "n3", UserID: "carol", Text: &text3, Visibility: model.NoteVisibilityPublic}
	svc.OnNoteCreated(n3, &model.User{ID: "carol", Username: "carol"})

	// 非メンバー dave のノート → アンテナに届かないはず
	text4 := "hello from dave"
	n4 := &model.Note{ID: "n4", UserID: "dave", Text: &text4, Visibility: model.NoteVisibilityPublic}
	svc.OnNoteCreated(n4, &model.User{ID: "dave", Username: "dave"})

	rows, err := svc.Notes(context.Background(), "owner", "ant1", 100, "", "")
	require.NoError(t, err)
	// alice と carol のノートだけが含まれ、bob と dave のノートは含まれない
	assert.Len(t, rows, 2)
	assert.Contains(t, rows, "n1")
	assert.Contains(t, rows, "n3")
	assert.NotContains(t, rows, "n2")
	assert.NotContains(t, rows, "n4")
}

// キーワードフィルタとリストソースを組み合わせた場合、リストメンバーかつ
// キーワードマッチするノートだけがアンテナに届く。
func TestOnNoteCreated_ListSource_WithKeywords(t *testing.T) {
	svc, repo := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)

	listID := "ul2"
	require.NoError(t, lists.Create(&model.UserList{ID: listID, UserID: "owner", Name: "devs"}))
	require.NoError(t, lists.AddMember(&model.UserListMembership{UserListID: listID, UserID: "alice"}))

	// list ソース + キーワード "golang" のアンテナ
	a := makeAntenna(t, "ant2", "owner", [][]string{{"golang"}}, func(a *model.Antenna) {
		a.Src = model.AntennaSourceList
		a.UserListID = &listID
	})
	repo.Antennas["ant2"] = a

	// alice のノートでキーワードマッチ → 届く
	text1 := "I love golang"
	svc.OnNoteCreated(
		&model.Note{ID: "n1", UserID: "alice", Text: &text1, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice"},
	)

	// alice のノートでキーワード不一致 → 届かない
	text2 := "I love python"
	svc.OnNoteCreated(
		&model.Note{ID: "n2", UserID: "alice", Text: &text2, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice"},
	)

	// 非メンバー bob のノートでキーワードマッチ → 届かない (リスト外)
	text3 := "I love golang too"
	svc.OnNoteCreated(
		&model.Note{ID: "n3", UserID: "bob", Text: &text3, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "bob", Username: "bob"},
	)

	rows, err := svc.Notes(context.Background(), "owner", "ant2", 100, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, rows)
}

// --- List source: edge cases -----------------------------------------------

// userListRepo が未注入のとき list ソースは常に不一致になる。
func TestMatchNote_ListSource_NilRepoMisses(t *testing.T) {
	svc, _ := newSvc(t)
	// SetUserListRepo を呼ばない
	listID := "ul1"
	a, err := svc.Create(CreateInput{OwnerID: "u1", Name: "noRepo", Src: model.AntennaSourceList, UserListID: &listID})
	require.NoError(t, err)
	text := "hi"
	assert.False(t, svc.matchNote(a, &model.Note{Text: &text, Visibility: model.NoteVisibilityPublic}, &model.User{ID: "alice", Username: "alice"}, nil))
}

// UserListID が nil のとき list ソースは不一致になる。
func TestMatchNote_ListSource_NilUserListID(t *testing.T) {
	svc, repo := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)
	a := makeAntenna(t, "ant-nil", "u1", nil, func(a *model.Antenna) {
		a.Src = model.AntennaSourceList
		a.UserListID = nil
	})
	repo.Antennas["ant-nil"] = a
	text := "hi"
	assert.False(t, svc.matchNote(a, &model.Note{Text: &text, Visibility: model.NoteVisibilityPublic}, &model.User{ID: "alice", Username: "alice"}, nil))
}

// UserListID が空文字列のとき list ソースは不一致になる。
func TestMatchNote_ListSource_EmptyUserListID(t *testing.T) {
	svc, repo := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)
	empty := ""
	a := makeAntenna(t, "ant-empty", "u1", nil, func(a *model.Antenna) {
		a.Src = model.AntennaSourceList
		a.UserListID = &empty
	})
	repo.Antennas["ant-empty"] = a
	text := "hi"
	assert.False(t, svc.matchNote(a, &model.Note{Text: &text, Visibility: model.NoteVisibilityPublic}, &model.User{ID: "alice", Username: "alice"}, nil))
}

// ListMembers がエラーを返す場合 list ソースは不一致になる。
func TestMatchNote_ListSource_ListMembersError(t *testing.T) {
	svc, repo := newSvc(t)
	svc.SetUserListRepo(&failingUserListRepo{})
	listID := "ul-err"
	a := makeAntenna(t, "ant-err", "u1", nil, func(a *model.Antenna) {
		a.Src = model.AntennaSourceList
		a.UserListID = &listID
	})
	repo.Antennas["ant-err"] = a
	text := "hi"
	assert.False(t, svc.matchNote(a, &model.Note{Text: &text, Visibility: model.NoteVisibilityPublic}, &model.User{ID: "alice", Username: "alice"}, nil))
}

// failingUserListRepo causes ListMembers and FindByID to fail.
type failingUserListRepo struct{}

func (r *failingUserListRepo) Create(_ *model.UserList) error { return nil }
func (r *failingUserListRepo) FindByID(_ string) (*model.UserList, error) {
	return nil, errors.New("boom")
}
func (r *failingUserListRepo) ListByUser(_ string) ([]*model.UserList, error) { return nil, nil }
func (r *failingUserListRepo) ListMembershipsByUser(_ string) ([]*model.UserListMembership, error) {
	return nil, nil
}
func (r *failingUserListRepo) CountByUser(_ string) (int64, error)         { return 0, nil }
func (r *failingUserListRepo) Delete(_ string) error                       { return nil }
func (r *failingUserListRepo) AddMember(_ *model.UserListMembership) error { return nil }
func (r *failingUserListRepo) RemoveMember(_, _ string) error              { return nil }
func (r *failingUserListRepo) CountMembers(_ string) (int64, error)        { return 0, nil }
func (r *failingUserListRepo) ListMembers(_ string) ([]*model.UserListMembership, error) {
	return nil, errors.New("list members error")
}
func (r *failingUserListRepo) ListMembershipsPage(_, _, _ string, _ int) ([]*model.UserListMembership, error) {
	return nil, errors.New("list memberships page error")
}
func (r *failingUserListRepo) ListMembersByListIDs(_ []string) (map[string][]string, error) {
	return nil, errors.New("list members by ids error")
}
func (r *failingUserListRepo) UpdateList(_ string, _ map[string]any) error { return nil }
func (r *failingUserListRepo) UpdateMembership(_, _ string, _ *bool) error { return nil }
func (r *failingUserListRepo) ListIDsByMember(_ string) ([]string, error)  { return nil, nil }
func (r *failingUserListRepo) ListIDsAndOwnersByMember(_ string) (map[string]string, error) {
	return nil, nil
}
func (r *failingUserListRepo) ListsContainingMember(_, _ string) ([]*model.UserList, error) {
	return nil, nil
}

// リストメンバーを追加・削除した後のアンテナ動作を検証する。
// メンバーを削除するとそのユーザーのノートはアンテナに届かなくなる。
func TestOnNoteCreated_ListSource_MemberRemoved(t *testing.T) {
	svc, repo := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)

	listID := "ul-rm"
	require.NoError(t, lists.Create(&model.UserList{ID: listID, UserID: "owner", Name: "team"}))
	require.NoError(t, lists.AddMember(&model.UserListMembership{UserListID: listID, UserID: "alice"}))
	require.NoError(t, lists.AddMember(&model.UserListMembership{UserListID: listID, UserID: "bob"}))

	a := makeAntenna(t, "ant-rm", "owner", nil, func(a *model.Antenna) {
		a.Src = model.AntennaSourceList
		a.UserListID = &listID
	})
	repo.Antennas["ant-rm"] = a

	// bob をリストから削除
	require.NoError(t, lists.RemoveMember(listID, "bob"))

	text := "post after removal"
	svc.OnNoteCreated(
		&model.Note{ID: "n-alice", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "alice", Username: "alice"},
	)
	svc.OnNoteCreated(
		&model.Note{ID: "n-bob", UserID: "bob", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "bob", Username: "bob"},
	)

	rows, err := svc.Notes(context.Background(), "owner", "ant-rm", 100, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"n-alice"}, rows)
}

// --- Visibility gate (#1464) ----------------------------------------------
//
// matchNote が note visibility に対して CanSeeNote 相当のチェックを行うことを
// 保証する。`src=all` / `src=users` のような broad source antenna が
// followers / specified note を pickup してしまう IDOR (#1464) の regression
// gate。

// followers note: antenna owner が follow していなければ matchNote が false。
func TestMatchNote_FollowersVisibility_NonFollowerOwnerRejected(t *testing.T) {
	svc, repo := newSvc(t)
	// followingRepo を空 (= owner は author を follow していない) で配線
	svc.SetFollowingRepo(testutil.NewMockFollowingRepository())
	repo.Antennas["a1"] = makeAntenna(t, "a1", "owner", [][]string{{"hi"}})

	text := "hi"
	n := &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityFollowers}
	author := &model.User{ID: "author", Username: "alice"}

	require.False(t, svc.matchNote(repo.Antennas["a1"], n, author, nil))
}

// followers note: antenna owner が follower なら matchNote が true。
func TestMatchNote_FollowersVisibility_FollowerOwnerAccepted(t *testing.T) {
	svc, repo := newSvc(t)
	follows := testutil.NewMockFollowingRepository()
	// owner → author を follow
	require.NoError(t, follows.Create(&model.Following{ID: "f1", FollowerID: "owner", FolloweeID: "author"}))
	svc.SetFollowingRepo(follows)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "owner", [][]string{{"hi"}})

	text := "hi"
	n := &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityFollowers}
	author := &model.User{ID: "author", Username: "alice"}

	require.True(t, svc.matchNote(repo.Antennas["a1"], n, author, nil))
}

// followers note: antenna owner == author なら follow チェック無しで通る。
func TestMatchNote_FollowersVisibility_SelfAuthored(t *testing.T) {
	svc, repo := newSvc(t)
	// followingRepo は無くて良い (本人 short-circuit)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "author", [][]string{{"hi"}})

	text := "hi"
	n := &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityFollowers}
	author := &model.User{ID: "author", Username: "alice"}

	require.True(t, svc.matchNote(repo.Antennas["a1"], n, author, nil))
}

// specified note: antenna owner が visibleUserIds に含まれていなければ false。
func TestMatchNote_SpecifiedVisibility_NonRecipientRejected(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "owner", [][]string{{"hi"}})

	text := "hi"
	n := &model.Note{
		ID: "n1", UserID: "author", Text: &text,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"someoneElse"},
	}
	author := &model.User{ID: "author", Username: "alice"}

	require.False(t, svc.matchNote(repo.Antennas["a1"], n, author, nil))
}

// specified note: antenna owner が visibleUserIds に含まれていれば true。
func TestMatchNote_SpecifiedVisibility_RecipientAccepted(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "owner", [][]string{{"hi"}})

	text := "hi"
	n := &model.Note{
		ID: "n1", UserID: "author", Text: &text,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"owner"},
	}
	author := &model.User{ID: "author", Username: "alice"}

	require.True(t, svc.matchNote(repo.Antennas["a1"], n, author, nil))
}

// followingRepo 未配線 + followers note → CanSeeNote の fail-closed semantics
// で本人以外は reject される (= IDOR fail-closed)。
func TestMatchNote_FollowersVisibility_NilFollowingRepoFailClosed(t *testing.T) {
	svc, repo := newSvc(t)
	// followingRepo を意図的に注入しない (本番でも `home` source 設定ミス検出時と同じ挙動)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "owner", [][]string{{"hi"}})

	text := "hi"
	n := &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityFollowers}
	author := &model.User{ID: "author", Username: "alice"}

	require.False(t, svc.matchNote(repo.Antennas["a1"], n, author, nil))
}

// OnNoteCreated 経由でも push されないことを確認 (REST/WS 両 surface への gate)。
func TestOnNoteCreated_FollowersNote_NotPushedToNonFollowerAntenna(t *testing.T) {
	svc, repo := newSvc(t)
	svc.SetFollowingRepo(testutil.NewMockFollowingRepository()) // 空 → owner は author を follow していない
	repo.Antennas["a1"] = makeAntenna(t, "a1", "owner", [][]string{{"hi"}})

	text := "hi there"
	svc.OnNoteCreated(
		&model.Note{ID: "n-followers", UserID: "author", Text: &text, Visibility: model.NoteVisibilityFollowers},
		&model.User{ID: "author", Username: "alice"},
	)

	rows, err := svc.Notes(context.Background(), "owner", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows, "non-follower antenna owner should not receive followers note")
}

// public note は visibility gate を素通りすることを confirm (regression guard)。
func TestOnNoteCreated_PublicNote_PushedRegardlessOfFollow(t *testing.T) {
	svc, repo := newSvc(t)
	svc.SetFollowingRepo(testutil.NewMockFollowingRepository()) // 空でも public は通る
	repo.Antennas["a1"] = makeAntenna(t, "a1", "owner", [][]string{{"hi"}})

	text := "hi there"
	svc.OnNoteCreated(
		&model.Note{ID: "n-public", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "author", Username: "alice"},
	)

	rows, err := svc.Notes(context.Background(), "owner", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"n-public"}, rows)
}

// countingFollowingRepo は Exists の呼び出し回数を記録する MockFollowingRepository
// の薄い wrap。#1467 review nit (home source の Exists 二重呼び出し回避) の回帰テスト
// 専用。Following struct を直接操作するため map にも直接 push する。
type countingFollowingRepo struct {
	*testutil.MockFollowingRepository
	existsCalls int
}

func newCountingFollowingRepo() *countingFollowingRepo {
	return &countingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository()}
}

func (c *countingFollowingRepo) Exists(followerID, followeeID string) (bool, error) {
	c.existsCalls++
	return c.MockFollowingRepository.Exists(followerID, followeeID)
}

// #1467 review nit: home source antenna が followers visibility note を pickup する
// ケースで、CanSeeNote 内 (`Exists(owner, author)`) と matchSource 内
// (`Exists(a.UserID, author.ID)`) の同一 pair に対する Exists を 1 回に畳めている
// ことを assert する。
func TestMatchNote_HomeSource_FollowersNote_NoDuplicateExists(t *testing.T) {
	svc, _ := newSvc(t)
	counting := newCountingFollowingRepo()
	// owner → author を follow
	require.NoError(t, counting.Create(&model.Following{ID: "f1", FollowerID: "owner", FolloweeID: "author"}))
	svc.SetFollowingRepo(counting)
	a, err := svc.Create(CreateInput{OwnerID: "owner", Name: "home-followers", Src: model.AntennaSourceHome})
	require.NoError(t, err)

	text := "hi"
	n := &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityFollowers}
	author := &model.User{ID: "author", Username: "author"}

	require.True(t, svc.matchNote(a, n, author, nil))
	assert.Equal(t, 1, counting.existsCalls,
		"Exists should be called exactly once for home source + followers visibility (no duplicate after #1467 review)")
}

// home source + public note では visibility gate (CanSeeNote) が Exists を呼ばないため、
// matchSource 側の Exists が 1 回だけ走る。NoDuplicateExists test の対照 (公開範囲が
// followers でないと最適化の前提も失われていないことを示す)。
func TestMatchNote_HomeSource_PublicNote_SingleExists(t *testing.T) {
	svc, _ := newSvc(t)
	counting := newCountingFollowingRepo()
	require.NoError(t, counting.Create(&model.Following{ID: "f1", FollowerID: "owner", FolloweeID: "author"}))
	svc.SetFollowingRepo(counting)
	a, err := svc.Create(CreateInput{OwnerID: "owner", Name: "home-public", Src: model.AntennaSourceHome})
	require.NoError(t, err)

	text := "hi"
	n := &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "author", Username: "author"}

	require.True(t, svc.matchNote(a, n, author, nil))
	assert.Equal(t, 1, counting.existsCalls,
		"public visibility skips CanSeeNote Exists; matchSource home performs the single call")
}

// --- StreamingPublisher (#1573) -------------------------------------------

// stubStreamingPublisher records PublishNote calls so OnNoteCreated's pub/sub
// publish can be asserted without a real stream layer.
type stubStreamingPublisher struct {
	calls []publishCall
}

type publishCall struct {
	topic  string
	noteID string
	author string
}

func (p *stubStreamingPublisher) PublishNote(topic string, n *model.Note, author *model.User) {
	pc := publishCall{topic: topic}
	if n != nil {
		pc.noteID = n.ID
	}
	if author != nil {
		pc.author = author.ID
	}
	p.calls = append(p.calls, pc)
}

// TestOnNoteCreated_PublishesMatchedNote covers #1573 課題1: a matched note is
// published to the antennaTimeline:<id> pub/sub topic (= streamKey) alongside
// the ZSET write, so the WS antenna channel receives live notes.
func TestOnNoteCreated_PublishesMatchedNote(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"misskey"}})
	pub := &stubStreamingPublisher{}
	svc.SetStreamingPublisher(pub)

	text := "hello misskey world"
	n := &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "author", Username: "alice"}
	svc.OnNoteCreated(n, author)

	require.Len(t, pub.calls, 1, "matched note must be published once")
	assert.Equal(t, "antennaTimeline:a1", pub.calls[0].topic,
		"publish topic must equal streamKey so AntennaChannel.Subscribe matches")
	assert.Equal(t, "n1", pub.calls[0].noteID)
	assert.Equal(t, "author", pub.calls[0].author)
}

// TestOnNoteCreated_NoMatchDoesNotPublish guards that a non-matching note is
// neither pushed to the stream nor published to pub/sub.
func TestOnNoteCreated_NoMatchDoesNotPublish(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"missing"}})
	pub := &stubStreamingPublisher{}
	svc.SetStreamingPublisher(pub)

	text := "hello world"
	svc.OnNoteCreated(
		&model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "author", Username: "alice"},
	)
	assert.Empty(t, pub.calls, "non-matching note must not be published")
}

// TestOnNoteCreated_PublishesToEachMatchedAntenna verifies one publish per
// matched antenna, each to its own streamKey topic.
func TestOnNoteCreated_PublishesToEachMatchedAntenna(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"misskey"}})
	repo.Antennas["a2"] = makeAntenna(t, "a2", "u2", [][]string{{"misskey"}})
	repo.Antennas["a3"] = makeAntenna(t, "a3", "u3", [][]string{{"other"}}) // no match
	pub := &stubStreamingPublisher{}
	svc.SetStreamingPublisher(pub)

	text := "hello misskey"
	svc.OnNoteCreated(
		&model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "author", Username: "alice"},
	)

	topics := make([]string, 0, len(pub.calls))
	for _, c := range pub.calls {
		topics = append(topics, c.topic)
	}
	assert.ElementsMatch(t, []string{"antennaTimeline:a1", "antennaTimeline:a2"}, topics,
		"each matched antenna gets a publish to its own streamKey topic; non-matching is skipped")
}

// TestOnNoteCreated_NilPublisherIsNoOp guards that an unwired publisher leaves
// the stream XAdd path intact (REST `antennas/notes` still works).
func TestOnNoteCreated_NilPublisherIsNoOp(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"misskey"}})
	// publisher 未配線でも panic せず stream XAdd は行われる。

	text := "hello misskey world"
	svc.OnNoteCreated(
		&model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "author", Username: "alice"},
	)
	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, rows)
}

// #2069 (upstream #17463): RemoveNote は antenna TL stream から noteId 一致 entry を
// 削除する。owner 検証 (not-found / not-owner)、存在しないノートは no-op 成功。
func TestRemoveNote(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	ctx := context.Background()
	for _, n := range []string{"n1", "n2", "n3"} {
		seedNote(t, streamKey("a1"), n)
	}

	// n2 を削除 → n1, n3 が残る。
	require.NoError(t, svc.RemoveNote("u1", "a1", "n2"))
	rows, err := svc.Notes(ctx, "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n1", "n3"}, rows, "n2 のみ削除される (#2069)")

	// 存在しないノートは no-op 成功。
	require.NoError(t, svc.RemoveNote("u1", "a1", "ghost"))

	// not-owner は ErrAccessDenied、未存在 antenna は ErrAntennaNotFound。
	assert.ErrorIs(t, svc.RemoveNote("other", "a1", "n1"), ErrAccessDenied)
	assert.ErrorIs(t, svc.RemoveNote("u1", "nope", "n1"), ErrAntennaNotFound)
}

// OnMoveAccount は src を users に挙げているアンテナへ dst の acct を**追記**する。
// src は消さない (#2419)。移行の引き継ぎは一貫して「旧側を消さず新側を足す」方向。
func TestOnMoveAccount(t *testing.T) {
	remoteHost := "remote.example"
	src := &model.User{ID: "src", Username: "Alice"}
	dst := &model.User{ID: "dst", Username: "Alice2", Host: &remoteHost}

	cases := []struct {
		name      string
		users     model.StringArray
		isActive  bool
		wantUsers model.StringArray
	}{
		{
			name:      "src を挙げているアンテナに dst を追記する",
			users:     model.StringArray{"@alice", "@bob"},
			isActive:  true,
			wantUsers: model.StringArray{"@alice", "@bob", "@alice2@remote.example"},
		},
		{
			// upstream 同様 acct 比較は大文字小文字を無視する。
			name:      "大文字小文字が違っても一致させる",
			users:     model.StringArray{"@ALICE"},
			isActive:  true,
			wantUsers: model.StringArray{"@ALICE", "@alice2@remote.example"},
		},
		{
			name:      "src が居ないアンテナは触らない",
			users:     model.StringArray{"@bob"},
			isActive:  true,
			wantUsers: model.StringArray{"@bob"},
		},
		{
			name:      "既に dst が居れば重複させない",
			users:     model.StringArray{"@alice", "@alice2@remote.example"},
			isActive:  true,
			wantUsers: model.StringArray{"@alice", "@alice2@remote.example"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newSvc(t)
			repo.Antennas["a1"] = &model.Antenna{
				ID: "a1", UserID: "owner", Src: model.AntennaSourceUsers,
				Users: tc.users, IsActive: tc.isActive,
			}

			svc.OnMoveAccount(src, dst)

			assert.Equal(t, tc.wantUsers, repo.Antennas["a1"].Users)
		})
	}
}

// nil 引数やリポジトリ未設定で panic しない。
func TestOnMoveAccount_NilArgs(t *testing.T) {
	svc, _ := newSvc(t)
	assert.NotPanics(t, func() { svc.OnMoveAccount(nil, &model.User{ID: "dst"}) })
	assert.NotPanics(t, func() { svc.OnMoveAccount(&model.User{ID: "src"}, nil) })
}

// antenna の timeline を読んだらその antenna の未読行を消す (#2406)。
//
// **これが無いと `/api/i` の `hasUnreadAntenna` が一度 true になると永久に true**
// のままになり、`antenna_note_unread` も単調増加する。matchNote 側が行を作る一方で
// 消す経路がどこにも無かった。
func TestNotes_ClearsUnreadRows(t *testing.T) {
	svc, repo := newSvc(t)
	unread := testutil.NewMockAntennaNoteUnreadRepository()
	svc.SetUnreadRepo(unread)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	repo.Antennas["a2"] = &model.Antenna{ID: "a2", UserID: "u1"}

	unread.Rows = []*model.AntennaNoteUnread{
		{UserID: "u1", AntennaID: "a1", NoteID: "n1"},
		{UserID: "u1", AntennaID: "a1", NoteID: "n2"},
		// 別 antenna の未読は残す。
		{UserID: "u1", AntennaID: "a2", NoteID: "n3"},
		// 別ユーザーの未読も残す。
		{UserID: "u2", AntennaID: "a1", NoteID: "n4"},
	}

	_, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)

	require.Len(t, unread.Rows, 2, "読んだ (user, antenna) の行だけ消える")
	for _, r := range unread.Rows {
		assert.False(t, r.UserID == "u1" && r.AntennaID == "a1",
			"u1/a1 の未読は消えている (残: %+v)", r)
	}
}

// unreadRepo 未配線でも timeline 取得は動く (既存の best-effort と同じ扱い)。
func TestNotes_WithoutUnreadRepo(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1"}
	require.NoError(t, svc.pushNote(context.Background(), "a1", "n1", time.Now()))

	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// 他人の antenna を読もうとした場合は所有者チェックで弾かれ、未読行にも触らない。
func TestNotes_ForeignAntennaDoesNotClearUnread(t *testing.T) {
	svc, repo := newSvc(t)
	unread := testutil.NewMockAntennaNoteUnreadRepository()
	svc.SetUnreadRepo(unread)
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "owner"}
	unread.Rows = []*model.AntennaNoteUnread{
		{UserID: "owner", AntennaID: "a1", NoteID: "n1"},
	}

	_, err := svc.Notes(context.Background(), "intruder", "a1", 10, "", "")
	require.Error(t, err)
	assert.Len(t, unread.Rows, 1, "所有者チェックで弾かれた場合は未読行を消さない")
}

// seedNote pushes a note id the way pushNote does (#2465 で Stream から ZSET へ)。
// テストが Redis の構造を直接組み立てているので、実装と同じ形に揃えておかないと
// 「テストは通るが本番は動かない」になる。
func seedNote(t *testing.T, key, noteID string) {
	t.Helper()
	require.NoError(t, testRedis.Client.ZAdd(context.Background(), key, redis.Z{Score: 0, Member: noteID}).Err())
}

// #2465 の回帰テスト。
//
// 以前は Redis Stream の entry id に **push 時刻**を使い、カーソルの境界は
// **ノートの作成時刻** (aidx) から作っていた。この 2 つは別の瞬間なので順序が
// 入れ替わりえて、ページ送りでノートが落ちた。
//
// ここでは「作成順と push 順が食い違う」状況を作り、それでもページ送りが
// 全件を返すことを固定する。ZSET + 辞書順なら push の順序は結果に影響しない。
func TestNotes_PaginationIgnoresPushOrder(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a-ord"] = &model.Antenna{ID: "a-ord", UserID: "u1"}
	ctx := context.Background()

	// **実 ID 生成器で作る。** 本番と同じ形式でないと、境界の扱いが production と
	// ずれたまま緑になる。作成時刻は昇順、push は**わざと逆順**にする。
	base := time.Now().Add(-time.Minute)
	ids := make([]string, 6)
	for i := range ids {
		ids[i] = idGen.Generate(base.Add(time.Duration(i) * 3 * time.Millisecond))
	}
	// push 時刻も作成時刻と食い違わせる (旧実装はこのずれで取りこぼした)。
	for i := len(ids) - 1; i >= 0; i-- {
		require.NoError(t, svc.pushNote(ctx, "a-ord", ids[i], time.Now()))
	}

	// limit 2 でページ送りして全件を集める。
	var got []string
	until := ""
	for {
		page, err := svc.Notes(ctx, "u1", "a-ord", 2, "", until)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		until = page[len(page)-1]
	}

	// newest-first で全件、重複なし。
	want := make([]string, len(ids))
	for i, id := range ids {
		want[len(ids)-1-i] = id
	}
	assert.Equal(t, want, got)
}

// 同じ瞬間に push された複数ノートを取りこぼさないこと。
//
// 旧実装は境界を ms 単位で切っており、「同 ms の entry は boundary note と
// タイとみなしてまとめて除外する」設計だった。タイになるのは push 時刻であって
// ノートの同一性ではないので、無関係なノートを巻き込んで落としていた。
func TestNotes_PaginationKeepsNotesPushedInSameInstant(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a-tie"] = &model.Antenna{ID: "a-tie", UserID: "u1"}
	ctx := context.Background()

	now := time.Now()
	base := now.Add(-time.Minute)
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = idGen.Generate(base.Add(time.Duration(i) * time.Millisecond))
	}
	for _, id := range ids {
		require.NoError(t, svc.pushNote(ctx, "a-tie", id, now)) // 全て同一 push 時刻
	}

	first, err := svc.Notes(ctx, "u1", "a-tie", 2, "", "")
	require.NoError(t, err)
	require.Equal(t, []string{ids[3], ids[2]}, first)

	second, err := svc.Notes(ctx, "u1", "a-tie", 2, "", first[len(first)-1])
	require.NoError(t, err)
	assert.Equal(t, []string{ids[1], ids[0]}, second, "同一 push 時刻でも取りこぼさない")
}

// 上限を超えたら古い側から落ちること。
func TestPushNote_TrimsToMaxNotes(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a-cap"] = &model.Antenna{ID: "a-cap", UserID: "u1"}
	ctx := context.Background()

	total := MaxNotesPerAntenna + 5
	for i := 0; i < total; i++ {
		require.NoError(t, svc.pushNote(ctx, "a-cap", fmt.Sprintf("nd%05d", i), time.Now()))
	}

	n, err := testRedis.Client.ZCard(ctx, streamKey("a-cap")).Result()
	require.NoError(t, err)
	assert.EqualValues(t, MaxNotesPerAntenna, n)

	// 残っているのは新しい側。
	page, err := svc.Notes(ctx, "u1", "a-cap", 1, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{fmt.Sprintf("nd%05d", total-1)}, page)
}

// 旧 Stream が残っている環境で WRONGTYPE を検出して張り替えること (#2465)。
// キー名を据え置いたので、この移行が効かないと antenna timeline が壊れる。
func TestPushNote_MigratesLegacyStream(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a-mig"] = &model.Antenna{ID: "a-mig", UserID: "u1"}
	ctx := context.Background()
	key := streamKey("a-mig")

	// 旧構造 (Stream) を作る。
	require.NoError(t, testRedis.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: key, Values: map[string]any{"noteId": "old1"},
	}).Err())

	// 読み取りは空を返す (壊れない)。
	page, err := svc.Notes(ctx, "u1", "a-mig", 10, "", "")
	require.NoError(t, err)
	assert.Empty(t, page)

	// push で張り替わる。
	require.NoError(t, svc.pushNote(ctx, "a-mig", "new1", time.Now()))
	page, err = svc.Notes(ctx, "u1", "a-mig", 10, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"new1"}, page)
}

// 旧 Stream に対する RemoveNote が落ちないこと。
func TestRemoveNote_LegacyStreamIsNoOp(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a-rml"] = &model.Antenna{ID: "a-rml", UserID: "u1"}
	ctx := context.Background()
	require.NoError(t, testRedis.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey("a-rml"), Values: map[string]any{"noteId": "x"},
	}).Err())

	assert.NoError(t, svc.RemoveNote("u1", "a-rml", "x"))
}
