package admin_test

import (
	"fmt"
	"net/http"
	"testing"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `admin/emoji/update` が system 所有の複製を作る経路 (#3014) のテスト。
//
// 直している失敗形は `admin/emoji/add` (#2999) と同じ「**操作者が自分の drive
// ファイルを消すと絵文字が壊れる**」で、差し替え (`update`) だけが利用者所有の
// ファイルを指したまま残っていた。

// seedUpdatableEmoji returns a handler seeded with one local emoji that already
// points at a system-owned image (= 差し替え前の状態)。
func seedUpdatableEmoji(t *testing.T) (*apiadmin.Handler, *testutil.MockEmojiRepository) {
	t.Helper()
	return setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "happy",
		OriginalURL: "https://example/system/old.png",
		PublicURL:   "https://example/system/old.png",
	})
}

// userOwnedImage is the drive row `admin/emoji/update` normally receives:
// モデレーターが直前に上げた自分のファイル。
func userOwnedImage(t *testing.T, h *apiadmin.Handler) *testutil.MockDriveFileRepository {
	t.Helper()
	owner := "u1"
	return ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
}

// 利用者所有のファイルで差し替えると、絵文字は複製のほうを指す。
//
// **元のファイルの URL がどこにも残らないことまで見る。** originalUrl だけを見て
// いると、publicUrl が元ファイル側に残る実装 (= 元を消すと表示が壊れる) を通す。
func TestEmojiUpdate_FileIDCopiesToSystemOwnedFile(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	userOwnedImage(t, h)
	fetcher := &fakeEmojiImageFetcher{
		copyDF: systemCopy("sys1", "https://example/system/copy.png", "https://example/system/copy.webp", "image/webp"),
	}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Len(t, fetcher.copyCalls, 1, "利用者所有のファイルは複製しなければならない")
	assert.Equal(t, "f_img", fetcher.copyCalls[0].ID, "複製元は指定された drive ファイル")

	got := findEmojiByID(t, repo, "e1")
	// 不変条件 (#722): originalUrl は必ず drive_file.url と一致する。webpublic を
	// 入れると DeleteOrphans の guard が外れて複製が消える。
	assert.Equal(t, "https://example/system/copy.png", got.OriginalURL)
	assert.Equal(t, "https://example/system/copy.webp", got.PublicURL)
	require.NotNil(t, got.Type)
	assert.Equal(t, "image/webp", *got.Type)
	assert.NotContains(t, got.OriginalURL, "/user/", "元ファイルの URL を参照し続けてはならない")
	assert.NotContains(t, got.PublicURL, "/user/", "元ファイルの URL を参照し続けてはならない")
}

// 複製に渡す名前とセンシティブは**更新後の値**。同じリクエストで name /
// isSensitive を変えていればそちらが載るので、複製にも同じ値を渡す。
func TestEmojiUpdate_CopyUsesUpdatedNameAndSensitive(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantName      string
		wantSensitive bool
	}{
		{
			name:          "省略時は現在の絵文字の値",
			body:          `{"id":"e1","fileId":"f_img"}`,
			wantName:      "happy",
			wantSensitive: false,
		},
		{
			name:          "同じリクエストの新しい値",
			body:          `{"id":"e1","fileId":"f_img","name":"renamed","isSensitive":true}`,
			wantName:      "renamed",
			wantSensitive: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := seedUpdatableEmoji(t)
			userOwnedImage(t, h)
			fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys2", "https://example/system/copy.png", "", "")}
			h.SetEmojiImageFetcher(fetcher)

			rec := doPost(h.EmojiUpdate, tc.body, adminUser)
			require.Equal(t, http.StatusNoContent, rec.Code)
			require.Len(t, fetcher.copyNames, 1)
			assert.Equal(t, tc.wantName, fetcher.copyNames[0], "複製の名前は更新後の絵文字名")
			assert.Equal(t, []bool{tc.wantSensitive}, fetcher.copySensitive, "isSensitive を複製にも渡す")
		})
	}
}

