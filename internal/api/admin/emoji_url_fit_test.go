package admin_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 保存先の URL が `emoji.originalUrl` / `publicUrl` に入らないときに 400 で返す
// (#3023)。
//
// `drive_file.url` は varchar(1024) だが `emoji` 側は varchar(512)。保存先の URL は
// `objectStorageBaseUrl` + prefix + `accessKey` (varchar(256)) で決まるので、長い
// prefix のオブジェクトストレージ構成では超える。通すと `Create` / `UpdateFields` が
// SQLSTATE 22001 で落ち、**操作者には直しようのない 5xx** になっていた。
//
// **列は今のところ広げない** — upstream も 512。広げない理由はもともと復路 (広げた後に
// 入った値は narrow できないので down が書けない) で、復路を保証しなくなった (#3191)
// 今は将来の選択肢になる。**URL は切らない** — 切った URL は別物 (#3018 と同じ判断)。
//
// **これで 5xx が全部消えるわけではない** — 射程は `internal/api/admin/emoji.go` の
// `emojiFileURLFits` の doc に書いてある (`drive_file` の派生 URL も varchar(512) なので、
// サムネイルを作る画像は複製の INSERT が先に落ちる)。

const emojiURLLimit = 512

// longURL returns a URL of exactly n runes.
func longURL(n int) string {
	const prefix = "https://s3.example/"
	return prefix + strings.Repeat("a", n-len(prefix))
}

func TestEmojiAdd_RejectsUnstorableFileURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		copy *model.DriveFile
	}{
		{"url が 1 文字超過", systemCopy("sys_u1", longURL(emojiURLLimit+1), "", "")},
		{"webpublic が 1 文字超過", systemCopy("sys_u2", longURL(emojiURLLimit), longURL(emojiURLLimit+1), "image/webp")},
		// **`url` 側だけが超過する形も要る。** `preferWebpublicURL` は
		// `webpublicUrl ?? url` なので、webpublic が空のケースだけだと
		// publicUrl 側の判定が url 側を包含し、**url 側の条件を外した変異が
		// 素通りする** (レビュー M1 で実測)。
		{"url だけ超過 (webpublic は収まる)", systemCopy("sys_u7", longURL(emojiURLLimit+1), longURL(emojiURLLimit), "image/webp")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := setupEmojiHandler(t)
			owner := "u1"
			ownedDriveFile(t, h, &model.DriveFile{
				ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
			})
			fetcher := &fakeEmojiImageFetcher{copyDF: tc.copy}
			h.SetEmojiImageFetcher(fetcher)

			rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			code, id := errorID(t, rec.Body.Bytes())
			assert.Equal(t, "INVALID_PARAM", code)
			assert.Equal(t, "3d81ceae-475f-4600-b2a8-2bc116157532", id)
			assert.Equal(t, []string{tc.copy.ID}, fetcher.deletedIDs,
				"弾いた複製が孤児として残っている")
			_, ferr := repo.FindByNameAndHost("happy", nil)
			assert.Error(t, ferr, "弾いたのに絵文字が作られている")
		})
	}
}

// 上限ちょうどは通す (境界を off-by-one で締めない)。
func TestEmojiAdd_AcceptsFileURLAtLimit(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	url := longURL(emojiURLLimit)
	h.SetEmojiImageFetcher(&fakeEmojiImageFetcher{copyDF: systemCopy("sys_u3", url, "", "")})

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, url, got.OriginalURL)
}

func TestEmojiUpdate_RejectsUnstorableFileURL(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "happy", OriginalURL: "https://example/system/old.png",
	})
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys_u4", longURL(emojiURLLimit+1), "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, []string{"sys_u4"}, fetcher.deletedIDs, "弾いた複製が孤児として残っている")
	assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL,
		"弾いたのに差し替わっている")
}

func TestEmojiCopy_RejectsUnstorableFileURL(t *testing.T) {
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "src1", Name: "unique", Host: &remoteHost,
		OriginalURL: "https://remote.example/emoji/x.png",
	})
	fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
		ID: "df_u1", URL: longURL(emojiURLLimit + 1), Type: "image/png",
	}}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, []string{"df_u1"}, fetcher.deletedIDs, "取り込んだ実体が孤児として残っている")
	_, ferr := repo.FindByNameAndHost("unique", nil)
	assert.Error(t, ferr, "弾いたのに絵文字が作られている")
}

