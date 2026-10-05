package admin_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 絵文字を作る 4 経路が、`Create` の失敗で複製を消す前に**載ったかを読み直す**
// (#3019)。
//
// **エラーが返っても INSERT は載っていることがある** — PostgreSQL は COMMIT を
// 送った後・ack が届く前に接続が切れると、サーバー側は commit 済みなのに
// クライアントはエラーを受け取る。そこで複製を消すと、**名前が使用中のまま画像
// だけ無い**状態になり、同じ名前で登録し直しても `DUPLICATE_NAME` で弾かれる
// (承認では申請が pending のまま残るので、再承認も同じ理由で通らない)。
// 差し替え (`update`) は #3014 で同じ判断を入れてある。

// landedThenFailingCreateRepo persists the row and then reports a failure —
// COMMIT の後・ack の前に接続が切れた形。
type landedThenFailingCreateRepo struct {
	*testutil.MockEmojiRepository
}

func (r *landedThenFailingCreateRepo) Create(e *model.Emoji) error {
	if err := r.MockEmojiRepository.Create(e); err != nil {
		return err
	}
	return assert.AnError
}

// unreadableCreateRepo fails the insert and then fails the re-read, too
// (DB がまだ落ちている形)。
type unreadableCreateRepo struct {
	*testutil.MockEmojiRepository
}

func (r *unreadableCreateRepo) Create(_ *model.Emoji) error { return assert.AnError }

func (r *unreadableCreateRepo) FindByID(_ string) (*model.Emoji, error) {
	return nil, assert.AnError
}

func TestEmojiAdd_KeepsCopyWhenCreateLanded(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	inner := testutil.NewMockEmojiRepository()
	h.SetEmojiRepo(&landedThenFailingCreateRepo{MockEmojiRepository: inner})
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	// **複製に webpublic を持たせる。** 持たせないと `originalUrl` と `publicUrl` が
	// 同じ値になり、照合がどちらを見ているか区別できない (#722 の不変条件は
	// `originalUrl` 側。#3014 のテストが同じ理由で同じ形にしてある)。
	fetcher := &fakeEmojiImageFetcher{
		copyDF: systemCopy("sys_l1", "https://example/system/copy.png", "https://example/system/copy.webp", "image/webp"),
	}
	h.SetEmojiImageFetcher(fetcher)

	// **warn を固定する。** 500 を返したのに絵文字は実在する、という食い違いは
	// ここでしか伝わらない (運用側はこれを見ないと「失敗したはずなのに
	// `DUPLICATE_NAME` になる」の原因に辿り着けない)。
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, fetcher.deletedIDs,
		"載っていた複製を消している (名前が使用中のまま画像だけ無い状態になる)")
	assert.Contains(t, logs.String(), "the write landed despite the error",
		"載っていたことがログに残っていない")
	assert.Contains(t, logs.String(), "sys_l1")
	got, err := inner.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://example/system/copy.png", got.OriginalURL)
}

func TestEmojiAdd_KeepsCopyWhenRereadFails(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiRepo(&unreadableCreateRepo{MockEmojiRepository: testutil.NewMockEmojiRepository()})
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	fetcher := &fakeEmojiImageFetcher{
		copyDF: systemCopy("sys_l2", "https://example/system/copy.png", "https://example/system/copy.webp", "image/webp"),
	}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Len(t, fetcher.copyCalls, 1, "複製を作る前に止まっている (テストが空虚)")
	assert.Empty(t, fetcher.deletedIDs, "載ったか分からない複製を消している")
}

// 取り込み (`admin/emoji/copy`) も同じ窓を持つ。
func TestEmojiCopy_KeepsDriveFileWhenCreateLanded(t *testing.T) {
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "src1", Name: "ok_name", Host: &remoteHost,
		OriginalURL: "https://remote.example/emoji/x.png",
	})
	h.SetEmojiRepo(&landedThenFailingCreateRepo{MockEmojiRepository: repo})
	webURL, webType := "https://local.example/files/x.webp", "image/webp"
	fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
		ID: "df1", URL: "https://local.example/files/x.png", Type: "image/png",
		WebpublicURL: &webURL, WebpublicType: &webType,
	}}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Len(t, fetcher.calls, 1, "取り込む前に止まっている (テストが空虚)")
	assert.Empty(t, fetcher.deletedIDs, "載っていた取り込みを消している")
	created, err := repo.FindByNameAndHost("ok_name", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://local.example/files/x.png", created.OriginalURL,
		"取り込んだ画像を指していない")
}

