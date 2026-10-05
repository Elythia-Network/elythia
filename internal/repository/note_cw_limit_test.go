package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// resolver が切っている長さで、実際の列制約を通ること (#2723)。
//
// mock は列制約を持たないので、resolver 側のテストだけでは「varchar(512) に入る
// 長さか」を確かめられない。上限は migration/000001_initial.up.sql が持っている
// ので、実 DB で突き合わせる。
func TestNote_CWColumnLimitAcceptsTruncatedValue(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_cwlimit_0", "cwlimituser")
	defer cleanupUser(t, u.ID)
	id := "note_cwlimit_00000000000000000a"
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id) })

	// resolver の noteCWMaxRunes と同じ値。**全角で埋める** — byte で切る実装だと
	// 3 倍になって入らない。
	cw := strings.Repeat("\u3042", 512)
	require.NoError(t, repo.Create(&model.Note{
		ID: id, UserID: u.ID, Visibility: model.NoteVisibilityPublic, CW: &cw,
	}))

	got, err := repo.FindByID(id)
	require.NoError(t, err)
	require.NotNil(t, got.CW)
	assert.Equal(t, 512, len([]rune(*got.CW)))
}

// 列の上限そのものを schema から固定する (#2723)。
//
// resolver 側の定数と 3 箇所に独立して 512 が書かれているだけだと、揃って動かせば
// 全部緑になる。列が変わったらここが落ちる。
func TestNote_CWColumnLimitIs512(t *testing.T) {
	var n int
	require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'note' AND column_name = 'cw'`).
		Scan(&n).Error)
	assert.Equal(t, 512, n, "note.cw の列長が変わっている (resolver の noteCWMaxRunes も直すこと)")
}
