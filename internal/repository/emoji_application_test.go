package repository

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

func cleanupEmojiApplications(t *testing.T) {
	t.Helper()
	testDB.Exec(`DELETE FROM "emoji_application"`)
}

func seedApplication(t *testing.T, id, userID, name, status string) *model.EmojiApplication {
	t.Helper()
	now := time.Now()
	app := &model.EmojiApplication{
		ID: id, UserID: userID, Kind: model.EmojiApplicationKindOwn,
		Status: status, Name: name, License: "自作",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, NewEmojiApplicationRepository(testDB).Create(app))
	return app
}

// **同じ人が同じ名前で審査待ちを積み増せないこと。** 部分一意索引が効いているか
// を実 DB で見る。番兵に落とさないと、利用者には「サーバーエラー」としか見えない。
func TestEmojiApplicationRepository_DuplicatePending(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_u1")
	repo := NewEmojiApplicationRepository(testDB)

	seedApplication(t, "ea_a1", "ea_u1", "sushi", model.EmojiApplicationPending)

	dup := &model.EmojiApplication{
		ID: "ea_a2", UserID: "ea_u1", Kind: model.EmojiApplicationKindOwn,
		Status: model.EmojiApplicationPending, Name: "sushi", License: "自作",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.ErrorIs(t, repo.Create(dup), ErrEmojiApplicationDuplicatePending)

	// **却下された後は同じ名前で出し直せること。** 条件を付けずに一意制約を
	// 張ると、ここが通らなくなる。
	testDB.Exec(`UPDATE "emoji_application" SET "status" = ? WHERE "id" = ?`,
		model.EmojiApplicationRejected, "ea_a1")
	require.NoError(t, repo.Create(dup))
}

// **一覧が呼び出し元の申請だけを返すこと。** 絞りを落とすと
// `/api/emoji-application/list-mine` が全員の申請 (却下理由・モデレーターへの
// 補足を含む) を返す。handler 側のテストは userID を「渡すこと」しか見ていない
// ので、ここで「使うこと」を固定する。
func TestEmojiApplicationRepository_ListByUserScopes(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_mine")
	createTestUser(t, "ea_other")
	repo := NewEmojiApplicationRepository(testDB)

	seedApplication(t, "ea_b1", "ea_mine", "mine1", model.EmojiApplicationPending)
	seedApplication(t, "ea_b2", "ea_other", "theirs", model.EmojiApplicationPending)
	seedApplication(t, "ea_b3", "ea_mine", "mine2", model.EmojiApplicationRejected)

	rows, err := repo.ListByUser("ea_mine", 50, "")
	require.NoError(t, err)
	require.Len(t, rows, 2, "他人の申請が混ざっている")
	for _, r := range rows {
		require.Equal(t, "ea_mine", r.UserID)
	}
	// 新しい順。
	require.Equal(t, "ea_b3", rows[0].ID)
}

func TestEmojiApplicationRepository_ListFilters(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_u2")
	repo := NewEmojiApplicationRepository(testDB)

	seedApplication(t, "ea_c1", "ea_u2", "pend", model.EmojiApplicationPending)
	seedApplication(t, "ea_c2", "ea_u2", "appr", model.EmojiApplicationApproved)
	seedApplication(t, "ea_c3", "ea_u2", "rej", model.EmojiApplicationRejected)

	pending, err := repo.List(EmojiApplicationFilterPending, 50, "")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "ea_c1", pending[0].ID)

	processed, err := repo.List(EmojiApplicationFilterProcessed, 50, "")
	require.NoError(t, err)
	require.Len(t, processed, 2, "審査済みが 2 件返らない")

	all, err := repo.List(EmojiApplicationFilterAll, 50, "")
	require.NoError(t, err)
	require.Len(t, all, 3)

	n, err := repo.CountPending()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	// keyset ページング。
	page, err := repo.List(EmojiApplicationFilterAll, 50, "ea_c2")
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "ea_c1", page[0].ID)
}

// **条件付き UPDATE が SQL レベルで効いていること (#2934 レビュー M1 / R2)。**
//
// service のテストは偽物が条件を Go で再実装しているので、本物の SQL を壊しても
// 緑になる。ここが唯一の担保。
func TestEmojiApplicationRepository_UpdateIfPending(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_u3")
	createTestUser(t, "ea_mod")
	repo := NewEmojiApplicationRepository(testDB)

	app := seedApplication(t, "ea_d1", "ea_u3", "sushi", model.EmojiApplicationPending)

	now := time.Now()
	emojiID, mod := "e-1", "ea_mod"
	app.Status = model.EmojiApplicationApproved
	app.EmojiID = &emojiID
	app.ProcessedByID = &mod
	app.ProcessedAt = &now
	app.UpdatedAt = now

	ok, err := repo.UpdateIfPending(app)
	require.NoError(t, err)
	require.True(t, ok)

	stored, err := repo.FindByID("ea_d1")
	require.NoError(t, err)
	require.Equal(t, model.EmojiApplicationApproved, stored.Status)
	require.Equal(t, "e-1", *stored.EmojiID, "emojiId が書かれていない")
	require.Equal(t, "ea_mod", *stored.ProcessedByID)
	require.NotNil(t, stored.ProcessedAt)
	// 触っていない列が壊れていないこと。
	require.Equal(t, "sushi", stored.Name)

	// **2 回目は負ける。** これが last-write-wins を防いでいる本体。
	reason := "潰れて読めません"
	app.Status = model.EmojiApplicationRejected
	app.RejectReason = &reason
	app.EmojiID = nil
	ok, err = repo.UpdateIfPending(app)
	require.NoError(t, err)
	require.False(t, ok, "処理済みの申請を上書きできてしまう")

	after, err := repo.FindByID("ea_d1")
	require.NoError(t, err)
	require.Equal(t, model.EmojiApplicationApproved, after.Status, "承認が却下で上書きされた")
	require.Equal(t, "e-1", *after.EmojiID, "emojiId が消えた")
	require.Nil(t, after.RejectReason)
}

// 却下では emojiId が NULL として書かれること (map の nil が反映される)。
func TestEmojiApplicationRepository_UpdateIfPendingWritesNull(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_u4")
	repo := NewEmojiApplicationRepository(testDB)

	app := seedApplication(t, "ea_e1", "ea_u4", "kusa", model.EmojiApplicationPending)
	reason := "だめ"
	app.Status = model.EmojiApplicationRejected
	app.RejectReason = &reason
	app.UpdatedAt = time.Now()

	ok, err := repo.UpdateIfPending(app)
	require.NoError(t, err)
	require.True(t, ok)

	stored, err := repo.FindByID("ea_e1")
	require.NoError(t, err)
	require.Equal(t, model.EmojiApplicationRejected, stored.Status)
	require.Equal(t, "だめ", *stored.RejectReason)
	require.Nil(t, stored.EmojiID)
}

func TestEmojiApplicationRepository_FindByIDNotFound(t *testing.T) {
	cleanupEmojiApplications(t)
	_, err := NewEmojiApplicationRepository(testDB).FindByID("missing")
	require.True(t, IsNotFound(err))
}

// seedApplicationAt inserts an application with an explicit createdAt so the
// rolling windows can be exercised without sleeping.
func seedApplicationAt(t *testing.T, id, userID, name, status string, createdAt time.Time) {
	t.Helper()
	app := &model.EmojiApplication{
		ID: id, UserID: userID, Kind: model.EmojiApplicationKindOwn,
		Status: status, Name: name, License: "自作",
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	require.NoError(t, NewEmojiApplicationRepository(testDB).Create(app))
}

func quotaApp(id, userID, name string, createdAt time.Time) *model.EmojiApplication {
	return &model.EmojiApplication{
		ID: id, UserID: userID, Kind: model.EmojiApplicationKindOwn,
		Status: model.EmojiApplicationPending, Name: name, License: "自作",
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}

// **境界を実 DB で固定する (#2958)。** 上限ちょうどの手前までは通り、達したら
// 弾く。`<` と `<=` を取り違えると 1 件多く通るが、単体では気付けない。
func TestEmojiApplicationRepository_CreateWithQuota_Boundary(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q1")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 2}}}

	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q1a", "ea_q1", "a", now), limits))
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q1b", "ea_q1", "b", now), limits))

	err := repo.CreateWithQuota(quotaApp("ea_q1c", "ea_q1", "c", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.Equal(t, "day", qe.Window.Name)
	require.Equal(t, 2, qe.Used)
	// 最古の 1 件が窓を出る時刻。ミリ秒までは一致しないので幅で見る。
	require.WithinDuration(t, now.Add(24*time.Hour), qe.RetryAt, time.Minute)

	// **弾いたときは行を作らないこと。** rollback が効いていないと、上限に
	// 達した後も申請が積み上がる。
	var n int64
	require.NoError(t, testDB.Model(&model.EmojiApplication{}).
		Where(`"userId" = ?`, "ea_q1").Count(&n).Error)
	require.Equal(t, int64(2), n)
}

// **窓の外は数えない。** 期間の引き算を間違えると、一度上限に達した人が
// 永久に申請できなくなる。
func TestEmojiApplicationRepository_CreateWithQuota_WindowExpiry(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q2")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	seedApplicationAt(t, "ea_q2a", "ea_q2", "a", model.EmojiApplicationRejected, now.Add(-25*time.Hour))

	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 1}}}
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q2b", "ea_q2", "b", now), limits))
}

// **却下・取り下げでも枠は戻らない (#2958 の完了条件)。** 戻すと、申請と
// 取り下げを繰り返すだけでモデレーターへの通知を無限に作れる。
func TestEmojiApplicationRepository_CreateWithQuota_CountsAllStatuses(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q3")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	for i, st := range []string{
		model.EmojiApplicationRejected,
		model.EmojiApplicationCanceled,
		model.EmojiApplicationApproved,
	} {
		seedApplicationAt(t, "ea_q3"+string(rune('a'+i)), "ea_q3",
			string(rune('a'+i)), st, now.Add(-time.Duration(i+1)*time.Hour))
	}

	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "week", Duration: 7 * 24 * time.Hour, Max: 3}}}
	err := repo.CreateWithQuota(quotaApp("ea_q3z", "ea_q3", "z", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.Equal(t, 3, qe.Used)
}

// **数えるのは自分の分だけ。** 絞りを落とすと、誰かが上限まで申請した時点で
// インスタンス全員が申請できなくなる。
func TestEmojiApplicationRepository_CreateWithQuota_ScopedToUser(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q4")
	createTestUser(t, "ea_q4other")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	seedApplicationAt(t, "ea_q4x", "ea_q4other", "x", model.EmojiApplicationPending, now)

	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 1}}}
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q4a", "ea_q4", "a", now), limits))
}

