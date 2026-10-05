package testutil

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// TestMockDriveFileRepository_MatchesProductionPredicates は #2747 で追加した、
// mock と production (internal/repository/drive_file.go) の選択条件のずれを
// 固定するテスト。
//
// #2744 と同じ型の drift が 2 系統残っていた。
//
//   - 述語そのものがずれていたもの: FindByAccessKey / FindByName (file) /
//     FindByName (folder) / ListForAdmin / ListSystemFiles
//   - 多重一致時の戻りが非決定だったもの: FindByAnyURL / FindByAnyAccessKey /
//     FindByURI (production の First() は ORDER BY id で最小 ID を返す)
//
// **既存テストが誤検証していたわけではない。** 修正前後で挙動が変わる
// fixture を持つ既存テストは fileType 経路の 2 件だけで (本 PR で
// `image/*` に直した)、access key の 2 メソッドはそもそも mock 経由で
// 呼ばれていなかった。残りは将来の誤用に対する予防。
func TestMockDriveFileRepository_MatchesProductionPredicates(t *testing.T) {
	ptr := func(s string) *string { return &s }

	t.Run("FindByAccessKey matches the primary key only", func(t *testing.T) {
		// production は `"accessKey" = ?` 単独。thumbnail / webpublic の OR は
		// 呼び出し側が primary 一致でしか swap しないため dead clause として
		// 削除済み (#637 review UR-014)。
		repo := NewMockDriveFileRepository()
		repo.Files["f1"] = &model.DriveFile{
			ID:                 "f1",
			AccessKey:          ptr("primary"),
			ThumbnailAccessKey: ptr("thumb"),
			WebpublicAccessKey: ptr("webpub"),
		}

		got, err := repo.FindByAccessKey("primary")
		require.NoError(t, err)
		assert.Equal(t, "f1", got.ID)

		_, err = repo.FindByAccessKey("thumb")
		assert.ErrorIs(t, err, ErrNotFound, "thumbnail key は primary 経路では引けない")
		_, err = repo.FindByAccessKey("webpub")
		assert.ErrorIs(t, err, ErrNotFound, "webpublic key は primary 経路では引けない")
	})

	t.Run("FindByAnyAccessKey matches all three columns", func(t *testing.T) {
		// production はこちらが 3 列 OR。2 経路の差を mock でも表現する (#1414)。
		repo := NewMockDriveFileRepository()
		repo.Files["f1"] = &model.DriveFile{
			ID:                 "f1",
			AccessKey:          ptr("primary"),
			ThumbnailAccessKey: ptr("thumb"),
			WebpublicAccessKey: ptr("webpub"),
		}

		for _, key := range []string{"primary", "thumb", "webpub"} {
			got, err := repo.FindByAnyAccessKey(key)
			require.NoError(t, err, "key=%s", key)
			assert.Equal(t, "f1", got.ID, "key=%s", key)
		}
		// 空文字ガードの検証には**空文字のキーを持つ fixture**が要る。
		// 非空キーだけだとガードを外してもループが何も掴まず ErrNotFound に
		// なり、テストが空振りする。
		blank := NewMockDriveFileRepository()
		blank.Files["blank"] = &model.DriveFile{ID: "blank", AccessKey: ptr(""), ThumbnailAccessKey: ptr("")}
		_, err := blank.FindByAnyAccessKey("")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("FindByName scopes to the folder", func(t *testing.T) {
		// production は folderID が nil なら `"folderId" IS NULL`。
		// 「どのフォルダでもよい」ではない。
		repo := NewMockDriveFileRepository()
		u := "u1"
		folder := "fld1"
		repo.Files["root"] = &model.DriveFile{ID: "root", UserID: &u, Name: "a.png"}
		repo.Files["infolder"] = &model.DriveFile{ID: "infolder", UserID: &u, Name: "a.png", FolderID: &folder}

		got, err := repo.FindByName(u, "a.png", nil)
		require.NoError(t, err)
		require.Len(t, got, 1, "folderID nil は root 直下のみ")
		assert.Equal(t, "root", got[0].ID)

		got, err = repo.FindByName(u, "a.png", &folder)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "infolder", got[0].ID)
	})

	t.Run("FindByAnyURL rejects the empty string", func(t *testing.T) {
		// production は空文字で即 ErrRecordNotFound。ガードが無いと URL 未設定の
		// fixture が map 反復順で任意に 1 件返る (テストが偶発的に通る/落ちる)。
		repo := NewMockDriveFileRepository()
		repo.Files["f1"] = &model.DriveFile{ID: "f1"}

		_, err := repo.FindByAnyURL("")
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

// TestMatchesDriveFileType は fileType 述語が production と同じ形であることを
// 固定する (#1772)。
//
// 無条件 prefix だと `image/*` がどこにも当たらず (`image/*` で始まる MIME は
// 無い)、逆に完全一致のつもりの指定がより長いサブタイプまで拾う。
// どちらも本番と逆に振れる。
func TestMatchesDriveFileType(t *testing.T) {
	cases := []struct {
		name     string
		actual   string
		fileType string
		want     bool
	}{
		{"empty filter matches anything", "image/png", "", true},
		{"wildcard matches the prefix", "image/png", "image/*", true},
		{"wildcard rejects another prefix", "video/mp4", "image/*", false},
		{"exact matches", "image/png", "image/png", true},
		{"exact rejects a longer subtype", "image/heic-sequence", "image/heic", false},
		{"trailing slash is not a wildcard", "image/png", "image/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, matchesDriveFileType(tc.actual, tc.fileType))
		})
	}
}

// TestMockDriveFileRepository_ListFileTypeSemantics は上の述語が実際に
// ListForAdmin / ListSystemFiles / ListByUser の 3 経路で使われていることを
// 固定する。#2747 以前は前 2 者だけが無条件 prefix で取り残されていた。
func TestMockDriveFileRepository_ListFileTypeSemantics(t *testing.T) {
	u := "u1"
	newRepo := func() *MockDriveFileRepository {
		repo := NewMockDriveFileRepository()
		repo.Files["img"] = &model.DriveFile{ID: "img", UserID: &u, Type: "image/png"}
		repo.Files["vid"] = &model.DriveFile{ID: "vid", UserID: &u, Type: "video/mp4"}
		repo.Files["sys"] = &model.DriveFile{ID: "sys", Type: "image/webp"}
		return repo
	}

	t.Run("ListForAdmin", func(t *testing.T) {
		rows, err := newRepo().ListForAdmin(u, "", "", "image/*", "", "", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "img", rows[0].ID)

		rows, err = newRepo().ListForAdmin(u, "", "", "image/", "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, rows, "`/*` の無い指定は完全一致")
	})

	t.Run("ListSystemFiles", func(t *testing.T) {
		rows, err := newRepo().ListSystemFiles("image/*", "", "", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "sys", rows[0].ID)

		rows, err = newRepo().ListSystemFiles("image/", "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, rows, "`/*` の無い指定は完全一致")
	})

	t.Run("ListByUser", func(t *testing.T) {
		rows, err := newRepo().ListByUser(u, nil, true, "image/*", "", "", "", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "img", rows[0].ID)

		rows, err = newRepo().ListByUser(u, nil, true, "image/", "", "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, rows, "`/*` の無い指定は完全一致")
	})

	// anyFolder=false のとき folderID nil が root 直下に限定されること
	// (matchesDriveFolder を ListByUser 経由でも踏む)。
	t.Run("ListByUser scopes to the folder", func(t *testing.T) {
		repo := newRepo()
		folder := "fld1"
		repo.Files["infolder"] = &model.DriveFile{ID: "infolder", UserID: &u, Type: "image/png", FolderID: &folder}

		rows, err := repo.ListByUser(u, nil, false, "", "", "", "", 10)
		require.NoError(t, err)
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		assert.NotContains(t, ids, "infolder", "folderID nil は root 直下のみ")
	})
}

// TestMockDriveFolderRepository_FindByNameScopesToParent は #2747 のレビューで
// 見つかった、file 側と同型の drift を固定する。
//
// production (internal/repository/drive_folder.go) は parentID が nil なら
// `"parentId" IS NULL`。mock は第 3 引数を捨てていたので、root と配下に同名
// フォルダがあると本番 1 件・mock 2 件になっていた。
func TestMockDriveFolderRepository_FindByNameScopesToParent(t *testing.T) {
	repo := NewMockDriveFolderRepository()
	u := "u1"
	parent := "p1"
	repo.Folders["root"] = &model.DriveFolder{ID: "root", UserID: &u, Name: "docs"}
	repo.Folders["nested"] = &model.DriveFolder{ID: "nested", UserID: &u, Name: "docs", ParentID: &parent}

	got, err := repo.FindByName(u, "docs", nil)
	require.NoError(t, err)
	require.Len(t, got, 1, "parentID nil は root 直下のみ")
	assert.Equal(t, "root", got[0].ID)

	got, err = repo.FindByName(u, "docs", &parent)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "nested", got[0].ID)
}

// TestMockDriveFileRepository_MultiMatchIsDeterministic は多重一致時の戻りが
// production と同じく最小 ID になることを固定する。
//
// production の `First()` は `ORDER BY id` を付けるので決定的だが、mock は
// map を回すので反復順に依存していた。同 fixture で実行ごとに別のファイルを
// 返すと、所有者判定の対象が揺れる。FindByMD5 は既に決定化してある。
func TestMockDriveFileRepository_MultiMatchIsDeterministic(t *testing.T) {
	ptr := func(s string) *string { return &s }
	const shared = "https://media.example/x.webp"

	t.Run("FindByAnyURL", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			repo := NewMockDriveFileRepository()
			repo.Files["b"] = &model.DriveFile{ID: "b", URL: shared}
			repo.Files["a"] = &model.DriveFile{ID: "a", ThumbnailURL: ptr(shared)}

			got, err := repo.FindByAnyURL(shared)
			require.NoError(t, err)
			require.Equal(t, "a", got.ID, "最小 ID を返す (production の ORDER BY id)")
		}
	})

	// uri の index は非 unique (migration/000040) なので多重一致は production
	// でも起きる。呼び出し回数は mock 経由で最多 (federation の dedup 経路)。
	t.Run("FindByURI", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			repo := NewMockDriveFileRepository()
			repo.Files["b"] = &model.DriveFile{ID: "b", URI: ptr("https://remote/o/1")}
			repo.Files["a"] = &model.DriveFile{ID: "a", URI: ptr("https://remote/o/1")}

			got, err := repo.FindByURI("https://remote/o/1")
			require.NoError(t, err)
			require.Equal(t, "a", got.ID, "最小 ID を返す (production の ORDER BY id)")
		}
	})

	t.Run("FindByAnyAccessKey", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			repo := NewMockDriveFileRepository()
			repo.Files["b"] = &model.DriveFile{ID: "b", AccessKey: ptr("k")}
			repo.Files["a"] = &model.DriveFile{ID: "a", ThumbnailAccessKey: ptr("k")}

			got, err := repo.FindByAnyAccessKey("k")
			require.NoError(t, err)
			require.Equal(t, "a", got.ID, "最小 ID を返す (production の ORDER BY id)")
		}
	})
}

