package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家 2026.10.1 (#17704) のお知らせの自動アーカイブ (#3809)。

// createAutoArchiveFixture inserts an announcement and, when active is false,
// drops isActive afterwards with UpdateFields (the path the admin update uses).
func createAutoArchiveFixture(t *testing.T, repo AnnouncementRepository, id string, active bool, at *time.Time, userID *string) {
	t.Helper()
	cleanupAnnouncement(t, id)
	a := &model.Announcement{ID: id, Title: id, Text: id, Icon: "info", Display: "normal", IsActive: true, AutoArchiveAt: at, UserID: userID}
	require.NoError(t, repo.Create(a))
	if !active {
		require.NoError(t, repo.UpdateFields(id, map[string]any{"isActive": false}))
	}
	t.Cleanup(func() { cleanupAnnouncement(t, id) })
}

func announcementIDSet(items []*model.Announcement) map[string]bool {
	out := map[string]bool{}
	for _, a := range items {
		out[a.ID] = true
	}
	return out
}

func TestAnnouncementRepository_AutoArchiveAtReadFilters(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	reader := insertTestUser(t, "u_ann_arc", "annarc")
	t.Cleanup(func() { cleanupUser(t, reader.ID) })

	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	// id は降順で先頭に来るよう "zz" で始める (limit で他の行に押し出されないため)。
	createAutoArchiveFixture(t, repo, "zzarc_future", true, &future, nil)
	createAutoArchiveFixture(t, repo, "zzarc_expired", true, &past, nil)
	createAutoArchiveFixture(t, repo, "zzarc_plain", true, nil, nil)
	createAutoArchiveFixture(t, repo, "zzarc_off", false, &future, nil)
	createAutoArchiveFixture(t, repo, "zzarc_user_expired", true, &past, &reader.ID)

	wantActive := map[string]bool{"zzarc_future": true, "zzarc_plain": true}
	wantArchived := map[string]bool{"zzarc_expired": true, "zzarc_off": true}
	check := func(t *testing.T, items []*model.Announcement, want map[string]bool, others ...string) {
		t.Helper()
		got := announcementIDSet(items)
		for _, id := range []string{"zzarc_future", "zzarc_expired", "zzarc_plain", "zzarc_off"} {
			assert.Equal(t, want[id], got[id], id)
		}
		for _, id := range others {
			assert.True(t, got[id], id)
		}
	}

	items, err := repo.List(true, 100, 0, "", "")
	require.NoError(t, err)
	check(t, items, wantActive)
	items, err = repo.List(false, 100, 0, "", "")
	require.NoError(t, err)
	check(t, items, wantArchived, "zzarc_user_expired")

	items, err = repo.ListGlobal(true, 100, "", "")
	require.NoError(t, err)
	check(t, items, wantActive)
	items, err = repo.ListGlobal(false, 100, "", "")
	require.NoError(t, err)
	check(t, items, wantArchived)

	items, err = repo.ListForUser(reader.ID, true, 100, "", "")
	require.NoError(t, err)
	check(t, items, wantActive)
	assert.False(t, announcementIDSet(items)["zzarc_user_expired"], "期限を過ぎた自分宛ては active に出さない")
	items, err = repo.ListForUser(reader.ID, false, 100, "", "")
	require.NoError(t, err)
	check(t, items, wantArchived, "zzarc_user_expired")

	items, err = repo.ListForAdmin("", "active", 100, "", "", time.Now())
	require.NoError(t, err)
	check(t, items, wantActive)
	items, err = repo.ListForAdmin("", "archived", 100, "", "", time.Now())
	require.NoError(t, err)
	check(t, items, wantArchived)
	items, err = repo.ListForAdmin(reader.ID, "archived", 100, "", "", time.Now())
	require.NoError(t, err)
	assert.True(t, announcementIDSet(items)["zzarc_user_expired"])

	// 未読 (i の hasUnreadAnnouncement / unreadAnnouncements) も期限を過ぎたものを数えない。
	items, err = repo.UnreadForUser(reader.ID)
	require.NoError(t, err)
	got := announcementIDSet(items)
	assert.True(t, got["zzarc_future"])
	assert.True(t, got["zzarc_plain"])
	assert.False(t, got["zzarc_expired"])
	assert.False(t, got["zzarc_off"])
	assert.False(t, got["zzarc_user_expired"])
}

func TestAnnouncementRepository_ArchiveExpired(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	now := time.Now().UTC().Truncate(time.Microsecond)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	exact := now
	createAutoArchiveFixture(t, repo, "zzarcx_expired", true, &past, nil)
	createAutoArchiveFixture(t, repo, "zzarcx_exact", true, &exact, nil)
	createAutoArchiveFixture(t, repo, "zzarcx_future", true, &future, nil)
	createAutoArchiveFixture(t, repo, "zzarcx_plain", true, nil, nil)
	createAutoArchiveFixture(t, repo, "zzarcx_off", false, &past, nil)

	// GORM は Create / Updates で updatedAt を自動で埋めるので、触らない行は前後で比べる。
	untouched := []string{"zzarcx_future", "zzarcx_plain", "zzarcx_off"}
	before := map[string]*time.Time{}
	for _, id := range untouched {
		a, err := repo.FindByID(id)
		require.NoError(t, err)
		before[id] = a.UpdatedAt
	}

	n, err := repo.ArchiveExpired(now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "isActive が true で期限ちょうどか過ぎた行だけ")

	for id, wantActive := range map[string]bool{
		"zzarcx_expired": false,
		"zzarcx_exact":   false,
		"zzarcx_future":  true,
		"zzarcx_plain":   true,
		"zzarcx_off":     false,
	} {
		a, err := repo.FindByID(id)
		require.NoError(t, err)
		assert.Equal(t, wantActive, a.IsActive, id)
	}
	for _, id := range []string{"zzarcx_expired", "zzarcx_exact"} {
		a, err := repo.FindByID(id)
		require.NoError(t, err)
		require.NotNil(t, a.UpdatedAt, id)
		assert.True(t, a.UpdatedAt.Equal(now), "%s の updatedAt は落とした時刻", id)
		require.NotNil(t, a.AutoArchiveAt, "期限の値は残す")
	}
	for _, id := range untouched {
		a, err := repo.FindByID(id)
		require.NoError(t, err)
		assert.Equal(t, before[id], a.UpdatedAt, "%s は触らない", id)
	}

	// 何度流しても同じ。
	n, err = repo.ArchiveExpired(now)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

func TestAnnouncementRepository_ArchiveExpired_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewAnnouncementRepository(testDB.WithContext(ctx))
	_, err := repo.ArchiveExpired(time.Now())
	assert.Error(t, err)
}

// 本家の migration と同じ列 (timestamptz、NULL 可) が作られていること。TS 2026.10.1 の
// DB に対しては IF NOT EXISTS で重ねない。
func TestAnnouncementAutoArchiveAtColumn(t *testing.T) {
	var row struct {
		DataType   string `gorm:"column:data_type"`
		IsNullable string `gorm:"column:is_nullable"`
	}
	require.NoError(t, testDB.Raw(`SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'announcement' AND column_name = 'autoArchiveAt'`).Scan(&row).Error)
	assert.Equal(t, "timestamp with time zone", row.DataType)
	assert.Equal(t, "YES", row.IsNullable)
}

// 期限ちょうどの時刻はアーカイブ側に入る (本家: active は autoArchiveAt > now、
// archived は autoArchiveAt <= now)。ListForAdmin は渡した now で絞る (#3809 レビュー)。
func TestAnnouncementRepository_ListForAdminAtExactExpiry(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	at := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	createAutoArchiveFixture(t, repo, "zzarcx_boundary", true, &at, nil)

	items, err := repo.ListForAdmin("", "active", 100, "", "", at)
	require.NoError(t, err)
	assert.False(t, announcementIDSet(items)["zzarcx_boundary"], "期限ちょうどの行が active に出た")
	items, err = repo.ListForAdmin("", "archived", 100, "", "", at)
	require.NoError(t, err)
	assert.True(t, announcementIDSet(items)["zzarcx_boundary"], "期限ちょうどの行が archived に出ない")

	before := at.Add(-time.Microsecond)
	items, err = repo.ListForAdmin("", "active", 100, "", "", before)
	require.NoError(t, err)
	assert.True(t, announcementIDSet(items)["zzarcx_boundary"], "期限の前の行が active に出ない")
}

// Create は isActive=false をそのまま書く (GORM の zero value で DB の既定値 true に
// ならない。作成の途中で期限が来たお知らせを、本家と同じく非アクティブで作るため)。
func TestAnnouncementRepository_CreateWritesInactive(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	cleanupAnnouncement(t, "zzarc_create_off")
	t.Cleanup(func() { cleanupAnnouncement(t, "zzarc_create_off") })
	require.NoError(t, repo.Create(&model.Announcement{ID: "zzarc_create_off", Title: "t", Text: "t", Icon: "info", Display: "normal", IsActive: false}))
	got, err := repo.FindByID("zzarc_create_off")
	require.NoError(t, err)
	assert.False(t, got.IsActive)
}