// 承認 (own) も同じ窓を持つ。**申請は pending のまま残る**ので、複製を消すと
// 再承認が `DUPLICATE_NAME` で通らなくなる。
func TestCreateFromApplication_KeepsCopyWhenCreateLanded(t *testing.T) {
	inner := newEmojiRepoWith("")
	files := newDriveRepoWith(pngFile())
	sysWeb, sysWebType := "https://x/sys-webpublic.webp", "image/webp"
	fetcher := &stubFetcher{copyFile: &model.DriveFile{
		ID: "sys_l3", URL: "https://x/sys-original.png", Type: "image/png",
		WebpublicURL: &sysWeb, WebpublicType: &sysWebType,
	}}
	h := newCreatorHandlerWithFetcher(t, inner, files, fetcher)
	h.SetEmojiRepo(&landedThenFailingCreateRepo{MockEmojiRepository: inner})

	_, err := h.CreateFromApplication(context.Background(), ownApplication())
	require.Error(t, err)
	assert.Empty(t, fetcher.deletedIDs,
		"載っていた複製を消している (申請は pending のままなので再承認が DUPLICATE_NAME になる)")
	created, ferr := inner.FindByNameAndHost("sushi", nil)
	require.NoError(t, ferr)
	assert.Equal(t, "https://x/sys-original.png", created.OriginalURL)
}

func TestCreateFromApplication_KeepsCopyWhenRereadFails(t *testing.T) {
	files := newDriveRepoWith(pngFile())
	sysWeb, sysWebType := "https://x/sys-webpublic.webp", "image/webp"
	fetcher := &stubFetcher{copyFile: &model.DriveFile{
		ID: "sys_l4", URL: "https://x/sys-original.png", Type: "image/png",
		WebpublicURL: &sysWeb, WebpublicType: &sysWebType,
	}}
	h := newCreatorHandlerWithFetcher(t, newEmojiRepoWith(""), files, fetcher)
	h.SetEmojiRepo(&unreadableCreateRepo{MockEmojiRepository: testutil.NewMockEmojiRepository()})

	// **申請 ID をログに残す。** 500 だけでは、その申請が却下待ちであることが
	// 運用側から分からない (複製を残しても消しても再承認は `DUPLICATE_NAME`)。
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	_, err := h.CreateFromApplication(context.Background(), ownApplication())
	require.Error(t, err)
	require.Len(t, fetcher.copySrc, 1, "複製を作る前に止まっている (テストが空虚)")
	assert.Empty(t, fetcher.deletedIDs, "載ったか分からない複製を消している")
	assert.Contains(t, logs.String(), "applicationId=a1",
		"どの申請が手当てを要するかログに残っていない")
}

// 承認 (remote) も同じ窓を持つ。取り込みは HTTP 越しなので、消すと相手サーバーへ
// もう一度取りに行くことになる。
func TestCreateFromRemoteApplication_KeepsFileWhenCreateLanded(t *testing.T) {
	inner := newRemoteEmojiRepo(t)
	sysWeb, sysWebType := "https://local.example/files/sushi.webp", "image/webp"
	fetcher := &stubFetcher{file: &model.DriveFile{
		ID: "sys_l5", URL: "https://local.example/files/sushi.png", Type: "image/png",
		WebpublicURL: &sysWeb, WebpublicType: &sysWebType,
	}}
	h := newCreatorHandlerWithFetcher(t, inner, newDriveRepoWith(pngFile()), fetcher)
	h.SetEmojiRepo(&landedThenFailingCreateRepo{MockEmojiRepository: inner})

	_, err := h.CreateFromApplication(context.Background(), remoteApplication())
	require.Error(t, err)
	assert.Empty(t, fetcher.deletedIDs, "載っていた取り込みを消している")
	assert.Equal(t, "https://local.example/files/sushi.png",
		inner.Emojis["sushi@"].OriginalURL, "取り込んだ画像を指していない")
}

