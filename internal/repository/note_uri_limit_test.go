package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// `note.uri` の列長を schema から固定する (#2723)。
//
// resolver は収まらない id の Note を拒否する。判断に使う 512 が resolver 側の
// 定数にしか無いと、列が変わったときに気付けない。
func TestNote_URIColumnLimitIs512(t *testing.T) {
	var n int
	require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'note' AND column_name = 'uri'`).
		Scan(&n).Error)
	assert.Equal(t, 512, n, "note.uri の列長が変わっている (resolver の noteURIMaxRunes も直すこと)")
}

// resolver が受理する最大長が実際に入ること (#2723)。
func TestNote_URIColumnLimitAcceptsMaxLengthValue(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_urilimit_0", "urilimituser")
	defer cleanupUser(t, u.ID)
	id := "note_urilimit_0000000000000000a"
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id) })

	// **全角で埋める** — byte で数える実装だと 3 倍になって入らない。
	uri := strings.Repeat("あ", 512)
	require.NoError(t, repo.Create(&model.Note{
		ID: id, UserID: u.ID, Visibility: model.NoteVisibilityPublic, URI: &uri,
	}))

	got, err := repo.FindByID(id)
	require.NoError(t, err)
	require.NotNil(t, got.URI)
	assert.Equal(t, 512, len([]rune(*got.URI)))
}
