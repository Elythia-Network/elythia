package maintenance

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/model"
)

func seedCountsInstance(t *testing.T, id, host string, notes, users int) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Instance{
		ID: id, Host: host, FirstRetrievedAt: time.Now(),
	}).Error)
	// default:0 の列は GORM が 0 を省いて DB の既定値に任せるので、値は後から書く。
	require.NoError(t, testDB.Exec(
		`UPDATE "instance" SET "notesCount" = ?, "usersCount" = ? WHERE id = ?`, notes, users, id,
	).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "instance" WHERE id = ?`, id) })
}

func seedCountsUser(t *testing.T, id string, host *string, mutate func(*model.User)) {
	t.Helper()
	u := &model.User{
		ID: id, Username: id, UsernameLower: id, Host: host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	if host == nil {
		token := "tok_" + id
		u.Token = &token
	}
	if mutate != nil {
		mutate(u)
	}
	require.NoError(t, testDB.Create(u).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
}

func seedCountsNote(t *testing.T, id, userID string, host *string, renoteID *string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Note{
		ID: id, UserID: userID, UserHost: host, RenoteID: renoteID,
		Visibility: model.NoteVisibilityPublic,
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id) })
}

func instanceCounts(t *testing.T, id string) (notes, users int) {
	t.Helper()
	var inst model.Instance
	require.NoError(t, testDB.Where("id = ?", id).First(&inst).Error)
	return inst.NotesCount, inst.UsersCount
}

func changesByHost(changes []InstanceCountChange) map[string]InstanceCountChange {
	out := make(map[string]InstanceCountChange, len(changes))
	for _, c := range changes {
		out[c.Host] = c
	}
	return out
}

// #3330 より前に作られた instance 行の notesCount / usersCount を、本家の
// instance chart の total と同じ定義 (note は userHost、user は host) で数え直す。
func TestBackfillInstanceCountsBatch(t *testing.T) {
	testDB.Exec(`DELETE FROM "note"`)
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "instance"`)

	a := "a.example"
	b := "b.example"
	other := "noinstance.example"

	// a: 0 / 1 のまま残った #3330 前の行。
	seedCountsInstance(t, "ic1", a, 0, 1)
	// b: 既に正しい行 (書かない)。
	seedCountsInstance(t, "ic2", b, 1, 1)
	// c: 実件数より大きく残った行 (0 へ下げる)。
	seedCountsInstance(t, "ic3", "c.example", 5, 4)

	seedCountsUser(t, "icl", nil, nil)
	seedCountsUser(t, "icu1", &a, nil)
	// 削除済み・凍結中も数える (本家は区別せずに積む)。
	seedCountsUser(t, "icu2", &a, func(u *model.User) { u.IsDeleted = true })
	seedCountsUser(t, "icu3", &a, func(u *model.User) { u.IsSuspended = true })
	seedCountsUser(t, "icu4", &b, nil)
	seedCountsUser(t, "icu5", &other, nil)

	seedCountsNote(t, "icn1", "icu1", &a, nil)
	seedCountsNote(t, "icn2", "icu2", &a, nil)
	// renote も数える (本家 NoteCreateService は renote でも加算する)。
	seedCountsNote(t, "icn3", "icu1", &a, sptr("icn1"))
	seedCountsNote(t, "icn4", "icu4", &b, nil)
	// ローカルと instance 行の無い host は対象外。
	seedCountsNote(t, "icn5", "icl", nil, nil)
	seedCountsNote(t, "icn6", "icu5", &other, nil)

	// --- dry-run は差分だけ返し、書かない ---
	res, err := BackfillInstanceCountsBatch(testDB, "", 100, true)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned)
	assert.Equal(t, "ic3", res.LastID)
	got := changesByHost(res.Changes)
	require.Len(t, got, 2, "b は既に正しいので差分に出ない")
	assert.Equal(t, InstanceCountChange{Host: a, OldNotes: 0, NewNotes: 3, OldUsers: 1, NewUsers: 3}, got[a])
	assert.Equal(t, InstanceCountChange{Host: "c.example", OldNotes: 5, NewNotes: 0, OldUsers: 4, NewUsers: 0}, got["c.example"])
	n, u := instanceCounts(t, "ic1")
	assert.Equal(t, [2]int{0, 1}, [2]int{n, u}, "dry-run は書かない")

	// --- 本実行 ---
	res, err = BackfillInstanceCountsBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned)
	assert.Equal(t, got, changesByHost(res.Changes), "本実行の差分は dry-run と同じ")

	for _, tc := range []struct {
		id           string
		notes, users int
	}{
		{"ic1", 3, 3},
		{"ic2", 1, 1},
		{"ic3", 0, 0},
	} {
		n, u := instanceCounts(t, tc.id)
		assert.Equal(t, [2]int{tc.notes, tc.users}, [2]int{n, u}, tc.id)
	}

	// --- 冪等: 2 回目は何も変えない ---
	res, err = BackfillInstanceCountsBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned)
	assert.Empty(t, res.Changes)

	// --- 末尾の先は空 ---
	res, err = BackfillInstanceCountsBatch(testDB, "ic3", 100, false)
	require.NoError(t, err)
	assert.Equal(t, InstanceCountsBackfillResult{}, res)
}