// **0 は無制限 (#2958 の完了条件)。** 既定値が 0 なので、ここが上限として
// 効くと既存のインスタンスで申請が全て塞がる。
func TestEmojiApplicationRepository_CreateWithQuota_ZeroIsUnlimited(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q5")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	limits := QuotaLimits{Windows: []QuotaWindow{
		{Name: "day", Duration: 24 * time.Hour, Max: 0},
		{Name: "week", Duration: 7 * 24 * time.Hour, Max: 0},
	}}
	for i := 0; i < 3; i++ {
		require.NoError(t, repo.CreateWithQuota(
			quotaApp("ea_q5"+string(rune('a'+i)), "ea_q5", string(rune('a'+i)), now), limits))
	}
	// windows 自体が空でも同じ
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q5z", "ea_q5", "z", now), QuotaLimits{}))
}

// **狭い窓が先に効くこと。** 日次 1 / 月次 5 のとき、2 件目は日次で弾かれる。
// 窓を評価する順序に依存していると、返す period が実態と食い違う。
func TestEmojiApplicationRepository_CreateWithQuota_NarrowestWindowWins(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q6")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	limits := QuotaLimits{Windows: []QuotaWindow{
		{Name: "month", Duration: 30 * 24 * time.Hour, Max: 5},
		{Name: "day", Duration: 24 * time.Hour, Max: 1},
	}}
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q6a", "ea_q6", "a", now), limits))
	err := repo.CreateWithQuota(quotaApp("ea_q6b", "ea_q6", "b", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.Equal(t, "day", qe.Window.Name)
}

// **複数の窓が同時に満杯なら、いちばん遅く空くものを案内すること (#2958)。**
// 最初に満杯だったものを返すと、案内した時刻に叩いてもまだ別の窓が満杯で、
// もう一度 429 になる (1 周目で最古を返していたのと同じ失敗形)。
func TestEmojiApplicationRepository_CreateWithQuota_LatestFullWindowWins(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q13")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	// week の窓 (168h) にだけ入る 2 件と、day の窓にも入る 3 件。
	// day は 3/3 で満杯、week は 5/5 で満杯。
	for i, h := range []int{72, 60, 6, 4, 2} {
		seedApplicationAt(t, "ea_q13"+string(rune('a'+i)), "ea_q13",
			string(rune('a'+i)), model.EmojiApplicationPending, now.Add(-time.Duration(h)*time.Hour))
	}
	limits := QuotaLimits{Windows: []QuotaWindow{
		{Name: "day", Duration: 24 * time.Hour, Max: 3},
		{Name: "week", Duration: 7 * 24 * time.Hour, Max: 5},
	}}

	err := repo.CreateWithQuota(quotaApp("ea_q13z", "ea_q13", "z", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	// day は最古 (now-6h) が抜ければ空く → now+18h。
	// week は最古 (now-72h) が抜ければ空く → now+96h。**遅い方を返す。**
	require.Equal(t, "week", qe.Window.Name)
	require.WithinDuration(t, now.Add(96*time.Hour), qe.RetryAt, time.Minute)

	// 案内した時刻には本当に通ること (両方の窓に空きができている)。
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q13z", "ea_q13", "z", qe.RetryAt), limits))
}

// **案内した時刻ちょうどで通ること。** 窓の判定は `createdAt >= since` なので
// 境界の行はまだ窓の中にいる。`entity.ISOMillis` は切り捨てるので、境界を
// そのまま広告すると、その値で再スケジュールするクライアントが必ず空振りする。
func TestEmojiApplicationRepository_CreateWithQuota_RetryAtIsInclusive(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q14")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	seedApplicationAt(t, "ea_q14a", "ea_q14", "a", model.EmojiApplicationPending, now.Add(-time.Hour))

	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 1}}}
	err := repo.CreateWithQuota(quotaApp("ea_q14z", "ea_q14", "z", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q14z", "ea_q14", "z", qe.RetryAt), limits))
}

// **重複の番兵は上限経路でも維持すること。** CreateWithQuota が Create を
// 置き換えるので、ここで潰すと「審査待ちが既にある」が 500 に化ける。
func TestEmojiApplicationRepository_CreateWithQuota_DuplicatePending(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q7")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 10}}}
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q7a", "ea_q7", "same", now), limits))
	require.ErrorIs(t,
		repo.CreateWithQuota(quotaApp("ea_q7b", "ea_q7", "same", now), limits),
		ErrEmojiApplicationDuplicatePending)
}

// **同時に投げても上限を超えないこと (#2958 の完了条件)。** 数える側と作る側を
// 別のトランザクションに分けると、両方が「空きあり」を読んで両方通る。
func TestEmojiApplicationRepository_CreateWithQuota_Concurrent(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q8")
	now := time.Now()
	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 3}}}

	const n = 8
	// **共有プールを張り替えない。** `testDB` は同パッケージの全テストが使う
	// ので、`SetMaxIdleConns` を書き換えて戻す形にすると、戻し漏れが後続の
	// テストへ波及する (#2795 が `internal/server` で踏んだ型)。しかも
	// `database/sql` は `MaxIdleConns` を読み戻せないので save/restore が
	// 書けない。goroutine ごとに専用のハンドルを開けば、既定のプール
	// (MaxIdleConns=2) のままでも 8 本が本当に同時に走る。
	repos := make([]EmojiApplicationRepository, n)
	for i := range repos {
		db, err := testutil.OpenTestDB()
		require.NoError(t, err)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = sqlDB.Close() })
		repos[i] = NewEmojiApplicationRepository(db)
	}

	// **合図の前に接続を掴ませる。** go func を起こした直後に close すると、
	// まだ `<-start` へ到達していない goroutine がいる。接続確立の往復が
	// 入ると 1 本目がトランザクションを終えてから最後が始まり、**advisory
	// lock を外しても緑になる** (実測で 30 回中 14 回)。
	var ready sync.WaitGroup
	ready.Add(n)
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			var one int
			_ = repos[i].(*emojiApplicationRepository).db.Raw("SELECT 1").Scan(&one).Error
			ready.Done()
			<-start
			errs <- repos[i].CreateWithQuota(
				quotaApp("ea_q8"+string(rune('a'+i)), "ea_q8", string(rune('a'+i)), now), limits)
		}(i)
	}
	ready.Wait()
	close(start)

	var ok, rejected int
	for i := 0; i < n; i++ {
		err := <-errs
		var qe *QuotaExceededError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &qe):
			rejected++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 3, ok)
	require.Equal(t, n-3, rejected)

	var stored int64
	require.NoError(t, testDB.Model(&model.EmojiApplication{}).
		Where(`"userId" = ?`, "ea_q8").Count(&stored).Error)
	require.Equal(t, int64(3), stored)
}

