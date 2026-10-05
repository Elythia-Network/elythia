package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupAdHandler returns a handler with AdRepo wired and optional seed rows.
// AdRepo を使う test の boilerplate (handler 構築 + repo 生成 + seed +
// SetAdRepo) を 1 行に圧縮する (#761)。戻り値の repo を直接 mutate して
// CreateErr 等の error injection も可能。
func setupAdHandler(t *testing.T, seed ...*model.Ad) (*apiadmin.Handler, *testutil.MockAdRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockAdRepository()
	for _, ad := range seed {
		require.NoError(t, repo.Create(ad))
	}
	h.SetAdRepo(repo)
	return h, repo
}

// setupAvatarDecorationHandler returns a handler with AvatarDecorationRepo
// wired and optional seed rows. Ad helper と同じ contract (#761)。
func setupAvatarDecorationHandler(t *testing.T, seed ...*model.AvatarDecoration) (*apiadmin.Handler, *testutil.MockAvatarDecorationRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockAvatarDecorationRepository()
	for _, d := range seed {
		require.NoError(t, repo.Create(d))
	}
	h.SetAvatarDecorationRepo(repo)
	return h, repo
}

// setupInviteHandler returns a handler with InviteRepo (RegistrationTicket
// repo) wired and optional seed rows. Ad helper と同じ contract (#761)。
func setupInviteHandler(t *testing.T, seed ...*model.RegistrationTicket) (*apiadmin.Handler, *testutil.MockRegistrationTicketRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockRegistrationTicketRepository()
	for _, tkt := range seed {
		require.NoError(t, repo.Create(tkt))
	}
	h.SetInviteRepo(repo)
	return h, repo
}

// --- Ad ---------------------------------------------------------------------

func TestAdCreate_Success(t *testing.T) {
	h, repo := setupAdHandler(t)
	rec := doPost(h.AdCreate,
		`{"url":"https://x","imageUrl":"https://y","place":"square","memo":"m","priority":"high","ratio":2,"dayOfWeek":3,"expiresAt":1760000000000,"startsAt":1759000000000,"isSensitive":true}`,
		adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got model.Ad
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "https://x", got.URL)
	assert.Equal(t, "square", got.Place)
	assert.Equal(t, 2, got.Ratio)
	assert.True(t, got.IsSensitive)
	assert.Contains(t, repo.Ads, got.ID)
	// #1772: expiresAt/startsAt は upstream toISOString() に合わせ UTC .000Z 形式。
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`, raw["expiresAt"])
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`, raw["startsAt"])
}

func TestAdCreate_MissingRequired(t *testing.T) {
	h, _ := setupAdHandler(t)
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.AdCreate, `{"url":""}`, adminUser).Code)
}

