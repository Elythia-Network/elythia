package admin_test

import (
	"encoding/json"
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

// `admin/emoji/add` が system 所有の複製を作る経路 (#2999) のテスト。
//
// 直している失敗形は「**操作者が自分の drive ファイルを消すと絵文字が壊れる**」で、
// 申請の承認 (#2966) / リモートの複製 (#670) は既に system 所有にしてある。

// systemCopy is the drive file a wired fetcher hands back.
func systemCopy(id, url, webURL, webType string) *model.DriveFile {
	df := &model.DriveFile{ID: id, Type: "image/png", URL: url}
	if webURL != "" {
		df.WebpublicURL = &webURL
	}
	if webType != "" {
		df.WebpublicType = &webType
	}
	return df
}

// ownedDriveFile returns a drive row owned by a local user (= the shape
// `admin/emoji/add` normally receives: モデレーターが直前に上げたファイル)。
func ownedDriveFile(t *testing.T, h *apiadmin.Handler, f *model.DriveFile) *testutil.MockDriveFileRepository {
	t.Helper()
	dr := testutil.NewMockDriveFileRepository()
	require.NoError(t, dr.Create(f))
	h.SetDriveFileRepo(dr)
	return dr
}

func errorID(t *testing.T, body []byte) (code, id string) {
	t.Helper()
	var resp struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Error.Code, resp.Error.ID
}

// 利用者所有のファイルを指定すると、絵文字は複製のほうを指す。
//
// **元のファイルの URL がどこにも残らないことまで見る。** originalUrl だけを見て
// いると、publicUrl が元ファイル側に残る実装 (= 元を消すと表示が壊れる) を通す。
func TestEmojiAdd_FileIDCopiesToSystemOwnedFile(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	fetcher := &fakeEmojiImageFetcher{
		copyDF: systemCopy("sys1", "https://example/system/copy.png", "https://example/system/copy.webp", "image/webp"),
	}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img","isSensitive":true}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, fetcher.copyCalls, 1, "利用者所有のファイルは複製しなければならない")
	assert.Equal(t, "f_img", fetcher.copyCalls[0].ID, "複製元は指定された drive ファイル")
	assert.Equal(t, "happy", fetcher.copyNames[0], "複製の名前は絵文字名")
	assert.Equal(t, []bool{true}, fetcher.copySensitive, "isSensitive を複製にも渡す")

	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	// 不変条件 (#722): originalUrl は必ず drive_file.url と一致する。webpublic を
	// 入れると DeleteOrphans の guard が外れて複製が消える。
	assert.Equal(t, "https://example/system/copy.png", got.OriginalURL)
	assert.Equal(t, "https://example/system/copy.webp", got.PublicURL)
	require.NotNil(t, got.Type)
	assert.Equal(t, "image/webp", *got.Type)
	assert.NotContains(t, got.OriginalURL, "/user/", "元ファイルの URL を参照し続けてはならない")
	assert.NotContains(t, got.PublicURL, "/user/", "元ファイルの URL を参照し続けてはならない")
	assert.Empty(t, fetcher.deletedIDs, "成功したのに複製を消している")
}

// 既に system 所有のファイルは複製しない (利用者の drive 操作から既に切れている)。
func TestEmojiAdd_FileIDSkipsCopyForSystemOwnedFile(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	// **`size` は上限超過にしてある。** 複製しない経路では読み出しが無いので、
	// 上限の前捌きも効いてはいけない (効くと、複製済みの大きな絵文字を登録
	// し直せなくなる)。
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_sys", Type: "image/png", URL: "https://example/system/already.png",
		Size: int(apiadmin.MaxEmojiCopyBytes) + 1,
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys2", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_sys"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, fetcher.copyCalls, "system 所有のファイルを二重に複製している")
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://example/system/already.png", got.OriginalURL)
}

// `userId` が NULL でも `userHost` があるリモートキャッシュは system 所有ではない。
// `admin/drive/clean-remote-files` で消えるので、複製しないと同じ形で壊れる。
func TestEmojiAdd_FileIDCopiesRemoteCachedFile(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	host := "remote.example"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_remote", UserHost: &host, Type: "image/png", URL: "https://remote.example/cached.png",
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys3", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_remote"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, fetcher.copyCalls, 1, "リモートキャッシュは system 所有ではない")
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://example/system/copy.png", got.OriginalURL)
}

// 複製した実体の MIME が allowlist 外なら弾き、作った複製を片付ける。
// `Upload` は AnalyseFile でバイト列から型を引き直すので、行の宣言と実体が
// ずれていると allowlist 外の型が絵文字として登録される。
func TestEmojiAdd_FileIDRejectsCopiedNonImageAndCleansUp(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	bad := systemCopy("sys4", "https://example/system/copy.bin", "", "")
	bad.Type = "text/plain"
	fetcher := &fakeEmojiImageFetcher{copyDF: bad}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "UNSUPPORTED_FILE_TYPE", code)
	assert.Equal(t, []string{"sys4"}, fetcher.deletedIDs, "弾いた複製は誰からも参照されないので消す")
	_, err := repo.FindByNameAndHost("happy", nil)
	assert.Error(t, err, "弾いたのに絵文字が作られている")
}