func TestCreateFromApplication_RejectsUnstorableFileURL(t *testing.T) {
	emojis := newEmojiRepoWith("")
	files := newDriveRepoWith(pngFile())
	fetcher := &stubFetcher{copyFile: &model.DriveFile{
		ID: "sys_u5", URL: longURL(emojiURLLimit + 1), Type: "image/png",
	}}

	_, err := newCreatorHandlerWithFetcher(t, emojis, files, fetcher).
		CreateFromApplication(context.Background(), ownApplication())
	require.ErrorIs(t, err, emojiapplication.ErrImageURLTooLong)
	assert.Equal(t, []string{"sys_u5"}, fetcher.deletedIDs, "弾いた複製が孤児として残っている")
	assert.Empty(t, emojis.Emojis, "弾いたのに絵文字が作られている")
}

func TestCreateFromRemoteApplication_RejectsUnstorableFileURL(t *testing.T) {
	emojis := newRemoteEmojiRepo(t)
	fetcher := &stubFetcher{file: &model.DriveFile{
		ID: "sys_u6", URL: longURL(emojiURLLimit + 1), Type: "image/png",
	}}

	_, err := newCreatorHandlerWithFetcher(t, emojis, newDriveRepoWith(pngFile()), fetcher).
		CreateFromApplication(context.Background(), remoteApplication())
	require.ErrorIs(t, err, emojiapplication.ErrImageURLTooLong)
	assert.Equal(t, []string{"sys_u6"}, fetcher.deletedIDs, "取り込んだ実体が孤児として残っている")
	// **ローカルの絵文字を作っていないこと。** 取り込み元のリモート絵文字
	// (`sushi_remote@example.com`) は残るので件数だけでは見分けられない。
	// `MockEmojiRepository` の key は `<name>@<host>` で、ローカルは host が空。
	assert.NotContains(t, emojis.Emojis, "sushi@", "弾いたのに絵文字が作られている")
	assert.Len(t, emojis.Emojis, 1, "取り込み元以外の行が増えている")
}

// **既に system 所有のファイルは複製しないので、消してはいけない。**
// その行はこの操作が作ったものではなく、他の絵文字が参照している可能性がある
// (承認済みの申請の画像や、以前に複製したもの)。400 で返すだけにする。
func TestEmojiAdd_KeepsSystemOwnedSourceWhenURLTooLong(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	// `UserID` / `UserHost` が nil = system 所有 (`drive.IsSystemOwned`)。
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_sys", Type: "image/png", URL: longURL(emojiURLLimit + 1),
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("never", "https://example/never.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_sys"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, "3d81ceae-475f-4600-b2a8-2bc116157532", id)
	assert.Empty(t, fetcher.copyCalls, "既に system 所有なのに複製している")
	assert.Empty(t, fetcher.deletedIDs,
		"この操作が作っていない system 所有ファイルを消している (他の絵文字が参照しうる)")
	_, ferr := repo.FindByNameAndHost("happy", nil)
	assert.Error(t, ferr, "弾いたのに絵文字が作られている")
}

// `admin/emoji/update` も同じ。**`fileId` は所有者で絞らずに解決する**ので、
// API を直に叩けば system 所有のファイルの id を渡せる。そこで複製を作らない経路に
// 入るため、消すと他の絵文字が参照しているファイルを巻き込む。
func TestEmojiUpdate_KeepsSystemOwnedSourceWhenURLTooLong(t *testing.T) {
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "happy", OriginalURL: "https://example/system/old.png",
	})
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_sys", Type: "image/png", URL: longURL(emojiURLLimit + 1),
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("never", "https://example/never.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_sys"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Empty(t, fetcher.copyCalls, "既に system 所有なのに複製している")
	assert.Empty(t, fetcher.deletedIDs,
		"この操作が作っていない system 所有ファイルを消している (他の絵文字が参照しうる)")
	assert.Equal(t, "https://example/system/old.png", findEmojiByID(t, repo, "e1").OriginalURL,
		"弾いたのに差し替わっている")
}

// 未配線の構成では複製そのものが無いので、この検査は元ファイルの URL に掛かる。
func TestEmojiAdd_FetcherUnsetChecksSourceURL(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: longURL(emojiURLLimit + 1),
	})

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	// **code / id も見る。** 手前に別の 400 が増えたときに、黙って別物を検査する
	// テストに化けるのを防ぐ (レビュー L7)。
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
	assert.Equal(t, "3d81ceae-475f-4600-b2a8-2bc116157532", id)
	_, ferr := repo.FindByNameAndHost("happy", nil)
	assert.Error(t, ferr)
}
