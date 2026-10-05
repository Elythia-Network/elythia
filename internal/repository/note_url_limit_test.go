package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// 列の上限そのものを schema から固定する (#2729)。
//
// resolver 側の `noteURLMaxRunes` と独立に同じ数値が書かれているだけだと、揃って
// 動かせば緑になる。upstream の `MiNote.url` も同じ varchar(512)。
func TestNote_URLColumnLimitIs512(t *testing.T) {
	var n int
	require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'note' AND column_name = 'url'`).
		Scan(&n).Error)
	assert.Equal(t, 512, n,
		"note.url の列長が変わっている (internal/core/federation/resolver.go の noteURLMaxRunes も直すこと)")
}

// resolver が「収まる」と判断した長さで実際に書けること (#2729)。
//
// mock repository は列制約を持たないので、resolver 側のテストだけでは「本当に入る
// 長さか」を確かめられない。**全角で埋める** — byte で数える実装だと 3 倍になって
// 入らない (列はコードポイント単位)。
func TestNote_URLColumnAcceptsMaxLengthValue(t *testing.T) {
	user := insertTestUser(t, "u_nurl_1", "nurluser")
	defer cleanupUser(t, user.ID)

	url := "https://remote.example/" + strings.Repeat("あ", 489)
	require.Equal(t, 512, len([]rune(url)))
	note := &model.Note{
		ID:         "n_nurl_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		URL:        &url,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, testDB.Create(note).Error)
	defer cleanupNote(t, note.ID)

	var stored string
	require.NoError(t, testDB.Raw(`SELECT "url" FROM "note" WHERE id = ?`, note.ID).Scan(&stored).Error)
	assert.Equal(t, 512, len([]rune(stored)))
}