// **`RetryAt` は「最古の 1 件が窓を出る時刻」(#2958)。** 上限ちょうどのときは
// 最古が抜ければ 1 件空く。**時刻を散らして seed するのが要点** — 全件を同じ
// 時刻で作ると `MIN` / `MAX` / `now` が区別できず、この計算が空虚になる。
func TestEmojiApplicationRepository_CreateWithQuota_RetryAtAtLimit(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q9")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	seedApplicationAt(t, "ea_q9a", "ea_q9", "a", model.EmojiApplicationPending, now.Add(-20*time.Hour))
	seedApplicationAt(t, "ea_q9b", "ea_q9", "b", model.EmojiApplicationRejected, now.Add(-4*time.Hour))

	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 2}}}
	err := repo.CreateWithQuota(quotaApp("ea_q9z", "ea_q9", "z", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	// 最古 (now-20h) が窓を出るのは now+4h。**最新 (now-4h) を採ると now+20h**
	// になるので、許容は 4 時間より十分小さく取る。
	require.WithinDuration(t, now.Add(4*time.Hour), qe.RetryAt, time.Minute)
}

// **上限を超えている状態では最古の 1 件では足りない (#2958)。** 上限を後から
// 下げた / 上限の緩いロールを外したときに、**新しい上限を既に超えている
// 利用者**がこの状態になる (下げても届いていない人は 429 にすらならない)。最古を返すと、
// 案内した時刻に叩いてもまた 429 になり、その再試行が 1 時間あたりの API
// レート制限を食い潰す。
func TestEmojiApplicationRepository_CreateWithQuota_RetryAtOverLimit(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q10")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	// **窓の外にも 1 件置く。** これが無いと「窓で絞らずに数え上げる」変異を
	// 検出できない (絞りを外しても並びの先頭が変わらないため)。
	seedApplicationAt(t, "ea_q10old", "ea_q10", "old", model.EmojiApplicationApproved, now.Add(-30*time.Hour))
	// **他人の行を窓の中に置く。** これが無いと「利用者で絞らずに数え上げる」
	// 変異を検出できない。絞りを落とすと他人の申請が自分の再試行時刻を決める。
	createTestUser(t, "ea_q10other")
	seedApplicationAt(t, "ea_q10o", "ea_q10other", "o", model.EmojiApplicationPending, now.Add(-22*time.Hour))
	for i, h := range []int{20, 16, 12, 8, 4} {
		seedApplicationAt(t, "ea_q10"+string(rune('a'+i)), "ea_q10",
			string(rune('a'+i)), model.EmojiApplicationPending, now.Add(-time.Duration(h)*time.Hour))
	}

	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 2}}}
	err := repo.CreateWithQuota(quotaApp("ea_q10z", "ea_q10", "z", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.Equal(t, 5, qe.Used)
	// 5 件のうち 4 件 (used - Max + 1) が抜けて初めて 1 < 2 になる。4 件目に
	// 古いのは now-8h なので、空くのは now+16h。**最古 (now-20h) を採ると
	// now+4h** で、その時刻にはまだ 4 件残っている。
	require.WithinDuration(t, now.Add(16*time.Hour), qe.RetryAt, time.Minute)

	// 案内した時刻に本当に通ること (ここを見ないと offset がずれても気付けない)。
	later := qe.RetryAt.Add(time.Second)
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_q10z", "ea_q10", "z", later), limits))
}

// **自作画像とリモート絵文字は枠を共有する (#2958 の完了条件)。** 種別ごとに
// 分けると、片方の枠を使い切ってももう片方から申請し続けられる。審査する側の
// 負担は種別では変わらない。
func TestEmojiApplicationRepository_CreateWithQuota_CountsBothKinds(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q11")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	host, rname := "remote.example", "kusa"
	require.NoError(t, testDB.Create(&model.EmojiApplication{
		ID: "ea_q11r", UserID: "ea_q11", Kind: model.EmojiApplicationKindRemote,
		Status: model.EmojiApplicationPending, Name: "r", License: "取り込み元",
		RemoteHost: &host, RemoteName: &rname,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}).Error)
	seedApplicationAt(t, "ea_q11o", "ea_q11", "o", model.EmojiApplicationPending, now.Add(-2*time.Hour))

	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 2}}}
	// own 1 + remote 1 = 2 で上限。種別で分けていると own は 1 件しか無いので通る。
	err := repo.CreateWithQuota(quotaApp("ea_q11z", "ea_q11", "z", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.Equal(t, 2, qe.Used)

	// リモート側から出しても同じ枠を見ること (逆向きも塞ぐ)。
	remote := &model.EmojiApplication{
		ID: "ea_q11z2", UserID: "ea_q11", Kind: model.EmojiApplicationKindRemote,
		Status: model.EmojiApplicationPending, Name: "z2", License: "取り込み元",
		RemoteHost: &host, RemoteName: &rname, CreatedAt: now, UpdatedAt: now,
	}
	require.ErrorAs(t, repo.CreateWithQuota(remote, limits), &qe)
}

// 番兵の文面が窓の名前を含むこと。errors.As で分岐できない呼び出し元 (ログ) が
// 唯一の手がかりにする。
func TestQuotaExceededError_Message(t *testing.T) {
	err := &QuotaExceededError{Window: QuotaWindow{Name: "week"}, Used: 3}
	require.Contains(t, err.Error(), "week")
}

// **createdAt が未設定なら現在時刻で窓を切ること。** ゼロ値のまま引き算すると
// 窓の起点が西暦 1 年になり、**過去の申請を全部数えて誰も申請できなくなる**。
// service は必ず埋めるので通常は通らない経路だが、fallback が逆向きに壊れて
// いても気付けないので固定する。
func TestEmojiApplicationRepository_CreateWithQuota_ZeroCreatedAtUsesNow(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_q12")
	repo := NewEmojiApplicationRepository(testDB)
	// 窓 (直前 24 時間) の外にだけ 1 件置く。
	seedApplicationAt(t, "ea_q12a", "ea_q12", "a", model.EmojiApplicationPending, time.Now().Add(-30*time.Hour))

	app := quotaApp("ea_q12z", "ea_q12", "z", time.Time{})
	app.CreatedAt = time.Time{}
	app.UpdatedAt = time.Now()
	limits := QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 1}}}
	require.NoError(t, repo.CreateWithQuota(app, limits))
}

// **審査待ちの件数で弾けること (#2977)。** 既存の一意索引は `(userId, name)`
// WHERE pending なので、名前を変えれば審査待ちは何件でも積める。
func TestEmojiApplicationRepository_CreateWithQuota_PendingLimit(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p1")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	limits := QuotaLimits{MaxPending: 2}

	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_p1a", "ea_p1", "a", now), limits))
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_p1b", "ea_p1", "b", now), limits))

	err := repo.CreateWithQuota(quotaApp("ea_p1c", "ea_p1", "c", now), limits)
	var pe *PendingLimitExceededError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, 2, pe.Used)
	require.Equal(t, 2, pe.Limit)

	// 弾いたときは行を作らないこと。
	var n int64
	require.NoError(t, testDB.Model(&model.EmojiApplication{}).
		Where(`"userId" = ?`, "ea_p1").Count(&n).Error)
	require.Equal(t, int64(2), n)

	// **Used と Limit を取り違えないこと。** 上限を後から下げると
	// `Used > Limit` になる。同じ値だけを試していると入れ替えても気付けない。
	err = repo.CreateWithQuota(quotaApp("ea_p1d", "ea_p1", "d", now), QuotaLimits{MaxPending: 1})
	require.ErrorAs(t, err, &pe)
	require.Equal(t, 2, pe.Used)
	require.Equal(t, 1, pe.Limit)
}

// **却下・取り下げ・承認で枠が戻ること (#2977 の完了条件)。** 期間上限とは逆で、
// こちらが絞るのはモデレーターが見る一覧の長さなので、処理済みは数えない。
func TestEmojiApplicationRepository_CreateWithQuota_PendingLimitCountsOnlyPending(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p2")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	for i, st := range []string{
		model.EmojiApplicationRejected,
		model.EmojiApplicationCanceled,
		model.EmojiApplicationApproved,
	} {
		seedApplicationAt(t, "ea_p2"+string(rune('a'+i)), "ea_p2",
			string(rune('a'+i)), st, now.Add(-time.Duration(i+1)*time.Hour))
	}

	// 処理済み 3 件があっても、審査待ちは 0 なので通る。
	require.NoError(t, repo.CreateWithQuota(
		quotaApp("ea_p2z", "ea_p2", "z", now), QuotaLimits{MaxPending: 1}))
}

// **数えるのは自分の分だけ。** 絞りを落とすと、誰かが上限まで積んだ時点で
// インスタンス全員が申請できなくなる。
func TestEmojiApplicationRepository_CreateWithQuota_PendingLimitScopedToUser(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p3")
	createTestUser(t, "ea_p3other")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	seedApplicationAt(t, "ea_p3x", "ea_p3other", "x", model.EmojiApplicationPending, now)

	require.NoError(t, repo.CreateWithQuota(
		quotaApp("ea_p3a", "ea_p3", "a", now), QuotaLimits{MaxPending: 1}))
}

// **0 は無制限。** 既定値なので、ここが上限として効くと全インスタンスで申請が
// 塞がる。窓も pending も無ければ Create へ委譲する経路になる。
func TestEmojiApplicationRepository_CreateWithQuota_PendingLimitZeroIsUnlimited(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p4")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	for i := 0; i < 3; i++ {
		require.NoError(t, repo.CreateWithQuota(
			quotaApp("ea_p4"+string(rune('a'+i)), "ea_p4", string(rune('a'+i)), now),
			QuotaLimits{MaxPending: 0}))
	}
}

// **自作画像とリモート絵文字は枠を共有する (#2977 の完了条件)。**
func TestEmojiApplicationRepository_CreateWithQuota_PendingLimitCountsBothKinds(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p5")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	host, rname := "remote.example", "kusa"
	require.NoError(t, testDB.Create(&model.EmojiApplication{
		ID: "ea_p5r", UserID: "ea_p5", Kind: model.EmojiApplicationKindRemote,
		Status: model.EmojiApplicationPending, Name: "r", License: "取り込み元",
		RemoteHost: &host, RemoteName: &rname, CreatedAt: now, UpdatedAt: now,
	}).Error)

	err := repo.CreateWithQuota(quotaApp("ea_p5z", "ea_p5", "z", now), QuotaLimits{MaxPending: 1})
	var pe *PendingLimitExceededError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, 1, pe.Used)
}

