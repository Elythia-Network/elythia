package repository

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/pgarray"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ipSearchDB は検索テスト専用の兄弟 schema。
//
// **`HasAnyHistory` がテーブル全体の絶対的な事実を返す**ので、同じパッケージの
// 他テストが残した `user_ip` の行が混ざると期待値が成立しない (docs/testing.md
// 「DB を使うテストの分離」)。専用 schema を一度だけ作って使い回す。
var (
	ipSearchDB    *gorm.DB
	ipSearchOnce  sync.Once
	ipSearchDBErr error
)

func ipSearchTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	ipSearchOnce.Do(func() {
		db, err := testutil.OpenTestDBSchema("ipsearch")
		if err != nil {
			ipSearchDBErr = err
			return
		}
		testutil.ApplyMigrations(db)
		ipSearchDB = db
	})
	require.NoError(t, ipSearchDBErr)
	require.NotNil(t, ipSearchDB)
	return ipSearchDB
}

func resetIPSearchFixtures(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`DELETE FROM "user_ip"`).Error)
}

// observeAt inserts one observation directly so the test controls the times.
func observeAt(t *testing.T, db *gorm.DB, userID, ip string, first, last time.Time, count int) {
	t.Helper()
	require.NoError(t, db.Create(&model.UserIP{
		UserID: userID, IP: ip,
		CreatedAt: first, LastSeenAt: last, ObservationCount: count,
	}).Error)
}

func userIDsOfRows(rows []UserIPAccountRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UserID)
	}
	return out
}

// 並びは最終観測の降順 → 初回観測の降順 → userId 昇順。
//
// **`userId` の tiebreaker が要る。** 同じ時刻の行で順序が実行ごとに変わると、
// offset で 2 ページ目を引いたときに同じ行が 2 回出たり抜けたりする。
func TestUserIPSearch_ListAccountsByIPOrder(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	// 最終観測が新しい順に並ぶ。
	observeAt(t, db, "u_old", "192.0.2.1", now.Add(-72*time.Hour), now.Add(-48*time.Hour), 1)
	observeAt(t, db, "u_new", "192.0.2.1", now.Add(-24*time.Hour), now.Add(-time.Hour), 2)
	// 最終観測が同じ 2 件。初回観測が新しい方が先。
	observeAt(t, db, "u_tie_b", "192.0.2.1", now.Add(-10*time.Hour), now.Add(-2*time.Hour), 1)
	observeAt(t, db, "u_tie_a", "192.0.2.1", now.Add(-5*time.Hour), now.Add(-2*time.Hour), 1)
	// 最終観測も初回観測も同じ 2 件。userId 昇順。
	observeAt(t, db, "u_zz", "192.0.2.1", now.Add(-3*time.Hour), now.Add(-3*time.Hour), 1)
	observeAt(t, db, "u_aa", "192.0.2.1", now.Add(-3*time.Hour), now.Add(-3*time.Hour), 1)
	// 別の IP は混ざらない。
	observeAt(t, db, "u_other", "198.51.100.1", now, now, 1)

	rows, err := repo.ListAccountsByIP("192.0.2.1", now.Add(-90*24*time.Hour), 50, 0)
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"u_new", "u_tie_a", "u_tie_b", "u_aa", "u_zz", "u_old"},
		userIDsOfRows(rows))
}

// 観測の値をそのまま返す。
func TestUserIPSearch_ReturnsObservationFields(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	first := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond)
	last := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	observeAt(t, db, "u1", "192.0.2.1", first, last, 7)

	rows, err := repo.ListAccountsByIP("192.0.2.1", first.Add(-time.Hour), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "u1", rows[0].UserID)
	assert.WithinDuration(t, first, rows[0].FirstSeen, time.Millisecond)
	assert.WithinDuration(t, last, rows[0].LastSeen, time.Millisecond)
	assert.Equal(t, 7, rows[0].ObservationCount)
}

