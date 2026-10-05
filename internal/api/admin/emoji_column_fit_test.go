package admin_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `admin/emoji/add` / `update` / 一括編集が、列に入らない値を弾く経路 (#3018)。
//
// 直している失敗形は「**利用者の入力で 5xx が立つ**」。`emoji` の
// `name` varchar(128) / `category` varchar(128) / `license` varchar(1024) /
// `aliases` varchar(128)[] を超える値は SQLSTATE 22001 が生のまま 500 になる
// (`internal/repository` の test schema で実測)。`admin/emoji/copy` は
// #2998 / #2726 で既に塞いであり、そちらは**リモートが決める値**なので切る。
// ここは人がその場で打つ値なので**本文は弾き、alias は要素ごと落とす**
// (申請経路 `emojiapplication.Service.Create` と同じ扱い)。

const (
	// 列の上限ちょうど / 1 つ超え。**全角で作る** — byte で数える実装なら
	// 「ちょうど」のケースが落ちる (varchar はコードポイントで数える)。
	//
	// **列ごとに別の定数にする。** `category` と `aliases` と
	// `roleIdsThatCanBeUsedThisEmojiAsReaction` はたまたま同じ 128 だが、
	// 独立に変わりうる (1 つに寄せると、片方の上限を小さくする変異が緑で通る)。
	// 値そのものは `emoji_column_fit_internal_test.go` が DDL の側と突き合わせる。
	catAtLimit    = 128
	licAtLimit    = 1024
	aliasAtLimit  = 128
	roleIDAtLimit = 128
	urlAtLimit    = 512
	invalidUUID   = "3d81ceae-475f-4600-b2a8-2bc116157532"
)

func runes(n int, r string) string { return strings.Repeat(r, n) }

// errorMessage returns the wire message so tests can tell which field the 400
// is about (`emojiValueTooLong` はそこにしか列名を出さない)。
func errorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Error.Message
}

// 列に入らない本文は 400。**error id は既存の `INVALID_PARAM`** を共有する
// (upstream の paramDef に maxLength が無いので対応する id が存在しない)。
func TestEmojiAdd_RejectsUnstorableBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{"category が 1 文字超過", "category", runes(catAtLimit+1, "あ")},
		{"license が 1 文字超過", "license", runes(licAtLimit+1, "い")},
		{"category に NUL", "category", "a\x00b"},
		{"license に NUL", "license", "a\x00b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := setupEmojiHandler(t)
			body, err := json.Marshal(map[string]any{
				"name": "happy", "url": "https://cdn.example/x.png", tc.field: tc.value,
			})
			require.NoError(t, err)

			rec := doPost(h.EmojiAdd, string(body), adminUser)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			code, id := errorID(t, rec.Body.Bytes())
			assert.Equal(t, "INVALID_PARAM", code)
			assert.Equal(t, invalidUUID, id)
			assert.Contains(t, errorMessage(t, rec.Body.Bytes()), tc.field, "どの列で弾いたか分からない")
			_, ferr := repo.FindByNameAndHost("happy", nil)
			assert.Error(t, ferr, "弾いたのに絵文字が作られている")
		})
	}
}

// 上限ちょうどは通し、値を切らずに保存する (境界を off-by-one で締めない)。
func TestEmojiAdd_AcceptsBodyAtLimit(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	cat, lic := runes(catAtLimit, "あ"), runes(licAtLimit, "い")
	body, err := json.Marshal(map[string]any{
		"name": "happy", "url": "https://cdn.example/x.png", "category": cat, "license": lic,
	})
	require.NoError(t, err)

	rec := doPost(h.EmojiAdd, string(body), adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	require.NotNil(t, got.Category)
	assert.Equal(t, cat, *got.Category, "上限ちょうどの値を切っている")
	require.NotNil(t, got.License)
	assert.Equal(t, lic, *got.License)
}

// alias は**要素ごとに落とす**。1 つが長すぎるだけで他まで捨てない。
// NUL は落としてから判定するので、NUL だけの要素は空になって消える。
func TestEmojiAdd_DropsUnstorableAliases(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	body, err := json.Marshal(map[string]any{
		"name": "happy", "url": "https://cdn.example/x.png",
		"aliases": []string{"ok", runes(aliasAtLimit+1, "あ"), "", "a\x00b", "\x00"},
	})
	require.NoError(t, err)

	rec := doPost(h.EmojiAdd, string(body), adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"ok", "ab"}, []string(got.Aliases))
}

