package maintenance

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
)

func seedUsersCountChannel(t *testing.T, id string, users int) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Channel{
		ID: id, Name: id, Color: "#86b300", AllowRenoteToExternal: true,
	}).Error)
	// default:0 の列は GORM が 0 を省いて DB の既定値に任せるので、値は後から書く。
	require.NoError(t, testDB.Exec(`UPDATE "channel" SET "usersCount" = ? WHERE id = ?`, users, id).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "channel" WHERE id = ?`, id) })
}

func seedChannelNote(t *testing.T, id, userID string, channelID *string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Note{
		ID: id, UserID: userID, ChannelID: channelID, Visibility: model.NoteVisibilityPublic,
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id) })
}

func channelUsersCount(t *testing.T, id string) int {
	t.Helper()
	var c model.Channel
	require.NoError(t, testDB.Where("id = ?", id).First(&c).Error)
	return c.UsersCount
}

func resetChannelUsersCountTables() {
	testDB.Exec(`DELETE FROM "note"`)
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "channel"`)
}

// usersCount を「いまそのチャンネルに投稿がある利用者の数」へ数え直す (#3369)。
func TestBackfillChannelUsersCountBatch(t *testing.T) {
	resetChannelUsersCountTables()

	ca, cb, cc := "cuc1", "cuc2", "cuc3"
	// a: フォロー数で数えていた行 (投稿した人は 2 人)。
	seedUsersCountChannel(t, ca, 5)
	// b: 既に正しい行 (書かない)。
	seedUsersCountChannel(t, cb, 1)
	// c: 投稿の無い行 (0 へ下げる)。
	seedUsersCountChannel(t, cc, 3)

	for _, u := range []string{"cucu1", "cucu2", "cucu3"} {
		seedCountsUser(t, u, nil, nil)
	}
	// 同じ人の複数の投稿は 1 人と数える。
	seedChannelNote(t, "cucn1", "cucu1", &ca)
	seedChannelNote(t, "cucn2", "cucu1", &ca)
	seedChannelNote(t, "cucn3", "cucu2", &ca)
	seedChannelNote(t, "cucn4", "cucu3", &cb)
	// チャンネルの無い投稿は数えない。
	seedChannelNote(t, "cucn5", "cucu3", nil)

	// --- dry-run は差分だけ返し、書かない ---
	res, err := BackfillChannelUsersCountBatch(testDB, "", 100, true)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned)
	assert.Equal(t, cc, res.LastID)
	want := []ChannelUsersCountChange{
		{ID: ca, OldUsers: 5, NewUsers: 2},
		{ID: cc, OldUsers: 3, NewUsers: 0},
	}
	assert.Equal(t, want, res.Changes, "b は既に正しいので差分に出ない")
	assert.Equal(t, 5, channelUsersCount(t, ca), "dry-run は書かない")

	// --- 本実行 ---
	res, err = BackfillChannelUsersCountBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned)
	assert.Equal(t, want, res.Changes, "本実行の差分は dry-run と同じ")
	assert.Equal(t, 2, channelUsersCount(t, ca))
	assert.Equal(t, 1, channelUsersCount(t, cb))
	assert.Equal(t, 0, channelUsersCount(t, cc))

	// --- 冪等: 2 回目は何も変えない ---
	res, err = BackfillChannelUsersCountBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned)
	assert.Empty(t, res.Changes)

	// --- 末尾の先は空 ---
	res, err = BackfillChannelUsersCountBatch(testDB, cc, 100, false)
	require.NoError(t, err)
	assert.Equal(t, ChannelUsersCountBackfillResult{}, res)
}

// 値が既に正しい行は UPDATE の WHERE で外れ、書き込みが起きない。xmin が
// 変わらないことで確かめる。
func TestBackfillChannelUsersCountBatch_SkipsUnchangedRows(t *testing.T) {
	resetChannelUsersCountTables()

	c := "cucs1"
	seedUsersCountChannel(t, c, 1)
	seedCountsUser(t, "cucsu1", nil, nil)
	seedChannelNote(t, "cucsn1", "cucsu1", &c)

	xmin := func() string {
		var x string
		require.NoError(t, testDB.Raw(`SELECT xmin::text FROM "channel" WHERE id = ?`, c).Row().Scan(&x))
		return x
	}
	before := xmin()
	res, err := BackfillChannelUsersCountBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Empty(t, res.Changes)
	assert.Equal(t, before, xmin(), "値が同じ行を書き換えない")
}