// 複製の失敗は種別ごとに分ける (#2792)。実体がもう無い / 大きすぎるは 400、
// ストレージや DB の障害は 500。error id は承認経路と同じものを返す。
func TestEmojiAdd_FileIDCopyFailureMapping(t *testing.T) {
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
			id:      "fc46b5a4-6b92-4c33-ac66-b806659bb5cf",
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
			h, repo := setupEmojiHandler(t)
			owner := "u1"
			ownedDriveFile(t, h, &model.DriveFile{
				ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
			})
			fetcher := &fakeEmojiImageFetcher{copyErr: tc.copyErr}
			h.SetEmojiImageFetcher(fetcher)

			rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
			require.Equal(t, tc.status, rec.Code)
			code, id := errorID(t, rec.Body.Bytes())
			assert.Equal(t, tc.code, code)
			assert.Equal(t, tc.id, id, "承認経路と同じ error id を返す")
			// **元ファイルを参照する絵文字を作らない。** 作ると直そうとしている
			// 依存をそのまま残すことになる。
			_, err := repo.FindByNameAndHost("happy", nil)
			assert.Error(t, err, "複製に失敗したのに絵文字が作られている")
			// 複製できていないので消しに行く相手もいない。
			assert.Empty(t, fetcher.deletedIDs, "作っていない複製を消しに行っている")
		})
	}
}

// **検証をすべて通す前に複製しない。** 先に複製すると、重複名や MIME で弾く分まで
// 実体を作って捨てることになり (upstream の `copy.ts` がその形)、誰からも参照されない
// 孤児が残る。`DeleteOrphans` は `admin/drive/cleanup` からしか走らないので、
// 掃除するまで実体ストレージを食い続ける。
//
// あわせて upstream の error 順序 (noSuchFile → duplicate → unsupportedFileType) も
// 固定する。非画像のファイルで同名の絵文字を作ろうとしたときは `DUPLICATE_NAME` が先。
func TestEmojiAdd_DoesNotCopyBeforeValidation(t *testing.T) {
	newHandler := func(t *testing.T, mime string, seed ...*model.Emoji) (*apiadmin.Handler, *fakeEmojiImageFetcher) {
		t.Helper()
		h, _ := setupEmojiHandler(t, seed...)
		owner := "u1"
		ownedDriveFile(t, h, &model.DriveFile{
			ID: "f_img", UserID: &owner, Type: mime, URL: "https://example/user/orig.png",
		})
		fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys_x", "https://example/system/copy.png", "", "")}
		h.SetEmojiImageFetcher(fetcher)
		return h, fetcher
	}

	t.Run("重複名は複製する前に弾く", func(t *testing.T) {
		h, fetcher := newHandler(t, "image/png", &model.Emoji{ID: "e1", Name: "happy"})
		rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "DUPLICATE_NAME", code)
		assert.Empty(t, fetcher.copyCalls, "重複で弾くのに複製を作っている (孤児になる)")
	})

	t.Run("非画像は複製する前に弾く", func(t *testing.T) {
		h, fetcher := newHandler(t, "text/plain")
		rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "UNSUPPORTED_FILE_TYPE", code)
		assert.Empty(t, fetcher.copyCalls, "MIME で弾くのに複製を作っている (孤児になる)")
	})

	t.Run("非画像かつ重複名なら DUPLICATE_NAME が先", func(t *testing.T) {
		h, fetcher := newHandler(t, "text/plain", &model.Emoji{ID: "e1", Name: "happy"})
		rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		code, _ := errorID(t, rec.Body.Bytes())
		assert.Equal(t, "DUPLICATE_NAME", code, "upstream の error 順序が変わっている")
		assert.Empty(t, fetcher.copyCalls)
	})
}

// 行の `size` が複製の上限を超えていたら、実体を読む前に断る。
// **`size` は前捌きで、上限の権威ではない** — 実体とずれうるので、読み出し側の
// 上限 (safehttp.ErrResponseTooLarge) は別に効いている (上のケース表)。
func TestEmojiAdd_FileIDRejectsOversizedRowBeforeReading(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_big", UserID: &owner, Type: "image/png", URL: "https://example/user/big.png",
		Size: int(apiadmin.MaxEmojiCopyBytes) + 1,
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys7", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_big"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "EMOJI_IMAGE_TOO_LARGE", code)
	assert.Equal(t, "6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64", id, "申請・承認と同じ error id を返す")
	assert.Empty(t, fetcher.copyCalls, "上限超過が分かっているのに実体を読んでいる")
	_, err := repo.FindByNameAndHost("happy", nil)
	assert.Error(t, err)
}