// **差し替える前に指していたファイルは消さない。** 同じファイルを別の絵文字が
// 指している可能性があり (#2999 は「元が既に system 所有なら複製しない」)、
// `admin/emoji/delete` も drive ファイルは残して `admin/drive/cleanup` に任せる。
// 参照が外れたものは孤児 guard (`orphanWhere`) が回収する。
func TestEmojiUpdate_KeepsPreviousFile(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	dr := userOwnedImage(t, h)
	// 差し替え前の絵文字が指している system 所有のファイル。
	require.NoError(t, dr.Create(&model.DriveFile{
		ID: "f_old_sys", Type: "image/png", URL: "https://example/system/old.png",
	}))
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys3", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	assert.Empty(t, fetcher.deletedIDs, "差し替え前のファイルを消しに行っている (共有していると他の絵文字が壊れる)")
	old, err := dr.FindByID("f_old_sys")
	require.NoError(t, err, "差し替え前の drive ファイルが消えている")
	assert.Equal(t, "https://example/system/old.png", old.URL)
	// 絵文字自体は複製を指している (= 参照が外れたので cleanup の対象になる)。
	assert.Equal(t, "https://example/system/copy.png", findEmojiByID(t, repo, "e1").OriginalURL)
}

// 既に system 所有のファイルは複製しない (利用者の drive 操作から既に切れている)。
func TestEmojiUpdate_FileIDSkipsCopyForSystemOwnedFile(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	// **`size` は上限超過にしてある。** 複製しない経路では読み出しが無いので、
	// 上限の前捌きも効いてはいけない (効くと、複製済みの大きな絵文字を別の絵文字へ
	// 付け替えられなくなる)。
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_sys", Type: "image/png", URL: "https://example/system/already.png",
		Size: int(apiadmin.MaxEmojiCopyBytes) + 1,
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys4", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_sys"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, fetcher.copyCalls, "system 所有のファイルを二重に複製している")
	assert.Equal(t, "https://example/system/already.png", findEmojiByID(t, repo, "e1").OriginalURL)
}

// `userId` が NULL でも `userHost` があるリモートキャッシュは system 所有ではない。
// `admin/drive/clean-remote-files` で消えるので、複製しないと同じ形で壊れる。
func TestEmojiUpdate_FileIDCopiesRemoteCachedFile(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	host := "remote.example"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_remote", UserHost: &host, Type: "image/png", URL: "https://remote.example/cached.png",
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys5", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_remote"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, fetcher.copyCalls, 1, "リモートキャッシュは system 所有ではない")
	assert.Equal(t, "https://example/system/copy.png", findEmojiByID(t, repo, "e1").OriginalURL)
}

// 複製した実体の MIME が allowlist 外なら弾き、作った複製を片付ける。
// `Upload` は AnalyseFile でバイト列から型を引き直すので、行の宣言と実体が
// ずれていると allowlist 外の型が絵文字として登録される。
func TestEmojiUpdate_FileIDRejectsCopiedNonImageAndCleansUp(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	userOwnedImage(t, h)
	bad := systemCopy("sys6", "https://example/system/copy.bin", "", "")
	bad.Type = "text/plain"
	h.SetEmojiImageFetcher(&fakeEmojiImageFetcher{copyDF: bad})

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "UNSUPPORTED_FILE_TYPE", code)
	assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL,
		"弾いたのに絵文字が差し替わっている")
}

// 弾いた複製は誰からも参照されないので消す (孤児として残さない)。
func TestEmojiUpdate_FileIDRejectsCopiedNonImageDeletesCopy(t *testing.T) {
	h, _ := seedUpdatableEmoji(t)
	userOwnedImage(t, h)
	bad := systemCopy("sys7", "https://example/system/copy.bin", "", "")
	bad.Type = "text/plain"
	fetcher := &fakeEmojiImageFetcher{copyDF: bad}
	h.SetEmojiImageFetcher(fetcher)

	require.Equal(t, http.StatusBadRequest,
		doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser).Code)
	assert.Equal(t, []string{"sys7"}, fetcher.deletedIDs, "弾いた複製が孤児として残っている")
}