// **複製より前に弾く。** 後ろに置くと、400 のたびに誰からも参照されない
// system 所有の複製が残る (#2999 / #3014 と同じ理由)。
func TestEmojiAdd_RejectsBodyBeforeCopy(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sysfit1", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	body, err := json.Marshal(map[string]any{
		"name": "happy", "fileId": "f_img", "category": runes(catAtLimit+1, "あ"),
	})
	require.NoError(t, err)

	rec := doPost(h.EmojiAdd, string(body), adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, fetcher.copyCalls, "列で弾くのに複製を作っている (孤児になる)")
}

func TestEmojiUpdate_RejectsUnstorableBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{"category が 1 文字超過", "category", runes(catAtLimit+1, "あ")},
		{"license が 1 文字超過", "license", runes(licAtLimit+1, "い")},
		{"category に NUL", "category", "a\x00b"},
		{"license に NUL", "license", "a\x00b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
			body, err := json.Marshal(map[string]any{"id": "e1", tc.field: tc.value})
			require.NoError(t, err)

			rec := doPost(h.EmojiUpdate, string(body), adminUser)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			code, id := errorID(t, rec.Body.Bytes())
			assert.Equal(t, "INVALID_PARAM", code)
			assert.Equal(t, invalidUUID, id)
			assert.Contains(t, errorMessage(t, rec.Body.Bytes()), tc.field, "どの列で弾いたか分からない")
			got := findEmojiByID(t, repo, "e1")
			assert.Nil(t, got.Category, "弾いたのに書き込まれている")
			assert.Nil(t, got.License, "弾いたのに書き込まれている")
		})
	}
}

func TestEmojiUpdate_AcceptsBodyAtLimit(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
	cat, lic := runes(catAtLimit, "あ"), runes(licAtLimit, "い")
	body, err := json.Marshal(map[string]any{"id": "e1", "category": cat, "license": lic})
	require.NoError(t, err)

	rec := doPost(h.EmojiUpdate, string(body), adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	got := findEmojiByID(t, repo, "e1")
	require.NotNil(t, got.Category)
	assert.Equal(t, cat, *got.Category)
	require.NotNil(t, got.License)
	assert.Equal(t, lic, *got.License)
}

func TestEmojiUpdate_DropsUnstorableAliases(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
	body, err := json.Marshal(map[string]any{
		"id": "e1", "aliases": []string{"ok", runes(aliasAtLimit+1, "あ"), "", "a\x00b"},
	})
	require.NoError(t, err)

	rec := doPost(h.EmojiUpdate, string(body), adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"ok", "ab"}, []string(findEmojiByID(t, repo, "e1").Aliases))
}

