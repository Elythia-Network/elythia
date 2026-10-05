package repository

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func cleanupInvite(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		testDB.Exec(`DELETE FROM "registration_ticket" WHERE id = ?`, id)
	}
}

func TestRegistrationTicketRepository_CreateAndListAll(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)

	// 前回の失敗テスト残骸を掃除してから始める。
	cleanupInvite(t, "rt_unused", "rt_used", "rt_exp")

	// usedBy は FK 制約 → user テーブルに実体が必要。テスト用の user を作る。
	u := insertTestUser(t, "rt_user", "rtu")
	defer cleanupUser(t, u.ID)

	unused := &model.RegistrationTicket{ID: "rt_unused", Code: "uuu-unused"}
	usedAt := time.Now().UTC().Truncate(time.Second)
	used := &model.RegistrationTicket{ID: "rt_used", Code: "uuu-used", UsedByID: &u.ID, UsedAt: &usedAt}
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	expired := &model.RegistrationTicket{ID: "rt_exp", Code: "uuu-exp", ExpiresAt: &past}

	require.NoError(t, repo.Create(unused))
	require.NoError(t, repo.Create(used))
	require.NoError(t, repo.Create(expired))
	defer cleanupInvite(t, unused.ID, used.ID, expired.ID)

	all, err := repo.List(RegistrationTicketAll, 100, 0, time.Now())
	require.NoError(t, err)
	ids := collectIDs(all)
	assert.Contains(t, ids, "rt_unused")
	assert.Contains(t, ids, "rt_used")
	assert.Contains(t, ids, "rt_exp")

	unusedRows, err := repo.List(RegistrationTicketUnused, 100, 0, time.Now())
	require.NoError(t, err)
	assert.Contains(t, collectIDs(unusedRows), "rt_unused")
	assert.NotContains(t, collectIDs(unusedRows), "rt_used")

	usedRows, err := repo.List(RegistrationTicketUsed, 100, 0, time.Now())
	require.NoError(t, err)
	assert.Contains(t, collectIDs(usedRows), "rt_used")
	assert.NotContains(t, collectIDs(usedRows), "rt_unused")

	expiredRows, err := repo.List(RegistrationTicketExpired, 100, 0, time.Now())
	require.NoError(t, err)
	assert.Contains(t, collectIDs(expiredRows), "rt_exp")
	assert.NotContains(t, collectIDs(expiredRows), "rt_unused")
}

func TestRegistrationTicketRepository_LimitOffsetDefaults(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	// 負数 / 0 は default に丸められる
	_, err := repo.List(RegistrationTicketAll, 0, -1, time.Now())
	require.NoError(t, err)
}

func collectIDs(rows []*model.RegistrationTicket) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// FindByCode / MarkUsed / FindByIDForUpdateTx / MarkUsedTx の挙動検証
// (#600 item 2 + #604 で追加されたメソッド)。
func TestRegistrationTicketRepository_FindByCodeAndMarkUsed(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_fc")
	defer cleanupInvite(t, "rt_fc")

	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_fc", Code: "rtfc_code"}))

	// FindByCode happy path
	got, err := repo.FindByCode("rtfc_code")
	require.NoError(t, err)
	assert.Equal(t, "rt_fc", got.ID)

	// FindByCode: not found
	_, err = repo.FindByCode("nonexistent")
	assert.Error(t, err)

	// MarkUsed happy path
	createTestUser(t, "rt_fc_user")
	defer testDB.Exec(`DELETE FROM "user" WHERE id = ?`, "rt_fc_user")
	require.NoError(t, repo.MarkUsed("rt_fc", "rt_fc_user"))

	got2, err := repo.FindByCode("rtfc_code")
	require.NoError(t, err)
	require.NotNil(t, got2.UsedByID)
	assert.Equal(t, "rt_fc_user", *got2.UsedByID)
	require.NotNil(t, got2.UsedAt)
}