// 複製の失敗は種別ごとに分ける (#2792)。実体がもう無い / 大きすぎるは 400、
// ストレージや DB の障害は 500。error id は add / 承認経路と同じものを返す。
func TestEmojiUpdate_FileIDCopyFailureMapping(t *testing.T) {
	cases := []struct {
		name    string
		copyErr error
		status  int
		code    string
		id      string
	}{
		{
			name:    "object gone",
			copyErr: fmt.Errorf("read source file: %w", coredrive.ErrObjectNotFound),
			status:  http.StatusBadRequest,
			code:    "NO_SUCH_FILE",
			// **`update.ts` が宣言している id。** drive 行が無いときと同じ
			// endpoint・同じ code なので、`add` の id を共有すると 1 つの
			// endpoint が `NO_SUCH_FILE` に 2 つの id を返すことになる。
			id: "14fb9fd9-0731-4e2f-aeb9-f09e4740333d",
		},
		{
			name:    "too large",
			copyErr: fmt.Errorf("read source file: %w", safehttp.ErrResponseTooLarge),
			status:  http.StatusBadRequest,
			code:    "EMOJI_IMAGE_TOO_LARGE",
			id:      "6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64",
		},
		{
			name:    "storage failure",
			copyErr: assert.AnError,
			status:  http.StatusInternalServerError,
			code:    "INTERNAL_ERROR",
			id:      "c2f7a3d1-58be-4e09-bb26-0d4a9e7f3c15",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := seedUpdatableEmoji(t)
			userOwnedImage(t, h)
			fetcher := &fakeEmojiImageFetcher{copyErr: tc.copyErr}
			h.SetEmojiImageFetcher(fetcher)

			rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
			require.Equal(t, tc.status, rec.Code)
			code, id := errorID(t, rec.Body.Bytes())
			assert.Equal(t, tc.code, code)
			assert.Equal(t, tc.id, id, "error id が endpoint の宣言とずれている")
			// **元ファイルを指す絵文字にしない。** すると直そうとしている依存を
			// そのまま作ることになる。差し替え前の画像は生きているので壊れない。
			assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL,
				"複製に失敗したのに絵文字が差し替わっている")
			// 複製できていないので消しに行く相手もいない。
			assert.Empty(t, fetcher.deletedIDs, "作っていない複製を消しに行っている")
		})
	}
}