// **両方満杯なら期間上限を返すこと (#2977)。** 審査待ちを返すと「取り下げれば
// 出せる」と案内することになるが、**期間上限も満杯なら取り下げても通らない**。
// しかも期間上限は全ステータスを数えるので**取り下げた行は枠を占有したまま
// 戻らない** — 案内に従うと申請を 1 件失ったうえに枠も消費する。
//
// `maxPerDay = maxPending` は運営者がいちばん自然に置く設定なので、これは
// 例外的な状況ではない。
func TestEmojiApplicationRepository_CreateWithQuota_WindowBeatsPendingLimit(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p6")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	seedApplicationAt(t, "ea_p6a", "ea_p6", "a", model.EmojiApplicationPending, now.Add(-time.Hour))

	limits := QuotaLimits{
		Windows:    []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 1}},
		MaxPending: 1,
	}
	err := repo.CreateWithQuota(quotaApp("ea_p6z", "ea_p6", "z", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe, "審査待ちが先に返っている")
	var pe *PendingLimitExceededError
	require.False(t, errors.As(err, &pe))

	// **`RetryAt` を出さないこと (#2977)。** 契約は「申請が通るようになる
	// 時刻」だが、審査待ちも満杯ならその時刻でも通らない。審査待ちが空く時刻は
	// モデレーター次第で予告できないので、嘘の時刻を広告しない。
	require.True(t, qe.RetryAt.IsZero(), "叩いても通らない時刻を広告している")

	// **取り下げても期間上限は空かないこと。** 「取り下げれば出せる」という
	// 案内が成り立たない根拠で、順序をこちらに倒している理由そのもの。
	require.NoError(t, testDB.Model(&model.EmojiApplication{}).
		Where(`"id" = ?`, "ea_p6a").Update("status", model.EmojiApplicationCanceled).Error)
	err = repo.CreateWithQuota(quotaApp("ea_p6z", "ea_p6", "z", now), limits)
	require.ErrorAs(t, err, &qe)
}

// **期間上限だけが満杯なら時刻を出すこと (#2977)。** 審査待ちに空きがあれば
// その時刻で本当に通るので、#2958 の契約どおり案内する。両方満杯のときだけ
// 時刻を落とすのであって、期間上限の案内そのものを弱めてはいけない。
func TestEmojiApplicationRepository_CreateWithQuota_WindowOnlyKeepsRetryAt(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p9")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	// 期間上限は満杯 (1/1)、審査待ちは空き (処理済みなので 0/1)。
	seedApplicationAt(t, "ea_p9a", "ea_p9", "a", model.EmojiApplicationRejected, now.Add(-time.Hour))

	limits := QuotaLimits{
		Windows:    []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 1}},
		MaxPending: 1,
	}
	err := repo.CreateWithQuota(quotaApp("ea_p9z", "ea_p9", "z", now), limits)
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.False(t, qe.RetryAt.IsZero(), "期間上限だけなら時刻を出せる")
	require.WithinDuration(t, now.Add(23*time.Hour), qe.RetryAt, time.Minute)

	// 案内した時刻に本当に通ること。
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_p9z", "ea_p9", "z", qe.RetryAt), limits))
}

// **審査待ちだけが満杯なら審査待ちを返すこと (#2977)。** 期間上限に空きが
// あるのに「いつ空くか」を案内すると、取り下げれば今すぐ出せることが伝わらない。
func TestEmojiApplicationRepository_CreateWithQuota_PendingLimitWhenWindowHasRoom(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p8")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	seedApplicationAt(t, "ea_p8a", "ea_p8", "a", model.EmojiApplicationPending, now.Add(-time.Hour))

	limits := QuotaLimits{
		Windows:    []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 10}},
		MaxPending: 1,
	}
	err := repo.CreateWithQuota(quotaApp("ea_p8z", "ea_p8", "z", now), limits)
	var pe *PendingLimitExceededError
	require.ErrorAs(t, err, &pe)

	// 取り下げれば今すぐ出せること (こちらは案内どおりに解決する)。
	require.NoError(t, testDB.Model(&model.EmojiApplication{}).
		Where(`"id" = ?`, "ea_p8a").Update("status", model.EmojiApplicationCanceled).Error)
	require.NoError(t, repo.CreateWithQuota(quotaApp("ea_p8z", "ea_p8", "z", now), limits))
}

// **同時に投げても審査待ちの上限を超えないこと (#2977 の完了条件)。**
func TestEmojiApplicationRepository_CreateWithQuota_PendingLimitConcurrent(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_p7")
	now := time.Now()
	limits := QuotaLimits{MaxPending: 3}

	const n = 8
	// 共有プールを触らない理由は _Concurrent と同じ。
	repos := make([]EmojiApplicationRepository, n)
	for i := range repos {
		db, err := testutil.OpenTestDB()
		require.NoError(t, err)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = sqlDB.Close() })
		repos[i] = NewEmojiApplicationRepository(db)
	}

	var ready sync.WaitGroup
	ready.Add(n)
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			var one int
			_ = repos[i].(*emojiApplicationRepository).db.Raw("SELECT 1").Scan(&one).Error
			ready.Done()
			<-start
			errs <- repos[i].CreateWithQuota(
				quotaApp("ea_p7"+string(rune('a'+i)), "ea_p7", string(rune('a'+i)), now), limits)
		}(i)
	}
	ready.Wait()
	close(start)

	var ok, rejected int
	for i := 0; i < n; i++ {
		err := <-errs
		var pe *PendingLimitExceededError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &pe):
			rejected++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 3, ok)
	require.Equal(t, n-3, rejected)

	var stored int64
	require.NoError(t, testDB.Model(&model.EmojiApplication{}).
		Where(`"userId" = ?`, "ea_p7").Count(&stored).Error)
	require.Equal(t, int64(3), stored)
}

// 番兵の文面が「審査待ち」を指すこと。
func TestPendingLimitExceededError_Message(t *testing.T) {
	err := &PendingLimitExceededError{Used: 3, Limit: 3}
	require.Contains(t, err.Error(), "awaiting review")
}

// seedRelated inserts an application with the fields the related-lookup uses.
func seedRelated(t *testing.T, id, userID, name, status string, host, rname, hash *string, at time.Time) {
	t.Helper()
	app := &model.EmojiApplication{
		ID: id, UserID: userID, Kind: model.EmojiApplicationKindOwn,
		Status: status, Name: name, License: "自作",
		RemoteHost: host, RemoteName: rname, FileHash: hash,
		CreatedAt: at, UpdatedAt: at,
	}
	if host != nil {
		app.Kind = model.EmojiApplicationKindRemote
	}
	require.NoError(t, testDB.Create(app).Error)
}

func strp(s string) *string { return &s }

// **3 つの条件で引けること (#2960)。** 同じ名前・同じリモート元・同じ画像の
// どれかで過去の判断を辿れないと、名前を変えた再申請や別人による再申請で
// 却下理由を見落とす。
func TestEmojiApplicationRepository_FindRelated_Matches(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_r1")
	createTestUser(t, "ea_r1other")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	// 審査中の申請 (own、名前 sushi、ハッシュ H1)。
	cur := &model.EmojiApplication{
		ID: "ea_r1cur", UserID: "ea_r1", Kind: model.EmojiApplicationKindOwn,
		Status: model.EmojiApplicationPending, Name: "sushi", License: "自作",
		FileHash: strp("H1"), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, testDB.Create(cur).Error)

	// 同じ名前 (別人・却下済み)
	seedRelated(t, "ea_r1name", "ea_r1other", "sushi", model.EmojiApplicationRejected, nil, nil, nil, now.Add(-time.Hour))
	// **却下理由と審査日時を実際に載せておく (レビュー R3-M1)。** これが無いと
	// `SELECT a.*` を列指定に絞る変更 (「使わない列を引かない」最適化) で
	// `rejectReason` / `processedAt` が NULL のまま返り、**審査画面から却下理由が
	// 消える**のに Go のテストが全部緑になる (実測)。repository は ID と
	// `MatchedBy()` しか見ておらず、handler のテストは stub が手で組んだ struct を
	// packer に通すだけなので、GORM の hydration を誰も検査していなかった。
	processedAt := now.Add(-30 * time.Minute)
	require.NoError(t, testDB.Model(&model.EmojiApplication{}).Where(`"id" = ?`, "ea_r1name").
		Updates(map[string]any{"rejectReason": "潰れて読めません", "processedAt": processedAt}).Error)
	// 同じ画像・名前は違う
	seedRelated(t, "ea_r1hash", "ea_r1other", "onigiri", model.EmojiApplicationApproved, nil, nil, strp("H1"), now.Add(-2*time.Hour))
	// 無関係
	seedRelated(t, "ea_r1none", "ea_r1other", "ramen", model.EmojiApplicationRejected, nil, nil, strp("H9"), now.Add(-3*time.Hour))

	got, err := repo.FindRelated(cur, 10, "")
	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	byID := map[string][]string{}
	for i := range got {
		ids = append(ids, got[i].ID)
		byID[got[i].ID] = got[i].MatchedBy()
	}
	require.ElementsMatch(t, []string{"ea_r1name", "ea_r1hash"}, ids, "無関係な申請が混ざっている / 一致したものが落ちている")
	require.Equal(t, []string{"name"}, byID["ea_r1name"])
	require.Equal(t, []string{"fileHash"}, byID["ea_r1hash"])

	// **自分自身は含めない。** 開いている行が並ぶと件数も意味も狂う。
	require.NotContains(t, ids, "ea_r1cur")

	// 却下理由と審査日時が実際に返ること (上のコメントの理由)。
	for i := range got {
		if got[i].ID != "ea_r1name" {
			continue
		}
		require.NotNil(t, got[i].RejectReason, "却下理由が返っていない")
		require.Equal(t, "潰れて読めません", *got[i].RejectReason)
		require.NotNil(t, got[i].ProcessedAt, "審査日時が返っていない")
	}
}