// **送った要素が全部落ちたら 400。** 空配列を書くのは「全消去」なので、落ちた結果を
// 204 で返すと**既存の値を黙って消す**方向へ倒れる (#3018 のレビュー H1)。#3018 より前は
// NOT NULL 違反や 22001 で 500 になっており、うるさいがデータは無事だった。
func TestEmojiColumnFit_RejectsWhenEveryElementDropped(t *testing.T) {
	long := runes(aliasAtLimit+1, "あ")

	t.Run("update の aliases", func(t *testing.T) {
		h, repo := setupEmojiHandler(t, &model.Emoji{
			ID: "e1", Name: "happy", Aliases: model.StringArray{"keep"},
		})
		body, err := json.Marshal(map[string]any{"id": "e1", "aliases": []string{long}})
		require.NoError(t, err)

		rec := doPost(h.EmojiUpdate, string(body), adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, id := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "INVALID_PARAM", code)
		assert.Equal(t, invalidUUID, id)
		assert.Contains(t, errorMessage(t, rec.Body.Bytes()), "aliases")
		assert.Equal(t, []string{"keep"}, []string(findEmojiByID(t, repo, "e1").Aliases),
			"1 つも入らなかっただけで既存の alias が消えている")
	})

	t.Run("update の roleIds", func(t *testing.T) {
		h, repo := setupEmojiHandler(t, &model.Emoji{
			ID: "e1", Name: "happy",
			RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray{"role_keep"},
		})
		body, err := json.Marshal(map[string]any{
			"id": "e1", "roleIdsThatCanBeUsedThisEmojiAsReaction": []string{long},
		})
		require.NoError(t, err)

		rec := doPost(h.EmojiUpdate, string(body), adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		// 空にするとリアクション制限そのものが外れる (`len(roleIds) > 0` でしか見ない)。
		assert.Equal(t, []string{"role_keep"},
			[]string(findEmojiByID(t, repo, "e1").RoleIDsThatCanBeUsedThisEmojiAsReaction),
			"1 つも入らなかっただけでリアクション制限が外れている")
	})

	t.Run("set-aliases-bulk", func(t *testing.T) {
		h, repo := setupEmojiHandler(t, &model.Emoji{
			ID: "e1", Name: "happy", Aliases: model.StringArray{"keep1", "keep2"},
		})
		body, err := json.Marshal(map[string]any{"ids": []string{"e1"}, "aliases": []string{long}})
		require.NoError(t, err)

		rec := doPost(h.EmojiSetAliasesBulk, string(body), adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, []string{"keep1", "keep2"}, []string(findEmojiByID(t, repo, "e1").Aliases),
			"同梱 frontend の一括タグ付けは入力をそのまま送るので、ここが通ると全選択ぶん消える")
	})

	t.Run("add-aliases-bulk", func(t *testing.T) {
		h, repo := setupEmojiHandler(t, &model.Emoji{
			ID: "e1", Name: "happy", Aliases: model.StringArray{"keep"},
		})
		body, err := json.Marshal(map[string]any{"ids": []string{"e1"}, "aliases": []string{long}})
		require.NoError(t, err)

		rec := doPost(h.EmojiAddAliasesBulk, string(body), adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, []string{"keep"}, []string(findEmojiByID(t, repo, "e1").Aliases))
	})

	t.Run("add", func(t *testing.T) {
		h, repo := setupEmojiHandler(t)
		body, err := json.Marshal(map[string]any{
			"name": "happy", "url": "https://cdn.example/x.png", "aliases": []string{long},
		})
		require.NoError(t, err)

		rec := doPost(h.EmojiAdd, string(body), adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		_, ferr := repo.FindByNameAndHost("happy", nil)
		assert.Error(t, ferr, "弾いたのに絵文字が作られている")
	})
}

// 明示した空配列は「全消去」として通す (落ちた結果と区別する)。
func TestEmojiUpdate_EmptyAliasArrayClears(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "happy", Aliases: model.StringArray{"old"},
	})

	require.Equal(t, http.StatusNoContent,
		doPost(h.EmojiUpdate, `{"id":"e1","aliases":[]}`, adminUser).Code)
	assert.Empty(t, []string(findEmojiByID(t, repo, "e1").Aliases))
}

// 改名は長さも見る。pattern は文字種しか縛らないので、129 文字の ASCII 名は
// 素通りして SQLSTATE 22001 になる。
func TestEmojiUpdate_RejectsOverlongRename(t *testing.T) {
	for _, tc := range []struct {
		name    string
		newName string
		want    int
	}{
		{"128 文字ちょうどは通る", strings.Repeat("a", 128), http.StatusNoContent},
		{"129 文字は弾く", strings.Repeat("a", 129), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
			body, err := json.Marshal(map[string]any{"id": "e1", "name": tc.newName})
			require.NoError(t, err)

			rec := doPost(h.EmojiUpdate, string(body), adminUser)
			require.Equal(t, tc.want, rec.Code)
			if tc.want == http.StatusBadRequest {
				assert.Equal(t, "happy", findEmojiByID(t, repo, "e1").Name)
			}
		})
	}
}