// **検証をすべて通す前に複製しない。** 先に複製すると、重複名や MIME で弾く分まで
// 実体を作って捨てることになり、誰からも参照されない孤児が残る。`DeleteOrphans` は
// `admin/drive/cleanup` からしか走らないので、掃除するまで実体ストレージを食い続ける。
func TestEmojiUpdate_DoesNotCopyBeforeValidation(t *testing.T) {
	newHandler := func(t *testing.T, mime string, seed ...*model.Emoji) (*apiadmin.Handler, *fakeEmojiImageFetcher) {
		t.Helper()
		all := append([]*model.Emoji{{
			ID: "e1", Name: "happy",
			OriginalURL: "https://example/system/old.png",
			PublicURL:   "https://example/system/old.png",
		}}, seed...)
		h, _ := setupEmojiHandler(t, all...)
		owner := "u1"
		ownedDriveFile(t, h, &model.DriveFile{
			ID: "f_img", UserID: &owner, Type: mime, URL: "https://example/user/orig.png",
		})
		fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys_x", "https://example/system/copy.png", "", "")}
		h.SetEmojiImageFetcher(fetcher)
		return h, fetcher
	}

	t.Run("同名の別絵文字があるリネームは複製する前に弾く", func(t *testing.T) {
		h, fetcher := newHandler(t, "image/png", &model.Emoji{ID: "e2", Name: "taken"})
		rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img","name":"taken"}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "SAME_NAME_EMOJI_EXISTS", code)
		assert.Empty(t, fetcher.copyCalls, "重複で弾くのに複製を作っている (孤児になる)")
	})

	t.Run("名前が規則に合わないリネームは複製する前に弾く", func(t *testing.T) {
		h, fetcher := newHandler(t, "image/png")
		rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img","name":"bad name"}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "INVALID_PARAM", code)
		assert.Empty(t, fetcher.copyCalls, "名前で弾くのに複製を作っている (孤児になる)")
	})

	t.Run("非画像は複製する前に弾く", func(t *testing.T) {
		h, fetcher := newHandler(t, "text/plain")
		rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "UNSUPPORTED_FILE_TYPE", code)
		assert.Empty(t, fetcher.copyCalls, "MIME で弾くのに複製を作っている (孤児になる)")
	})

	t.Run("絵文字が無ければ複製する前に弾く", func(t *testing.T) {
		h, fetcher := newHandler(t, "image/png")
		rec := doPost(h.EmojiUpdate, `{"id":"missing","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Empty(t, fetcher.copyCalls, "更新先が無いのに複製を作っている (孤児になる)")
	})

	t.Run("存在しない fileId は NO_SUCH_FILE が先", func(t *testing.T) {
		h, fetcher := newHandler(t, "image/png")
		rec := doPost(h.EmojiUpdate, `{"id":"missing","fileId":"nope"}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, id := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "NO_SUCH_FILE", code, "upstream の error 順序が変わっている (#1772)")
		assert.Equal(t, "14fb9fd9-0731-4e2f-aeb9-f09e4740333d", id)
		assert.Empty(t, fetcher.copyCalls)
	})
}

// 行の `size` が複製の上限を超えていたら、実体を読む前に断る。
// **`size` は前捌きで、上限の権威ではない** — 実体とずれうるので、読み出し側の
// 上限 (safehttp.ErrResponseTooLarge) は別に効いている (上のケース表)。
func TestEmojiUpdate_FileIDRejectsOversizedRowBeforeReading(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_big", UserID: &owner, Type: "image/png", URL: "https://example/user/big.png",
		Size: int(apiadmin.MaxEmojiCopyBytes) + 1,
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys8", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_big"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "EMOJI_IMAGE_TOO_LARGE", code)
	assert.Equal(t, "6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64", id, "add / 承認と同じ error id を返す")
	assert.Empty(t, fetcher.copyCalls, "上限超過が分かっているのに実体を読んでいる")
	assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL)
}

// 上限ちょうどは通す (境界を off-by-one で締めない)。
func TestEmojiUpdate_FileIDAcceptsRowAtSizeLimit(t *testing.T) {
	h, _ := seedUpdatableEmoji(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_max", UserID: &owner, Type: "image/png", URL: "https://example/user/max.png",
		Size: int(apiadmin.MaxEmojiCopyBytes),
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys9", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_max"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, fetcher.copyCalls, 1)
}

// fetcher が nil, nil を返しても panic させない (実装の契約違反を 500 に落とす)。
func TestEmojiUpdate_FileIDCopyReturnsNoFile(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	userOwnedImage(t, h)
	h.SetEmojiImageFetcher(&fakeEmojiImageFetcher{})

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL)
}

// 更新先の行がもう無ければ複製を片付ける。絵文字は差し替え前の URL を指したままで、
// 作った複製は誰からも参照されない孤児になる。
func TestEmojiUpdate_RowGoneDeletesSystemCopy(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	// `UpdateFields` は RowsAffected==0 を not-found へ昇格する (#650 問題 2)。
	repo.UpdateErr = testutil.ErrNotFound
	userOwnedImage(t, h)
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys10", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, []string{"sys10"}, fetcher.deletedIDs, "更新先が消えたのに複製が孤児として残っている")
}