// **remote 承認の「消す」側も固定する (#3019 のレビュー H4)。** keep 側だけを
// 足すと、後始末そのものを外す変更が緑で通る (実測)。
func TestCreateFromRemoteApplication_DeletesFileWhenCreateDidNotLand(t *testing.T) {
	inner := newRemoteEmojiRepo(t)
	fetcher := &stubFetcher{file: &model.DriveFile{
		ID: "sys_l6", URL: "https://local.example/files/sushi.png", Type: "image/png",
	}}
	h := newCreatorHandlerWithFetcher(t, inner, newDriveRepoWith(pngFile()), fetcher)
	h.SetEmojiRepo(&failingCreateEmojiRepo{MockEmojiRepository: inner})

	// **remote 側の warn も固定する。** own だけ assert すると、片側のログが
	// 落ちても緑で通る (#3019 のレビュー M1 で実測)。
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	_, err := h.CreateFromApplication(context.Background(), remoteApplication())
	require.Error(t, err)
	assert.Equal(t, []string{"sys_l6"}, fetcher.deletedIDs,
		"載らなかった取り込みが孤児として残っている")
	assert.Contains(t, logs.String(), "applicationId=a2",
		"どの申請が手当てを要するかログに残っていない")
}

// **`copiedURL` が空なら触らない (#3019 のレビュー L5)。** 空同士は「一致」に
// 見えてしまい、照合として成立しない。倒す向きは「消さない」で、残った複製は
// 孤児 cleanup が回収する。後始末バッチの `unrepairableFromRow` も同じ曖昧さを
// 理由に、URL の無い行を複製対象から外している。
func TestEmojiAdd_KeepsCopyWhenCopiedURLIsEmpty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiRepo(&failingCreateEmojiRepo{testutil.NewMockEmojiRepository()})
	owner := "u1"
	ownedDriveFile(t, h, &model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/user/orig.png",
	})
	// 複製の URL が空 = 実体の URL を決められなかった状態。
	fetcher := &fakeEmojiImageFetcher{copyDF: &model.DriveFile{ID: "sys_l7", Type: "image/png"}}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, fetcher.deletedIDs, "照合できないのに消している")
}

// **複製を作る経路が増えたら、この一覧も増やす (#3019 のレビュー L3)。**
// 「4 経路すべてが同じ述語を通る」を宣言しているのに、5 本目が足されたときに
// 気付く仕組みが無いと、宣言の射程とゲートの射程がずれる (#2874 と同じ形)。
//
// **`h.emojiRepo.Create` を呼ぶ関数を数え、後始末を通しているかを見る。**
// `Create` の直後で複製を消す経路はすべて `cleanupUnreferencedEmojiCopy` を
// 通すこと。複製を作らない経路 (AP の `upsertEmojis` は別パッケージ) はここに
// 現れない。
func TestEmojiCreateSitesGoThroughLandedCheck(t *testing.T) {
	fset := token.NewFileSet()
	// 対象は非テストのソースだけ。テストは fake を差し込むので数に入らない。
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	var creators []string
	found := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var createsEmoji, checksLanded bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "Create" && exprText(fset, fun.X) == "h.emojiRepo" {
						createsEmoji = true
					}
					if fun.Sel.Name == "cleanupUnreferencedEmojiCopy" {
						checksLanded = true
					}
				}
				return true
			})
			if createsEmoji {
				found++
				if !checksLanded {
					creators = append(creators, path+":"+fn.Name.Name)
				}
			}
		}
	}
	assert.Empty(t, creators,
		"`emojiRepo.Create` を呼ぶのに載ったかを読み直していない経路がある (複製を作らないなら、その旨を doc に足して除外すること)")
	// **1 つも拾えなかったら落とす。** 書き方が変わって走査が空振りすると、
	// 検査していないのに緑になる。現状は add / copy / 承認 own・remote の 4 つ。
	assert.Equal(t, 4, found, "`emojiRepo.Create` の走査が実態と合っていない")
}

// exprText renders an expression back to source so the receiver can be matched.
func exprText(fset *token.FileSet, e ast.Expr) string {
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return ""
	}
	return buf.String()
}