// **複製より前に弾く** (add と同じ理由)。
func TestEmojiUpdate_RejectsBodyBeforeCopy(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "happy", OriginalURL: "https://example/system/old.png",
	})
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sysfit2", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	body, err := json.Marshal(map[string]any{
		"id": "e1", "fileId": "f_img", "license": runes(licAtLimit+1, "い"),
	})
	require.NoError(t, err)

	rec := doPost(h.EmojiUpdate, string(body), adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, fetcher.copyCalls, "列で弾くのに複製を作っている (孤児になる)")
	assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL)
}

// 一括編集も同じ規則。**per-emoji の log だけ残して全件が黙って失敗する**のを
// 止めるのが目的なので、書き込む前に弾く。
func TestEmojiSetCategoryBulk_RejectsUnstorable(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
	body, err := json.Marshal(map[string]any{"ids": []string{"e1"}, "category": runes(catAtLimit+1, "あ")})
	require.NoError(t, err)

	rec := doPost(h.EmojiSetCategoryBulk, string(body), adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, invalidUUID, id)
	assert.Contains(t, errorMessage(t, rec.Body.Bytes()), "category", "どの列で弾いたか分からない")
	assert.Nil(t, findEmojiByID(t, repo, "e1").Category, "弾いたのに書き込まれている")
}

func TestEmojiSetLicenseBulk_RejectsUnstorable(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
	body, err := json.Marshal(map[string]any{"ids": []string{"e1"}, "license": runes(licAtLimit+1, "い")})
	require.NoError(t, err)

	rec := doPost(h.EmojiSetLicenseBulk, string(body), adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, invalidUUID, id)
	assert.Contains(t, errorMessage(t, rec.Body.Bytes()), "license", "どの列で弾いたか分からない")
	assert.Nil(t, findEmojiByID(t, repo, "e1").License, "弾いたのに書き込まれている")
}

// 上限ちょうどの一括編集は通る。
func TestEmojiSetCategoryBulk_AcceptsAtLimit(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
	cat := runes(catAtLimit, "あ")
	body, err := json.Marshal(map[string]any{"ids": []string{"e1"}, "category": cat})
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, doPost(h.EmojiSetCategoryBulk, string(body), adminUser).Code)
	got := findEmojiByID(t, repo, "e1")
	require.NotNil(t, got.Category)
	assert.Equal(t, cat, *got.Category)
}

func TestEmojiSetAliasesBulk_DropsUnstorable(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
	body, err := json.Marshal(map[string]any{
		"ids": []string{"e1"}, "aliases": []string{"ok", runes(aliasAtLimit+1, "あ"), "a\x00b"},
	})
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, doPost(h.EmojiSetAliasesBulk, string(body), adminUser).Code)
	assert.Equal(t, []string{"ok", "ab"}, []string(findEmojiByID(t, repo, "e1").Aliases))
}

func TestEmojiAddAliasesBulk_DropsUnstorable(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "happy", Aliases: model.StringArray{"keep"},
	})
	body, err := json.Marshal(map[string]any{
		"ids": []string{"e1"}, "aliases": []string{"ok", runes(aliasAtLimit+1, "あ")},
	})
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, doPost(h.EmojiAddAliasesBulk, string(body), adminUser).Code)
	assert.Equal(t, []string{"keep", "ok"}, []string(findEmojiByID(t, repo, "e1").Aliases),
		"既存の alias を巻き込まずに、入らない要素だけ落とすこと")
}