// **窓は最終観測で切る。** 初回観測で切ると「初めて見たのは窓の外だが今も
// 使っている」候補が落ちる (#3103 の保持と同じ理由)。
func TestUserIPSearch_WindowFiltersByLastSeen(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	// 初回は窓の外だが最終は窓の中 → 出る。
	observeAt(t, db, "u_live", "192.0.2.1", now.Add(-100*24*time.Hour), now.Add(-time.Hour), 2)
	// 初回も最終も窓の外 → 出ない。
	observeAt(t, db, "u_stale", "192.0.2.1", now.Add(-100*24*time.Hour), now.Add(-95*24*time.Hour), 1)

	rows, err := repo.ListAccountsByIP("192.0.2.1", now.Add(-90*24*time.Hour), 10, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"u_live"}, userIDsOfRows(rows))
}

// limit と offset が効く。効かないと画面が無制限に行を受け取る。
func TestUserIPSearch_LimitAndOffset(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		// 最終観測をずらして順序を決定的にする。
		observeAt(t, db, string(rune('a'+i)), "192.0.2.1",
			now.Add(-time.Duration(i+1)*time.Hour), now.Add(-time.Duration(i+1)*time.Hour), 1)
	}
	since := now.Add(-90 * 24 * time.Hour)

	page1, err := repo.ListAccountsByIP("192.0.2.1", since, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, userIDsOfRows(page1))

	page2, err := repo.ListAccountsByIP("192.0.2.1", since, 2, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"c", "d"}, userIDsOfRows(page2))

	// 端を越えたら空。
	beyond, err := repo.ListAccountsByIP("192.0.2.1", since, 2, 10)
	require.NoError(t, err)
	assert.Empty(t, beyond)
}

// 引数の丸め。limit <= 0 を 0 のまま渡すと `LIMIT 0` で常に空になる。
func TestUserIPSearch_ClampsArguments(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	observeAt(t, db, "u1", "192.0.2.1", now, now, 1)
	since := now.Add(-time.Hour)

	rows, err := repo.ListAccountsByIP("192.0.2.1", since, 0, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "limit <= 0 が 1 に丸められていない")

	rows, err = repo.ListAccountsByIP("192.0.2.1", since, 10, -5)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "offset < 0 が 0 に丸められていない")
}

// 一致が無ければ空。エラーにしない (「一致なし」は正常な結果)。
func TestUserIPSearch_NoMatchIsEmpty(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)

	rows, err := NewUserIPSearchRepository(db).ListAccountsByIP(
		"203.0.113.77", time.Now().Add(-24*time.Hour), 10, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// **「一致なし」と「そもそも記録が無い」を区別する材料。**
func TestUserIPSearch_HasAnyHistory(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	has, err := repo.HasAnyHistory()
	require.NoError(t, err)
	assert.False(t, has, "行が無いのに記録ありと答えている")

	// 窓の外の行しか無くても「記録はある」。記録の有無と窓は別の問い。
	old := time.Now().Add(-365 * 24 * time.Hour)
	observeAt(t, db, "u1", "192.0.2.1", old, old, 1)

	has, err = repo.HasAnyHistory()
	require.NoError(t, err)
	assert.True(t, has)
}

// SQL が壊れていれば err で返る (空の結果にしない)。
func TestUserIPSearch_PropagatesError(t *testing.T) {
	// **テーブルの無い schema へつなぐ。** トランザクションで search_path を
	// 壊す形は使えない — 照会は自分でトランザクションを開くので、既に tx の
	// `*gorm.DB` を渡すと SQL に到達する前に落ちる (検査が別物になる)。
	repo := NewUserIPSearchRepository(emptySchemaDB(t))

	rows, err := repo.ListAccountsByIP("192.0.2.1", time.Now(), 10, 0)
	require.Error(t, err)
	assert.Nil(t, rows)
	assert.Contains(t, err.Error(), "user_ip accounts by ip")
	// **SQL に到達していることまで見る。** ラッパー側の文字列だけだと、
	// fn へ届く前に失敗する形 (2 周目に直した形そのもの) へ戻しても緑になる。
	assert.Contains(t, err.Error(), "42P01", "SQL に到達していない: "+err.Error())

	has, err := repo.HasAnyHistory()
	require.Error(t, err)
	assert.False(t, has)
	assert.Contains(t, err.Error(), "user_ip has any history")
}

// --- #3105: 共有 IP の抽出 ---

// 窓の中の IP を最終観測の新しい順に返す。**打ち切ったときに残るのが重みの
// 大きい IP になる**ようにこの順序が要る (時間減衰は古いほど軽い)。
func TestUserIPSearch_ListIPsByUser(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	observeAt(t, db, "u1", "192.0.2.3", now.Add(-40*time.Hour), now.Add(-3*time.Hour), 1)
	observeAt(t, db, "u1", "192.0.2.1", now.Add(-40*time.Hour), now.Add(-time.Hour), 1)
	observeAt(t, db, "u1", "192.0.2.2", now.Add(-40*time.Hour), now.Add(-2*time.Hour), 1)
	// 窓の外は出ない。
	observeAt(t, db, "u1", "192.0.2.9", now.Add(-100*24*time.Hour), now.Add(-95*24*time.Hour), 1)
	// 他人の IP は混ざらない。
	observeAt(t, db, "u2", "198.51.100.1", now, now, 1)

	rows, err := repo.ListIPsByUser("u1", now.Add(-90*24*time.Hour), 10)
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.IP)
	}
	assert.Equal(t, []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, got)
	assert.WithinDuration(t, now.Add(-time.Hour), rows[0].LastSeen, time.Millisecond)
}

