package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func cleanupEmoji(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "emoji" WHERE id = ?`, id)
}

func TestEmojiRepository_FindByNameAndHost_Local(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e := &model.Emoji{
		ID:          "e_local",
		Name:        "smile",
		OriginalURL: "https://example.com/smile.png",
	}
	require.NoError(t, testDB.Create(e).Error)
	defer cleanupEmoji(t, e.ID)

	found, err := repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	assert.Equal(t, "smile", found.Name)
}

// UpdateFields は roleIdsThatCanBeUsedThisEmojiAsReaction (varchar[]) と type を
// 実 DB に正しく書き込む。model.StringArray でラップした array / 空 array / *string
// type が round-trip することを実 PostgreSQL で検証する (aliases #729 と同型の
// NULL 化罠を回帰防止)。
func TestEmojiRepository_UpdateFields_RoleIdsAndType(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	e := &model.Emoji{ID: "e_upd_roles", Name: "rolesx", OriginalURL: "https://example.com/x.png"}
	require.NoError(t, testDB.Create(e).Error)
	defer cleanupEmoji(t, e.ID)

	// roleIds (非空) + type を書き込む。
	require.NoError(t, repo.UpdateFields(e.ID, map[string]any{
		"roleIdsThatCanBeUsedThisEmojiAsReaction": model.StringArray{"r1", "r2"},
		"type": "image/webp",
	}))
	got, err := repo.FindByID(e.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"r1", "r2"}, []string(got.RoleIDsThatCanBeUsedThisEmojiAsReaction))
	require.NotNil(t, got.Type)
	assert.Equal(t, "image/webp", *got.Type)

	// 空 array へのリセットも NOT NULL 制約に違反せず '{}' になる。
	require.NoError(t, repo.UpdateFields(e.ID, map[string]any{
		"roleIdsThatCanBeUsedThisEmojiAsReaction": model.StringArray{},
	}))
	got, err = repo.FindByID(e.ID)
	require.NoError(t, err)
	assert.Empty(t, []string(got.RoleIDsThatCanBeUsedThisEmojiAsReaction))
}

// #1948-13: UpdateFields / UpdateFieldsMany は updatedAt を bump する
// (upstream CustomEmojiService は全 mutation で updatedAt=now を書く)。
func TestEmojiRepository_UpdateFieldsBumpsUpdatedAt(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	e := &model.Emoji{ID: "e_bump", Name: "bumpx", OriginalURL: "https://example.com/x.png"}
	require.NoError(t, testDB.Create(e).Error)
	defer cleanupEmoji(t, e.ID)
	// updatedAt を明示的に NULL にしておく。
	require.NoError(t, testDB.Exec(`UPDATE emoji SET "updatedAt" = NULL WHERE id = ?`, e.ID).Error)

	require.NoError(t, repo.UpdateFields(e.ID, map[string]any{"name": "bumped"}))
	got, err := repo.FindByID(e.ID)
	require.NoError(t, err)
	require.NotNil(t, got.UpdatedAt, "UpdateFields は updatedAt を bump する (#1948-13)")

	// UpdateFieldsMany も bump する。
	require.NoError(t, testDB.Exec(`UPDATE emoji SET "updatedAt" = NULL WHERE id = ?`, e.ID).Error)
	require.NoError(t, repo.UpdateFieldsMany([]string{e.ID}, map[string]any{"category": "x"}))
	got, err = repo.FindByID(e.ID)
	require.NoError(t, err)
	require.NotNil(t, got.UpdatedAt, "UpdateFieldsMany は updatedAt を bump する (#1948-13)")
}

// #1948-13: list-remote の query は case-sensitive LIKE かつ %/_ を escape する。
func TestEmojiRepository_ListRemoteCaseSensitiveAndEscape(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	host := "remote.example"
	lower := &model.Emoji{ID: "e_lr_lower", Name: "catface", Host: &host, OriginalURL: "https://r/x.png"}
	upper := &model.Emoji{ID: "e_lr_upper", Name: "CATFACE", Host: &host, OriginalURL: "https://r/y.png"}
	pct := &model.Emoji{ID: "e_lr_pct", Name: "a%b", Host: &host, OriginalURL: "https://r/z.png"}
	for _, e := range []*model.Emoji{lower, upper, pct} {
		require.NoError(t, testDB.Create(e).Error)
		defer cleanupEmoji(t, e.ID)
	}

	// query "cat" は case-sensitive で "catface" のみ match (CATFACE は除外)。
	got, err := repo.ListRemoteWithFilter("cat", host, "", "", 100, 0)
	require.NoError(t, err)
	ids := emojiIDSet(got)
	assert.Contains(t, ids, lower.ID, "case-sensitive LIKE で catface が match (#1948-13)")
	assert.NotContains(t, ids, upper.ID, "case-sensitive LIKE で CATFACE は除外 (#1948-13)")

	// query "a%b" の % は escape され literal match ("a%b" 名のみ)。
	got, err = repo.ListRemoteWithFilter("a%b", host, "", "", 100, 0)
	require.NoError(t, err)
	ids = emojiIDSet(got)
	assert.Contains(t, ids, pct.ID, "% は escape され literal match する (#1948-13)")
	assert.NotContains(t, ids, lower.ID, "% は wildcard として解釈されない")
}

func emojiIDSet(emojis []*model.Emoji) map[string]struct{} {
	ids := make(map[string]struct{}, len(emojis))
	for _, e := range emojis {
		ids[e.ID] = struct{}{}
	}
	return ids
}

func TestEmojiRepository_FindByNameAndHost_Remote(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	host := "remote.example"
	e := &model.Emoji{
		ID:          "e_remote",
		Name:        "smile",
		Host:        &host,
		OriginalURL: "https://remote.example/smile.png",
	}
	require.NoError(t, testDB.Create(e).Error)
	defer cleanupEmoji(t, e.ID)

	found, err := repo.FindByNameAndHost("smile", &host)
	require.NoError(t, err)
	assert.Equal(t, &host, found.Host)
}

func TestEmojiRepository_FindByNameAndHost_NotFound(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	_, err := repo.FindByNameAndHost("ghost", nil)
	assert.Error(t, err)
}

func TestEmojiRepository_FindByNameAndHost_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewEmojiRepository(testDB.WithContext(ctx))
	_, err := repo.FindByNameAndHost("x", nil)
	assert.Error(t, err)
}

func TestEmojiRepository_CRUD(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e := &model.Emoji{ID: "e_crud", Name: "testcrud", OriginalURL: "https://example.com/x.png"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	found, err := repo.FindByID(e.ID)
	require.NoError(t, err)
	assert.Equal(t, "testcrud", found.Name)

	require.NoError(t, repo.UpdateFields(e.ID, map[string]any{"name": "updated"}))
	found, _ = repo.FindByID(e.ID)
	assert.Equal(t, "updated", found.Name)

	require.NoError(t, repo.Delete(e.ID))
	_, err = repo.FindByID(e.ID)
	assert.Error(t, err)
}

func TestEmojiRepository_FindByID_NotFound(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	_, err := repo.FindByID("ghost")
	assert.Error(t, err)
}

func TestEmojiRepository_ListWithFilter(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e := &model.Emoji{ID: "e_lwf", Name: "filtertest", OriginalURL: "https://example.com/f.png"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	emojis, err := repo.ListWithFilter("filter", "", true, "", "", 10, 0)
	require.NoError(t, err)
	assert.NotEmpty(t, emojis)

	// category filter
	emojis, err = repo.ListWithFilter("", "nonexistent", true, "", "", 10, 0)
	require.NoError(t, err)
	assert.Empty(t, emojis)

	// pagination
	emojis, err = repo.ListWithFilter("", "", true, "", "", 1, 0)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(emojis), 1)
}

// upstream list.ts は makePaginationQuery で id DESC (最新が先頭)。mk-go も
// カーソル無し時は DESC で並べる (#1543)。
func TestEmojiRepository_ListWithFilter_DefaultOrderDesc(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	e1 := &model.Emoji{ID: "e_ord_1", Name: "ordone", OriginalURL: "https://example.com/1.png"}
	e2 := &model.Emoji{ID: "e_ord_2", Name: "ordtwo", OriginalURL: "https://example.com/2.png"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	emojis, err := repo.ListWithFilter("", "", true, "", "", 100, 0)
	require.NoError(t, err)
	// id DESC なので e_ord_2 が e_ord_1 より前に来る。
	var i1, i2 = -1, -1
	for i, e := range emojis {
		if e.ID == "e_ord_1" {
			i1 = i
		}
		if e.ID == "e_ord_2" {
			i2 = i
		}
	}
	require.NotEqual(t, -1, i1)
	require.NotEqual(t, -1, i2)
	assert.Less(t, i2, i1, "id DESC: e_ord_2 が先")
}

// query が :name: 形式なら該当名の完全一致集合で絞る (#1543)。
func TestEmojiRepository_ListWithFilter_ColonNameExactMatch(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	exact := &model.Emoji{ID: "e_cn_exact", Name: "alpha", OriginalURL: "https://example.com/a.png"}
	prefixed := &model.Emoji{ID: "e_cn_prefixed", Name: "alphabet", OriginalURL: "https://example.com/ab.png"}
	require.NoError(t, repo.Create(exact))
	require.NoError(t, repo.Create(prefixed))
	defer cleanupEmoji(t, exact.ID)
	defer cleanupEmoji(t, prefixed.ID)

	// ":alpha:" は alpha のみ完全一致 (部分一致の alphabet は含めない)。
	emojis, err := repo.ListWithFilter(":alpha:", "", true, "", "", 100, 0)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, e := range emojis {
		ids[e.ID] = true
	}
	assert.True(t, ids["e_cn_exact"])
	assert.False(t, ids["e_cn_prefixed"], ":name: は完全一致のみ")
}

// query が :name: 形式でなければ name / aliases / category の部分一致で絞る (#1543)。
func TestEmojiRepository_ListWithFilter_AliasAndCategoryMatch(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	byAlias := &model.Emoji{ID: "e_q_alias", Name: "nomatch1", Aliases: model.StringArray{"zzqterm"}, OriginalURL: "https://example.com/al.png"}
	byCategory := &model.Emoji{ID: "e_q_cat", Name: "nomatch2", Category: strPtrEmoji("zzqterm-cat"), OriginalURL: "https://example.com/c.png"}
	unrelated := &model.Emoji{ID: "e_q_none", Name: "nomatch3", OriginalURL: "https://example.com/n.png"}
	require.NoError(t, repo.Create(byAlias))
	require.NoError(t, repo.Create(byCategory))
	require.NoError(t, repo.Create(unrelated))
	defer cleanupEmoji(t, byAlias.ID)
	defer cleanupEmoji(t, byCategory.ID)
	defer cleanupEmoji(t, unrelated.ID)

	emojis, err := repo.ListWithFilter("zzqterm", "", true, "", "", 100, 0)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, e := range emojis {
		ids[e.ID] = true
	}
	assert.True(t, ids["e_q_alias"], "aliases 部分一致")
	assert.True(t, ids["e_q_cat"], "category 部分一致")
	assert.False(t, ids["e_q_none"], "無関係は除外")
}

func strPtrEmoji(s string) *string { return &s }

func TestEmojiRepository_ListWithFilter_DefaultLimit(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	emojis, err := repo.ListWithFilter("", "", true, "", "", 0, 0) // limit=0 → default 50
	require.NoError(t, err)
	_ = emojis
}

func TestEmojiRepository_ListWithFilter_LimitCap(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	emojis, err := repo.ListWithFilter("", "", true, "", "", 999, 0)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(emojis), 500)
}

func TestEmojiRepository_ListWithFilter_Offset(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	emojis, err := repo.ListWithFilter("", "", true, "", "", 10, 99999)
	require.NoError(t, err)
	assert.Empty(t, emojis)
}

func TestEmojiRepository_ListWithFilter_NonLocal(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	emojis, err := repo.ListWithFilter("", "", false, "", "", 10, 0)
	require.NoError(t, err)
	_ = emojis // ローカルフィルタなし
}

func TestEmojiRepository_ListWithFilter_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewEmojiRepository(testDB.WithContext(ctx))
	_, err := repo.ListWithFilter("", "", true, "", "", 10, 0)
	assert.Error(t, err)
}

// clearLocalEmojiは本ファイルのemojiテストが使うローカル絵文字行
// (idがe_* / em_*で始まるもの) のみを削除する。
// 並行実行される他パッケージ (例: internal/core/mediaproxy) のテストが
// host IS NULLのemojiを挿入するため、広域なDELETEは競合を引き起こす
// (Devin review #259: race condition指摘)。接頭辞で絞ることで本パッケージ
// のテスト領域に限定する。
func clearLocalEmoji(t *testing.T) {
	t.Helper()
	require.NoError(t, testDB.Exec(
		`DELETE FROM "emoji" WHERE host IS NULL AND (id LIKE 'e\_%' ESCAPE '\' OR id LIKE 'em\_%' ESCAPE '\')`,
	).Error)
}