// FindByIDForUpdateTx と MarkUsedTx は同 transaction 内で動作する想定。
// transaction を張って 2 操作を回し、commit 後に反映を確認する。
func TestRegistrationTicketRepository_TxMethods(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_tx")
	defer cleanupInvite(t, "rt_tx")

	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_tx", Code: "rttx_code"}))
	createTestUser(t, "rt_tx_user")
	defer testDB.Exec(`DELETE FROM "user" WHERE id = ?`, "rt_tx_user")

	err := testDB.Transaction(func(tx *gorm.DB) error {
		ticket, err := repo.FindByIDForUpdateTx(tx, "rt_tx")
		if err != nil {
			return err
		}
		assert.Equal(t, "rt_tx", ticket.ID)
		return repo.MarkUsedTx(tx, "rt_tx", "rt_tx_user")
	})
	require.NoError(t, err)

	got, err := repo.FindByCode("rttx_code")
	require.NoError(t, err)
	require.NotNil(t, got.UsedByID)
	assert.Equal(t, "rt_tx_user", *got.UsedByID)
}

// FindByIDForUpdateTx で存在しない ID を引いたら error
func TestRegistrationTicketRepository_FindByIDForUpdateTx_NotFound(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	err := testDB.Transaction(func(tx *gorm.DB) error {
		_, err := repo.FindByIDForUpdateTx(tx, "nonexistent")
		return err
	})
	assert.Error(t, err)
}