// 値が既に正しい行は UPDATE の WHERE で外れ、書き込みが起きない (行ロックも
// 取らない)。xmin が変わらないことで確かめる。
func TestBackfillInstanceCountsBatch_SkipsUnchangedRows(t *testing.T) {
	testDB.Exec(`DELETE FROM "note"`)
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "instance"`)

	h := "same.example"
	seedCountsInstance(t, "ics1", h, 1, 1)
	seedCountsUser(t, "icsu1", &h, nil)
	seedCountsNote(t, "icsn1", "icsu1", &h, nil)

	xmin := func() string {
		var x string
		require.NoError(t, testDB.Raw(`SELECT xmin::text FROM "instance" WHERE id = ?`, "ics1").Row().Scan(&x))
		return x
	}
	before := xmin()
	res, err := BackfillInstanceCountsBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Empty(t, res.Changes)
	assert.Equal(t, before, xmin(), "値が同じ行を書き換えない")
}

// 数える文と書く文が分かれている。数え終えた後 (書く前) に別の接続から
// instance 行を lock_timeout 付きで更新でき (= 数えた行のロックを握っていない)、
// その間に増えた投稿は書く値に入らない (= 書く文が集計し直していない)。
func TestBackfillInstanceCountsBatch_CountsBeforeWriting(t *testing.T) {
	testDB.Exec(`DELETE FROM "note"`)
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "instance"`)

	h := "split.example"
	seedCountsInstance(t, "icp1", h, 0, 0)
	seedCountsUser(t, "icpu1", &h, nil)
	seedCountsNote(t, "icpn1", "icpu1", &h, nil)

	orig := instanceCountsAfterCount
	t.Cleanup(func() { instanceCountsAfterCount = orig })
	called := false
	instanceCountsAfterCount = func() {
		called = true
		require.NoError(t, testDB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`SET LOCAL lock_timeout = '1s'`).Error; err != nil {
				return err
			}
			return tx.Exec(`UPDATE "instance" SET "followersCount" = 7 WHERE id = ?`, "icp1").Error
		}))
		seedCountsNote(t, "icpn2", "icpu1", &h, nil)
	}

	res, err := BackfillInstanceCountsBatch(testDB, "", 100, false)
	require.NoError(t, err)
	require.True(t, called)
	require.Len(t, res.Changes, 1)
	n, u := instanceCounts(t, "icp1")
	assert.Equal(t, [2]int{1, 1}, [2]int{n, u}, "数えた後に増えた投稿は書く値に入らない")

	// 次の実行で拾う (冪等なので、ずれは再実行で直る)。
	instanceCountsAfterCount = orig
	_, err = BackfillInstanceCountsBatch(testDB, "", 100, false)
	require.NoError(t, err)
	n, _ = instanceCounts(t, "icp1")
	assert.Equal(t, 2, n)
}

// 数えた後に別の書き込みで値が一致した行は書かず、Changes にも出さない
// (本実行の Changes は「書いた行」)。
func TestBackfillInstanceCountsBatch_ReportsOnlyWrittenRows(t *testing.T) {
	testDB.Exec(`DELETE FROM "note"`)
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "instance"`)

	h := "race.example"
	seedCountsInstance(t, "icr1", h, 0, 0)
	seedCountsUser(t, "icru1", &h, nil)

	orig := instanceCountsAfterCount
	t.Cleanup(func() { instanceCountsAfterCount = orig })
	instanceCountsAfterCount = func() {
		require.NoError(t, testDB.Exec(`UPDATE "instance" SET "usersCount" = 1 WHERE id = ?`, "icr1").Error)
	}

	res, err := BackfillInstanceCountsBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Empty(t, res.Changes, "書かなかった行は出さない")
}

// 書く文へ渡す VALUES は id の順に並ぶ (起動時の RecomputeFollowCounts と
// ロックを取る順を揃えるため)。入力の順に依らない。
func TestInstanceCountsValuesAreOrderedByID(t *testing.T) {
	placeholders, args := instanceCountsValues([]instanceCountValue{
		{ID: "c", Notes: 3, Users: 30},
		{ID: "a", Notes: 1, Users: 10},
		{ID: "b", Notes: 2, Users: 20},
	})
	assert.Equal(t, "(?::varchar, ?::int, ?::int), (?::varchar, ?::int, ?::int), (?::varchar, ?::int, ?::int)", placeholders)
	assert.Equal(t, []any{"a", 1, 10, "b", 2, 20, "c", 3, 30}, args)
}

// keyset で batch を区切り、LastID を渡すと続きから流れる。
func TestBackfillInstanceCountsBatch_Keyset(t *testing.T) {
	testDB.Exec(`DELETE FROM "note"`)
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "instance"`)

	for i := 1; i <= 5; i++ {
		h := fmt.Sprintf("k%d.example", i)
		seedCountsInstance(t, fmt.Sprintf("ick%d", i), h, 0, 0)
		seedCountsUser(t, fmt.Sprintf("icku%d", i), &h, nil)
	}

	cursor := ""
	var batches, changed int
	for {
		res, err := BackfillInstanceCountsBatch(testDB, cursor, 2, false)
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
		_, u := instanceCounts(t, fmt.Sprintf("ick%d", i))
		assert.Equal(t, 1, u)
	}
}