// TestMockDriveFileRepository_RemainingDivergences は #2755 で解消した乖離を
// 固定する。issue が挙げた 4 件と、実装中に見つかった folder 側の同型
// (FindByIDs の重複) が対象。いずれも #2747 のテーマ (述語と多重一致時の
// 決定性) と独立していたため別 issue にしたもの。
func TestMockDriveFileRepository_RemainingDivergences(t *testing.T) {
	ptr := func(s string) *string { return &s }

	// production の `id IN ?` は重複した id を渡しても行を 1 度しか返さない。
	// **到達可能な乖離だった** — AP renderer の addAttachments は戻り行をその
	// まま `attachment` に並べるので、重複 id で Document が二重になる。
	// (entity の pack は `n.FileIDs` 駆動なので、そちらは production でも
	// 重複する。#2755 の issue 本文にあった「pack が二重になる」は誤り。)
	t.Run("FindByIDs deduplicates the input", func(t *testing.T) {
		m := NewMockDriveFileRepository()
		m.Files["f1"] = &model.DriveFile{ID: "f1"}
		m.Files["f2"] = &model.DriveFile{ID: "f2"}

		got, err := m.FindByIDs([]string{"f1", "f1", "f2", "f1"})
		require.NoError(t, err)
		require.Len(t, got, 2, "重複した id でも行は 1 度だけ")
		// **順序は assert しない。** production の `Find` に ORDER BY は無く
		// 戻り順は不定 (federation/resolver.go も「戻り順は不定なので map で
		// 再整列する」と書いている)。ここで入力順を固定すると、mock でしか
		// 成り立たない前提をテストが承認することになる。
		assert.ElementsMatch(t, []string{"f1", "f2"}, []string{got[0].ID, got[1].ID})

		// 存在しない id は黙って落ちる (production の IN も同じ)。
		got, err = m.FindByIDs([]string{"missing", "missing"})
		require.NoError(t, err)
		assert.Empty(t, got)

		// **folder 側も同じ。** doc が「IN を模す」と言っている以上、片方だけ
		// 重複を返すと主張が偽になる (production の drive_folder も `id IN ?`)。
		mf := NewMockDriveFolderRepository()
		mf.Folders["d1"] = &model.DriveFolder{ID: "d1"}
		mf.Folders["d2"] = &model.DriveFolder{ID: "d2"}
		folders, err := mf.FindByIDs([]string{"d1", "d1", "d2", "d1"})
		require.NoError(t, err)
		require.Len(t, folders, 2, "folder 側も重複した id で行は 1 度だけ")
		assert.ElementsMatch(t, []string{"d1", "d2"}, []string{folders[0].ID, folders[1].ID})
	})

	// production は limit > 100 を 100 に丸める。handler は
	// pagination.ResolveLimit が 100 超を **400 で弾く** (丸めない) ので
	// endpoint 経由では repo に届かないが、mock を直に叩くテストが production
	// では返らない件数を受け取れてしまう。
	t.Run("admin list limits are clamped at 100", func(t *testing.T) {
		m := NewMockDriveFileRepository()
		host := "remote.example"
		for i := 0; i < 150; i++ {
			id := "f" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			m.Files[id] = &model.DriveFile{ID: id, UserHost: &host}
		}
		rows, err := m.ListForAdmin("", "remote", "", "", "", "", 999)
		require.NoError(t, err)
		assert.Len(t, rows, 100, "ListForAdmin は 100 で頭打ち")

		m2 := NewMockDriveFileRepository()
		for i := 0; i < 150; i++ {
			id := "s" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			m2.Files[id] = &model.DriveFile{ID: id}
		}
		rows, err = m2.ListSystemFiles("", "", "", 999)
		require.NoError(t, err)
		assert.Len(t, rows, 100, "ListSystemFiles は 100 で頭打ち")
	})

	// production は folderId を実際に更新する。記録するだけだと「移動後に
	// 読み直す」テストが書けない。**userID の絞り込みが IDOR guard の本体**で、
	// 記録 (BulkFolderUserID) はその補助にすぎない。
	t.Run("UpdateBulkFolder applies the move, scoped to the owner", func(t *testing.T) {
		m := NewMockDriveFileRepository()
		m.Files["mine"] = &model.DriveFile{ID: "mine", UserID: ptr("u1")}
		m.Files["theirs"] = &model.DriveFile{ID: "theirs", UserID: ptr("u2")}
		m.Files["orphan"] = &model.DriveFile{ID: "orphan"}

		// **mock は folder の実在を見ない。** production の `drive_file.folderId`
		// は `drive_folder` への FK なので、実在しない id では FK 違反になる。
		// endpoint 経由では handler が所有権付きで存在確認するので到達しない。
		require.NoError(t, m.UpdateBulkFolder("u1", []string{"mine", "theirs", "orphan"}, ptr("fold1")))

		require.NotNil(t, m.Files["mine"].FolderID)
		assert.Equal(t, "fold1", *m.Files["mine"].FolderID, "自分の file は移動する")
		assert.Nil(t, m.Files["theirs"].FolderID, "他人の file は動かない (IDOR guard)")
		assert.Nil(t, m.Files["orphan"].FolderID, "owner 無しの行も動かない")

		// nil folderId (= ルートへ戻す) も通ること。
		require.NoError(t, m.UpdateBulkFolder("u1", []string{"mine"}, nil))
		assert.Nil(t, m.Files["mine"].FolderID)
	})

	// map 反復 + unstable sort だと同値行の並びが run ごとに変わる。
	// **production は同値行の順序を保証しない**ので、これは再現性のための
	// 決定化であって「この順序が正しい」という主張ではない。
	t.Run("equal sort keys are ordered deterministically", func(t *testing.T) {
		build := func() *MockDriveFileRepository {
			m := NewMockDriveFileRepository()
			for _, id := range []string{"f1", "f2", "f3", "f4", "f5"} {
				m.Files[id] = &model.DriveFile{ID: id, UserID: ptr("u1"), Name: "same", Size: 10}
			}
			return m
		}
		ids := func(rows []*model.DriveFile) []string {
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, r.ID)
			}
			return out
		}
		// **limit は実値を渡す。** mock の `limit <= 0` は「無制限」だが
		// production は `limit == 0` のとき `LIMIT 0` = 0 行になる
		// (mock_drive.go の ListByUser の doc 参照)。0 で呼ぶと、その乖離を
		// 将来揃えた瞬間にこのテストが空の slice 同士を比べる空振りに化ける。
		for _, sortKey := range []string{"+name", "-name", "+size", "-size"} {
			var first []string
			for i := 0; i < 20; i++ {
				rows, err := build().ListByUser("u1", nil, true, "", sortKey, "", "", 10)
				require.NoError(t, err)
				require.Len(t, rows, 5, "5 件すべて返ること (空同士の比較で空振りしない)")
				got := ids(rows)
				if first == nil {
					first = got
					continue
				}
				require.Equal(t, first, got, "sort=%s で同値行の並びが run ごとに変わる", sortKey)
			}
		}
	})

	// **sort 方向は production の order 句と 1 対 1 で突き合わせる。** 同値行
	// だけの fixture では分岐は通るが比較結果が観測されないので、値を散らす。
	// production: +createdAt→id DESC / -createdAt→id ASC / +name→name DESC /
	// -name→name ASC / +size→size DESC / -size→size ASC。
	t.Run("sort directions match production", func(t *testing.T) {
		// **name と size を逆相関にしないこと。** name 昇順 = size 降順の
		// fixture だと `-name` と `+size` が同じ期待値になり、**ソートキーの
		// 取り違えを観測できない** (方向の反転は殺せるので気付きにくい)。
		m := NewMockDriveFileRepository()
		m.Files["f1"] = &model.DriveFile{ID: "f1", UserID: ptr("u1"), Name: "a", Size: 10}
		m.Files["f2"] = &model.DriveFile{ID: "f2", UserID: ptr("u1"), Name: "b", Size: 30}
		m.Files["f3"] = &model.DriveFile{ID: "f3", UserID: ptr("u1"), Name: "c", Size: 20}
		ids := func(rows []*model.DriveFile) []string {
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, r.ID)
			}
			return out
		}
		cases := map[string][]string{
			"+createdAt": {"f3", "f2", "f1"}, // id DESC
			"-createdAt": {"f1", "f2", "f3"}, // id ASC
			"+name":      {"f3", "f2", "f1"}, // name DESC
			"-name":      {"f1", "f2", "f3"}, // name ASC
			"+size":      {"f2", "f3", "f1"}, // size DESC (30, 20, 10)
			"-size":      {"f1", "f3", "f2"}, // size ASC
			"":           {"f3", "f2", "f1"}, // 未指定は id DESC
		}
		for sortKey, want := range cases {
			rows, err := m.ListByUser("u1", nil, true, "", sortKey, "", "", 10)
			require.NoError(t, err)
			assert.Equal(t, want, ids(rows), "sort=%q", sortKey)
		}
	})

	// tie は id 昇順に落ちること (安定ソートであることの mock 内部契約)。
	// **production はこの順序を保証しない**ので、他のテストがこれに依存しては
	// いけない。ここで固定するのは mock の再現性そのもの。
	t.Run("ties fall back to id ascending", func(t *testing.T) {
		// **tie グループを 2 つ以上、13 件以上置く。** 全部同名 (= tie が 1 group)
		// だと比較関数が常に false を返し、pdqsort が挿入ソート経路で順序を
		// 保つので、安定ソートを外す変異を殺せない (40 件まで試して素通りした)。
		// 2 group × 13 件で実際に崩れることを確認済み。
		m := NewMockDriveFileRepository()
		var wantN1, wantN0 []string
		for i := 0; i < 13; i++ {
			id := fmt.Sprintf("f%02d", i)
			name := fmt.Sprintf("n%d", i%2)
			m.Files[id] = &model.DriveFile{ID: id, UserID: ptr("u1"), Name: name, Size: 1}
			if i%2 == 1 {
				wantN1 = append(wantN1, id) // name="n1" が先 (+name は name DESC)
			} else {
				wantN0 = append(wantN0, id)
			}
		}
		rows, err := m.ListByUser("u1", nil, true, "", "+name", "", "", 20)
		require.NoError(t, err)
		require.Len(t, rows, 13)
		got := make([]string, 0, len(rows))
		for _, r := range rows {
			got = append(got, r.ID)
		}
		assert.Equal(t, append(append([]string{}, wantN1...), wantN0...), got,
			"name グループ内は id 昇順 (= 安定ソート)")
	})

	// limit 未指定 (<= 0) の既定値は production と同じ 30。
	// **30 は repo 層の既定。** endpoint の既定は 10 (`ResolveLimit(req.Limit,
	// 10, 100)`) なので、handler 経由でこの分岐には入らない。
	t.Run("admin list default limit is 30", func(t *testing.T) {
		m := NewMockDriveFileRepository()
		for i := 0; i < 40; i++ {
			id := "d" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			m.Files[id] = &model.DriveFile{ID: id}
		}
		rows, err := m.ListSystemFiles("", "", "", 0)
		require.NoError(t, err)
		assert.Len(t, rows, 30, "limit <= 0 は 30 に倒す")

		host := "remote.example"
		m2 := NewMockDriveFileRepository()
		for i := 0; i < 40; i++ {
			id := "e" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			m2.Files[id] = &model.DriveFile{ID: id, UserHost: &host}
		}
		rows, err = m2.ListForAdmin("", "remote", "", "", "", "", 0)
		require.NoError(t, err)
		assert.Len(t, rows, 30)
	})
}