// リモート元での照合。**希望するローカル名が変わっていても辿れること**が要点。
func TestEmojiApplicationRepository_FindRelated_RemoteSource(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_r2")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	host, rname := "remote.example", "kusa"

	cur := &model.EmojiApplication{
		ID: "ea_r2cur", UserID: "ea_r2", Kind: model.EmojiApplicationKindRemote,
		Status: model.EmojiApplicationPending, Name: "kusa_new", License: "取り込み元",
		RemoteHost: &host, RemoteName: &rname, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, testDB.Create(cur).Error)
	// 同じリモート元・別のローカル名で却下済み
	seedRelated(t, "ea_r2old", "ea_r2", "kusa_old", model.EmojiApplicationRejected,
		&host, &rname, nil, now.Add(-time.Hour))
	// 別のホストの同名絵文字
	otherHost := "other.example"
	seedRelated(t, "ea_r2oth", "ea_r2", "kusa_x", model.EmojiApplicationRejected,
		&otherHost, &rname, nil, now.Add(-2*time.Hour))
	// **同じホストの別の絵文字。** host だけで照合すると、そのインスタンス
	// から取り込んだ全申請が「関連」として並ぶ。
	otherName := "not_kusa"
	seedRelated(t, "ea_r2same", "ea_r2", "kusa_y", model.EmojiApplicationRejected,
		&host, &otherName, nil, now.Add(-3*time.Hour))

	got, err := repo.FindRelated(cur, 10, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "ea_r2old", got[0].ID)
	require.Equal(t, []string{"remoteSource"}, got[0].MatchedBy())
}

// **複数の理由で一致したら全部出す。** 片方しか出ないと、名前を変えれば
// 別物として通ると誤解される。
func TestEmojiApplicationRepository_FindRelated_MultipleReasons(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_r3")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	host, rname := "remote.example", "kusa"

	cur := &model.EmojiApplication{
		ID: "ea_r3cur", UserID: "ea_r3", Kind: model.EmojiApplicationKindRemote,
		Status: model.EmojiApplicationPending, Name: "kusa", License: "取り込み元",
		RemoteHost: &host, RemoteName: &rname, FileHash: strp("H1"),
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, testDB.Create(cur).Error)
	seedRelated(t, "ea_r3all", "ea_r3", "kusa", model.EmojiApplicationRejected,
		&host, &rname, strp("H1"), now.Add(-time.Hour))

	got, err := repo.FindRelated(cur, 10, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, []string{"name", "remoteSource", "fileHash"}, got[0].MatchedBy())
}

// **ハッシュを持たない申請同士を「同じ画像」にしない。** drive のファイルが
// 消えた申請は NULL のままなので、NULL 同士が一致すると無関係な履歴が並ぶ。
func TestEmojiApplicationRepository_FindRelated_NullHashDoesNotMatch(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_r4")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	cur := &model.EmojiApplication{
		ID: "ea_r4cur", UserID: "ea_r4", Kind: model.EmojiApplicationKindOwn,
		Status: model.EmojiApplicationPending, Name: "sushi", License: "自作",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, testDB.Create(cur).Error)
	// 名前もハッシュも違う (どちらも NULL)
	seedRelated(t, "ea_r4oth", "ea_r4", "ramen", model.EmojiApplicationRejected, nil, nil, nil, now.Add(-time.Hour))

	got, err := repo.FindRelated(cur, 10, "")
	require.NoError(t, err)
	require.Empty(t, got, "ハッシュを持たない申請同士が一致している")

	// **空文字も同じ扱い。** service は空を保存しないが、古いデータや手で
	// 入れた行は空文字を持ちうる。空同士が「同じ画像」になると、ハッシュの
	// 無い申請が全部まとめて並ぶ。
	cur.FileHash = strp("")
	require.NoError(t, testDB.Model(cur).Update("fileHash", "").Error)
	seedRelated(t, "ea_r4emp", "ea_r4", "udon", model.EmojiApplicationRejected, nil, nil, strp(""), now.Add(-2*time.Hour))

	got, err = repo.FindRelated(cur, 10, "")
	require.NoError(t, err)
	require.Empty(t, got, "空ハッシュ同士が一致している")

	// **リモート元も同じ。** own の申請 (remoteHost なし) が、空文字の
	// remoteHost を持つ行と一致すると、無関係な履歴がまとめて並ぶ。
	empty := ""
	seedRelated(t, "ea_r4rem", "ea_r4", "soba", model.EmojiApplicationRejected, &empty, &empty, nil, now.Add(-3*time.Hour))

	got, err = repo.FindRelated(cur, 10, "")
	require.NoError(t, err)
	require.Empty(t, got, "own の申請が空のリモート元と一致している")

	// **名前も同じ扱い (レビュー R2-Low1)。** `Service.Create` が空名を弾くので
	// 現状は到達しないが、ガードを外しても落ちないままだと「3 条件を非対称に
	// しない」という意図が検証されない。空名同士が一致すると、名前を持たない
	// 行がまとめて「関連する過去の申請」として並ぶ。
	require.NoError(t, testDB.Model(cur).Update("name", "").Error)
	cur.Name = ""
	seedRelated(t, "ea_r4nam", "ea_r4", "", model.EmojiApplicationRejected, nil, nil, nil, now.Add(-4*time.Hour))

	got, err = repo.FindRelated(cur, 10, "")
	require.NoError(t, err)
	require.Empty(t, got, "空の名前同士が一致している")
}

// ページング。**id の降順で切る** — createdAt で切ると同時刻の行を取りこぼす。
func TestEmojiApplicationRepository_FindRelated_Paging(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_r5")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	cur := &model.EmojiApplication{
		ID: "ea_r5cur", UserID: "ea_r5", Kind: model.EmojiApplicationKindOwn,
		Status: model.EmojiApplicationPending, Name: "sushi", License: "自作",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, testDB.Create(cur).Error)
	for i := 0; i < 5; i++ {
		seedRelated(t, "ea_r5a"+string(rune('a'+i)), "ea_r5", "sushi",
			model.EmojiApplicationRejected, nil, nil, nil, now.Add(-time.Duration(i+1)*time.Hour))
	}

	first, err := repo.FindRelated(cur, 2, "")
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, "ea_r5ae", first[0].ID, "id の降順になっていない")

	second, err := repo.FindRelated(cur, 2, first[1].ID)
	require.NoError(t, err)
	require.Len(t, second, 2)
	require.Less(t, second[0].ID, first[1].ID, "untilId より後ろが返っている")

	// **repository 側の clamp も通す (レビュー R3-L3)。** handler が clamp して
	// いるので実害は小さいが、backstop そのものが一度も実行されていなかった。
	// 0 と範囲外はどちらも既定 (10) に落ちる。
	for _, limit := range []int{0, -1, 101} {
		all, err := repo.FindRelated(cur, limit, "")
		require.NoError(t, err)
		require.Len(t, all, 5, "limit=%d が既定に落ちていない", limit)
	}
}

// **件数はステータスごとに出す。** 「却下2 / 承認1」が分かると、開く前に
// 見るべき履歴かどうか判断できる。
func TestEmojiApplicationRepository_CountRelated(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_r6")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	cur := &model.EmojiApplication{
		ID: "ea_r6cur", UserID: "ea_r6", Kind: model.EmojiApplicationKindOwn,
		Status: model.EmojiApplicationPending, Name: "sushi", License: "自作",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, testDB.Create(cur).Error)
	createTestUser(t, "ea_r6other")
	for i, st := range []string{
		model.EmojiApplicationRejected, model.EmojiApplicationRejected,
		model.EmojiApplicationApproved, model.EmojiApplicationCanceled,
		model.EmojiApplicationPending,
	} {
		// **審査待ちだけ別人にする。** 部分一意索引 (userId, name) WHERE
		// pending があるので、同じ人が同じ名前で 2 件 pending にはできない。
		// 「別人が同じ名前で申請中」は実際に起きる形でもある。
		owner := "ea_r6"
		if st == model.EmojiApplicationPending {
			owner = "ea_r6other"
		}
		seedRelated(t, "ea_r6"+string(rune('a'+i)), owner, "sushi", st, nil, nil, nil,
			now.Add(-time.Duration(i+1)*time.Hour))
	}

	got, err := repo.CountRelated(cur)
	require.NoError(t, err)
	require.Equal(t, RelatedCounts{Total: 5, Pending: 1, Approved: 1, Rejected: 2, Canceled: 1}, got)
}

// **一覧と件数が同じ条件で動くこと。** 別々に書くと「件数はあるのに中身が
// 空」「中身はあるのに 0 件」になる。
func TestEmojiApplicationRepository_RelatedCountMatchesList(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_r7")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	host, rname := "remote.example", "kusa"

	cur := &model.EmojiApplication{
		ID: "ea_r7cur", UserID: "ea_r7", Kind: model.EmojiApplicationKindRemote,
		Status: model.EmojiApplicationPending, Name: "kusa", License: "取り込み元",
		RemoteHost: &host, RemoteName: &rname, FileHash: strp("H1"),
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, testDB.Create(cur).Error)
	seedRelated(t, "ea_r7a", "ea_r7", "kusa", model.EmojiApplicationRejected, nil, nil, nil, now.Add(-time.Hour))
	seedRelated(t, "ea_r7b", "ea_r7", "x", model.EmojiApplicationApproved, &host, &rname, nil, now.Add(-2*time.Hour))
	seedRelated(t, "ea_r7c", "ea_r7", "y", model.EmojiApplicationRejected, nil, nil, strp("H1"), now.Add(-3*time.Hour))
	seedRelated(t, "ea_r7d", "ea_r7", "z", model.EmojiApplicationRejected, nil, nil, strp("H9"), now.Add(-4*time.Hour))

	counts, err := repo.CountRelated(cur)
	require.NoError(t, err)
	list, err := repo.FindRelated(cur, 100, "")
	require.NoError(t, err)
	require.Equal(t, counts.Total, len(list), "件数と一覧の条件がずれている")
	require.Equal(t, 3, counts.Total)
}

