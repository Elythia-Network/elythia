package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupChannel(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "channel_following" WHERE "followeeId" = ?`, id)
	testDB.Exec(`DELETE FROM "channel" WHERE id = ?`, id)
}

func newTestChannel(id, name string, ownerID *string) *model.Channel {
	return &model.Channel{
		ID:                    id,
		Name:                  name,
		UserID:                ownerID,
		Color:                 "#86b300",
		AllowRenoteToExternal: true,
	}
}

func TestChannelRepository_CreateAndFindByID(t *testing.T) {
	repo := NewChannelRepository(testDB)
	user := insertTestUser(t, "u_chr_1", "channeluser1")
	defer cleanupUser(t, user.ID)

	uid := user.ID
	c := newTestChannel("ch_cr_1", "alpha channel", &uid)
	require.NoError(t, repo.Create(c))
	defer cleanupChannel(t, c.ID)

	got, err := repo.FindByID(c.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha channel", got.Name)
	require.NotNil(t, got.UserID)
	assert.Equal(t, user.ID, *got.UserID)
}

func TestChannelRepository_FindByID_NotFound(t *testing.T) {
	repo := NewChannelRepository(testDB)
	_, err := repo.FindByID("missing")
	assert.Error(t, err)
}

// SearchDescription=true は name OR description にマッチ、false は name のみ。
func TestChannelRepository_List_SearchDescription(t *testing.T) {
	repo := NewChannelRepository(testDB)
	user := insertTestUser(t, "u_chr_sd", "channelusersd")
	defer cleanupUser(t, user.ID)
	uid := user.ID

	desc := "about gophers and go"
	c := newTestChannel("ch_sd_1", "sd-plainname", &uid)
	c.Description = &desc
	require.NoError(t, repo.Create(c))
	defer cleanupChannel(t, c.ID)

	// nameAndDescription (SearchDescription=true): description 一致で hit。
	rows, err := repo.List(model.ChannelListFilter{Query: "gophers", SearchDescription: true})
	require.NoError(t, err)
	found := false
	for _, r := range rows {
		if r.ID == "ch_sd_1" {
			found = true
		}
	}
	assert.True(t, found, "SearchDescription=true は description 一致を返す")

	// nameOnly (SearchDescription=false): description 一致は除外。
	rows, err = repo.List(model.ChannelListFilter{Query: "gophers", SearchDescription: false})
	require.NoError(t, err)
	for _, r := range rows {
		assert.NotEqual(t, "ch_sd_1", r.ID, "nameOnly は description を見ない")
	}
}

func TestChannelRepository_UpdateFields(t *testing.T) {
	repo := NewChannelRepository(testDB)
	user := insertTestUser(t, "u_chr_2", "channeluser2")
	defer cleanupUser(t, user.ID)

	uid := user.ID
	c := newTestChannel("ch_cr_2", "beta", &uid)
	require.NoError(t, repo.Create(c))
	defer cleanupChannel(t, c.ID)

	desc := "updated description"
	require.NoError(t, repo.UpdateFields(c.ID, map[string]any{
		"name":        "beta updated",
		"description": &desc,
	}))

	got, err := repo.FindByID(c.ID)
	require.NoError(t, err)
	assert.Equal(t, "beta updated", got.Name)
	require.NotNil(t, got.Description)
	assert.Equal(t, "updated description", *got.Description)
}

func TestChannelRepository_UpdateFields_NoOp(t *testing.T) {
	repo := NewChannelRepository(testDB)
	require.NoError(t, repo.UpdateFields("any", nil))
}

func TestChannelRepository_IncrementCount(t *testing.T) {
	repo := NewChannelRepository(testDB)
	user := insertTestUser(t, "u_chr_3", "channeluser3")
	defer cleanupUser(t, user.ID)

	uid := user.ID
	c := newTestChannel("ch_cr_3", "gamma", &uid)
	require.NoError(t, repo.Create(c))
	defer cleanupChannel(t, c.ID)

	require.NoError(t, repo.IncrementCount(c.ID, "notesCount", 3))
	require.NoError(t, repo.IncrementCount(c.ID, "usersCount", 1))

	got, err := repo.FindByID(c.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, got.NotesCount)
	assert.Equal(t, 1, got.UsersCount)
}

func TestChannelRepository_List_Filters(t *testing.T) {
	repo := NewChannelRepository(testDB)
	user := insertTestUser(t, "u_chr_4", "channeluser4")
	defer cleanupUser(t, user.ID)
	uid := user.ID

	a := newTestChannel("ch_lst_a", "list-alpha", &uid)
	a.NotesCount = 5
	a.UsersCount = 1
	now := time.Now()
	a.LastNotedAt = &now
	b := newTestChannel("ch_lst_b", "list-beta", &uid)
	b.NotesCount = 1
	b.IsArchived = true
	c := newTestChannel("ch_lst_c", "list-gamma", nil)
	c.NotesCount = 10

	for _, ch := range []*model.Channel{a, b, c} {
		require.NoError(t, repo.Create(ch))
		defer cleanupChannel(t, ch.ID)
	}

	rows, err := repo.List(model.ChannelListFilter{Query: "list-"})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rows), 3)

	rows, err = repo.List(model.ChannelListFilter{Query: "list-", OwnerID: user.ID})
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	archivedTrue := true
	rows, err = repo.List(model.ChannelListFilter{Query: "list-", IsArchived: &archivedTrue})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "list-beta", rows[0].Name)

	archivedFalse := false
	rows, err = repo.List(model.ChannelListFilter{Query: "list-", IsArchived: &archivedFalse})
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestChannelRepository_List_Sort(t *testing.T) {
	repo := NewChannelRepository(testDB)
	user := insertTestUser(t, "u_chr_5", "channeluser5")
	defer cleanupUser(t, user.ID)
	uid := user.ID

	a := newTestChannel("ch_srt_a", "sort-a", &uid)
	a.NotesCount = 1
	a.UsersCount = 5
	b := newTestChannel("ch_srt_b", "sort-b", &uid)
	b.NotesCount = 5
	b.UsersCount = 1
	for _, ch := range []*model.Channel{a, b} {
		require.NoError(t, repo.Create(ch))
		defer cleanupChannel(t, ch.ID)
	}

	for _, sortBy := range []string{
		"+lastNotedAt", "-lastNotedAt", "+name", "-name",
		"+notesCount", "-notesCount", "+usersCount", "-usersCount", "+id", "-id", "",
	} {
		rows, err := repo.List(model.ChannelListFilter{Query: "sort-", SortBy: sortBy, Limit: 10})
		require.NoError(t, err)
		assert.Len(t, rows, 2)
	}
}

// #1540: owned/search の cursor 無し default は id DESC、featured は
// lastNotedAt IS NOT NULL で絞る。
func TestChannelRepository_List_IDSortAndLastNotedAtFilter(t *testing.T) {
	repo := NewChannelRepository(testDB)
	uid := insertTestUser(t, "u_chr_ids", "channeluserids").ID
	defer cleanupUser(t, uid)

	now := time.Now()
	a := newTestChannel("ch_ids_a", "ids-a", &uid) // lastNotedAt なし
	b := newTestChannel("ch_ids_b", "ids-b", &uid)
	b.LastNotedAt = &now
	for _, ch := range []*model.Channel{a, b} {
		require.NoError(t, repo.Create(ch))
		defer cleanupChannel(t, ch.ID)
	}

	// SortBy "-id" は id 降順 (ch_ids_b > ch_ids_a)。
	rows, err := repo.List(model.ChannelListFilter{Query: "ids-", SortBy: "-id", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "ch_ids_b", rows[0].ID)
	assert.Equal(t, "ch_ids_a", rows[1].ID)

	// LastNotedAtNotNull は lastNotedAt の無い channel を除外する。
	rows, err = repo.List(model.ChannelListFilter{Query: "ids-", LastNotedAtNotNull: true, SortBy: "-lastNotedAt", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "ch_ids_b", rows[0].ID)
}

func TestChannelRepository_List_LimitClamp(t *testing.T) {
	repo := NewChannelRepository(testDB)
	rows, err := repo.List(model.ChannelListFilter{Limit: 9999})
	require.NoError(t, err)
	assert.NotNil(t, rows)
	rows, err = repo.List(model.ChannelListFilter{Limit: -10})
	require.NoError(t, err)
	assert.NotNil(t, rows)
}

func TestChannelRepository_List_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewChannelRepository(db)
	_, err := repo.List(model.ChannelListFilter{})
	assert.Error(t, err)
}

// LIKE パターンのメタ文字はリテラル照合になる (#2518、upstream channels/search
// の sqlLikeEscape 相当)。素通しだと _ が任意 1 文字に化けて誤ヒットする。
func TestChannelRepository_List_EscapesLikePattern(t *testing.T) {
	repo := NewChannelRepository(testDB)
	user := insertTestUser(t, "u_chr_esc", "channeluseresc")
	defer cleanupUser(t, user.ID)
	uid := user.ID

	lit := newTestChannel("ch_esc_1", "dev_talk room", &uid)
	require.NoError(t, repo.Create(lit))
	defer cleanupChannel(t, lit.ID)
	// 無エスケープなら "dev_talk" の _ が任意 1 文字になり、こちらも誤ヒットする。
	trap := newTestChannel("ch_esc_2", "devXtalk room", &uid)
	require.NoError(t, repo.Create(trap))
	defer cleanupChannel(t, trap.ID)
	pct := newTestChannel("ch_esc_3", "sale 100% off", &uid)
	require.NoError(t, repo.Create(pct))
	defer cleanupChannel(t, pct.ID)

	rows, err := repo.List(model.ChannelListFilter{Query: "dev_talk"})
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	assert.Contains(t, ids, "ch_esc_1")
	assert.NotContains(t, ids, "ch_esc_2", "_ をワイルドカード解釈しない")

	rows, err = repo.List(model.ChannelListFilter{Query: "100% off"})
	require.NoError(t, err)
	ids = ids[:0]
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	assert.Contains(t, ids, "ch_esc_3", "% をリテラル照合できる")
}

// insertChannelNote stores a note by userID in channelID and removes it at the
// end of the test.
func insertChannelNote(t *testing.T, id, userID, channelID string) {
	t.Helper()
	cid := channelID
	require.NoError(t, testDB.Create(&model.Note{
		ID: id, UserID: userID, ChannelID: &cid, Visibility: model.NoteVisibilityPublic,
	}).Error)
	t.Cleanup(func() { cleanupNote(t, id) })
}

func channelUsersCount(t *testing.T, repo ChannelRepository, id string) int {
	t.Helper()
	got, err := repo.FindByID(id)
	require.NoError(t, err)
	return got.UsersCount
}

// usersCount は本家と同じく、チャンネルに初めて投稿した利用者の分だけ増える
// (#3369)。
func TestChannelRepository_IncrementUsersCountOnFirstPost(t *testing.T) {
	repo := NewChannelRepository(testDB)
	alice := insertTestUser(t, "u_chuc_a", "chucalice")
	defer cleanupUser(t, alice.ID)
	bob := insertTestUser(t, "u_chuc_b", "chucbob")
	defer cleanupUser(t, bob.ID)

	c := newTestChannel("ch_uc_1", "users count", nil)
	require.NoError(t, repo.Create(c))
	defer cleanupChannel(t, c.ID)
	other := newTestChannel("ch_uc_2", "other channel", nil)
	require.NoError(t, repo.Create(other))
	defer cleanupChannel(t, other.ID)

	// 別のチャンネルへの投稿は、このチャンネルの最初の投稿の判定に入らない。
	insertChannelNote(t, "n_uc_00", alice.ID, other.ID)

	insertChannelNote(t, "n_uc_01", alice.ID, c.ID)
	require.NoError(t, repo.IncrementUsersCountOnFirstPost(c.ID, alice.ID, "n_uc_01"))
	assert.Equal(t, 1, channelUsersCount(t, repo, c.ID), "first post counts the author")

	insertChannelNote(t, "n_uc_02", alice.ID, c.ID)
	require.NoError(t, repo.IncrementUsersCountOnFirstPost(c.ID, alice.ID, "n_uc_02"))
	assert.Equal(t, 1, channelUsersCount(t, repo, c.ID), "second post by the same author does not count")

	insertChannelNote(t, "n_uc_03", bob.ID, c.ID)
	require.NoError(t, repo.IncrementUsersCountOnFirstPost(c.ID, bob.ID, "n_uc_03"))
	assert.Equal(t, 2, channelUsersCount(t, repo, c.ID), "another author counts")
	assert.Equal(t, 0, channelUsersCount(t, repo, other.ID), "only the given channel changes")
}

// 同じ利用者の投稿が 2 件とも保存された後に、hook が (逆順で) 走っても、加算は
// 最初の投稿の 1 回だけになる。本家の「後から数えて 1 なら」はこの場合に 1 回も
// 加算しない。
func TestChannelRepository_IncrementUsersCountOnFirstPost_OutOfOrder(t *testing.T) {
	repo := NewChannelRepository(testDB)
	alice := insertTestUser(t, "u_chuc_c", "chucconc")
	defer cleanupUser(t, alice.ID)
	c := newTestChannel("ch_uc_3", "concurrent", nil)
	require.NoError(t, repo.Create(c))
	defer cleanupChannel(t, c.ID)

	insertChannelNote(t, "n_uc_11", alice.ID, c.ID)
	insertChannelNote(t, "n_uc_12", alice.ID, c.ID)
	require.NoError(t, repo.IncrementUsersCountOnFirstPost(c.ID, alice.ID, "n_uc_12"))
	require.NoError(t, repo.IncrementUsersCountOnFirstPost(c.ID, alice.ID, "n_uc_11"))
	assert.Equal(t, 1, channelUsersCount(t, repo, c.ID))
}

// 最初の投稿を消して投稿し直すと、また 1 増える (本家と同じ)。hook が走る前に
// 消された投稿では増やさない。
func TestChannelRepository_IncrementUsersCountOnFirstPost_DeletedNotes(t *testing.T) {
	repo := NewChannelRepository(testDB)
	alice := insertTestUser(t, "u_chuc_d", "chucdel")
	defer cleanupUser(t, alice.ID)
	c := newTestChannel("ch_uc_4", "deleted", nil)
	require.NoError(t, repo.Create(c))
	defer cleanupChannel(t, c.ID)

	insertChannelNote(t, "n_uc_21", alice.ID, c.ID)
	require.NoError(t, repo.IncrementUsersCountOnFirstPost(c.ID, alice.ID, "n_uc_21"))
	cleanupNote(t, "n_uc_21")

	insertChannelNote(t, "n_uc_22", alice.ID, c.ID)
	require.NoError(t, repo.IncrementUsersCountOnFirstPost(c.ID, alice.ID, "n_uc_22"))
	assert.Equal(t, 2, channelUsersCount(t, repo, c.ID), "re-posting after deleting the first post counts again")

	// 保存されていない (hook の前に消された) 投稿では増やさない。
	require.NoError(t, repo.IncrementUsersCountOnFirstPost(c.ID, alice.ID, "n_uc_20"))
	assert.Equal(t, 2, channelUsersCount(t, repo, c.ID), "a note that no longer exists does not count")
}