func TestEmojiRepository_ListLocal_Empty(t *testing.T) {
	clearLocalEmoji(t)
	repo := NewEmojiRepository(testDB)
	emojis, err := repo.ListLocal()
	require.NoError(t, err)
	assert.Empty(t, emojis)
}

func TestEmojiRepository_ListLocal_ReturnsLocalOnly(t *testing.T) {
	clearLocalEmoji(t)
	repo := NewEmojiRepository(testDB)

	local := &model.Emoji{ID: "e_ll", Name: "local_smile", OriginalURL: "https://example.com/s.png"}
	require.NoError(t, testDB.Create(local).Error)
	defer cleanupEmoji(t, local.ID)

	host := "remote.example"
	remote := &model.Emoji{ID: "e_lr", Name: "remote_smile", Host: &host, OriginalURL: "https://remote.example/s.png"}
	require.NoError(t, testDB.Create(remote).Error)
	defer cleanupEmoji(t, remote.ID)

	emojis, err := repo.ListLocal()
	require.NoError(t, err)
	assert.Len(t, emojis, 1)
	assert.Equal(t, "local_smile", emojis[0].Name)
}

func TestEmojiRepository_ListLocal_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewEmojiRepository(testDB.WithContext(ctx))
	_, err := repo.ListLocal()
	assert.Error(t, err)
}