// **窓は最終観測で切る。** 初回観測で切ると「初めて見たのは窓の外だが今も
// 使っている」IP が起点から落ち、そこで一致していた候補が丸ごと出なくなる。
func TestUserIPSearch_ListIPsByUserWindowIsLastSeen(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	// 初回は窓の外だが最終は窓の中 → 起点に入る。
	observeAt(t, db, "u1", "192.0.2.1", now.Add(-100*24*time.Hour), now.Add(-time.Hour), 2)
	// 初回も最終も窓の外 → 入らない。
	observeAt(t, db, "u1", "192.0.2.2", now.Add(-100*24*time.Hour), now.Add(-95*24*time.Hour), 1)

	rows, err := repo.ListIPsByUser("u1", now.Add(-90*24*time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "窓が createdAt で切られている")
	assert.Equal(t, "192.0.2.1", rows[0].IP)
}

// **並びは最終観測の降順。** 50 本で切る設計の根拠がここに乗っている
// (打ち切ったときに残るのが重みの大きい IP になる)。IP 昇順と紛れない
// fixture で固定する。
func TestUserIPSearch_ListIPsByUserOrderIsLastSeen(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	// IP の昇順と最終観測の降順がわざと逆になるように置く。
	observeAt(t, db, "u1", "192.0.2.1", now.Add(-40*time.Hour), now.Add(-9*time.Hour), 1)
	observeAt(t, db, "u1", "192.0.2.2", now.Add(-40*time.Hour), now.Add(-5*time.Hour), 1)
	observeAt(t, db, "u1", "192.0.2.3", now.Add(-40*time.Hour), now.Add(-time.Hour), 1)

	rows, err := repo.ListIPsByUser("u1", now.Add(-90*24*time.Hour), 10)
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.IP)
	}
	assert.Equal(t, []string{"192.0.2.3", "192.0.2.2", "192.0.2.1"}, got,
		"IP 昇順で返っている (最終観測の降順になっていない)")
}

// limit で切れる。切らないと起点の IP 数を外から無制限にできる。
func TestUserIPSearch_ListIPsByUserLimits(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		observeAt(t, db, "u1", "192.0.2."+string(rune('1'+i)), now, now.Add(-time.Duration(i)*time.Hour), 1)
	}
	since := now.Add(-90 * 24 * time.Hour)

	rows, err := repo.ListIPsByUser("u1", since, 2)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	// limit <= 0 を 0 のまま渡すと `LIMIT 0` で常に空になる。
	rows, err = repo.ListIPsByUser("u1", since, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "limit <= 0 が 1 に丸められていない")
}

func sharedKeys(rows []UserIPSharedRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.IP+"/"+r.UserID)
	}
	return out
}