// **ステータスで絞れること (#2961)。** モデレーション画面は「却下されている
// ものだけ見たい」が主用途なので、絞りが効かないと使い物にならない。
func TestEmojiApplicationRepository_ListByUserFiltered_Status(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_f1")
	createTestUser(t, "ea_f1other")
	repo := NewEmojiApplicationRepository(testDB)

	seedApplication(t, "ea_f1a", "ea_f1", "pend", model.EmojiApplicationPending)
	seedApplication(t, "ea_f1b", "ea_f1", "appr", model.EmojiApplicationApproved)
	seedApplication(t, "ea_f1c", "ea_f1", "rej", model.EmojiApplicationRejected)
	seedApplication(t, "ea_f1d", "ea_f1", "can", model.EmojiApplicationCanceled)
	// **他人の行が混ざらないこと。** userId を落とすと全員の履歴が出る。
	seedApplication(t, "ea_f1x", "ea_f1other", "pend", model.EmojiApplicationPending)

	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"", []string{"ea_f1d", "ea_f1c", "ea_f1b", "ea_f1a"}},
		{"all", []string{"ea_f1d", "ea_f1c", "ea_f1b", "ea_f1a"}},
		{model.EmojiApplicationPending, []string{"ea_f1a"}},
		{model.EmojiApplicationApproved, []string{"ea_f1b"}},
		{model.EmojiApplicationRejected, []string{"ea_f1c"}},
		{model.EmojiApplicationCanceled, []string{"ea_f1d"}},
	} {
		rows, err := repo.ListByUserFiltered("ea_f1", tc.status, "", 50, "")
		require.NoError(t, err)
		got := make([]string, 0, len(rows))
		for i := range rows {
			got = append(got, rows[i].ID)
		}
		require.Equal(t, tc.want, got, "status=%q の絞り込み", tc.status)
	}

	// **未知の status は全件に倒さない。** 絞ったつもりで全部出るほうが危険側。
	rows, err := repo.ListByUserFiltered("ea_f1", "escalated", "", 50, "")
	require.NoError(t, err)
	require.Empty(t, rows, "未知の status で全件返っている")
}

// **検索の LIKE メタ文字をエスケープすること (#2961)。** 素通しすると
// `foo_bar` が `fooXbar` に当たり、`%` の 1 文字で全件返る。モデレーターは
// 「この名前の申請は無い」と読むので、取り違えたまま判断する。
func TestEmojiApplicationRepository_ListByUserFiltered_Query(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_f2")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	seedRelated(t, "ea_f2a", "ea_f2", "foo_bar", model.EmojiApplicationRejected, nil, nil, nil, now.Add(-time.Hour))
	seedRelated(t, "ea_f2b", "ea_f2", "fooXbar", model.EmojiApplicationRejected, nil, nil, nil, now.Add(-2*time.Hour))
	host, rname := "remote.example", "kusa"
	seedRelated(t, "ea_f2c", "ea_f2", "sushi", model.EmojiApplicationApproved, &host, &rname, nil, now.Add(-3*time.Hour))

	ids := func(status, query string) []string {
		rows, err := repo.ListByUserFiltered("ea_f2", status, query, 50, "")
		require.NoError(t, err)
		out := make([]string, 0, len(rows))
		for i := range rows {
			out = append(out, rows[i].ID)
		}
		return out
	}

	// `_` はワイルドカードにならない。
	require.Equal(t, []string{"ea_f2a"}, ids("", "foo_bar"), "_ がワイルドカードとして効いている")
	// `%` も同じ。全件が返ってはいけない。
	require.Empty(t, ids("", "%"), "% で全件返っている")
	// **バックスラッシュもエスケープすること (レビュー M1)。** seed に `\` を
	// 含む行を入れて実際に当てる — 含まない行しか無いと、エスケープを外しても
	// 0 件のままで通ってしまう (空虚なアサーションだった)。
	seedRelated(t, "ea_f2d", "ea_f2", `a\_b`, model.EmojiApplicationRejected, nil, nil, nil, now.Add(-4*time.Hour))
	seedRelated(t, "ea_f2e", "ea_f2", `a\Xb`, model.EmojiApplicationRejected, nil, nil, nil, now.Add(-5*time.Hour))
	require.Equal(t, []string{"ea_f2d"}, ids("", `a\_b`),
		`\ をエスケープしていないので _ がワイルドカードとして効いている`)
	// 普通の部分一致は効く。
	require.ElementsMatch(t, []string{"ea_f2a", "ea_f2b"}, ids("", "bar"))
	// 大文字小文字を区別しない (ILIKE)。
	require.ElementsMatch(t, []string{"ea_f2a", "ea_f2b"}, ids("", "BAR"))
	// リモート元でも引ける。**host と name の両方**が対象。
	require.Equal(t, []string{"ea_f2c"}, ids("", "remote.example"))
	require.Equal(t, []string{"ea_f2c"}, ids("", "kusa"))
	// 絞り込みと併用できる。
	require.Empty(t, ids(model.EmojiApplicationPending, "bar"))
}

// ページングと件数。
func TestEmojiApplicationRepository_ListByUserFiltered_Paging(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_f3")
	createTestUser(t, "ea_f3other")
	repo := NewEmojiApplicationRepository(testDB)

	for i := 0; i < 5; i++ {
		seedApplication(t, "ea_f3"+string(rune('a'+i)), "ea_f3", "n"+string(rune('a'+i)),
			model.EmojiApplicationRejected)
	}
	seedApplication(t, "ea_f3z", "ea_f3other", "other", model.EmojiApplicationRejected)

	first, err := repo.ListByUserFiltered("ea_f3", "", "", 2, "")
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, "ea_f3e", first[0].ID, "id の降順になっていない")

	second, err := repo.ListByUserFiltered("ea_f3", "", "", 2, first[1].ID)
	require.NoError(t, err)
	require.Len(t, second, 2)
	require.Less(t, second[0].ID, first[1].ID, "untilId より後ろが返っている")

	counts, err := repo.CountByUserStatus("ea_f3")
	require.NoError(t, err)
	require.Equal(t, 5, counts.Total, "他人の行が数に混ざっている")
	require.Equal(t, 5, counts.Rejected)
	require.Equal(t, 0, counts.Pending)
}

// 集計はステータスごとに出る。
func TestEmojiApplicationRepository_CountByUserStatus(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_f4")
	repo := NewEmojiApplicationRepository(testDB)

	seedApplication(t, "ea_f4a", "ea_f4", "a", model.EmojiApplicationPending)
	seedApplication(t, "ea_f4b", "ea_f4", "b", model.EmojiApplicationRejected)
	seedApplication(t, "ea_f4c", "ea_f4", "c", model.EmojiApplicationRejected)
	seedApplication(t, "ea_f4d", "ea_f4", "d", model.EmojiApplicationApproved)
	seedApplication(t, "ea_f4e", "ea_f4", "e", model.EmojiApplicationCanceled)

	got, err := repo.CountByUserStatus("ea_f4")
	require.NoError(t, err)
	require.Equal(t, StatusCounts{Total: 5, Pending: 1, Approved: 1, Rejected: 2, Canceled: 1}, got)

	// 1 件も無いユーザーはゼロ値。
	createTestUser(t, "ea_f4none")
	empty, err := repo.CountByUserStatus("ea_f4none")
	require.NoError(t, err)
	require.Equal(t, StatusCounts{}, empty)
}

// **表示する使用状況が、実際に効いている判定と一致すること (#2961)。**
// 別の SQL で数えると「画面は 2/3 なのに実際は弾かれる」という形でずれる。
// ここが両者を突き合わせる唯一の場所。
func TestEmojiApplicationRepository_QuotaUsage_MatchesEnforcement(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_f5")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	windows := []QuotaWindow{
		{Name: "day", Duration: 24 * time.Hour, Max: 2},
		{Name: "week", Duration: 7 * 24 * time.Hour, Max: 5},
		// 上限なしの窓も「何件出しているか」は返す。
		{Name: "month", Duration: 30 * 24 * time.Hour, Max: 0},
	}

	// 窓の中に 2 件 (day が満杯)、外に 1 件。
	seedApplicationAt(t, "ea_f5a", "ea_f5", "a", model.EmojiApplicationRejected, now.Add(-2*time.Hour))
	seedApplicationAt(t, "ea_f5b", "ea_f5", "b", model.EmojiApplicationCanceled, now.Add(-3*time.Hour))
	seedApplicationAt(t, "ea_f5c", "ea_f5", "c", model.EmojiApplicationApproved, now.Add(-40*time.Hour))

	usage, err := repo.QuotaUsage("ea_f5", QuotaLimits{Windows: windows}, now)
	require.NoError(t, err)
	require.Len(t, usage.Windows, 3)
	require.Equal(t, 2, usage.Windows[0].Used, "day の使用数が合わない")
	require.Equal(t, 3, usage.Windows[1].Used, "week の使用数が合わない")
	require.Equal(t, 3, usage.Windows[2].Used, "上限なしの窓でも件数は数える")

	// 満杯の窓にだけ RetryAt が入る。
	require.False(t, usage.Windows[0].RetryAt.IsZero(), "満杯の窓に次回可能時刻が無い")
	require.True(t, usage.Windows[1].RetryAt.IsZero(), "空きのある窓に次回可能時刻が入っている")
	require.True(t, usage.Windows[2].RetryAt.IsZero(), "上限なしの窓に次回可能時刻が入っている")

	// **作成側が返す時刻と一致すること。** ここが噛み合っていないと、画面の
	// 案内どおりに再申請しても弾かれる。
	err = repo.CreateWithQuota(quotaApp("ea_f5new", "ea_f5", "new", now), QuotaLimits{Windows: windows})
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe, "満杯なのに作成できている")
	require.Equal(t, "day", qe.Window.Name)
	require.Equal(t, usage.Windows[0].Used, qe.Used, "画面の使用数と実際の判定がずれている")
	require.WithinDuration(t, usage.Windows[0].RetryAt, qe.RetryAt, 0,
		"画面の次回可能時刻と実際の判定がずれている")
}