// **更新が載ったかを読み直して複製の行方を決める。** 後始末バッチ (#2990) の
// `finishFailedUpdate` と同じ 3 分岐。
//
// **あわせて DB 障害を not-found に丸めない (#2792)。** 接続断が「そんな絵文字は
// 無い」に化けると、クライアントからは区別できず監視でも 5xx が立たない。
func TestEmojiUpdate_UpdateFailureDecidesCopyByReread(t *testing.T) {
	// (1) 載っていない失敗は複製を消す。**DB 障害だけの話ではない** — 列幅超過
	// (SQLSTATE 22001。`category` は varchar(128) で `EmojiUpdate` に長さ検査が
	// 無い) や一意制約違反のように、**モデレーターの入力で確定的に踏める**形が
	// ある。`admin/drive/cleanup` は手動でしか走らないので、残すと掃除するまで
	// 実体を食う。
	t.Run("載っていなければ消す", func(t *testing.T) {
		h, repo := seedUpdatableEmoji(t)
		repo.UpdateErr = assert.AnError
		userOwnedImage(t, h)
		fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys11", "https://example/system/copy.png", "", "")}
		h.SetEmojiImageFetcher(fetcher)

		rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, []string{"sys11"}, fetcher.deletedIDs, "載らなかった複製が孤児として残っている")
		assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL)
	})

	// (2) 載っていたら消さない。PostgreSQL は COMMIT を送った後・ack が届く前に
	// 接続が切れるとエラーを返すが、**更新のほうは載っている**。そこで消すと絵文字が
	// 存在しないファイルを指し、画像が恒久的に 404 になる (差し替え前の画像はもう
	// 参照されていないので戻らない)。
	t.Run("載っていたら消さない", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		inner := testutil.NewMockEmojiRepository()
		require.NoError(t, inner.Create(&model.Emoji{
			ID: "e1", Name: "happy",
			OriginalURL: "https://example/system/old.png",
			PublicURL:   "https://example/system/old.png",
		}))
		h.SetEmojiRepo(&landedThenFailingEmojiRepo{MockEmojiRepository: inner})
		userOwnedImage(t, h)
		// **複製に webpublic を持たせる。** 持たせないと `originalUrl` と
		// `publicUrl` が同じ値になり、「載ったか」の判定がどちらを見ているか
		// 区別できない (#722 の不変条件は `originalUrl` 側)。
		fetcher := &fakeEmojiImageFetcher{
			copyDF: systemCopy("sys12", "https://example/system/copy.png", "https://example/system/copy.webp", "image/webp"),
		}
		h.SetEmojiImageFetcher(fetcher)

		rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Empty(t, fetcher.deletedIDs,
			"載っていた複製を消している (絵文字が存在しないファイルを指す)")
		after := findEmojiByID(t, inner, "e1")
		assert.Equal(t, "https://example/system/copy.png", after.OriginalURL)
		assert.Equal(t, "https://example/system/copy.webp", after.PublicURL)
	})

	// (3) 読み直せなければ残す。**参照されている複製を消すほうが、参照されない
	// 複製を残すより悪い** — 後者は孤児 cleanup が回収する。
	t.Run("読み直せなければ残す", func(t *testing.T) {
		h, fetcher := newRereadFailureHandler(t, assert.AnError, "sys13")

		rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Empty(t, fetcher.deletedIDs, "載ったか分からない複製を消している")
	})

	// (4) 行がもう無ければ消す。**「読み直せない」と混ぜない** — not-found は
	// 「載っていない」ことが確定した状態で、複製は誰からも参照されない。
	t.Run("行が消えていたら消す", func(t *testing.T) {
		h, fetcher := newRereadFailureHandler(t, testutil.ErrNotFound, "sys14")

		rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, []string{"sys14"}, fetcher.deletedIDs, "参照する行が消えたのに複製が残っている")
	})
}