// 上限ちょうどは通す (境界を off-by-one で締めない)。
func TestEmojiAdd_FileIDAcceptsRowAtSizeLimit(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_max", UserID: &owner, Type: "image/png", URL: "https://example/user/max.png",
		Size: int(apiadmin.MaxEmojiCopyBytes),
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys8", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_max"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, fetcher.copyCalls, 1)
}

// fetcher が nil, nil を返しても panic させない (実装の契約違反を 500 に落とす)。
func TestEmojiAdd_FileIDCopyReturnsNoFile(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	h.SetEmojiImageFetcher(&fakeEmojiImageFetcher{})

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	_, err := repo.FindByNameAndHost("happy", nil)
	assert.Error(t, err)
}

// 絵文字の作成に失敗したら複製を片付ける (誰からも参照されない孤児になる)。
func TestEmojiAdd_CreateFailureDeletesSystemCopy(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiRepo(&failingCreateEmojiRepo{testutil.NewMockEmojiRepository()})
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys5", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, []string{"sys5"}, fetcher.deletedIDs, "作成に失敗した複製が孤児として残っている")
}

// legacy の `url` 直接指定は取り込まない (所有者の概念が無く、外部 URL も指せる)。
func TestEmojiAdd_URLPathDoesNotCopy(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys6", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","url":"https://cdn.example/happy.png"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, fetcher.copyCalls)
	assert.Empty(t, fetcher.calls, "url 経路で HTTP 取り込みを走らせない")
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example/happy.png", got.OriginalURL)
}

// fetcher 未配線なら drive の URL をそのまま使う (EmojiCopy と同じ degradation)。
// 未配線そのものは router の起動時チェックが知らせる。
func TestEmojiAdd_FetcherUnsetKeepsDriveURL(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	owner := "u1"
	// 未配線でも読み出しは無いので、上限の前捌きは効かない (L-B と同じ理由)。
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
		Size: int(apiadmin.MaxEmojiCopyBytes) + 1,
	})

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://example/user/orig.png", got.OriginalURL)
}

// `idGen` 未配線は複製を作る前に 500 で止める。
//
// **配線上は起きない** (`NewHandler` でしか設定できず、router は必ず実 generator を
// 渡す) が、`Generate` は複製の**後**に呼ぶので、素通りさせると panic して誰からも
// 参照されない複製が残る。`CreateFromApplication` が同じ形を冒頭に持っている。
func TestEmojiAdd_IDGenUnsetReturns500BeforeCopy(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h := apiadmin.NewHandler(nil, nil, metaRepo, userRepo, nil)
	h.SetEmojiRepo(testutil.NewMockEmojiRepository())
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys9", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, fetcher.copyCalls, "作った複製を誰も参照できない状態で止まっている")
}

// 存在しない fileId は NO_SUCH_FILE。**upstream の最初のエラー**なので、
// 複製が絡む経路より前に返ることを固定する。
func TestEmojiAdd_UnknownFileIDReturnsNoSuchFile(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	ownedDriveFile(t, h, &model.DriveFile{ID: "f_other", Type: "image/png", URL: "https://example/x.png"})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys10", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"missing"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "NO_SUCH_FILE", code)
	assert.Equal(t, "fc46b5a4-6b92-4c33-ac66-b806659bb5cf", id)
	assert.Empty(t, fetcher.copyCalls)
}

// url も fileId も無ければ 400 INVALID_PARAM (drive repo が配線済みでも同じ)。
func TestEmojiAdd_NeitherURLNorFileIDReturns400(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	ownedDriveFile(t, h, &model.DriveFile{ID: "f_img", Type: "image/png", URL: "https://example/x.png"})

	rec := doPost(h.EmojiAdd, `{"name":"happy"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorID(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_PARAM", code)
}

// **重複チェックの DB 障害を「重複なし」に倒さない (#2792)。** 倒すと接続断のあいだ
// 同名の絵文字が作れてしまう。
func TestEmojiAdd_DuplicateLookupFailureReturns500(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiRepo(&failingFindByNameEmojiRepo{testutil.NewMockEmojiRepository()})
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	fetcher := &fakeEmojiImageFetcher{copyDF: systemCopy("sys11", "https://example/system/copy.png", "", "")}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, fetcher.copyCalls, "重複か分からないのに複製を作っている")
}

// failingFindByNameEmojiRepo fails the local-duplicate lookup with a non
// not-found error (= DB 障害)。
type failingFindByNameEmojiRepo struct {
	*testutil.MockEmojiRepository
}

func (f *failingFindByNameEmojiRepo) FindByNameAndHost(_ string, _ *string) (*model.Emoji, error) {
	return nil, assert.AnError
}
