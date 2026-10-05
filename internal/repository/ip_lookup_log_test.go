package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func resetIPLookupLog(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`DELETE FROM "ip_lookup_log"`).Error)
}

func auditRow(id, userID, kind, ip, target string, sinceDays, count int, at time.Time) *model.IPLookupLog {
	return &model.IPLookupLog{
		ID: id, UserID: userID, Kind: kind, IP: ip, TargetUserID: target,
		SinceDays: sinceDays, ResultCount: count, CreatedAt: at,
	}
}

func auditIDs(rows []*model.IPLookupLog) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// 書いた値がそのまま読める。**結果そのものは列に無い** (件数だけ)。
func TestIPLookupLog_CreateAndList(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPLookupLog(t, db)
	repo := NewIPLookupLogRepository(db)

	at := time.Now().Truncate(time.Millisecond)
	require.NoError(t, repo.Create(auditRow("a1", "mod1", model.IPLookupKindIP, "192.0.2.1", "", 90, 3, at)))

	rows, err := repo.List(10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "mod1", rows[0].UserID)
	assert.Equal(t, model.IPLookupKindIP, rows[0].Kind)
	assert.Equal(t, "192.0.2.1", rows[0].IP)
	assert.Equal(t, "", rows[0].TargetUserID)
	assert.Equal(t, 90, rows[0].SinceDays)
	assert.Equal(t, 3, rows[0].ResultCount)
	assert.WithinDuration(t, at, rows[0].CreatedAt, time.Millisecond)
}

// 利用者起点の照会は IP が空で対象が入る。**列を分けない設計の確認。**
func TestIPLookupLog_RelatedKind(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPLookupLog(t, db)
	repo := NewIPLookupLogRepository(db)

	at := time.Now().Truncate(time.Millisecond)
	require.NoError(t, repo.Create(auditRow("a1", "mod1", model.IPLookupKindRelated, "", "target1", 30, 5, at)))

	rows, err := repo.List(10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, model.IPLookupKindRelated, rows[0].Kind)
	assert.Equal(t, "", rows[0].IP)
	assert.Equal(t, "target1", rows[0].TargetUserID)
}

// **並びは新しい順 → id 降順。** 同じ時刻の行で順序が実行ごとに変わると、
// ページを送ったときに同じ行が 2 回出たり抜けたりする。
func TestIPLookupLog_ListOrder(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPLookupLog(t, db)
	repo := NewIPLookupLogRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	require.NoError(t, repo.Create(auditRow("a_old", "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0, now.Add(-2*time.Hour))))
	require.NoError(t, repo.Create(auditRow("a_new", "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0, now)))
	// 同じ時刻の 2 件。id 降順で並ぶ。
	require.NoError(t, repo.Create(auditRow("a_tie_a", "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0, now.Add(-time.Hour))))
	require.NoError(t, repo.Create(auditRow("a_tie_z", "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0, now.Add(-time.Hour))))

	rows, err := repo.List(10, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"a_new", "a_tie_z", "a_tie_a", "a_old"}, auditIDs(rows))
}

// limit / offset が効く。丸めも見る (limit <= 0 を渡すと常に空になる)。
func TestIPLookupLog_ListPaging(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPLookupLog(t, db)
	repo := NewIPLookupLogRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		id := "a" + string(rune('0'+i))
		require.NoError(t, repo.Create(auditRow(id, "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0,
			now.Add(-time.Duration(i)*time.Hour))))
	}

	page1, err := repo.List(2, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"a0", "a1"}, auditIDs(page1))

	page2, err := repo.List(2, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"a2", "a3"}, auditIDs(page2))

	clamped, err := repo.List(0, 0)
	require.NoError(t, err)
	assert.Len(t, clamped, 1, "limit <= 0 が 1 に丸められていない")

	negative, err := repo.List(2, -5)
	require.NoError(t, err)
	assert.Len(t, negative, 2, "offset < 0 が 0 に丸められていない")
}

// **保持期間で刈る。** 刈らないと照会に使った IP が永久に残り、
// moderation_log に IP を書くのと変わらなくなる。
func TestIPLookupLog_DeleteOlderThan(t *testing.T) {
	db := ipSearchTestDB(t)
	resetIPLookupLog(t, db)
	repo := NewIPLookupLogRepository(db)

	now := time.Now().Truncate(time.Millisecond)
	require.NoError(t, repo.Create(auditRow("a_old", "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0, now.Add(-100*24*time.Hour))))
	require.NoError(t, repo.Create(auditRow("a_keep", "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0, now.Add(-10*24*time.Hour))))
	// 境界ちょうどは消さない (`<` なので)。
	threshold := now.Add(-90 * 24 * time.Hour)
	require.NoError(t, repo.Create(auditRow("a_edge", "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0, threshold)))

	n, err := repo.DeleteOlderThan(threshold)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	rows, err := repo.List(10, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a_keep", "a_edge"}, auditIDs(rows))
}

// SQL が壊れていれば err で返る (空の一覧にしない、#2792)。
func TestIPLookupLog_PropagatesError(t *testing.T) {
	// テーブルの無い schema へつなぐ (理由は user_ip_search_test.go と同じ)。
	repo := NewIPLookupLogRepository(emptySchemaDB(t))

	_, err := repo.List(10, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ip_lookup_log list")
	// SQL に到達していることまで見る (理由は user_ip_search_test.go と同じ)。
	assert.Contains(t, err.Error(), "42P01", "SQL に到達していない: "+err.Error())

	err = repo.Create(auditRow("a1", "m", model.IPLookupKindIP, "192.0.2.1", "", 90, 0, time.Now()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ip_lookup_log create")

	_, err = repo.DeleteOlderThan(time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ip_lookup_log prune")
}
