package repository

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 保持の基準は**最終観測**。`createdAt` (= 初回観測) を基準にすると、
// 「初めて見たのは 100 日前だが今も使っている IP」まで消える。
func TestUserIPRepository_DeleteLastSeenBefore(t *testing.T) {
	repo := NewUserIPRepository(testDB)
	u := insertTestUser(t, "ip_del", "ipdel")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "user_ip" WHERE "userId" = ?`, u.ID)

	long := time.Now().Add(-100 * 24 * time.Hour)
	// 100 日前に初めて見て、それ以来見ていない → 消える。
	require.NoError(t, testDB.Create(&model.UserIP{
		UserID: u.ID, IP: "1.1.1.1", CreatedAt: long, LastSeenAt: long, ObservationCount: 1,
	}).Error)
	// 100 日前に初めて見たが、今も使っている → **残る**。
	require.NoError(t, testDB.Create(&model.UserIP{
		UserID: u.ID, IP: "3.3.3.3", CreatedAt: long, LastSeenAt: time.Now(), ObservationCount: 2,
	}).Error)
	// 直近に初めて見た → 残る。
	require.NoError(t, repo.Observe(u.ID, "2.2.2.2", time.Now()))

	n, err := repo.DeleteLastSeenBefore(time.Now().Add(-90 * 24 * time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "最終観測が 90 日より古い行だけ削除")

	rows, err := repo.ListByUser(u.ID, 10)
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.IP)
	}
	assert.ElementsMatch(t, []string{"2.2.2.2", "3.3.3.3"}, got)
}

// **`createdAt` は衝突しても動かない (初回観測)。** upstream の
// `INSERT ... orIgnore` と同じ意味にするための中核。TS から引き継いだ DB では
// TS が書いた行は初回観測なので、動かすと同じ列が行によって別の意味になる。
func TestUserIPRepository_ObserveKeepsFirstSeen(t *testing.T) {
	repo := NewUserIPRepository(testDB)
	u := insertTestUser(t, "ip_obs", "ipobs")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "user_ip" WHERE "userId" = ?`, u.ID)

	first := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond)
	later := time.Now().Add(-1 * time.Hour).Truncate(time.Millisecond)

	require.NoError(t, repo.Observe(u.ID, "192.0.2.1", first))
	require.NoError(t, repo.Observe(u.ID, "192.0.2.1", later))

	rows, err := repo.ListByUser(u.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "同じ (利用者, IP) は 1 行のまま")
	assert.WithinDuration(t, first, rows[0].CreatedAt, time.Millisecond, "createdAt が動いた")
	assert.WithinDuration(t, later, rows[0].LastSeenAt, time.Millisecond, "lastSeenAt が更新されていない")
	assert.Equal(t, 2, rows[0].ObservationCount)
}

// 観測が前後して届いても最終観測が巻き戻らない。記録は goroutine で走るので
// 順序の保証が無く、巻き戻ると保持の刈り取り基準まで古くなる。
func TestUserIPRepository_ObserveDoesNotRewindLastSeen(t *testing.T) {
	repo := NewUserIPRepository(testDB)
	u := insertTestUser(t, "ip_rew", "iprew")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "user_ip" WHERE "userId" = ?`, u.ID)

	newer := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	older := time.Now().Add(-5 * time.Hour).Truncate(time.Millisecond)

	require.NoError(t, repo.Observe(u.ID, "192.0.2.9", newer))
	require.NoError(t, repo.Observe(u.ID, "192.0.2.9", older))

	rows, err := repo.ListByUser(u.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.WithinDuration(t, newer, rows[0].LastSeenAt, time.Millisecond, "最終観測が巻き戻った")
	// 初回観測は古い側へ動かさない (upstream の orIgnore も動かさない)。
	assert.WithinDuration(t, newer, rows[0].CreatedAt, time.Millisecond)
	assert.Equal(t, 2, rows[0].ObservationCount)
}

// 別々の IP は別の行。
func TestUserIPRepository_ObserveSeparatesIPs(t *testing.T) {
	repo := NewUserIPRepository(testDB)
	u := insertTestUser(t, "ip_u1", "ipu1")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "user_ip" WHERE "userId" = ?`, u.ID)

	now := time.Now()
	require.NoError(t, repo.Observe(u.ID, "192.168.1.1", now))
	require.NoError(t, repo.Observe(u.ID, "10.0.0.1", now))

	ips, err := repo.ListByUser(u.ID, 10)
	require.NoError(t, err)
	assert.Len(t, ips, 2)
}