// **upstream が宣言する error を新しい 400 で先取りしない (#3018)。** 2 つ問題が
// あるリクエストで先に返る error が変わると、drop-in クライアントの分岐が変わる。
// `add.ts` は noSuchFile → duplicateName → unsupportedFileType、`update.ts` は
// noSuchFile → noSuchEmoji の順に宣言している。
//
// **`unsupportedFileType` より前になるのは意図的** — MIME の検査は `fileId` 経路の
// 中にあり、その後ろへ置くと `url` 経路が検査から外れる。
func TestEmojiColumnFit_DoesNotPreemptUpstreamErrors(t *testing.T) {
	longCat := runes(catAtLimit+1, "あ")

	t.Run("add: 存在しない fileId が先", func(t *testing.T) {
		h, _ := setupEmojiHandler(t)
		ownedDriveFile(t, h, &model.DriveFile{ID: "f_other", Type: "image/png", URL: "https://example/x.png"})
		body, err := json.Marshal(map[string]any{"name": "happy", "fileId": "nope", "category": longCat})
		require.NoError(t, err)

		rec := doPost(h.EmojiAdd, string(body), adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "NO_SUCH_FILE", code)
	})

	t.Run("add: 重複名が先", func(t *testing.T) {
		h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
		body, err := json.Marshal(map[string]any{
			"name": "happy", "url": "https://cdn.example/x.png", "category": longCat,
		})
		require.NoError(t, err)

		rec := doPost(h.EmojiAdd, string(body), adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "DUPLICATE_NAME", code)
	})

	t.Run("update: 絵文字が無いのが先", func(t *testing.T) {
		h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
		body, err := json.Marshal(map[string]any{"id": "missing", "category": longCat})
		require.NoError(t, err)

		rec := doPost(h.EmojiUpdate, string(body), adminUser)
		require.Equal(t, http.StatusNotFound, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "NO_SUCH_EMOJI", code)
	})

	t.Run("update: 存在しない fileId が先", func(t *testing.T) {
		h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
		ownedDriveFile(t, h, &model.DriveFile{ID: "f_other", Type: "image/png", URL: "https://example/x.png"})
		body, err := json.Marshal(map[string]any{"id": "e1", "fileId": "nope", "category": longCat})
		require.NoError(t, err)

		rec := doPost(h.EmojiUpdate, string(body), adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "NO_SUCH_FILE", code)
	})
}

// `roleIdsThatCanBeUsedThisEmojiAsReaction` も `aliases` と同じ varchar(128)[] で、
// 同じ request struct から無検証で書かれていた (#3018)。**実在するロールかは見ない**
// ので、落ちるのは id として最初から存在しえない長さのものだけ。
func TestEmojiColumnFit_DropsUnstorableRoleIDs(t *testing.T) {
	long := runes(roleIDAtLimit+1, "あ")

	t.Run("add", func(t *testing.T) {
		h, repo := setupEmojiHandler(t)
		body, err := json.Marshal(map[string]any{
			"name": "happy", "url": "https://cdn.example/x.png",
			"roleIdsThatCanBeUsedThisEmojiAsReaction": []string{"role1", long},
		})
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, doPost(h.EmojiAdd, string(body), adminUser).Code)
		got, err := repo.FindByNameAndHost("happy", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"role1"}, []string(got.RoleIDsThatCanBeUsedThisEmojiAsReaction))
	})

	t.Run("update", func(t *testing.T) {
		h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
		body, err := json.Marshal(map[string]any{
			"id": "e1", "roleIdsThatCanBeUsedThisEmojiAsReaction": []string{"role1", long},
		})
		require.NoError(t, err)

		require.Equal(t, http.StatusNoContent, doPost(h.EmojiUpdate, string(body), adminUser).Code)
		got := findEmojiByID(t, repo, "e1")
		assert.Equal(t, []string{"role1"}, []string(got.RoleIDsThatCanBeUsedThisEmojiAsReaction))
	})
}