// CountByCreatorSince は creatorID + id > sinceID の組み合わせで rolling
// window count を返す (#1029 PR-2 で invite/create + invite/limit が使う)。
func TestRegistrationTicketRepository_CountByCreatorSince(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_cc_1", "rt_cc_2", "rt_cc_3", "rt_cc_other")
	defer cleanupInvite(t, "rt_cc_1", "rt_cc_2", "rt_cc_3", "rt_cc_other")
	u := insertTestUser(t, "rt_cc_u", "rtccu")
	defer cleanupUser(t, u.ID)
	other := insertTestUser(t, "rt_cc_o", "rtcco")
	defer cleanupUser(t, other.ID)

	// creatorID と id (時刻 prefix) で window を切る。アルファベット順比較で
	// id > sinceID を満たすので rt_cc_2 / rt_cc_3 のみ count される。
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_cc_1", Code: "ccc-1", CreatedByID: &u.ID}))
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_cc_2", Code: "ccc-2", CreatedByID: &u.ID}))
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_cc_3", Code: "ccc-3", CreatedByID: &u.ID}))
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_cc_other", Code: "ccc-other", CreatedByID: &other.ID}))

	count, err := repo.CountByCreatorSince(u.ID, "rt_cc_1")
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	// 他 creator は count されない
	count, err = repo.CountByCreatorSince(u.ID, "")
	require.NoError(t, err)
	assert.Equal(t, int64(3), count)

	count, err = repo.CountByCreatorSince(other.ID, "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

// `createdById` が NULL の行は creator 系のどの query にも出ない (#2805 / #2813)。
//
// **これは SQL の性質そのものを見るテスト。** 承認由来の ticket は `createdById` を
// 入れないので、審査した管理者の `invite/create` 上限 (`CountByCreatorSince`) も
// 個人の `invite/list` (`ListByCreator`) も食わない。`WHERE "createdById" = ?` は
// NULL に当たらない、という一点に全部が乗っている。**手書きの fake では再現でき
// ない**ので実 DB で押さえる。`admin/invite/list` が使う `ListSorted` には出る。
func TestRegistrationTicketRepository_NullCreatorIsExcludedFromCreatorQueries(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_nc_owned", "rt_nc_null")
	defer cleanupInvite(t, "rt_nc_owned", "rt_nc_null")
	u := insertTestUser(t, "rt_nc_u", "rtncu")
	defer cleanupUser(t, u.ID)

	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_nc_owned", Code: "nc-owned", CreatedByID: &u.ID}))
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_nc_null", Code: "nc-null"}))

	count, err := repo.CountByCreatorSince(u.ID, "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "NULL の行は上限に数えない")

	owned, err := repo.ListByCreator(u.ID, "", "", 100)
	require.NoError(t, err)
	require.Len(t, owned, 1, "NULL の行は個人の一覧に出ない")
	assert.Equal(t, "rt_nc_owned", owned[0].ID)

	rows, err := repo.ListSorted("", "", 100, 0, time.Now())
	require.NoError(t, err)
	found := false
	for _, r := range rows {
		if r.ID == "rt_nc_null" {
			found = true
		}
	}
	assert.True(t, found, "admin 側の一覧には出る")
}

// cancelled context で error 経路を通すことで coverage を担保する。
func TestRegistrationTicketRepository_CountByCreatorSince_Error(t *testing.T) {
	repo := NewRegistrationTicketRepository(cancelledDB(t))
	_, err := repo.CountByCreatorSince("any", "")
	assert.Error(t, err)
}

// FindByID は invite/delete の access check で使う。
func TestRegistrationTicketRepository_FindByID(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_fid")
	defer cleanupInvite(t, "rt_fid")
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_fid", Code: "fid-code"}))

	got, err := repo.FindByID("rt_fid")
	require.NoError(t, err)
	assert.Equal(t, "rt_fid", got.ID)

	_, err = repo.FindByID("missing")
	assert.Error(t, err)
}

func TestRegistrationTicketRepository_FindByID_DBError(t *testing.T) {
	repo := NewRegistrationTicketRepository(cancelledDB(t))
	_, err := repo.FindByID("any")
	assert.Error(t, err)
}

// ListByCreator は invite/list で「自分の発行 invite」を絞り込む。
func TestRegistrationTicketRepository_ListByCreator(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_lc_1", "rt_lc_2", "rt_lc_o")
	defer cleanupInvite(t, "rt_lc_1", "rt_lc_2", "rt_lc_o")
	u := insertTestUser(t, "rt_lc_u", "rtlcu")
	defer cleanupUser(t, u.ID)
	other := insertTestUser(t, "rt_lc_o", "rtlco")
	defer cleanupUser(t, other.ID)

	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_lc_1", Code: "lc-1", CreatedByID: &u.ID}))
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_lc_2", Code: "lc-2", CreatedByID: &u.ID}))
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_lc_o", Code: "lc-o", CreatedByID: &other.ID}))

	// 自 user のみ取得 + DESC sort
	rows, err := repo.ListByCreator(u.ID, "", "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "rt_lc_2", rows[0].ID, "id DESC: 大きい方が先")
	assert.Equal(t, "rt_lc_1", rows[1].ID)

	// since cursor: rt_lc_1 より大きい id のみ → rt_lc_2 のみ
	rows, err = repo.ListByCreator(u.ID, "rt_lc_1", "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "rt_lc_2", rows[0].ID)

	// until cursor: rt_lc_2 より小さい id のみ → rt_lc_1 のみ
	rows, err = repo.ListByCreator(u.ID, "", "rt_lc_2", 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "rt_lc_1", rows[0].ID)

	// limit clamp (0 -> 30, > 100 -> 100)
	rows, err = repo.ListByCreator(u.ID, "", "", 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	rows, err = repo.ListByCreator(u.ID, "", "", 9999)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestRegistrationTicketRepository_ListByCreator_DBError(t *testing.T) {
	repo := NewRegistrationTicketRepository(cancelledDB(t))
	_, err := repo.ListByCreator("any", "", "", 10)
	assert.Error(t, err)
}

// #1545: ListSorted の sort enum (createdAt = id, usedAt 順 + NULLS 配置)。
func TestRegistrationTicketRepository_ListSorted(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rts_a", "rts_b", "rts_c")
	u := insertTestUser(t, "rts_user", "rtsu")
	defer cleanupUser(t, u.ID)

	t1 := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	t2 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	// a: usedAt=t1, b: usedAt=t2, c: 未使用 (usedAt nil)。id は a<b<c。
	a := &model.RegistrationTicket{ID: "rts_a", Code: "rts-a", UsedByID: &u.ID, UsedAt: &t1}
	b := &model.RegistrationTicket{ID: "rts_b", Code: "rts-b", UsedByID: &u.ID, UsedAt: &t2}
	c := &model.RegistrationTicket{ID: "rts_c", Code: "rts-c"}
	for _, tk := range []*model.RegistrationTicket{a, b, c} {
		require.NoError(t, repo.Create(tk))
	}
	defer cleanupInvite(t, a.ID, b.ID, c.ID)

	only := func(rows []*model.RegistrationTicket) []string {
		var out []string
		for _, r := range rows {
			switch r.ID {
			case "rts_a", "rts_b", "rts_c":
				out = append(out, r.ID)
			}
		}
		return out
	}

	// -createdAt = id ASC。
	asc, err := repo.ListSorted(RegistrationTicketAll, "-createdAt", 100, 0, time.Now())
	require.NoError(t, err)
	got := only(asc)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"rts_a", "rts_b", "rts_c"}, got)

	// +createdAt = id DESC。
	desc, err := repo.ListSorted(RegistrationTicketAll, "+createdAt", 100, 0, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{"rts_c", "rts_b", "rts_a"}, only(desc))

	// +usedAt = usedAt DESC NULLS LAST (b=t2 newest, a=t1, c=nil last)。
	uDesc, err := repo.ListSorted(RegistrationTicketAll, "+usedAt", 100, 0, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{"rts_b", "rts_a", "rts_c"}, only(uDesc))

	// -usedAt = usedAt ASC NULLS FIRST (c=nil first, a=t1, b=t2)。
	uAsc, err := repo.ListSorted(RegistrationTicketAll, "-usedAt", 100, 0, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{"rts_c", "rts_a", "rts_b"}, only(uAsc))
}

// #2083: MarkPending は usedAt + pendingUserId をセットし usedById は立てない
// (確認メール再送防止 ticket 予約)。pendingUserId は user_pending 行の ID で user に
// 存在しないため、000064 で FK を撤去済 (FK が残っていると本テストが FK 違反で落ちる)。
func TestRegistrationTicketRepository_MarkPending(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_mp")
	defer cleanupInvite(t, "rt_mp")
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_mp", Code: "rtmp_code"}))

	// pendingID は user に存在しない user_pending ID (FK 撤去後なので書ける)。
	require.NoError(t, repo.MarkPending("rt_mp", "pending_xyz"))

	got, err := repo.FindByCode("rtmp_code")
	require.NoError(t, err)
	require.NotNil(t, got.UsedAt, "usedAt がセットされる")
	require.NotNil(t, got.PendingID)
	assert.Equal(t, "pending_xyz", *got.PendingID)
	assert.Nil(t, got.UsedByID, "MarkPending は usedById を立てない")
}

// **同じコードで並行に確保しても 1 件だけが取れる。** 検証 (読み取り) と消費の間に
// 隙間があると 1 枚の招待コードで複数のアカウントができる。条件付き UPDATE の
// 影響行数で 1 件だけを通すことを、実 DB に並行で投げて確かめる。
func TestRegistrationTicketRepository_ClaimForSignup_OnlyOneWins(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_claim")
	defer cleanupInvite(t, "rt_claim")
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_claim", Code: "rtclaim_code"}))

	const n = 10
	var wg sync.WaitGroup
	var wins atomic.Int32
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := repo.ClaimForSignup("rt_claim", false)
			assert.NoError(t, err)
			if ok {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load())
}

func TestRegistrationTicketRepository_ClaimAndRelease(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	cleanupInvite(t, "rt_claim2")
	defer cleanupInvite(t, "rt_claim2")
	require.NoError(t, repo.Create(&model.RegistrationTicket{ID: "rt_claim2", Code: "rtclaim2_code"}))

	ok, err := repo.ClaimForSignup("rt_claim2", false)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = repo.ClaimForSignup("rt_claim2", false)
	require.NoError(t, err)
	assert.False(t, ok, "確保済みは取れない")

	// 失敗した登録の後に戻すと、また取れる。
	require.NoError(t, repo.ReleaseClaim("rt_claim2"))
	ok, err = repo.ClaimForSignup("rt_claim2", false)
	require.NoError(t, err)
	assert.True(t, ok)

	// メール確認制: 確認メールから 30 分を過ぎた確保は取り直せる。
	past := time.Now().Add(-SignupConfirmationResendWindow - time.Minute)
	require.NoError(t, testDB.Exec(`UPDATE "registration_ticket" SET "usedAt" = ? WHERE id = ?`, past, "rt_claim2").Error)
	ok, err = repo.ClaimForSignup("rt_claim2", false)
	require.NoError(t, err)
	assert.False(t, ok, "メール確認制でなければ期限切れの確保も取れない")
	ok, err = repo.ClaimForSignup("rt_claim2", true)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = repo.ClaimForSignup("rt_claim2", true)
	require.NoError(t, err)
	assert.False(t, ok, "30 分以内の確保は取れない")

	// アカウントに紐付いたら、戻しても未使用に戻らない。
	createTestUser(t, "rt_claim2_user")
	defer testDB.Exec(`DELETE FROM "user" WHERE id = ?`, "rt_claim2_user")
	require.NoError(t, repo.MarkUsed("rt_claim2", "rt_claim2_user"))
	require.NoError(t, repo.ReleaseClaim("rt_claim2"))
	got, err := repo.FindByCode("rtclaim2_code")
	require.NoError(t, err)
	assert.NotNil(t, got.UsedAt)
	ok, err = repo.ClaimForSignup("rt_claim2", true)
	require.NoError(t, err)
	assert.False(t, ok)
}