func TestEmojiRepository_FindManyByIDs(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e1 := &model.Emoji{ID: "em_m1", Name: "many1", OriginalURL: "https://x"}
	e2 := &model.Emoji{ID: "em_m2", Name: "many2", OriginalURL: "https://y"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	rows, err := repo.FindManyByIDs([]string{e1.ID, e2.ID, "missing"})
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	empty, err := repo.FindManyByIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestEmojiRepository_UpdateFields_NotFound(t *testing.T) {
	// #650 問題 2: 存在しない id への UpdateFields は GORM の Updates が
	// 何も更新せずに Error=nil を返すため、以前は呼び出し側で「成功」と
	// 誤認していた。RowsAffected==0 を gorm.ErrRecordNotFound に昇格する。
	repo := NewEmojiRepository(testDB)

	err := repo.UpdateFields("nonexistent_id_xyz", map[string]any{"category": "x"})
	require.Error(t, err)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestEmojiRepository_UpdateFields_Success(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e := &model.Emoji{ID: "em_uf1", Name: "upd_target", OriginalURL: "https://x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	require.NoError(t, repo.UpdateFields(e.ID, map[string]any{"category": "happy"}))
	after, err := repo.FindByID(e.ID)
	require.NoError(t, err)
	require.NotNil(t, after.Category)
	assert.Equal(t, "happy", *after.Category)
}

// TestEmojiRepository_UpdateFields_AliasesEmptySlice は #729 regression
// guard。`model.StringArray{}` (空 slice) で aliases 列を更新する経路が
// PostgreSQL の NOT NULL DEFAULT '{}' 制約と整合することを確認する。
//
// 旧 EmojiUpdate handler は `[]string` を直接 GORM に渡していて、空 slice
// が NULL として serialize され "null value in column \"aliases\" violates
// not-null constraint" で UpdateFields が失敗していた (frontend が
// aliases:[] で送るたびに NO_SUCH_EMOJI が返る病状)。本 test は
// `model.StringArray` 経由の UpdateFields が成功することを保証する。
func TestEmojiRepository_UpdateFields_AliasesEmptySlice(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	e := &model.Emoji{
		ID: "em_uf_alias", Name: "alias_target", OriginalURL: "https://x",
		Aliases: model.StringArray{"old"},
	}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	// 空 slice の model.StringArray は SQL の '{}' に変換される
	require.NoError(t, repo.UpdateFields(e.ID, map[string]any{"aliases": model.StringArray{}}))
	after, err := repo.FindByID(e.ID)
	require.NoError(t, err)
	assert.Empty(t, []string(after.Aliases), "empty model.StringArray should clear aliases without NULL constraint violation")

	// 通常の値も問題なく更新できる
	require.NoError(t, repo.UpdateFields(e.ID, map[string]any{"aliases": model.StringArray{"a", "b"}}))
	after, err = repo.FindByID(e.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, []string(after.Aliases))
}

func TestEmojiRepository_UpdateFieldsMany(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e1 := &model.Emoji{ID: "em_u1", Name: "up1", OriginalURL: "https://x"}
	e2 := &model.Emoji{ID: "em_u2", Name: "up2", OriginalURL: "https://y"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	require.NoError(t, repo.UpdateFieldsMany([]string{e1.ID, e2.ID}, map[string]any{
		"category": "animals",
	}))
	after1, _ := repo.FindByID(e1.ID)
	after2, _ := repo.FindByID(e2.ID)
	require.NotNil(t, after1.Category)
	require.NotNil(t, after2.Category)
	assert.Equal(t, "animals", *after1.Category)
	assert.Equal(t, "animals", *after2.Category)

	// 空 ids / 空 fields は no-op
	require.NoError(t, repo.UpdateFieldsMany(nil, map[string]any{"x": 1}))
	require.NoError(t, repo.UpdateFieldsMany([]string{e1.ID}, map[string]any{}))
}

// TestEmojiRepository_UpdateFieldsMany_AliasesPqStringArray は #882 で
// 発覚した bulk drift の regression guard。core/api/admin の bulk handler
// は plain []string を渡すと GORM が record literal `('a','b')` を生成
// して SQLSTATE 42804 (column type mismatch) になっていた。model.StringArray
// で wrap した場合に array literal `'{a,b}'` として正しく serialize され
// ることを確認する (UpdateFields_AliasesEmptySlice の bulk 版)。
func TestEmojiRepository_UpdateFieldsMany_AliasesPqStringArray(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	e1 := &model.Emoji{ID: "em_ufm_a1", Name: "ufm_a1", OriginalURL: "https://x"}
	e2 := &model.Emoji{ID: "em_ufm_a2", Name: "ufm_a2", OriginalURL: "https://y"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	// 通常の値: model.StringArray wrap で 2 emoji 同時 update
	require.NoError(t, repo.UpdateFieldsMany([]string{e1.ID, e2.ID}, map[string]any{
		"aliases": model.StringArray{"alpha", "beta"},
	}))
	a1, _ := repo.FindByID(e1.ID)
	a2, _ := repo.FindByID(e2.ID)
	assert.Equal(t, []string{"alpha", "beta"}, []string(a1.Aliases))
	assert.Equal(t, []string{"alpha", "beta"}, []string(a2.Aliases))

	// 空 slice: NOT NULL 制約に当たらず '{}' として保存される
	require.NoError(t, repo.UpdateFieldsMany([]string{e1.ID, e2.ID}, map[string]any{
		"aliases": model.StringArray{},
	}))
	a1, _ = repo.FindByID(e1.ID)
	assert.Empty(t, []string(a1.Aliases))
}

func TestEmojiRepository_DeleteMany(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e1 := &model.Emoji{ID: "em_d1", Name: "del1", OriginalURL: "https://x"}
	e2 := &model.Emoji{ID: "em_d2", Name: "del2", OriginalURL: "https://y"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	require.NoError(t, repo.DeleteMany([]string{e1.ID, e2.ID}))
	_, err := repo.FindByID(e1.ID)
	assert.Error(t, err)
	_, err = repo.FindByID(e2.ID)
	assert.Error(t, err)

	// 空 slice は no-op
	require.NoError(t, repo.DeleteMany(nil))
}

func TestEmojiRepository_ListRemoteWithFilter(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	host := "remote.example"
	e1 := &model.Emoji{ID: "em_r1", Name: "rcat", Host: &host, OriginalURL: "https://x"}
	e2 := &model.Emoji{ID: "em_r2", Name: "rdog", Host: &host, OriginalURL: "https://y"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	rows, err := repo.ListRemoteWithFilter("", host, "", "", 10, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rows), 2)

	filtered, err := repo.ListRemoteWithFilter("cat", host, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, filtered, 1)
	assert.Equal(t, e1.ID, filtered[0].ID)

	// limit default / cap
	_, err = repo.ListRemoteWithFilter("", host, "", "", 0, 0) // default 30
	require.NoError(t, err)
	_, err = repo.ListRemoteWithFilter("", host, "", "", 1000, 0) // clamped to 100
	require.NoError(t, err)
}

func TestEmojiRepository_ListV2_Basic(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e1 := &model.Emoji{ID: "ev_b1", Name: "v2alpha", OriginalURL: "https://x"}
	e2 := &model.Emoji{ID: "ev_b2", Name: "v2beta", OriginalURL: "https://y"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	rows, err := repo.ListV2(model.EmojiV2Filter{Limit: 10})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rows), 2)
}

func TestEmojiRepository_ListV2_HostType(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	local := &model.Emoji{ID: "ev_h1", Name: "v2local", OriginalURL: "https://x"}
	host := "v2remote.example"
	remote := &model.Emoji{ID: "ev_h2", Name: "v2remote", Host: &host, OriginalURL: "https://y"}
	require.NoError(t, repo.Create(local))
	require.NoError(t, repo.Create(remote))
	defer cleanupEmoji(t, local.ID)
	defer cleanupEmoji(t, remote.ID)

	// localのみ
	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{HostType: "local", Name: "v2"},
		Limit: 100,
	})
	require.NoError(t, err)
	for _, r := range rows {
		assert.Nil(t, r.Host)
	}

	// remoteのみ
	rows, err = repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{HostType: "remote", Name: "v2"},
		Limit: 100,
	})
	require.NoError(t, err)
	for _, r := range rows {
		assert.NotNil(t, r.Host)
	}
}

func TestEmojiRepository_ListV2_NameFilter(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e := &model.Emoji{ID: "ev_n1", Name: "v2uniquename", OriginalURL: "https://x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Name: "v2unique"},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "ev_n1", rows[0].ID)
}

// #1999: v2 buildV2Query は upstream fetchEmojis に揃える: case-sensitive LIKE +
// escapeLike + multipleWordsToQuery (空白分割 OR) + aliases 要素単位 + updatedAt date cast。
func TestEmojiRepository_ListV2_QueryParity(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	mk := func(id, name, category string, aliases []string) *model.Emoji {
		e := &model.Emoji{ID: id, Name: name, Category: &category, Aliases: model.StringArray(aliases), OriginalURL: "https://x"}
		require.NoError(t, repo.Create(e))
		t.Cleanup(func() { cleanupEmoji(t, id) })
		return e
	}
	mk("evp_upper", "ParityUpper", "Cat", []string{"smile", "happy_face"})
	mk("evp_lower", "paritylower", "cat", []string{"frown"})

	// case-sensitive: "Parity" は ParityUpper のみ match (paritylower は小文字)。
	rows, err := repo.ListV2(model.EmojiV2Filter{Query: &model.EmojiV2Query{Name: "Parity"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "evp_upper", rows[0].ID)

	// multipleWordsToQuery: 空白区切りは各語の OR。"Upper lower" は両方 match。
	rows, err = repo.ListV2(model.EmojiV2Filter{Query: &model.EmojiV2Query{Name: "Upper lower"}, Limit: 10})
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	// escapeLike: "%" は literal 扱いで wildcard 化しない (どちらにも無いので 0 件)。
	rows, err = repo.ListV2(model.EmojiV2Filter{Query: &model.EmojiV2Query{Name: "Par%ity"}, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, rows)

	// aliases は要素単位突合: "happy_face" の under_score は escape され、"smile" alias は要素一致。
	rows, err = repo.ListV2(model.EmojiV2Filter{Query: &model.EmojiV2Query{Aliases: "smile"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "evp_upper", rows[0].ID)
}

func TestEmojiRepository_ListV2_SortKeys(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e1 := &model.Emoji{ID: "ev_s1", Name: "v2zebra", OriginalURL: "https://x"}
	e2 := &model.Emoji{ID: "ev_s2", Name: "v2apple", OriginalURL: "https://y"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	// name ASC
	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query:    &model.EmojiV2Query{Name: "v2"},
		SortKeys: []string{"+name"},
		Limit:    10,
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), 2)
	// v2apple < v2zebra
	for i := 0; i < len(rows)-1; i++ {
		assert.LessOrEqual(t, rows[i].Name, rows[i+1].Name)
	}
}

func TestEmojiRepository_ListV2_Pagination(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	ids := []string{"ev_p1", "ev_p2", "ev_p3"}
	for i, id := range ids {
		e := &model.Emoji{ID: id, Name: "v2page" + string(rune('a'+i)), OriginalURL: "https://x"}
		require.NoError(t, repo.Create(e))
		defer cleanupEmoji(t, id)
	}

	// page 1, limit 2
	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Name: "v2page"},
		Limit: 2,
		Page:  1,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	// page 2
	rows, err = repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Name: "v2page"},
		Limit: 2,
		Page:  2,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestEmojiRepository_ListV2_SinceUntilID(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e1 := &model.Emoji{ID: "ev_c1", Name: "v2cursor1", OriginalURL: "https://x"}
	e2 := &model.Emoji{ID: "ev_c2", Name: "v2cursor2", OriginalURL: "https://y"}
	require.NoError(t, repo.Create(e1))
	require.NoError(t, repo.Create(e2))
	defer cleanupEmoji(t, e1.ID)
	defer cleanupEmoji(t, e2.ID)

	// sinceId: ev_c1より後
	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query:   &model.EmojiV2Query{Name: "v2cursor"},
		SinceID: "ev_c1",
		Limit:   10,
	})
	require.NoError(t, err)
	for _, r := range rows {
		assert.Greater(t, r.ID, "ev_c1")
	}

	// untilId: ev_c2より前
	rows, err = repo.ListV2(model.EmojiV2Filter{
		Query:   &model.EmojiV2Query{Name: "v2cursor"},
		UntilID: "ev_c2",
		Limit:   10,
	})
	require.NoError(t, err)
	for _, r := range rows {
		assert.Less(t, r.ID, "ev_c2")
	}
}

func TestEmojiRepository_ListV2_BooleanFilters(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e := &model.Emoji{ID: "ev_bf1", Name: "v2sensitive", IsSensitive: true, LocalOnly: true, OriginalURL: "https://x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	boolTrue := true
	boolFalse := false

	// isSensitive=true
	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Name: "v2sensitive", IsSensitive: &boolTrue},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	// isSensitive=false
	rows, err = repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Name: "v2sensitive", IsSensitive: &boolFalse},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, rows)

	// localOnly=true
	rows, err = repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Name: "v2sensitive", LocalOnly: &boolTrue},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestEmojiRepository_ListV2_DefaultsAndLimitClamp(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	// limit=0 → default 10
	rows, err := repo.ListV2(model.EmojiV2Filter{Limit: 0})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(rows), 10)

	// limit > 100 → clamped
	rows, err = repo.ListV2(model.EmojiV2Filter{Limit: 999})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(rows), 100)
}