// `aliases` を落として送ると**全消去**になっていた (#3018 のレビュー H1)。
// upstream は `aliases` を required にしているので schema validator が 400 を返す形。
// mk-go も 400 で返し、**データを触らない**。
func TestEmojiSetAliasesBulk_RequiresAliases(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "happy", Aliases: model.StringArray{"keep1", "keep2"},
	})

	rec := doPost(h.EmojiSetAliasesBulk, `{"ids":["e1"]}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, invalidUUID, id)
	assert.Equal(t, []string{"keep1", "keep2"},
		[]string(findEmojiByID(t, repo, "e1").Aliases), "省略しただけで alias が消えている")
}

// 明示した空配列は「全消去」として通す (省略と区別する)。
func TestEmojiSetAliasesBulk_EmptyArrayClears(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "happy", Aliases: model.StringArray{"old"},
	})

	require.Equal(t, http.StatusNoContent,
		doPost(h.EmojiSetAliasesBulk, `{"ids":["e1"],"aliases":[]}`, adminUser).Code)
	assert.Empty(t, []string(findEmojiByID(t, repo, "e1").Aliases))
}

// `url` 直接指定 (mk-go 独自の legacy 経路) も列に入らなければ 400 (#3018)。
// `emoji.originalUrl` / `publicUrl` は varchar(512) で、この経路は利用者の文字列を
// そのまま両方へ入れる。
func TestEmojiAdd_RejectsUnstorableURL(t *testing.T) {
	long := "https://cdn.example/" + strings.Repeat("a", 512)
	h, repo := setupEmojiHandler(t)
	body, err := json.Marshal(map[string]any{"name": "happy", "url": long})
	require.NoError(t, err)

	rec := doPost(h.EmojiAdd, string(body), adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, invalidUUID, id)
	assert.Contains(t, errorMessage(t, rec.Body.Bytes()), "url", "どの列で弾いたか分からない")
	_, ferr := repo.FindByNameAndHost("happy", nil)
	assert.Error(t, ferr, "弾いたのに絵文字が作られている")
}

// 上限ちょうどの URL は通す。**全角で作る** — byte で数える実装なら落ちる。
func TestEmojiAdd_AcceptsURLAtLimit(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	url := "https://cdn.example/" + runes(urlAtLimit-len("https://cdn.example/"), "あ")
	require.Equal(t, urlAtLimit, len([]rune(url)))
	body, err := json.Marshal(map[string]any{"name": "happy", "url": url})
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, doPost(h.EmojiAdd, string(body), adminUser).Code)
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, url, got.OriginalURL)
}

// 上限ちょうどの license を一括編集で通す (`set-category-bulk` と同じ理由で、
// 上限の取り違えをここで落とす)。
func TestEmojiSetLicenseBulk_AcceptsAtLimit(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
	lic := runes(licAtLimit, "い")
	body, err := json.Marshal(map[string]any{"ids": []string{"e1"}, "license": lic})
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, doPost(h.EmojiSetLicenseBulk, string(body), adminUser).Code)
	got := findEmojiByID(t, repo, "e1")
	require.NotNil(t, got.License)
	assert.Equal(t, lic, *got.License)
}

// `url` も NUL を通さない (長さと同じく SQLSTATE 22021 で 500 になる)。
func TestEmojiAdd_RejectsURLWithNUL(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	body, err := json.Marshal(map[string]any{"name": "happy", "url": "https://cdn.example/a\x00b.png"})
	require.NoError(t, err)

	rec := doPost(h.EmojiAdd, string(body), adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	_, ferr := repo.FindByNameAndHost("happy", nil)
	assert.Error(t, ferr)
}

// `url` の 400 も upstream の error を先取りしない (`category` と同じ規則)。
func TestEmojiAdd_URLCheckDoesNotPreemptDuplicate(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
	body, err := json.Marshal(map[string]any{
		"name": "happy", "url": "https://cdn.example/" + strings.Repeat("a", 512),
	})
	require.NoError(t, err)

	rec := doPost(h.EmojiAdd, string(body), adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "DUPLICATE_NAME", code, "upstream の error 順序が変わっている")
}

// 一括編集も NUL を通さない (呼び出し側だけ長さ判定に差し替える形を止める)。
func TestEmojiBulk_RejectsNUL(t *testing.T) {
	t.Run("set-category-bulk", func(t *testing.T) {
		h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
		body, err := json.Marshal(map[string]any{"ids": []string{"e1"}, "category": "a\x00b"})
		require.NoError(t, err)

		require.Equal(t, http.StatusBadRequest, doPost(h.EmojiSetCategoryBulk, string(body), adminUser).Code)
		assert.Nil(t, findEmojiByID(t, repo, "e1").Category)
	})

	t.Run("set-license-bulk", func(t *testing.T) {
		h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "happy"})
		body, err := json.Marshal(map[string]any{"ids": []string{"e1"}, "license": "a\x00b"})
		require.NoError(t, err)

		require.Equal(t, http.StatusBadRequest, doPost(h.EmojiSetLicenseBulk, string(body), adminUser).Code)
		assert.Nil(t, findEmojiByID(t, repo, "e1").License)
	})
}