// 起点の IP を使ったアカウントを返す。**対象本人も含める** — 「その IP を使った
// アカウント数」は本人を含む数なので、SQL で落とすと数えられなくなる。
func TestUserIPSearch_ListSharedIPAccounts(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	observeAt(t, db, "target", "192.0.2.1", now.Add(-40*time.Hour), now.Add(-time.Hour), 1)
	observeAt(t, db, "u_a", "192.0.2.1", now.Add(-40*time.Hour), now.Add(-2*time.Hour), 1)
	observeAt(t, db, "u_b", "192.0.2.1", now.Add(-40*time.Hour), now.Add(-3*time.Hour), 1)
	observeAt(t, db, "target", "192.0.2.2", now.Add(-40*time.Hour), now.Add(-4*time.Hour), 1)
	observeAt(t, db, "u_a", "192.0.2.2", now.Add(-40*time.Hour), now.Add(-5*time.Hour), 1)
	// 起点に含まれない IP のアカウントは出ない。
	observeAt(t, db, "u_c", "198.51.100.1", now, now, 1)
	// 窓の外は出ない。
	observeAt(t, db, "u_d", "192.0.2.1", now.Add(-100*24*time.Hour), now.Add(-95*24*time.Hour), 1)

	since := now.Add(-90 * 24 * time.Hour)
	rows, err := repo.ListSharedIPAccounts([]string{"192.0.2.1", "192.0.2.2"}, since, 50)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"192.0.2.1/target", "192.0.2.1/u_a", "192.0.2.1/u_b",
		"192.0.2.2/target", "192.0.2.2/u_a",
	}, sharedKeys(rows))
	assert.WithinDuration(t, now.Add(-time.Hour), rows[0].LastSeen, time.Millisecond)
}

// 窓の中の行だけを返す。件数はこの行数から呼び出し側が数える。
func TestUserIPSearch_SharedIPAccountsWindow(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	observeAt(t, db, "target", "192.0.2.1", now, now, 1)
	for _, u := range []string{"u_a", "u_b", "u_c"} {
		observeAt(t, db, u, "192.0.2.1", now, now, 1)
	}
	// 窓の外の 1 件は出ない (検索と基準を揃える)。
	observeAt(t, db, "u_old", "192.0.2.1", now.Add(-100*24*time.Hour), now.Add(-95*24*time.Hour), 1)

	rows, err := repo.ListSharedIPAccounts([]string{"192.0.2.1"}, now.Add(-90*24*time.Hour), 50)
	require.NoError(t, err)
	assert.Len(t, rows, 4, "対象本人を含む窓内の行が返っていない")
}

// **perIP で切る。** 切らないと 1 つの IP で数千行を返しうる。
// 各 IP の最終観測が新しい順に残す (重みの大きいほうを残す)。
func TestUserIPSearch_SharedIPAccountsPerIPCap(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	// 最終観測が新しい順に u_0 > u_1 > ... となるよう並べる。
	for i := 0; i < 6; i++ {
		observeAt(t, db, "u_"+string(rune('0'+i)), "192.0.2.1", now, now.Add(-time.Duration(i+1)*time.Hour), 1)
	}

	rows, err := repo.ListSharedIPAccounts([]string{"192.0.2.1"}, now.Add(-90*24*time.Hour), 2)
	require.NoError(t, err)
	require.Len(t, rows, 2, "perIP で切れていない")
	assert.Equal(t, []string{"u_0", "u_1"}, []string{rows[0].UserID, rows[1].UserID})

	// perIP <= 0 を 0 のまま渡すと常に空になる。
	rows, err = repo.ListSharedIPAccounts([]string{"192.0.2.1"}, now.Add(-90*24*time.Hour), 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "perIP <= 0 が 1 に丸められていない")
}

// **1 つの IP の上限が他の IP を食わない。** 全体の LIMIT で切る形だと、
// アカウントの多い IP 1 本で他の起点の候補が全部落ちる。
func TestUserIPSearch_SharedIPAccountsCapIsPerIP(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)
	repo := NewUserIPSearchRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		observeAt(t, db, "crowd_"+string(rune('0'+i)), "192.0.2.1", now, now, 1)
	}
	observeAt(t, db, "lonely", "192.0.2.2", now, now, 1)

	rows, err := repo.ListSharedIPAccounts([]string{"192.0.2.1", "192.0.2.2"}, now.Add(-90*24*time.Hour), 2)
	require.NoError(t, err)
	byIP := map[string]int{}
	for _, r := range rows {
		byIP[r.IP]++
	}
	assert.Equal(t, 2, byIP["192.0.2.1"])
	assert.Equal(t, 1, byIP["192.0.2.2"], "混雑した IP の上限が別の IP の候補を食っている")
}