func TestEmojiRepository_ListV2_InvalidSortKeyIgnored(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	// 不正なsortKeyは無視されエラーにならない
	rows, err := repo.ListV2(model.EmojiV2Filter{
		SortKeys: []string{"+invalid", "x", "-name"},
		Limit:    10,
	})
	require.NoError(t, err)
	_ = rows
}

func TestEmojiRepository_ListV2_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewEmojiRepository(testDB.WithContext(ctx))
	_, err := repo.ListV2(model.EmojiV2Filter{Limit: 10})
	assert.Error(t, err)
}

func TestEmojiRepository_CountV2_Basic(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e := &model.Emoji{ID: "ev_ct1", Name: "v2countme", OriginalURL: "https://x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	count, err := repo.CountV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Name: "v2countme"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestEmojiRepository_CountV2_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewEmojiRepository(testDB.WithContext(ctx))
	_, err := repo.CountV2(model.EmojiV2Filter{})
	assert.Error(t, err)
}

func TestEmojiRepository_ListV2_CategoryFilter(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	cat := "v2animals"
	e := &model.Emoji{ID: "ev_cf1", Name: "v2catfilter", Category: &cat, OriginalURL: "https://x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Category: "v2animal"},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	rows, err = repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Category: "nonexistent"},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestEmojiRepository_ListV2_LicenseFilter(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	lic := "CC-BY-4.0"
	e := &model.Emoji{ID: "ev_lf1", Name: "v2licfilter", License: &lic, OriginalURL: "https://x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{License: "CC-BY"},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestEmojiRepository_ListV2_HostFilter(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	host := "v2host.example"
	e := &model.Emoji{ID: "ev_hf1", Name: "v2hostfilter", Host: &host, OriginalURL: "https://x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Host: "v2host"},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestEmojiRepository_ListV2_AliasesFilter(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	e := &model.Emoji{
		ID: "ev_af1", Name: "v2aliasfilter",
		Aliases:     []string{"v2happy", "v2joy"},
		OriginalURL: "https://x",
	}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Aliases: "v2happy"},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestEmojiRepository_ListV2_TypeFilter(t *testing.T) {
	repo := NewEmojiRepository(testDB)

	tp := "image/webp"
	e := &model.Emoji{ID: "ev_tf1", Name: "v2typefilter", Type: &tp, OriginalURL: "https://x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)

	rows, err := repo.ListV2(model.EmojiV2Filter{
		Query: &model.EmojiV2Query{Type: "webp"},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// #2034: v2 list の originalUrl filter は upstream fetchEmojis で無視されるので、
// originalUrl query を指定しても絞り込まれない。
func TestEmojiRepository_ListV2_OriginalURLIgnored(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	e := &model.Emoji{ID: "evou1", Name: "evouname", OriginalURL: "https://real.example/x"}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)
	rows, err := repo.ListV2(model.EmojiV2Filter{Query: &model.EmojiV2Query{Name: "evouname", OriginalURL: "nonmatching"}, Limit: 10})
	require.NoError(t, err)
	assert.Len(t, rows, 1, "originalUrl filter は無視される (#2034)")
}

// #2034: legacy list の aliases は要素単位で突合する (array_to_string 結合の要素境界
// 跨ぎ誤 match を避ける)。
func TestEmojiRepository_ListWithFilter_AliasesElementBoundary(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	e := &model.Emoji{ID: "elb1", Name: "elbname", OriginalURL: "https://x", Aliases: model.StringArray{"foo", "bar"}}
	require.NoError(t, repo.Create(e))
	defer cleanupEmoji(t, e.ID)
	// "foo,bar" は結合文字列 "foo,bar" には部分一致するが、単一 alias には無い。
	rows, err := repo.ListWithFilter("foo,bar", "", false, "", "", 10, 0)
	require.NoError(t, err)
	assert.Empty(t, rows, "要素境界を跨いだ query は match しない (#2034)")
	// 単一要素 "foo" は match する。
	rows, err = repo.ListWithFilter("foo", "", false, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}