// newRereadFailureHandler builds a handler whose update fails and whose re-read
// then returns rereadErr.
func newRereadFailureHandler(t *testing.T, rereadErr error, copyID string) (*apiadmin.Handler, *fakeEmojiImageFetcher) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	inner := testutil.NewMockEmojiRepository()
	require.NoError(t, inner.Create(&model.Emoji{
		ID: "e1", Name: "happy",
		OriginalURL: "https://example/system/old.png",
		PublicURL:   "https://example/system/old.png",
	}))
	h.SetEmojiRepo(&failingRereadEmojiRepo{MockEmojiRepository: inner, rereadErr: rereadErr})
	userOwnedImage(t, h)
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy(copyID, "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)
	return h, fetcher
}

// landedThenFailingEmojiRepo applies the update and then reports a failure —
// COMMIT の後・ack の前に接続が切れた形。
type landedThenFailingEmojiRepo struct {
	*testutil.MockEmojiRepository
}

func (r *landedThenFailingEmojiRepo) UpdateFields(id string, fields map[string]any) error {
	if err := r.MockEmojiRepository.UpdateFields(id, fields); err != nil {
		return err
	}
	return assert.AnError
}

// failingRereadEmojiRepo fails the update and then returns rereadErr from the
// re-read。**1 回目の `FindByID` だけ通す** — あれはハンドラが更新前の絵文字を
// 解決するためのもので、そこで落とすと複製を作る前に返ってしまう。
type failingRereadEmojiRepo struct {
	*testutil.MockEmojiRepository
	rereadErr error
	finds     int
}

func (r *failingRereadEmojiRepo) UpdateFields(_ string, _ map[string]any) error {
	return assert.AnError
}

func (r *failingRereadEmojiRepo) FindByID(id string) (*model.Emoji, error) {
	r.finds++
	if r.finds > 1 {
		return nil, r.rereadErr
	}
	return r.MockEmojiRepository.FindByID(id)
}

// **drive ファイルの lookup が DB 障害なら 500 (#2792)。** not-found に丸めると、
// 接続断のあいだ「そんなファイルは無い」が返り、原因から遠い症状になる。
func TestEmojiUpdate_DriveLookupFailureReturns500(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	h.SetDriveFileRepo(&failingFindDriveFileRepo{testutil.NewMockDriveFileRepository()})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys15", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, fetcher.copyCalls, "ファイルを引けていないのに複製を作っている")
	assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL)
}

// **リネーム時の重複チェックを DB 障害で skip しない (#2792)。** 倒すと接続断の
// あいだ同名の絵文字を作れてしまい、しかも複製まで作ることになる
// (`admin/emoji/add` の同じ経路は #2999 で塞いである)。
func TestEmojiUpdate_DuplicateLookupFailureReturns500(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockEmojiRepository()
	require.NoError(t, repo.Create(&model.Emoji{
		ID: "e1", Name: "happy",
		OriginalURL: "https://example/system/old.png",
		PublicURL:   "https://example/system/old.png",
	}))
	h.SetEmojiRepo(&failingFindByNameEmojiRepo{repo})
	userOwnedImage(t, h)
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys16", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img","name":"renamed"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, fetcher.copyCalls, "重複か分からないのに複製を作っている")
	assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL)
}

// failingFindDriveFileRepo fails the drive lookup with a non not-found error
// (= DB 障害)。
type failingFindDriveFileRepo struct {
	*testutil.MockDriveFileRepository
}

func (f *failingFindDriveFileRepo) FindByID(_ string) (*model.DriveFile, error) {
	return nil, assert.AnError
}

// fetcher 未配線なら drive の URL をそのまま使う (EmojiAdd と同じ degradation)。
// 未配線そのものは router の起動時チェックが知らせる。
func TestEmojiUpdate_FetcherUnsetKeepsDriveURL(t *testing.T) {
	h, repo := seedUpdatableEmoji(t)
	owner := "u1"
	// 未配線でも読み出しは無いので、上限の前捌きは効かない。
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
		Size: int(apiadmin.MaxEmojiCopyBytes) + 1,
	})

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "https://example/user/orig.png", findEmojiByID(t, repo, "e1").OriginalURL)
}
