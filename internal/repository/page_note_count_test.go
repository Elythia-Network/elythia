package repository

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// referencedNoteIDs mirrors upstream PageService.collectReferencedNotes.
func TestReferencedNoteIDs(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"note blocks", `[{"type":"note","note":"a"},{"type":"text","text":"x"},{"type":"note","note":"b"}]`, []string{"a", "b"}},
		{"duplicates count once", `[{"type":"note","note":"a"},{"type":"note","note":"a"}]`, []string{"a"}},
		{"section children are walked", `[{"type":"section","children":[{"type":"note","note":"a"},{"type":"section","children":[{"type":"note","note":"b"}]}]}]`, []string{"a", "b"}},
		{"children of other blocks are ignored", `[{"type":"text","children":[{"type":"note","note":"a"}]}]`, nil},
		{"non-string note is ignored", `[{"type":"note","note":1},{"type":"note"}]`, nil},
		{"non-object blocks are ignored", `[1,"x",null,[{"type":"note","note":"a"}]]`, nil},
		{"non-array content", `{"type":"note","note":"a"}`, nil},
		{"broken json", `[`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, referencedNoteIDs([]byte(tc.content)))
		})
	}
}

// Page writes keep pageCount of the referenced notes in step, like upstream
// PageService (#3293): create +1, update by the diff, delete -1.
func TestPageRepository_MaintainsNotePageCount(t *testing.T) {
	repo := NewPageRepository(testDB)
	user := insertTestUser(t, "u_pgcnt", "pgcntuser")
	defer cleanupUser(t, user.ID)
	for _, id := range []string{"n_pgcnt_a", "n_pgcnt_b", "n_pgcnt_c"} {
		insertTestNote(t, id, user.ID)
	}
	count := func(id string) int16 {
		var v int16
		require.NoError(t, testDB.Raw(`SELECT "pageCount" FROM "note" WHERE id = ?`, id).Scan(&v).Error)
		return v
	}

	p := newTestPage("pg_pgcnt_1", user.ID, "pgcnt1")
	p.Content = datatypes.JSON([]byte(`[{"type":"note","note":"n_pgcnt_a"},{"type":"note","note":"n_pgcnt_a"},{"type":"section","children":[{"type":"note","note":"n_pgcnt_b"}]},{"type":"note","note":"n_pgcnt_missing"}]`))
	require.NoError(t, repo.Create(p))
	defer cleanupPage(t, p.ID)
	assert.EqualValues(t, 1, count("n_pgcnt_a"), "同じページ内の重複は 1 回")
	assert.EqualValues(t, 1, count("n_pgcnt_b"))
	assert.EqualValues(t, 0, count("n_pgcnt_c"))

	// 2 つ目のページからも a を参照する。
	p2 := newTestPage("pg_pgcnt_2", user.ID, "pgcnt2")
	p2.Content = datatypes.JSON([]byte(`[{"type":"note","note":"n_pgcnt_a"}]`))
	require.NoError(t, repo.Create(p2))
	defer cleanupPage(t, p2.ID)
	assert.EqualValues(t, 2, count("n_pgcnt_a"))

	// content 以外の更新は数えない。
	require.NoError(t, repo.UpdateFields(p.ID, map[string]any{"title": "t2"}))
	assert.EqualValues(t, 2, count("n_pgcnt_a"))
	assert.EqualValues(t, 1, count("n_pgcnt_b"))

	// b を外して c を足す。a は残るので変わらない。page_service は string で渡す。
	require.NoError(t, repo.UpdateFields(p.ID, map[string]any{
		"content": `[{"type":"note","note":"n_pgcnt_a"},{"type":"note","note":"n_pgcnt_c"}]`,
	}))
	assert.EqualValues(t, 2, count("n_pgcnt_a"))
	assert.EqualValues(t, 0, count("n_pgcnt_b"))
	assert.EqualValues(t, 1, count("n_pgcnt_c"))

	require.NoError(t, repo.Delete(p))
	assert.EqualValues(t, 1, count("n_pgcnt_a"))
	assert.EqualValues(t, 0, count("n_pgcnt_c"))

	// 消えたページをもう一度消しても二重に減らさない。
	require.NoError(t, repo.Delete(p))
	assert.EqualValues(t, 1, count("n_pgcnt_a"))
}