// IP を 1 つも渡さなければ引かない (空の `IN ()` は SQL として不正)。
func TestUserIPSearch_SharedIPAccountsEmptyInput(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)

	rows, err := NewUserIPSearchRepository(db).ListSharedIPAccounts(nil, time.Now(), 10)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// SQL が壊れていれば err で返る (空の結果にしない)。
func TestUserIPSearch_SharedPropagatesError(t *testing.T) {
	// **テーブルの無い schema へつなぐ。** トランザクションで search_path を
	// 壊す形は使えない — 照会は自分でトランザクションを開くので、既に tx の
	// `*gorm.DB` を渡すと SQL に到達する前に落ちる (検査が別物になる)。
	repo := NewUserIPSearchRepository(emptySchemaDB(t))

	_, err := repo.ListIPsByUser("u1", time.Now(), 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "user_ip ips by user")
	// SQL に到達していることまで見る (理由は PropagatesError と同じ)。
	assert.Contains(t, err.Error(), "42P01", "SQL に到達していない: "+err.Error())

	_, err = repo.ListSharedIPAccounts([]string{"192.0.2.1"}, time.Now(), 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "user_ip shared accounts")
}

// **関連候補の抽出が有界であること** (#3105 / migration 000095)。
//
// 見るのは**実際に読んだ行数**で、実行計画の形ではない。plan の形は表の大きさと
// 統計に依るので、**手元で何度も回して統計が古くなった schema では Bitmap Index
// Scan + Sort が正しい plan になり、形で判定すると確定的に落ちる** (CI は毎回
// クリーンな DB なので緑のまま = 手元で繰り返す開発者だけが踏む、#2756 と同型)。
//
// 有界かどうかは「`perIP` を超えて読んでいないか」で決まるので、そちらを直接見る。
// `userId` が並びに入っていて index に無いと、同じ `lastSeenAt` が固まっている
// ぶんを全部読んでから並べ替えることになり、この assertion が落ちる。
func TestUserIPSearch_SharedLookupReadsBoundedRows(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPSearchFixtures(t, db)

	// **同じ `lastSeenAt` を固める。** ばらけていると `(ip, lastSeenAt DESC)` でも
	// incremental sort が効いて有界になるので、index の差が出ない。
	now := time.Now().Truncate(time.Millisecond)
	require.NoError(t, db.Exec(`
		INSERT INTO "user_ip" ("userId", "ip", "createdAt", "lastSeenAt", "observationCount")
		SELECT 'crowd' || i, '192.0.2.1', ?, ?, 1 FROM generate_series(1, 500) AS i`,
		now, now).Error)
	// planner に index を選ばせるだけの散らばりを別 IP で作る。
	require.NoError(t, db.Exec(`
		INSERT INTO "user_ip" ("userId", "ip", "createdAt", "lastSeenAt", "observationCount")
		SELECT 'other' || i, '198.51.100.' || (i % 250 + 1), ?, ?::timestamptz - make_interval(secs => i), 1
		FROM generate_series(1, 2500) AS i`, now, now).Error)
	require.NoError(t, db.Exec(`ANALYZE "user_ip"`).Error)

	// **index 以外の選択肢を落としてから測る。** 小さい表では「全部読んで並べ替え
	// る」ほうが実際に安いので、planner はそちらを選ぶ (それは正しい判断)。ここで
	// 見たいのは**表が育ったときに有界な選択肢があるか** = index が並び順を丸ごと
	// 賄えるかで、それは代替を落とせば行数に出る。
	tx := db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	require.NoError(t, tx.Exec(`SET LOCAL enable_seqscan = off`).Error)
	require.NoError(t, tx.Exec(`SET LOCAL enable_bitmapscan = off`).Error)

	const perIP = 10
	var raw []string
	require.NoError(t, tx.Raw(`EXPLAIN (ANALYZE, FORMAT JSON) `+userIPSharedSQL,
		pgarray.StringArray([]string{"192.0.2.1"}), now.Add(-90*24*time.Hour), perIP).
		Scan(&raw).Error)
	require.Len(t, raw, 1)

	read := rowsReadFromUserIP(t, raw[0])
	// **上限の数倍で切る。** index の並びに乗っていれば `perIP` 件で止まる。
	// `userId` が index に無いと、同じ `lastSeenAt` のぶん (500 件) を全部読む。
	assert.LessOrEqual(t, read, perIP*3,
		"1 IP から perIP を大きく超える行を読んでいる (走査が有界でない):\n"+raw[0])
}