// 数える文と書く文が分かれている。数え終えた後 (書く前) に別の接続から
// channel 行を lock_timeout 付きで更新でき、その間に投稿した人は書く値に入らない。
func TestBackfillChannelUsersCountBatch_CountsBeforeWriting(t *testing.T) {
	resetChannelUsersCountTables()

	c := "cucp1"
	seedUsersCountChannel(t, c, 0)
	seedCountsUser(t, "cucpu1", nil, nil)
	seedCountsUser(t, "cucpu2", nil, nil)
	seedChannelNote(t, "cucpn1", "cucpu1", &c)

	orig := channelUsersCountAfterCount
	t.Cleanup(func() { channelUsersCountAfterCount = orig })
	called := false
	channelUsersCountAfterCount = func() {
		called = true
		require.NoError(t, testDB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`SET LOCAL lock_timeout = '1s'`).Error; err != nil {
				return err
			}
			return tx.Exec(`UPDATE "channel" SET "notesCount" = 7 WHERE id = ?`, c).Error
		}))
		seedChannelNote(t, "cucpn2", "cucpu2", &c)
	}

	res, err := BackfillChannelUsersCountBatch(testDB, "", 100, false)
	require.NoError(t, err)
	require.True(t, called)
	require.Len(t, res.Changes, 1)
	assert.Equal(t, 1, channelUsersCount(t, c), "数えた後に投稿した人は書く値に入らない")

	// 次の実行で拾う (冪等なので、ずれは再実行で直る)。
	channelUsersCountAfterCount = orig
	_, err = BackfillChannelUsersCountBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 2, channelUsersCount(t, c))
}

// 数えた後に別の書き込みで値が一致した行は書かず、Changes にも出さない
// (本実行の Changes は「書いた行」)。
func TestBackfillChannelUsersCountBatch_ReportsOnlyWrittenRows(t *testing.T) {
	resetChannelUsersCountTables()

	c := "cucr1"
	seedUsersCountChannel(t, c, 0)
	seedCountsUser(t, "cucru1", nil, nil)
	seedChannelNote(t, "cucrn1", "cucru1", &c)

	orig := channelUsersCountAfterCount
	t.Cleanup(func() { channelUsersCountAfterCount = orig })
	channelUsersCountAfterCount = func() {
		require.NoError(t, testDB.Exec(`UPDATE "channel" SET "usersCount" = 1 WHERE id = ?`, c).Error)
	}

	res, err := BackfillChannelUsersCountBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Empty(t, res.Changes, "書かなかった行は出さない")
}

// 書く文へ渡す VALUES は id の順に並ぶ。入力の順に依らない。
func TestChannelUsersCountValuesAreOrderedByID(t *testing.T) {
	placeholders, args := channelUsersCountValues([]channelUsersCountValue{
		{ID: "c", Users: 3},
		{ID: "a", Users: 1},
		{ID: "b", Users: 2},
	})
	assert.Equal(t, "(?::varchar, ?::int), (?::varchar, ?::int), (?::varchar, ?::int)", placeholders)
	assert.Equal(t, []any{"a", 1, "b", 2, "c", 3}, args)
}

// keyset で batch を区切り、LastID を渡すと続きから流れる。
func TestBackfillChannelUsersCountBatch_Keyset(t *testing.T) {
	resetChannelUsersCountTables()

	seedCountsUser(t, "cucku1", nil, nil)
	for i := 1; i <= 5; i++ {
		c := fmt.Sprintf("cuck%d", i)
		seedUsersCountChannel(t, c, 0)
		seedChannelNote(t, fmt.Sprintf("cuckn%d", i), "cucku1", &c)
	}

	cursor := ""
	var batches, changed int
	for {
		res, err := BackfillChannelUsersCountBatch(testDB, cursor, 2, false)
		require.NoError(t, err)
		if res.Scanned == 0 {
			break
		}
		require.LessOrEqual(t, res.Scanned, 2)
		require.Greater(t, res.LastID, cursor)
		cursor = res.LastID
		changed += len(res.Changes)
		batches++
	}
	assert.Equal(t, 3, batches)
	assert.Equal(t, 5, changed)
	for i := 1; i <= 5; i++ {
		assert.Equal(t, 1, channelUsersCount(t, fmt.Sprintf("cuck%d", i)))
	}

	// 途中から再開すると、それより前の行は読まない。
	testDB.Exec(`UPDATE "channel" SET "usersCount" = 9`)
	res, err := BackfillChannelUsersCountBatch(testDB, "cuck3", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Scanned)
	assert.Equal(t, 9, channelUsersCount(t, "cuck3"), "the resume cursor is exclusive")
	assert.Equal(t, 1, channelUsersCount(t, "cuck4"))
}