// **「その IP を使ったアカウント」が index で引けること (#3066 の中核クエリ)。**
// index が無いと `user_ip` の seq scan になる。
func TestUserIPRepository_IPLookupUsesIndex(t *testing.T) {
	u := insertTestUser(t, "ip_idx", "ipidx")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "user_ip" WHERE "userId" = ?`, u.ID)

	// **行が少ないと planner が seq scan を選ぶ。** index が「使える」ことを
	// 見たいので、選ばせるだけの行数を入れる。
	rows := make([]*model.UserIP, 0, 2000)
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 2000; i++ {
		rows = append(rows, &model.UserIP{
			UserID: u.ID, IP: fmt.Sprintf("198.51.%d.%d", i/256, i%256),
			CreatedAt: base, LastSeenAt: base, ObservationCount: 1,
		})
	}
	require.NoError(t, testDB.CreateInBatches(rows, 500).Error)
	require.NoError(t, testDB.Exec(`ANALYZE "user_ip"`).Error)

	var plan []string
	require.NoError(t, testDB.Raw(
		`EXPLAIN SELECT "userId" FROM "user_ip" WHERE ip = ? ORDER BY "lastSeenAt" DESC LIMIT 30`,
		"198.51.3.7").Scan(&plan).Error)
	joined := strings.Join(plan, "\n")
	// **完全名で照合する。** 前方一致だと #3105 が張り替えた
	// `IDX_user_ip_ip_lastSeenAt_userId` でも通り、実 schema に存在しない名前を
	// 主張したまま緑になる。
	assert.Contains(t, joined, "IDX_user_ip_ip_lastSeenAt_userId", "IP 検索が index を使っていない:\n"+joined)
	assert.NotContains(t, joined, "Seq Scan on user_ip", "IP 検索が seq scan になっている:\n"+joined)
}

// **並びは upstream と同じ `id DESC`。** upstream `admin/get-user-ips` が
// `order: { id: 'DESC' }` なので、observation の時刻ではなく行の採番順で返す。
// 時刻で並べ替えると、backdate した行の順序が upstream と食い違う。
func TestUserIPRepository_ListByUserOrdersByIDDesc(t *testing.T) {
	repo := NewUserIPRepository(testDB)
	u := insertTestUser(t, "ip_ord", "ipord")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "user_ip" WHERE "userId" = ?`, u.ID)

	now := time.Now()
	// **時刻の順序と採番の順序を逆にする。** 揃えてしまうと、どちらで並べても
	// 同じ結果になり基準を固定できない。
	require.NoError(t, repo.Observe(u.ID, "192.0.2.1", now))                    // id 小 / createdAt 新
	require.NoError(t, repo.Observe(u.ID, "192.0.2.2", now.Add(-72*time.Hour))) // id 大 / createdAt 旧

	rows, err := repo.ListByUser(u.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"192.0.2.2", "192.0.2.1"},
		[]string{rows[0].IP, rows[1].IP}, "id の降順になっていない")
}

func TestUserIPRepository_ListByUser_Empty(t *testing.T) {
	repo := NewUserIPRepository(testDB)
	ips, err := repo.ListByUser("ghost-ip-user", 10)
	require.NoError(t, err)
	assert.Empty(t, ips)
}

// limit <= 0 は既定の 30 に丸める。
//
// **存在しない user で見ても空虚。** `LIMIT 0` でも `LIMIT 30` でも結果は空なので、
// 丸めを外した変異が素通りする (実測)。30 件より多く入れて件数で見る。
func TestUserIPRepository_ListByUser_DefaultLimit(t *testing.T) {
	repo := NewUserIPRepository(testDB)
	u := insertTestUser(t, "ip_lim", "iplim")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "user_ip" WHERE "userId" = ?`, u.ID)

	now := time.Now()
	for i := 0; i < 35; i++ {
		require.NoError(t, repo.Observe(u.ID, fmt.Sprintf("203.0.113.%d", i), now))
	}

	ips, err := repo.ListByUser(u.ID, 0)
	require.NoError(t, err)
	assert.Len(t, ips, 30, "limit <= 0 が既定の 30 に丸められていない")

	ips, err = repo.ListByUser(u.ID, 5)
	require.NoError(t, err)
	assert.Len(t, ips, 5)
}