// rowsReadFromUserIP sums `Actual Rows` over every plan node that scans user_ip.
func rowsReadFromUserIP(t *testing.T, planJSON string) int {
	t.Helper()
	var plans []map[string]any
	require.NoError(t, json.Unmarshal([]byte(planJSON), &plans))
	total := 0
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if name, _ := node["Relation Name"].(string); name == "user_ip" {
			if rows, ok := node["Actual Rows"].(float64); ok {
				total += int(rows)
			}
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, c := range children {
				if m, ok := c.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	for _, p := range plans {
		if m, ok := p["Plan"].(map[string]any); ok {
			walk(m)
		}
	}
	return total
}

// **照会は DB 側でも時間で切る** (#3106)。
//
// 走査の上限 (起点 50 本 × 候補 200 件) は「返す量」を決めるもので、これは
// 「その上限の中でも遅いときに何秒で諦めるか」。index が効かない構成や DB 側の
// 競合では、有界な問い合わせでも待たされる。
//
// **本番コードが掛けた上限そのものを見る。** コールバックの中でテストが
// `SET LOCAL` し直す形にすると、`withLookupTimeout` から Exec を丸ごと消しても
// 緑のまま通る (実測)。ここでは (a) トランザクションの中で実際に効いている値、
// (b) それが定数と一致すること、の 2 つを見る。
func TestUserIPSearch_LookupTimeoutApplies(t *testing.T) {
	db := ipSearchTestDB(t)
	repo := NewUserIPSearchRepository(db).(*userIPSearchRepository)

	require.Equal(t, lookupStatementTimeout, repo.lookupTimeout,
		"コンストラクタが既定の上限を配線していない")

	// **`SHOW` を同じトランザクションの中で引く。** 別接続に掛かっていれば
	// セッション既定 (`0` = 無制限) が返るので、掛け違いもここで落ちる。
	var effective string
	require.NoError(t, repo.withLookupTimeout(func(tx *gorm.DB) error {
		return tx.Raw(`SHOW statement_timeout`).Scan(&effective).Error
	}))
	assert.Equal(t, "10s", effective,
		"withLookupTimeout がトランザクションに上限を掛けていない")
}

// **実際に切れることまで見る。** 値が入っているだけでは、書式が違って
// PostgreSQL に無視される形 (単位の取り違えなど) を捕まえられない。
//
// 上限はフィールドなので、10 秒待たずに縮めて試せる。**縮めるのはテスト側の
// フィールドで、コールバックの中で `SET LOCAL` し直すのではない** — 後者だと
// 本番コードの Exec を消しても通る。
func TestUserIPSearch_LookupTimeoutCancels(t *testing.T) {
	db := ipSearchTestDB(t)
	repo := NewUserIPSearchRepository(db).(*userIPSearchRepository)
	repo.lookupTimeout = 200 * time.Millisecond

	start := time.Now()
	err := repo.withLookupTimeout(func(tx *gorm.DB) error {
		return tx.Exec(`SELECT pg_sleep(5)`).Error
	})
	require.Error(t, err, "statement_timeout が効いていない (5 秒眠れてしまった)")
	assert.Contains(t, err.Error(), "57014", "取り消し以外の理由で失敗している: "+err.Error())
	assert.Less(t, time.Since(start), 5*time.Second, "上限で切れずに眠り切っている")
}

// **セッションに残さない。** `SET LOCAL` はトランザクション限りなので、同じ接続を
// 使い回す後続のクエリに上限が漏れない。漏れると無関係なクエリまで切れる。
func TestUserIPSearch_LookupTimeoutDoesNotLeak(t *testing.T) {
	db := ipSearchTestDB(t)
	repo := NewUserIPSearchRepository(db).(*userIPSearchRepository)
	repo.lookupTimeout = time.Millisecond

	// 上限が漏れれば、この後の 50ms のクエリが 57014 で落ちる。
	require.NoError(t, repo.withLookupTimeout(func(tx *gorm.DB) error { return nil }))

	var out int
	require.NoError(t, db.Raw(`SELECT 1 FROM pg_sleep(0.05)`).Scan(&out).Error,
		"直前の SET LOCAL がセッションに残っている")
}