func TestAdCreate_Defaults(t *testing.T) {
	h, _ := setupAdHandler(t)
	rec := doPost(h.AdCreate, `{"url":"https://x","imageUrl":"https://y"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got model.Ad
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "square", got.Place)
	assert.Equal(t, "middle", got.Priority)
	assert.Equal(t, 1, got.Ratio)
}

func TestAdCreate_RepoError(t *testing.T) {
	h, repo := setupAdHandler(t)
	repo.CreateErr = assertError{}
	rec := doPost(h.AdCreate, `{"url":"https://x","imageUrl":"https://y"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// fakeMetaResponseInvalidator counts InvalidateResponseCache calls.
type fakeMetaResponseInvalidator struct{ calls int }

func (f *fakeMetaResponseInvalidator) InvalidateResponseCache() { f.calls++ }

// ad の変更は /api/meta の `ads` に載るので、response cache を落とす (#2649)。
// 落とし忘れると TTL 分だけ古い ads が返り続ける。
func TestAd_MutationsInvalidateMetaResponseCache(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		h, _ := setupAdHandler(t)
		inv := &fakeMetaResponseInvalidator{}
		h.SetMetaResponseInvalidator(inv)
		rec := doPost(h.AdCreate, `{"url":"https://x","imageUrl":"https://y"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, 1, inv.calls)
	})

	t.Run("create failure does not invalidate", func(t *testing.T) {
		h, repo := setupAdHandler(t)
		repo.CreateErr = assertError{}
		inv := &fakeMetaResponseInvalidator{}
		h.SetMetaResponseInvalidator(inv)
		rec := doPost(h.AdCreate, `{"url":"https://x","imageUrl":"https://y"}`, adminUser)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, 0, inv.calls)
	})

	t.Run("update", func(t *testing.T) {
		h, _ := setupAdHandler(t, &model.Ad{ID: "ad1", URL: "https://x", Place: "square"})
		inv := &fakeMetaResponseInvalidator{}
		h.SetMetaResponseInvalidator(inv)
		rec := doPost(h.AdUpdate, `{"id":"ad1","memo":"m"}`, adminUser)
		require.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, 1, inv.calls)
	})

	t.Run("delete", func(t *testing.T) {
		h, _ := setupAdHandler(t, &model.Ad{ID: "ad1", URL: "https://x", Place: "square"})
		inv := &fakeMetaResponseInvalidator{}
		h.SetMetaResponseInvalidator(inv)
		rec := doPost(h.AdDelete, `{"id":"ad1"}`, adminUser)
		require.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, 1, inv.calls)
	})

	t.Run("unwired invalidator does not panic", func(t *testing.T) {
		h, _ := setupAdHandler(t)
		rec := doPost(h.AdCreate, `{"url":"https://x","imageUrl":"https://y"}`, adminUser)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestAdList_Paginated(t *testing.T) {
	ads := make([]*model.Ad, 5)
	for i := 0; i < 5; i++ {
		ads[i] = &model.Ad{
			ID: fmt.Sprintf("a%02d", i), URL: "u", ImageURL: "i", Place: "square", Priority: "middle", Ratio: 1,
			StartsAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(time.Hour),
		}
	}
	h, _ := setupAdHandler(t, ads...)

	rec := doPost(h.AdList, `{"limit":3,"offset":0}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []model.Ad
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 3)
	// id DESC
	assert.Equal(t, "a04", rows[0].ID)

	var rawRows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rawRows))
	shapetest.Assert(t, "Ad", rawRows[0]) // L3 (#1292)
}

// #1545: publishing=true は配信中 (startsAt<=now<expiresAt) のみ返す。
func TestAdList_PublishingFilter(t *testing.T) {
	now := time.Now()
	h, _ := setupAdHandler(t,
		&model.Ad{ID: "a_live", URL: "u", ImageURL: "i", Place: "square", Priority: "middle", Ratio: 1,
			StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}, // 配信中
		&model.Ad{ID: "a_expired", URL: "u", ImageURL: "i", Place: "square", Priority: "middle", Ratio: 1,
			StartsAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}, // 終了
		&model.Ad{ID: "a_future", URL: "u", ImageURL: "i", Place: "square", Priority: "middle", Ratio: 1,
			StartsAt: now.Add(time.Hour), ExpiresAt: now.Add(2 * time.Hour)}, // 未開始
	)

	rec := doPost(h.AdList, `{"limit":10,"publishing":true}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "a_live", rows[0]["id"])

	// publishing=false は非配信 (終了 + 未開始)。
	rec = doPost(h.AdList, `{"limit":10,"publishing":false}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	rows = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
}

// #1545: untilId による cursor pagination。
func TestAdList_CursorPagination(t *testing.T) {
	ads := make([]*model.Ad, 5)
	for i := 0; i < 5; i++ {
		ads[i] = &model.Ad{ID: fmt.Sprintf("a%02d", i), URL: "u", ImageURL: "i", Place: "square", Priority: "middle", Ratio: 1,
			StartsAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(time.Hour)}
	}
	h, _ := setupAdHandler(t, ads...)

	rec := doPost(h.AdList, `{"limit":10,"untilId":"a03"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	// a03 より小さい id (a02,a01,a00) を id DESC で返す。
	require.Len(t, rows, 3)
	assert.Equal(t, "a02", rows[0]["id"])
}

func TestAdUpdate_PartialFields(t *testing.T) {
	h, repo := setupAdHandler(t,
		&model.Ad{ID: "a1", URL: "old", Place: "square", Priority: "middle", Ratio: 1, ImageURL: "i"},
	)

	rec := doPost(h.AdUpdate,
		`{"id":"a1","url":"new","priority":"high"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "new", repo.Ads["a1"].URL)
	assert.Equal(t, "high", repo.Ads["a1"].Priority)
}

func TestAdUpdate_NotFound(t *testing.T) {
	h, _ := setupAdHandler(t)
	assert.Equal(t, http.StatusNotFound,
		doPost(h.AdUpdate, `{"id":"missing","url":"x"}`, adminUser).Code)
}

func TestAdUpdate_MissingID(t *testing.T) {
	h, _ := setupAdHandler(t)
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.AdUpdate, `{}`, adminUser).Code)
}

// Explicit expiresAt:0 in Update はクライアントが明示的に 1970-01-01 を指定した
// ケースとして扱う (nil なら変更なし)。handler 側の millisOrNow 適用で now に
// 読み替えてしまう regression を防ぐ。
func TestAdUpdate_ExplicitZeroExpiresAtStays1970(t *testing.T) {
	h, repo := setupAdHandler(t,
		&model.Ad{
			ID: "a1", URL: "u", Place: "square", Priority: "middle", Ratio: 1, ImageURL: "i",
			StartsAt:  time.Now(),
			ExpiresAt: time.Now().Add(time.Hour),
		},
	)

	rec := doPost(h.AdUpdate, `{"id":"a1","expiresAt":0}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// time.UnixMilli(0) は UTC 1970-01-01。time.Now() と大きく異なること。
	got := repo.Ads["a1"].ExpiresAt
	assert.Equal(t, int64(0), got.UnixMilli(),
		"explicit expiresAt:0 should map to UNIX epoch, not time.Now()")
}

func TestAdDelete_WithRepo(t *testing.T) {
	h, repo := setupAdHandler(t, &model.Ad{ID: "a1"})

	rec := doPost(h.AdDelete, `{"id":"a1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Ads, "a1")
}

// --- AvatarDecorations ------------------------------------------------------

func TestAvatarDecorationsCreate_Success(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	rec := doPost(h.AvatarDecorationsCreate,
		`{"name":"deco","description":"d","url":"https://i","roleIdsThatCanBeUsedThisDecoration":["r1","r2"]}`,
		adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got model.AvatarDecoration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "deco", got.Name)
	assert.Equal(t, []string(got.RoleIDs), []string{"r1", "r2"})
}

// create レスポンスに createdAt (aidx ID 由来) が含まれ、roleIds が省略時でも
// null でなく [] で返ること (TS schema は nullable:false)。
func TestAvatarDecorationsCreate_IncludesCreatedAt(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	rec := doPost(h.AvatarDecorationsCreate,
		`{"name":"deco","description":"d","url":"https://i"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotNil(t, resp["createdAt"], "create response must include createdAt")
	createdAt, _ := resp["createdAt"].(string)
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T`, createdAt, "createdAt は ISO 文字列")
	assert.Equal(t, []any{}, resp["roleIdsThatCanBeUsedThisDecoration"], "roleIds 省略時は null でなく []")
}

// list は handler 生成 (有効 aidx) の行で createdAt が非 null になること。
func TestAvatarDecorationsList_IncludesCreatedAt(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	// 有効な aidx を持つ行を handler 経由で作る。
	require.Equal(t, http.StatusOK,
		doPost(h.AvatarDecorationsCreate, `{"name":"deco","description":"d","url":"https://i"}`, adminUser).Code)

	rec := doPost(h.AvatarDecorationsList, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.NotNil(t, rows[0]["createdAt"], "list row must have non-null createdAt")
	assert.Equal(t, []any{}, rows[0]["roleIdsThatCanBeUsedThisDecoration"])
}

func TestAvatarDecorationsCreate_MissingName(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.AvatarDecorationsCreate, `{"url":"https://i"}`, adminUser).Code)
}

// upstream Misskey #17034 (= 2026.5.0): avatar_decoration に category 列を
// 追加し、create / update / list / get-avatar-decorations の各 API で round-trip
// する。create で category 指定 → 戻り値 + DB row に反映、update で別 string
// への更新 → DB row 更新、create 時の category 省略 → null 永続化、を確認。
// 加えて update で `"category": null` を送った時は no-update (= upstream の
// `ps.category === undefined` と同 semantics) になることも seal する。
func TestAvatarDecorationsCategory_RoundTrip(t *testing.T) {
	h, repo := setupAvatarDecorationHandler(t)

	// 1) Create with category
	createBody := `{"name":"deco","description":"d","url":"https://i",` +
		`"roleIdsThatCanBeUsedThisDecoration":[],"category":"holiday"}`
	rec := doPost(h.AvatarDecorationsCreate, createBody, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var created model.AvatarDecoration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Category, "create response should include category")
	assert.Equal(t, "holiday", *created.Category)
	require.NotNil(t, repo.Decorations[created.ID].Category, "DB row should persist category")
	assert.Equal(t, "holiday", *repo.Decorations[created.ID].Category)

	// 2) Update category to a different string
	rec = doPost(h.AvatarDecorationsUpdate,
		`{"id":"`+created.ID+`","category":"event"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, repo.Decorations[created.ID].Category)
	assert.Equal(t, "event", *repo.Decorations[created.ID].Category)

	// 3) Update with `"category": null` → no-update (= 現状の holiday → event
	// と進んだ後の `event` がそのまま保持される)。Go の json.Unmarshal は JSON
	// null を *string の nil として埋めるため、handler 側の `if req.Category
	// != nil` 分岐に入らず変更されない。`clear` したい場合は明示的に
	// "category": "" を送る運用 (= upstream `ps.category === undefined` と
	// 同じ振る舞い)。
	rec = doPost(h.AvatarDecorationsUpdate,
		`{"id":"`+created.ID+`","category":null}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, repo.Decorations[created.ID].Category, "null update should NOT clear category")
	assert.Equal(t, "event", *repo.Decorations[created.ID].Category, "category remains event after null update")

	// 4) Create without category → DB row has Category=nil
	rec = doPost(h.AvatarDecorationsCreate,
		`{"name":"plain","description":"","url":"https://i","roleIdsThatCanBeUsedThisDecoration":[]}`,
		adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var plain model.AvatarDecoration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &plain))
	assert.Nil(t, plain.Category, "create without category → null in response")
	assert.Nil(t, repo.Decorations[plain.ID].Category, "DB row should have null category")
}

func TestAvatarDecorationsList_WithRepo(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t,
		&model.AvatarDecoration{ID: "d1", Name: "a"},
		&model.AvatarDecoration{ID: "d2", Name: "b"},
	)
	rec := doPost(h.AvatarDecorationsList, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []model.AvatarDecoration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2)
}

// upstream list.ts は limit/sinceId/untilId/sinceDate/untilDate/userId を受けるが
// getAll(true) で全件返す。mk-go も params を受理しつつ全件返す (#1543)。
func TestAvatarDecorationsList_AcceptsParamsReturnsAll(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t,
		&model.AvatarDecoration{ID: "d1", Name: "a"},
		&model.AvatarDecoration{ID: "d2", Name: "b"},
	)
	// limit=1 を送っても TS 同様に全件 (2 件) 返る。
	rec := doPost(h.AvatarDecorationsList, `{"limit":1,"sinceId":"x","userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []model.AvatarDecoration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2, "params を受理しても全件返す (TS getAll(true) 互換)")
}

func TestAvatarDecorationsList_InvalidJSON(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	rec := doPost(h.AvatarDecorationsList, `{not`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAvatarDecorationsUpdate_Success(t *testing.T) {
	h, repo := setupAvatarDecorationHandler(t,
		&model.AvatarDecoration{ID: "d1", Name: "old", URL: "u"},
	)
	rec := doPost(h.AvatarDecorationsUpdate,
		`{"id":"d1","name":"new","roleIdsThatCanBeUsedThisDecoration":["r"]}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "new", repo.Decorations["d1"].Name)
	assert.Equal(t, []string(repo.Decorations["d1"].RoleIDs), []string{"r"})
}

func TestAvatarDecorationsUpdate_NotFound(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	assert.Equal(t, http.StatusNotFound,
		doPost(h.AvatarDecorationsUpdate, `{"id":"missing","name":"x"}`, adminUser).Code)
}

func TestAvatarDecorationsUpdate_MissingID(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.AvatarDecorationsUpdate, `{}`, adminUser).Code)
}

func TestAvatarDecorationsDelete_WithRepo(t *testing.T) {
	h, repo := setupAvatarDecorationHandler(t, &model.AvatarDecoration{ID: "d1"})
	rec := doPost(h.AvatarDecorationsDelete, `{"id":"d1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Decorations, "d1")
}

// --- Invite -----------------------------------------------------------------

func TestInviteCreate_Default(t *testing.T) {
	h, _ := setupInviteHandler(t)
	rec := doPost(h.InviteCreate, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.NotEmpty(t, rows[0]["code"])
	// createdAt が含まれる (Misskey 本家互換)
	assert.NotNil(t, rows[0]["createdAt"])
	assert.Equal(t, false, rows[0]["used"])
}

func TestInviteCreate_MultipleCount(t *testing.T) {
	h, _ := setupInviteHandler(t)
	rec := doPost(h.InviteCreate, `{"count":5}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 5)
}

func TestInviteCreate_CountClampedToMax(t *testing.T) {
	h, _ := setupInviteHandler(t)
	rec := doPost(h.InviteCreate, `{"count":250}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 100)
}

func TestInviteCreate_InvalidExpiresAt(t *testing.T) {
	h, _ := setupInviteHandler(t)
	rec := doPost(h.InviteCreate, `{"expiresAt":"not-a-date"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInviteList_FilterUnused(t *testing.T) {
	usedBy := "u1"
	usedAt := time.Now()
	h, _ := setupInviteHandler(t,
		&model.RegistrationTicket{ID: "t1", Code: "c1"},
		&model.RegistrationTicket{ID: "t2", Code: "c2", UsedByID: &usedBy, UsedAt: &usedAt},
	)

	rec := doPost(h.InviteList, `{"type":"unused"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "t1", rows[0]["id"])
	assert.Equal(t, false, rows[0]["used"])
}

// #1545: sort=-createdAt は id ASC (古い順)、default は id DESC。
func TestInviteList_Sort(t *testing.T) {
	h, _ := setupInviteHandler(t,
		&model.RegistrationTicket{ID: "t1", Code: "c1"},
		&model.RegistrationTicket{ID: "t2", Code: "c2"},
		&model.RegistrationTicket{ID: "t3", Code: "c3"},
	)
	// default (sort 省略) は id DESC。
	rec := doPost(h.InviteList, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 3)
	assert.Equal(t, "t3", rows[0]["id"])

	// -createdAt は id ASC。
	rec = doPost(h.InviteList, `{"sort":"-createdAt"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	rows = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 3)
	assert.Equal(t, "t1", rows[0]["id"])
}

// #1048 / #1049: createdBy / usedBy が hardcoded nil で返って frontend で
// "system" / "不明（メール認証待ち）" 表示に倒れていた regression の seal。
// userRepo を wire して bulk fetch + UserLite pack 経路を確認する。
func TestInviteList_PacksCreatedByAndUsedBy(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	adminName := "alice admin"
	signupName := "bob signed up"
	userRepo.Users["u_admin"] = &model.User{ID: "u_admin", Username: "alice", Name: &adminName}
	userRepo.Users["u_used"] = &model.User{ID: "u_used", Username: "bob", Name: &signupName}

	repo := testutil.NewMockRegistrationTicketRepository()
	createdBy := "u_admin"
	usedBy := "u_used"
	usedAt := time.Now()
	require.NoError(t, repo.Create(&model.RegistrationTicket{
		ID: "t1", Code: "c1", CreatedByID: &createdBy,
	}))
	require.NoError(t, repo.Create(&model.RegistrationTicket{
		ID: "t2", Code: "c2", CreatedByID: &createdBy,
		UsedByID: &usedBy, UsedAt: &usedAt,
	}))
	h.SetInviteRepo(repo)

	rec := doPost(h.InviteList, `{"type":"all"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)

	// 結果を id で lookup table 化 (List は id DESC で来るが順依存を避ける)
	byID := make(map[string]map[string]any, len(rows))
	for _, r := range rows {
		byID[r["id"].(string)] = r
	}

	// t1: createdBy のみ pack される、usedBy は null
	t1 := byID["t1"]
	require.NotNil(t, t1["createdBy"], "createdBy が UserLite で pack される")
	createdByT1, ok := t1["createdBy"].(map[string]any)
	require.True(t, ok, "createdBy は map (= UserLite shape)")
	assert.Equal(t, "u_admin", createdByT1["id"])
	assert.Equal(t, "alice", createdByT1["username"])
	assert.Nil(t, t1["usedBy"])

	// t2: createdBy + usedBy 両方 pack される
	t2 := byID["t2"]
	require.NotNil(t, t2["createdBy"])
	require.NotNil(t, t2["usedBy"])
	usedByT2, ok := t2["usedBy"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "u_used", usedByT2["id"])
	assert.Equal(t, "bob", usedByT2["username"])
}

// createdById / usedById が NULL の ticket (= migration 経由の旧 row 等) は
// createdBy / usedBy も null を返す (= 従来挙動と互換)。
func TestInviteList_NilUserIDsReturnNilPackedFields(t *testing.T) {
	h, _ := setupInviteHandler(t,
		&model.RegistrationTicket{ID: "t1", Code: "c1"}, // createdById / usedById 両方 nil
	)
	rec := doPost(h.InviteList, `{"type":"all"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0]["createdBy"])
	assert.Nil(t, rows[0]["usedBy"])
}

// --- Promo ------------------------------------------------------------------

// stubNoteFinder satisfies admin.NoteFinder with just FindByID.
type stubNoteFinder struct {
	note *model.Note
	err  error
}

func (s *stubNoteFinder) FindByID(_ string) (*model.Note, error) { return s.note, s.err }

type stubPromoRepo struct {
	created  *model.PromoNote
	exists   bool
	existErr error
}

func (s *stubPromoRepo) Create(p *model.PromoNote) error {
	s.created = p
	return nil
}
func (s *stubPromoRepo) ListActive(_ time.Time) ([]*model.PromoNote, error) { return nil, nil }
func (s *stubPromoRepo) Exists(_ string) (bool, error)                      { return s.exists, s.existErr }

func TestPromoCreate_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	promo := &stubPromoRepo{}
	h.SetPromoNoteRepo(promo)
	note := &model.Note{ID: "n1", UserID: "authorU", Visibility: model.NoteVisibilityPublic}
	h.SetNoteFinder(&stubNoteFinder{note: note})

	expires := time.Now().Add(24 * time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"noteId":"n1","expiresAt":%d}`, expires)
	rec := doPost(h.PromoCreate, body, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, promo.created)
	assert.Equal(t, "n1", promo.created.NoteID)
	assert.Equal(t, "authorU", promo.created.UserID)
}

// #1466: followers / home / specified visibility note の promote 試行は
// ACCESS_DENIED (403) で reject される。promo 表示 endpoint が実装された時に
// 非 public note の本文が viewer に漏れる latent IDOR を create 段で塞ぐ。
func TestPromoCreate_NonPublicNoteRejected(t *testing.T) {
	cases := []struct {
		name string
		vis  model.NoteVisibility
	}{
		{"followers", model.NoteVisibilityFollowers},
		{"home", model.NoteVisibilityHome},
		{"specified", model.NoteVisibilitySpecified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			promo := &stubPromoRepo{}
			h.SetPromoNoteRepo(promo)
			h.SetNoteFinder(&stubNoteFinder{note: &model.Note{ID: "n1", UserID: "authorU", Visibility: tc.vis}})

			expires := time.Now().Add(24 * time.Hour).UnixMilli()
			body := fmt.Sprintf(`{"noteId":"n1","expiresAt":%d}`, expires)
			rec := doPost(h.PromoCreate, body, adminUser)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "non-public note must be rejected with 403")
			assert.Nil(t, promo.created, "promo row must not be persisted for non-public note")

			var respBody map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &respBody))
			errField, ok := respBody["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "ACCESS_DENIED", errField["code"])
		})
	}
}

// #1469 review (指摘 2): promoNoteRepo は配線済み + noteFinder 未配線の状態で
// visibility check ができないまま promote が通る fail-open を塞ぐ。production の
// router.go では SetPromoNoteRepo の直後に SetNoteFinder が必ず呼ばれるため
// 経路に達しないが、設定ミス検出 + 兄弟 PR (#1467/#1468/#1460) の fail-closed
// doctrine と揃えるため明示 gate する。
func TestPromoCreate_NoteFinderUnwired_FailClosed(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	promo := &stubPromoRepo{}
	h.SetPromoNoteRepo(promo)
	// SetNoteFinder は故意に呼ばない

	expires := time.Now().Add(24 * time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"noteId":"n1","expiresAt":%d}`, expires)
	rec := doPost(h.PromoCreate, body, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"missing noteFinder must fail-closed instead of bypassing visibility check")
	assert.Nil(t, promo.created, "promo row must not be persisted when visibility cannot be verified")
}

func TestPromoCreate_MissingNoteID(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetPromoNoteRepo(&stubPromoRepo{})
	h.SetNoteFinder(&stubNoteFinder{})
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.PromoCreate, `{}`, adminUser).Code)
}

// #1539: expiresAt 欠落は upstream required により 400 (旧 mk-go は epoch 0 で保存)。
func TestPromoCreate_MissingExpiresAt(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetPromoNoteRepo(&stubPromoRepo{})
	h.SetNoteFinder(&stubNoteFinder{})
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.PromoCreate, `{"noteId":"n1"}`, adminUser).Code)
}

func TestPromoCreate_NoSuchNote(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetPromoNoteRepo(&stubPromoRepo{})
	// **not-found を返す。** 以前は汎用の `assertError{}` を渡して 400 を
	// 期待していたが、それは「DB 障害でも 400」を固定していた。#2792 で
	// 種別を分けたので、404 相当を出すのは not-found のときだけ。障害が
	// 500 になることは `TestAdmin_DBFailureIsNot4xx` が見る。
	h.SetNoteFinder(&stubNoteFinder{err: repository.ErrNotFound})
	rec := doPost(h.PromoCreate, `{"noteId":"missing","expiresAt":1}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPromoCreate_AlreadyPromoted(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetPromoNoteRepo(&stubPromoRepo{exists: true})
	h.SetNoteFinder(&stubNoteFinder{note: &model.Note{ID: "n1", UserID: "u", Visibility: model.NoteVisibilityPublic}})
	rec := doPost(h.PromoCreate, `{"noteId":"n1","expiresAt":1}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errField, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "ALREADY_PROMOTED", errField["code"])
}

// --- thin nil-repo smoke tests ---
//
// nil repo 経路 (newTestHandler は ad / avatar / invite / promo の repo を
// wire しない) で 204 / 200 が返ることを担保する軽量テスト。

func TestAdCreate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.AdCreate, `{}`, adminUser).Code)
}

func TestAdDelete(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.AdDelete, `{}`, adminUser).Code)
}

func TestAdList(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.AdList, `{}`, adminUser).Code)
}

func TestAdUpdate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.AdUpdate, `{}`, adminUser).Code)
}

func TestAvatarDecorationsCreate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.AvatarDecorationsCreate, `{}`, adminUser).Code)
}

func TestAvatarDecorationsDelete(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.AvatarDecorationsDelete, `{}`, adminUser).Code)
}

func TestAvatarDecorationsList(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.AvatarDecorationsList, `{}`, adminUser).Code)
}

func TestAvatarDecorationsUpdate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.AvatarDecorationsUpdate, `{}`, adminUser).Code)
}

func TestInviteCreate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.InviteCreate, `{}`, adminUser).Code)
}

// --- moderation log assertions (#662 + #665) ---

func TestAdCreate_WritesModerationLog(t *testing.T) {
	h, _ := setupAdHandler(t)
	repo := attachModLog(t, h)

	rec := doPost(h.AdCreate, `{"url":"https://x","imageUrl":"https://y"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "createAd", repo.Snapshot()[0].Type)
}

func TestAdUpdate_WritesModerationLog(t *testing.T) {
	h, _ := setupAdHandler(t,
		&model.Ad{ID: "ad1", URL: "https://x", ImageURL: "https://y"},
	)
	repo := attachModLog(t, h)

	url := "https://updated"
	body := fmt.Sprintf(`{"id":"ad1","url":%q}`, url)
	rec := doPost(h.AdUpdate, body, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "updateAd", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	require.NotNil(t, info["before"])
	require.NotNil(t, info["after"])
}

func TestAdDelete_WritesModerationLog(t *testing.T) {
	h, _ := setupAdHandler(t,
		&model.Ad{ID: "ad1", URL: "https://x", ImageURL: "https://y"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.AdDelete, `{"id":"ad1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "deleteAd", repo.Snapshot()[0].Type)
}

func TestInviteCreate_WritesModerationLog(t *testing.T) {
	h, _ := setupInviteHandler(t)
	repo := attachModLog(t, h)

	rec := doPost(h.InviteCreate, `{"count":2}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "createInvitation", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	require.NotNil(t, info["invitations"])
}

func TestAvatarDecorationsCreate_WritesModerationLog(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	repo := attachModLog(t, h)

	rec := doPost(h.AvatarDecorationsCreate, `{"name":"crown","url":"https://x"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "createAvatarDecoration", repo.Snapshot()[0].Type)
}

func TestAvatarDecorationsUpdate_WritesModerationLog(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t,
		&model.AvatarDecoration{ID: "d1", Name: "crown"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.AvatarDecorationsUpdate, `{"id":"d1","name":"jewel"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "updateAvatarDecoration", repo.Snapshot()[0].Type)
}

func TestAvatarDecorationsDelete_WritesModerationLog(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t,
		&model.AvatarDecoration{ID: "d1", Name: "doomed"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.AvatarDecorationsDelete, `{"id":"d1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "deleteAvatarDecoration", repo.Snapshot()[0].Type)
}

func TestInviteList(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.InviteList, `{}`, adminUser).Code)
}

func TestPromoCreate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.PromoCreate, `{}`, adminUser).Code)
}

// catalogInvalidatorSpy counts Invalidate() calls from the avatar decoration
// mutation handlers.
type catalogInvalidatorSpy struct{ calls int }

func (s *catalogInvalidatorSpy) Invalidate() { s.calls++ }

// avatar-decorations の create / update / delete が entity 側 catalog cache を
// invalidate すること (#2258)。TTL 任せだと作成直後に装着された decoration が
// packer の lookup miss で silent drop され、DB に行があるのに API 応答は
// avatarDecorations: [] になる。
func TestAvatarDecorations_MutationsInvalidateCatalogCache(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		h, _ := setupAvatarDecorationHandler(t)
		spy := &catalogInvalidatorSpy{}
		h.SetAvatarDecorationInvalidator(spy)

		rec := doPost(h.AvatarDecorationsCreate,
			`{"name":"deco","url":"https://example.test/d.png"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, 1, spy.calls)
	})

	t.Run("update", func(t *testing.T) {
		h, _ := setupAvatarDecorationHandler(t, &model.AvatarDecoration{
			ID: "d1", Name: "deco", URL: "https://example.test/d.png",
		})
		spy := &catalogInvalidatorSpy{}
		h.SetAvatarDecorationInvalidator(spy)

		rec := doPost(h.AvatarDecorationsUpdate, `{"id":"d1","name":"renamed"}`, adminUser)
		require.Less(t, rec.Code, 300)
		assert.Equal(t, 1, spy.calls)
	})

	t.Run("delete", func(t *testing.T) {
		h, _ := setupAvatarDecorationHandler(t, &model.AvatarDecoration{
			ID: "d1", Name: "deco", URL: "https://example.test/d.png",
		})
		spy := &catalogInvalidatorSpy{}
		h.SetAvatarDecorationInvalidator(spy)

		rec := doPost(h.AvatarDecorationsDelete, `{"id":"d1"}`, adminUser)
		require.Less(t, rec.Code, 300)
		assert.Equal(t, 1, spy.calls)
	})
}

// **結果に影響しないカーソルでも 400 (#3025)。** upstream の paramDef は
// `misskey:id` なので ajv が弾く。ここだけ 200 を返すと「カーソルは 400」という
// 規則の例外になり、`untilId` を bind する handler を機械的に検査できなくなる。
func TestAvatarDecorationsList_RejectsUnstorableCursor(t *testing.T) {
	h, _ := setupAvatarDecorationHandler(t)
	rec := doPost(h.AvatarDecorationsList, `{"untilId":"a\u0000b"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"列に入らないカーソルを 200 で受けている: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}