// **審査待ちの上限も読み取り側に効くこと (レビュー H1)。** 窓と審査待ちの
// 両方が満杯なら、窓が空く時刻に叩いてもまだ通らない。作成側だけがこの規則を
// 持っていたとき、読み取り側は**その時刻でも通らない時刻を画面に広告して
// いた** (実測)。あわせて、窓に空きがあっても審査待ちが満杯なら申請は通らない
// ので、その件数も返さないと画面が「空きあり」と描いてしまう。
func TestEmojiApplicationRepository_QuotaUsage_PendingLimit(t *testing.T) {
	cleanupEmojiApplications(t)
	defer cleanupEmojiApplications(t)
	createTestUser(t, "ea_f6")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	seedApplicationAt(t, "ea_f6a", "ea_f6", "a", model.EmojiApplicationPending, now.Add(-time.Hour))
	seedApplicationAt(t, "ea_f6b", "ea_f6", "b", model.EmojiApplicationPending, now.Add(-2*time.Hour))

	day := QuotaWindow{Name: "day", Duration: 24 * time.Hour, Max: 2}

	// (1) 窓が満杯 + 審査待ちも満杯 → 時刻を出さない (作成側と同じ)。
	both, err := repo.QuotaUsage("ea_f6", QuotaLimits{Windows: []QuotaWindow{day}, MaxPending: 2}, now)
	require.NoError(t, err)
	require.True(t, both.PendingFull)
	require.Equal(t, 2, both.Pending)
	require.Equal(t, 2, both.MaxPending)
	require.True(t, both.Windows[0].RetryAt.IsZero(),
		"審査待ちも満杯なのに、その時刻に叩いても通らない時刻を広告している")

	createErr := repo.CreateWithQuota(quotaApp("ea_f6new", "ea_f6", "new", now),
		QuotaLimits{Windows: []QuotaWindow{day}, MaxPending: 2})
	var qe *QuotaExceededError
	require.ErrorAs(t, createErr, &qe)
	require.True(t, qe.RetryAt.IsZero(), "作成側と読み取り側で時刻の扱いが違う")

	// (2) 窓に空きがあっても審査待ちが満杯なら申請は通らない。件数を返さないと
	// 画面は「空きあり」と描く。
	roomy, err := repo.QuotaUsage("ea_f6",
		QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 10}}, MaxPending: 2}, now)
	require.NoError(t, err)
	require.True(t, roomy.PendingFull, "審査待ちが満杯なのに空きありとして返っている")
	require.Equal(t, 2, roomy.Pending)
	require.True(t, roomy.Windows[0].RetryAt.IsZero(), "空きのある窓に時刻が入っている")

	pendErr := repo.CreateWithQuota(quotaApp("ea_f6n2", "ea_f6", "n2", now),
		QuotaLimits{Windows: []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 10}}, MaxPending: 2})
	var pe *PendingLimitExceededError
	require.ErrorAs(t, pendErr, &pe, "実際の申請は審査待ちの上限で弾かれるのに、画面は空きありと描いている")

	// (3) 上限なしなら満杯にならない。**それでも件数は返す (レビュー M2)。**
	// 0 のままだと、同じレスポンスの中で `counts.pending` と食い違う
	// (既定が無制限なのでほぼ全ての構成がこれに当たる)。
	none, err := repo.QuotaUsage("ea_f6", QuotaLimits{Windows: []QuotaWindow{day}}, now)
	require.NoError(t, err)
	require.False(t, none.PendingFull)
	require.Equal(t, 0, none.MaxPending)
	require.Equal(t, 2, none.Pending, "上限なしのとき審査待ちの件数が返らない")
	require.False(t, none.Windows[0].RetryAt.IsZero(), "審査待ちが無制限なら窓の時刻はそのまま")
}

func seedQuotaReset(t *testing.T, id, userID, by string, at time.Time) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.EmojiApplicationQuotaReset{
		ID: id, UserID: userID, ResetByID: by, Reason: "再申請してもらうため", CreatedAt: at,
	}).Error)
}

func cleanupQuotaResets(t *testing.T) {
	t.Helper()
	testDB.Exec(`DELETE FROM "emoji_application_quota_reset"`)
}

// **リセットが枠を戻すこと、そして履歴は残ること (#2962)。** 行を消して枠を
// 空けると、過去の判断 (#2960 の審査材料) も同時に消える。
func TestEmojiApplicationRepository_QuotaReset_ClearsWindowButKeepsRows(t *testing.T) {
	cleanupEmojiApplications(t)
	cleanupQuotaResets(t)
	defer func() { cleanupEmojiApplications(t); cleanupQuotaResets(t) }()
	createTestUser(t, "ea_g1")
	createTestUser(t, "ea_g1mod")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	// 窓の中に 2 件 = day (Max 2) は満杯。
	seedApplicationAt(t, "ea_g1a", "ea_g1", "a", model.EmojiApplicationRejected, now.Add(-3*time.Hour))
	seedApplicationAt(t, "ea_g1b", "ea_g1", "b", model.EmojiApplicationCanceled, now.Add(-2*time.Hour))
	windows := []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 2}}

	full, err := repo.QuotaUsage("ea_g1", QuotaLimits{Windows: windows}, now)
	require.NoError(t, err)
	require.Equal(t, 2, full.Windows[0].Used)
	require.False(t, full.Windows[0].RetryAt.IsZero(), "満杯なのに次回可能時刻が無い")

	// リセット (1 時間前の申請より後ろ) を入れる。
	resetAt := now.Add(-time.Hour)
	seedQuotaReset(t, "ea_g1r", "ea_g1", "ea_g1mod", resetAt)

	after, err := repo.QuotaUsage("ea_g1", QuotaLimits{Windows: windows, ResetAt: resetAt}, now)
	require.NoError(t, err)
	require.Equal(t, 0, after.Windows[0].Used, "リセット後の枠が戻っていない")
	require.True(t, after.Windows[0].RetryAt.IsZero(), "空きがあるのに次回可能時刻が出ている")

	// **申請の行は残っている。** 審査材料を消さないのがこの設計の要点。
	var n int64
	require.NoError(t, testDB.Model(&model.EmojiApplication{}).
		Where(`"userId" = ?`, "ea_g1").Count(&n).Error)
	require.EqualValues(t, 2, n, "リセットで申請の行が消えている")

	// **作成側にも同じ境界が効く。** 片方だけだと「画面は空きありなのに弾かれる」
	// (またはその逆) になる。
	require.NoError(t, repo.CreateWithQuota(
		quotaApp("ea_g1new", "ea_g1", "new", now),
		QuotaLimits{Windows: windows, ResetAt: resetAt}),
		"リセット後も申請が弾かれている")
}

// **リセットより後の申請は数えること。** 境界を「以降」にしないと、リセット後に
// 出した分まで無かったことになり、上限が実質無効になる。
func TestEmojiApplicationRepository_QuotaReset_CountsAfterReset(t *testing.T) {
	cleanupEmojiApplications(t)
	cleanupQuotaResets(t)
	defer func() { cleanupEmojiApplications(t); cleanupQuotaResets(t) }()
	createTestUser(t, "ea_g2")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	resetAt := now.Add(-2 * time.Hour)

	seedApplicationAt(t, "ea_g2old", "ea_g2", "old", model.EmojiApplicationRejected, now.Add(-3*time.Hour))
	seedApplicationAt(t, "ea_g2new", "ea_g2", "new", model.EmojiApplicationPending, now.Add(-time.Hour))
	windows := []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 2}}

	usage, err := repo.QuotaUsage("ea_g2", QuotaLimits{Windows: windows, ResetAt: resetAt}, now)
	require.NoError(t, err)
	require.Equal(t, 1, usage.Windows[0].Used, "リセット後の申請が数えられていない")
}

// **窓より古いリセットは何も変えない。** 既に窓の外なので効かせる対象が無い。
func TestEmojiApplicationRepository_QuotaReset_OlderThanWindowIsNoop(t *testing.T) {
	cleanupEmojiApplications(t)
	cleanupQuotaResets(t)
	defer func() { cleanupEmojiApplications(t); cleanupQuotaResets(t) }()
	createTestUser(t, "ea_g3")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	seedApplicationAt(t, "ea_g3a", "ea_g3", "a", model.EmojiApplicationRejected, now.Add(-2*time.Hour))
	// **窓の外にあり、かつ古いリセットより後ろの行を置く。** これが無いと、
	// 境界を `max(...)` ではなく「リセットがあれば必ずそれ」にする変異でも
	// 同じ件数になり、テストが空虚になる (実測)。
	seedApplicationAt(t, "ea_g3old", "ea_g3", "old", model.EmojiApplicationRejected, now.Add(-30*time.Hour))
	windows := []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 2}}

	usage, err := repo.QuotaUsage("ea_g3",
		QuotaLimits{Windows: windows, ResetAt: now.Add(-48 * time.Hour)}, now)
	require.NoError(t, err)
	require.Equal(t, 1, usage.Windows[0].Used,
		"窓より古いリセットが窓を広げている (max(期間の開始, リセット) になっていない)")
}