// Decrementing never takes pageCount below 0 (same floor as IncrementCount,
// #3291): notes referenced by pages created before #3293 sit at 0.
func TestPageRepository_PageCountNeverGoesNegative(t *testing.T) {
	repo := NewPageRepository(testDB)
	user := insertTestUser(t, "u_pgneg", "pgneguser")
	defer cleanupUser(t, user.ID)
	insertTestNote(t, "n_pgneg", user.ID)

	p := newTestPage("pg_pgneg", user.ID, "pgneg")
	p.Content = datatypes.JSON([]byte(`[{"type":"note","note":"n_pgneg"}]`))
	require.NoError(t, repo.Create(p))
	defer cleanupPage(t, p.ID)
	// #3293 より前に作ったページと同じ状態にする。
	require.NoError(t, testDB.Exec(`UPDATE "note" SET "pageCount" = 0 WHERE id = 'n_pgneg'`).Error)

	require.NoError(t, repo.Delete(p))
	var v int16
	require.NoError(t, testDB.Raw(`SELECT "pageCount" FROM "note" WHERE id = 'n_pgneg'`).Scan(&v).Error)
	assert.EqualValues(t, 0, v)
}

// Concurrent content updates of the same page stay consistent: the old content
// is read under the row lock, so two updates never diff against the same old
// value (upstream locks with for_no_key_update for the same reason).
func TestPageRepository_ConcurrentUpdatesKeepPageCountConsistent(t *testing.T) {
	repo := NewPageRepository(testDB)
	user := insertTestUser(t, "u_pgconc", "pgconcuser")
	defer cleanupUser(t, user.ID)
	insertTestNote(t, "n_pgconc", user.ID)

	p := newTestPage("pg_pgconc", user.ID, "pgconc")
	require.NoError(t, repo.Create(p))
	defer cleanupPage(t, p.ID)

	with := `[{"type":"note","note":"n_pgconc"}]`
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := "[]"
			if i%2 == 0 {
				content = with
			}
			assert.NoError(t, repo.UpdateFields(p.ID, map[string]any{"content": content}))
		}(i)
	}
	wg.Wait()

	var final string
	require.NoError(t, testDB.Raw(`SELECT content::text FROM "page" WHERE id = ?`, p.ID).Scan(&final).Error)
	var got int16
	require.NoError(t, testDB.Raw(`SELECT "pageCount" FROM "note" WHERE id = 'n_pgconc'`).Scan(&got).Error)
	want := 0
	if len(referencedNoteIDs([]byte(final))) > 0 {
		want = 1
	}
	assert.EqualValues(t, want, got, fmt.Sprintf("final content %s", final))
}

// pageCount stops at the smallint maximum instead of failing the page write.
func TestPageRepository_PageCountCappedAtSmallintMax(t *testing.T) {
	repo := NewPageRepository(testDB)
	user := insertTestUser(t, "u_pgcap", "pgcapuser")
	defer cleanupUser(t, user.ID)
	insertTestNote(t, "n_pgcap", user.ID)
	require.NoError(t, testDB.Exec(`UPDATE "note" SET "pageCount" = 32767 WHERE id = 'n_pgcap'`).Error)

	p := newTestPage("pg_pgcap", user.ID, "pgcap")
	p.Content = datatypes.JSON([]byte(`[{"type":"note","note":"n_pgcap"}]`))
	require.NoError(t, repo.Create(p), "上限で止まり、ページの作成は失敗しない")
	defer cleanupPage(t, p.ID)
	var got int16
	require.NoError(t, testDB.Raw(`SELECT "pageCount" FROM "note" WHERE id = 'n_pgcap'`).Scan(&got).Error)
	assert.EqualValues(t, 32767, got)
}
