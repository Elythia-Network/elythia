package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// arrayElementMaxLength returns the varchar(n) element limit of an array column.
//
// **information_schema では読めない。** 配列列の `columns.character_maximum_length`
// は NULL で、`element_types` も要素の typmod を持たない (実測)。pg_catalog の
// format_type だけが `character varying(256)[]` を返す (#2726)。
func arrayElementMaxLength(t *testing.T, table, column string) string {
	t.Helper()
	var ft string
	require.NoError(t, testDB.Raw(`SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relname = ? AND a.attname = ? AND a.attnum > 0`,
		table, column).Scan(&ft).Error)
	return ft
}

// 列の上限そのものを schema から固定する (#2726)。
//
// resolver 側の pollChoiceMaxRunes / upsertEmojis の定数と独立に同じ数値が
// 書かれているだけだと、揃って動かせば全部緑になる。
func TestRemoteArrayColumnLimits(t *testing.T) {
	cases := []struct {
		table  string
		column string
		want   string
	}{
		{"poll", "choices", "character varying(256)[]"},
		{"note", "emojis", "character varying(128)[]"},
		{"user", "emojis", "character varying(128)[]"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, arrayElementMaxLength(t, tc.table, tc.column),
			"%s.%s の要素長が変わっている (internal/core/federation/resolver.go の定数も直すこと)",
			tc.table, tc.column)
	}
}

// resolver が切った長さで実際に書けること (#2726)。**全角で埋める** — byte で
// 切る実装だと 3 倍になって入らない。
func TestPoll_ChoicesAcceptMaxLengthValues(t *testing.T) {
	repo := NewPollRepository(testDB)
	user := insertTestUser(t, "u_pcl_1", "pcluser")
	defer cleanupUser(t, user.ID)

	note := &model.Note{
		ID:         "n_pcl_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		HasPoll:    true,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, testDB.Create(note).Error)
	defer cleanupNote(t, note.ID)

	choice := strings.Repeat("あ", 256)
	require.NoError(t, repo.Create(&model.Poll{
		NoteID:         note.ID,
		Choices:        model.StringArray{choice, choice},
		Votes:          model.Int64Array{0, 0},
		NoteVisibility: model.NoteVisibilityPublic,
		UserID:         user.ID,
	}))

	found, err := repo.FindByNoteID(note.ID)
	require.NoError(t, err)
	require.Len(t, found.Choices, 2)
	assert.Equal(t, 256, len([]rune(found.Choices[0])))
}