// **`retryAt` にも境界が効くこと。** リセット後の行だけで満杯になったときの
// 次回可能時刻は、リセット前の行に引きずられてはいけない。
func TestEmojiApplicationRepository_QuotaReset_RetryAtUsesBoundary(t *testing.T) {
	cleanupEmojiApplications(t)
	cleanupQuotaResets(t)
	defer func() { cleanupEmojiApplications(t); cleanupQuotaResets(t) }()
	createTestUser(t, "ea_g4")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()
	resetAt := now.Add(-5 * time.Hour)

	// リセット前 (古い) 1 件 + リセット後 1 件で、Max 1 の窓を満杯にする。
	seedApplicationAt(t, "ea_g4old", "ea_g4", "old", model.EmojiApplicationRejected, now.Add(-10*time.Hour))
	seedApplicationAt(t, "ea_g4new", "ea_g4", "new", model.EmojiApplicationPending, now.Add(-time.Hour))
	windows := []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 1}}

	usage, err := repo.QuotaUsage("ea_g4", QuotaLimits{Windows: windows, ResetAt: resetAt}, now)
	require.NoError(t, err)
	require.Equal(t, 1, usage.Windows[0].Used)
	require.False(t, usage.Windows[0].RetryAt.IsZero())
	// リセット後の行 (1 時間前) が窓を出る時刻 = now + 23 時間。古い行
	// (10 時間前) を採ると now + 14 時間になり、**その時刻に叩いても弾かれる**。
	require.WithinDuration(t, now.Add(23*time.Hour), usage.Windows[0].RetryAt, time.Minute,
		"リセット前の行を基準に次回可能時刻を出している")

	// 作成側も同じ時刻を返すこと。
	err = repo.CreateWithQuota(quotaApp("ea_g4x", "ea_g4", "x", now),
		QuotaLimits{Windows: windows, ResetAt: resetAt})
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	require.WithinDuration(t, usage.Windows[0].RetryAt, qe.RetryAt, 0,
		"画面と実際の判定で次回可能時刻がずれている")
}

// リセットの記録そのもの。**最新の 1 件を採ること**と、履歴が積まれること。
func TestEmojiApplicationQuotaResetRepository(t *testing.T) {
	cleanupQuotaResets(t)
	defer cleanupQuotaResets(t)
	createTestUser(t, "ea_g5")
	createTestUser(t, "ea_g5other")
	repo := NewEmojiApplicationQuotaResetRepository(testDB)
	now := time.Now()

	// 未実施なら nil (エラーではない)。
	got, err := repo.LatestByUser("ea_g5")
	require.NoError(t, err)
	require.Nil(t, got, "未実施が not-found エラーになっている")

	require.NoError(t, repo.Create(&model.EmojiApplicationQuotaReset{
		ID: "qr1", UserID: "ea_g5", ResetByID: "m1", Reason: "1回目", CreatedAt: now.Add(-2 * time.Hour),
	}))
	require.NoError(t, repo.Create(&model.EmojiApplicationQuotaReset{
		ID: "qr2", UserID: "ea_g5", ResetByID: "m2", Reason: "2回目", CreatedAt: now.Add(-time.Hour),
	}))
	// 他人の行が混ざらないこと。
	require.NoError(t, repo.Create(&model.EmojiApplicationQuotaReset{
		ID: "qr3", UserID: "ea_g5other", ResetByID: "m1", Reason: "別人", CreatedAt: now,
	}))

	got, err = repo.LatestByUser("ea_g5")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "qr2", got.ID, "最新の 1 件を採っていない")
	require.Equal(t, "2回目", got.Reason)
	require.Equal(t, "m2", got.ResetByID)

	// **履歴は上書きされない。** 誰がいつ何回戻したかが残る。
	var n int64
	require.NoError(t, testDB.Model(&model.EmojiApplicationQuotaReset{}).
		Where(`"userId" = ?`, "ea_g5").Count(&n).Error)
	require.EqualValues(t, 2, n, "履歴が上書きされている")
}

// **審査待ちの上限にはリセットが効かないこと (#2962 / レビュー M1)。**
// 効いてしまうと、審査待ちが上限に達している利用者を「戻した」だけで
// **審査待ちを無制限に積めるようになる** — モデレーターの一覧が 1 人で埋まる。
// リセットは「期間内に何件出したか」を戻すもので、審査待ちは「今まさに
// 処理を待っている件数」なので、戻しても申請は残っている。
func TestEmojiApplicationRepository_QuotaReset_DoesNotClearPendingLimit(t *testing.T) {
	cleanupEmojiApplications(t)
	cleanupQuotaResets(t)
	defer func() { cleanupEmojiApplications(t); cleanupQuotaResets(t) }()
	createTestUser(t, "ea_g6")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	seedApplicationAt(t, "ea_g6a", "ea_g6", "a", model.EmojiApplicationPending, now.Add(-3*time.Hour))
	seedApplicationAt(t, "ea_g6b", "ea_g6", "b", model.EmojiApplicationPending, now.Add(-2*time.Hour))
	// **申請より新しいリセット。** 期間の窓は空になるが、審査待ちは変わらない。
	resetAt := now.Add(-time.Hour)

	limits := QuotaLimits{
		Windows:    []QuotaWindow{{Name: "day", Duration: 24 * time.Hour, Max: 2}},
		MaxPending: 2,
		ResetAt:    resetAt,
	}
	usage, err := repo.QuotaUsage("ea_g6", limits, now)
	require.NoError(t, err)
	require.Equal(t, 0, usage.Windows[0].Used, "期間の枠が戻っていない")
	require.Equal(t, 2, usage.Pending, "リセットで審査待ちの件数まで戻っている")
	require.True(t, usage.PendingFull, "リセットで審査待ちの上限まで解除されている")

	// 作成側も同じ — 期間は空いても審査待ちで弾かれる。
	err = repo.CreateWithQuota(quotaApp("ea_g6new", "ea_g6", "new", now), limits)
	var pe *PendingLimitExceededError
	require.ErrorAs(t, err, &pe, "リセットで審査待ちの上限まで解除されている")
	require.Equal(t, 2, pe.Used)
}

// **同時刻のリセットでも「最後の 1 件」が決まること (レビュー L3)。**
// tie-break が無いとプラン依存になり、**実行者と理由が入れ替わって見える**
// (境界の値は同じなので枠の計算は変わらず、画面以外では気付けない)。
func TestEmojiApplicationQuotaResetRepository_TieBreak(t *testing.T) {
	cleanupQuotaResets(t)
	defer cleanupQuotaResets(t)
	createTestUser(t, "ea_g7")
	repo := NewEmojiApplicationQuotaResetRepository(testDB)
	at := time.Now().Truncate(time.Millisecond)

	require.NoError(t, repo.Create(&model.EmojiApplicationQuotaReset{
		ID: "zzA", UserID: "ea_g7", ResetByID: "m1", Reason: "先に入れた", CreatedAt: at,
	}))
	require.NoError(t, repo.Create(&model.EmojiApplicationQuotaReset{
		ID: "zzB", UserID: "ea_g7", ResetByID: "m2", Reason: "後に入れた", CreatedAt: at,
	}))

	got, err := repo.LatestByUser("ea_g7")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "zzB", got.ID, "同時刻のリセットで最後の 1 件が決まっていない")
	require.Equal(t, "m2", got.ResetByID, "実行者が入れ替わって見える")
}

// **3 つの窓すべてに効くこと (#2962 / レビュー M1)。** issue の完了条件が
// 名指ししている項目だが、既存のテストはどれも `day` 1 本だけで、
// 「week / month には効かせない」変異が全パッケージ緑で通っていた (実測)。
// 壊れると、週次が満杯の利用者を戻しても `429 period=week` で弾かれ続け、
// 画面も 20/20 のまま — 完了条件そのものが壊れているのに CI は緑になる。
func TestEmojiApplicationRepository_QuotaReset_AppliesToAllWindows(t *testing.T) {
	cleanupEmojiApplications(t)
	cleanupQuotaResets(t)
	defer func() { cleanupEmojiApplications(t); cleanupQuotaResets(t) }()
	createTestUser(t, "ea_g8")
	repo := NewEmojiApplicationRepository(testDB)
	now := time.Now()

	// 3 つの窓それぞれの中に 1 件ずつ置く (day の中 / week の中 / month の中)。
	seedApplicationAt(t, "ea_g8a", "ea_g8", "a", model.EmojiApplicationRejected, now.Add(-2*time.Hour))
	seedApplicationAt(t, "ea_g8b", "ea_g8", "b", model.EmojiApplicationRejected, now.Add(-3*24*time.Hour))
	seedApplicationAt(t, "ea_g8c", "ea_g8", "c", model.EmojiApplicationRejected, now.Add(-20*24*time.Hour))

	windows := []QuotaWindow{
		{Name: "day", Duration: 24 * time.Hour, Max: 1},
		{Name: "week", Duration: 7 * 24 * time.Hour, Max: 2},
		{Name: "month", Duration: 30 * 24 * time.Hour, Max: 3},
	}

	full, err := repo.QuotaUsage("ea_g8", QuotaLimits{Windows: windows}, now)
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 3},
		[]int{full.Windows[0].Used, full.Windows[1].Used, full.Windows[2].Used},
		"リセット前の使用数が想定と違う")

	// **全部の行より新しいリセット。** 3 窓とも 0 になるのが正。
	after, err := repo.QuotaUsage("ea_g8",
		QuotaLimits{Windows: windows, ResetAt: now.Add(-time.Hour)}, now)
	require.NoError(t, err)
	require.Equal(t, []int{0, 0, 0},
		[]int{after.Windows[0].Used, after.Windows[1].Used, after.Windows[2].Used},
		"リセットが効いていない窓がある (day だけに効かせていないか)")
	for i := range after.Windows {
		require.True(t, after.Windows[i].RetryAt.IsZero(),
			"%s の窓が満杯のまま", after.Windows[i].Window.Name)
	}
}
