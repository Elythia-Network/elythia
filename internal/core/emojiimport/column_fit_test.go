package emojiimport_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiimport"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `admin/emoji/import-zip` が列に入らない値を弾き、**元の絵文字を落とさない**
// (#3021)。
//
// 直している失敗形は 2 つ。(a) `meta.json` 由来の値が無検証で、列に入らないと
// `Create` が SQLSTATE 22001 で落ちる。(b) **同名の既存絵文字を先に消してから
// 作っていた**ので、(a) でも画像の取り込みでも失敗すると**消した絵文字が戻らない**。

const (
	catLimit     = 128
	licLimit     = 1024
	aliasLimit   = 128
	nameLimit    = 128
	urlLimit     = 512
	existingName = "smile"
)

// importOne builds a one-record zip for the given emoji fields.
func importOne(t *testing.T, emoji map[string]any) []byte {
	t.Helper()
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": emoji},
	})
	return buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", pngBytes(t)}})
}

// 列に入らない本文はレコードごと skip する。**既存の絵文字は消さない。**
func TestRun_SkipsRecordsThatDoNotFit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		emoji map[string]any
	}{
		{"name が 1 文字超過", map[string]any{"name": strings.Repeat("a", nameLimit+1)}},
		{"category が 1 文字超過", map[string]any{"name": existingName, "category": strings.Repeat("あ", catLimit+1)}},
		{"category に NUL", map[string]any{"name": existingName, "category": "a\x00b"}},
		{"license が 1 文字超過", map[string]any{"name": existingName, "license": strings.Repeat("い", licLimit+1)}},
		{"license に NUL", map[string]any{"name": existingName, "license": "a\x00b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, repo, _ := newDeps(t, importOne(t, tc.emoji))
			require.NoError(t, repo.Create(&model.Emoji{
				ID: "old", Name: existingName, OriginalURL: "https://example/old.png",
			}))

			res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
			require.NoError(t, err)
			assert.Equal(t, 0, res.Imported)
			assert.Equal(t, 1, res.Skipped)
			// **元の絵文字が残っていること。** ここが #3021 の本体 — 先に消して
			// いたので、弾いた時点で手元の絵文字が失われていた。
			found, ferr := repo.FindByNameAndHost(existingName, nil)
			require.NoError(t, ferr, "弾いたのに既存の絵文字が消えている")
			assert.Equal(t, "old", found.ID)
			// **名前が違うケースでは、上の assert は素通りする** (seed を引いて
			// いるだけ)。弾いた名前で行が作られていないことを別に見る。
			if n, _ := tc.emoji["name"].(string); n != existingName {
				_, nerr := repo.FindByNameAndHost(n, nil)
				assert.Error(t, nerr, "弾いたのに新しい絵文字が作られている")
			}
		})
	}
}

// alias は**要素ごとに落とす**。1 つが長すぎるだけで他まで捨てない。
func TestRun_DropsUnstorableAliases(t *testing.T) {
	deps, _, repo, _ := newDeps(t, importOne(t, map[string]any{
		"name":    "smile",
		"aliases": []string{"ok", strings.Repeat("あ", aliasLimit+1), "", "a\x00b"},
	}))

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
	found, ferr := repo.FindByNameAndHost("smile", nil)
	require.NoError(t, ferr)
	assert.Equal(t, []string{"ok", "ab"}, []string(found.Aliases))
}

// 上限ちょうどは通す (境界を off-by-one で締めない)。**全角で作る** — byte で
// 数える実装なら落ちる。
func TestRun_AcceptsValuesAtLimit(t *testing.T) {
	cat, lic, alias := strings.Repeat("あ", catLimit), strings.Repeat("い", licLimit), strings.Repeat("う", aliasLimit)
	deps, _, repo, _ := newDeps(t, importOne(t, map[string]any{
		"name": strings.Repeat("a", nameLimit), "category": cat, "license": lic,
		"aliases": []string{alias},
	}))

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
	found, ferr := repo.FindByNameAndHost(strings.Repeat("a", nameLimit), nil)
	require.NoError(t, ferr)
	require.NotNil(t, found.Category)
	assert.Equal(t, cat, *found.Category)
	require.NotNil(t, found.License)
	assert.Equal(t, lic, *found.License)
	assert.Equal(t, []string{alias}, []string(found.Aliases))
}

// 画像の取り込みに失敗しても元の絵文字を消さない (**取り込んでから消す**)。
//
// **保存先を書けなくして Upload を落とす。** `Uploader` は具体型なので差し替えが
// 効かず、壊れた画像は octet-stream として保存されてしまう (= 失敗しない)。
func TestRun_KeepsExistingWhenUploadFails(t *testing.T) {
	deps, _, repo, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	deps.Uploader = readOnlyUploader(t)
	require.NoError(t, repo.Create(&model.Emoji{
		ID: "old", Name: existingName, OriginalURL: "https://example/old.png",
	}))

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Imported)
	assert.Equal(t, 1, res.Skipped)
	found, ferr := repo.FindByNameAndHost(existingName, nil)
	require.NoError(t, ferr, "取り込みに失敗しただけで既存の絵文字が消えている")
	assert.Equal(t, "old", found.ID)
}

// readOnlyUploader returns a drive service whose local storage cannot be written.
func readOnlyUploader(t *testing.T) *drive.Service {
	t.Helper()
	// **root では DAC が効かない。** 書けてしまうと `Imported == 0` の前提が崩れて
	// 赤くなる (静かに緑にはならない)。同種のテストと同じく skip する。
	if os.Geteuid() == 0 {
		t.Skip("root では読み取り専用ディレクトリに書けてしまう")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return drive.NewService(fileRepo, folderRepo,
		drive.NewLocalStorage(dir, "https://example.com/files"), idGen)
}

// `Create` が失敗したら、消した行を戻す。
func TestRun_RestoresReplacedEmojiWhenCreateFails(t *testing.T) {
	deps, _, repo, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	require.NoError(t, repo.Create(&model.Emoji{
		ID: "old", Name: existingName, OriginalURL: "https://example/old.png",
	}))
	deps.EmojiRepo = &failingCreateRepo{MockEmojiRepository: repo}

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Imported)
	assert.Equal(t, 1, res.Skipped)
	found, ferr := repo.FindByNameAndHost(existingName, nil)
	require.NoError(t, ferr, "作成に失敗したのに消した絵文字が戻っていない")
	assert.Equal(t, "old", found.ID)
	assert.Equal(t, "https://example/old.png", found.OriginalURL)
}

// failingCreateRepo fails only the insert of the *new* row, so the restore path
// can run (戻す側まで失敗させると、何を確かめているのか分からなくなる)。
type failingCreateRepo struct {
	*testutil.MockEmojiRepository
	failed bool
}

func (r *failingCreateRepo) Create(e *model.Emoji) error {
	if !r.failed {
		r.failed = true
		return errors.New("boom")
	}
	return r.MockEmojiRepository.Create(e)
}

// rowRepo は同名の行を**複数持てる** double。
//
// **`MockEmojiRepository` では足りない。** あちらは `name@host` をキーにした map
// なので、同名 2 行という壊れ方を構造的に表現できず、`len(Emojis) == 1` の
// アサーションが常に通る (実測で空虚だった)。local emoji は `host IS NULL` で
// 一意制約が効かないので、実際には 2 行できる。
type rowRepo struct {
	*testutil.MockEmojiRepository
	rows []*model.Emoji
	// createErr / deleteErr は「エラーは返るが書き込みは載る」窓を作るためのもの。
	createErr   error
	createLands bool
	// createAlwaysErrs にすると戻す側の Create も失敗する。
	createAlwaysErrs bool
	deleteErr        error
	deleteLands      bool
	// findByIDErr / findByNameErr は「読み直せない」窓を作る。
	findByIDErr   error
	findByNameErr error
}

func newRowRepo(seed ...*model.Emoji) *rowRepo {
	return &rowRepo{MockEmojiRepository: testutil.NewMockEmojiRepository(), rows: seed}
}

func (r *rowRepo) Create(e *model.Emoji) error {
	if r.createErr != nil {
		if r.createLands {
			r.rows = append(r.rows, e)
		}
		err := r.createErr
		if !r.createAlwaysErrs {
			// 既定では戻す側の Create を通す (戻せるかを見たいわけではない)。
			r.createErr = nil
		}
		return err
	}
	r.rows = append(r.rows, e)
	return nil
}

func (r *rowRepo) Delete(id string) error {
	if r.deleteErr != nil {
		if r.deleteLands {
			r.remove(id)
		}
		return r.deleteErr
	}
	r.remove(id)
	return nil
}

func (r *rowRepo) remove(id string) {
	out := r.rows[:0]
	for _, e := range r.rows {
		if e.ID != id {
			out = append(out, e)
		}
	}
	r.rows = out
}

func (r *rowRepo) FindByID(id string) (*model.Emoji, error) {
	if r.findByIDErr != nil {
		return nil, r.findByIDErr
	}
	for _, e := range r.rows {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, testutil.ErrNotFound
}

func (r *rowRepo) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	if r.findByNameErr != nil {
		return nil, r.findByNameErr
	}
	for _, e := range r.rows {
		if e.Name != name {
			continue
		}
		// production は host が nil なら `host IS NULL`、値があるときだけ
		// `host = ?` (`internal/repository/emoji.go`)。
		if (host == nil) != (e.Host == nil) {
			continue
		}
		if host != nil && *host != *e.Host {
			continue
		}
		return e, nil
	}
	return nil, testutil.ErrNotFound
}

func (r *rowRepo) ids() []string {
	out := make([]string, 0, len(r.rows))
	for _, e := range r.rows {
		out = append(out, e.ID)
	}
	return out
}

// 同名の既存行を消せなかったら、新しい行を作らない (同名の行が 2 つ残らない)。
func TestRun_DoesNotCreateWhenDeleteFails(t *testing.T) {
	repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
	repo.deleteErr = errors.New("boom")
	deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	deps.EmojiRepo = repo

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Imported)
	assert.Equal(t, []string{"old"}, repo.ids(),
		"消せていないのに新しい行を作っている (同名が 2 つ残る)")
}

// **削除が載っていたなら続ける (#3021 のレビュー H2)。** COMMIT の後・ack の前に
// 接続が切れると、消えているのにエラーが返る。そこで skip すると、元の絵文字が
// 消えたまま新しい行も作られない = この issue の被害そのもの。
func TestRun_ProceedsWhenDeleteLandedDespiteError(t *testing.T) {
	repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
	repo.deleteErr = errors.New("boom")
	repo.deleteLands = true
	deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	deps.EmojiRepo = repo

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported, "消えているのに skip して元の絵文字を失っている")
	require.Len(t, repo.rows, 1)
	assert.NotEqual(t, "old", repo.rows[0].ID)
}

// **作成が載っていたなら戻さない (#3021 のレビュー H1)。** 戻すと同名の行が 2 つ
// 残り、名前引きは id 順で古いほう (戻した行) を返すので、取り込んだ絵文字が
// 見えないまま一覧にだけ 2 件出る。
func TestRun_DoesNotRestoreWhenCreateLandedDespiteError(t *testing.T) {
	repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
	repo.createErr = errors.New("boom")
	repo.createLands = true
	deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	deps.EmojiRepo = repo

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Imported)
	require.Len(t, repo.rows, 1, "載っていた行に加えて元の行も戻している (同名が 2 つ残る)")
	assert.NotEqual(t, "old", repo.rows[0].ID)
}

// **読み直せないときは戻す。** 倒す向きが #3019 と逆なのは、残る状態が違うため —
// あちらで残るのは参照されない複製 (孤児 cleanup が回収する) だが、こちらで残らない
// のは運用者が手で入れた絵文字で、戻さなければ自動では復旧しない。
func TestRun_RestoresWhenCreateLandingIsUnknown(t *testing.T) {
	repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
	repo.createErr = errors.New("boom")
	repo.findByIDErr = errors.New("connection reset")
	deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	deps.EmojiRepo = repo

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Imported)
	assert.Equal(t, []string{"old"}, repo.ids(),
		"載ったか分からないだけで、運用者が入れた絵文字を失っている")
}

// 載っていなければ戻す (こちらが既定)。
func TestRun_RestoresWhenCreateDidNotLand(t *testing.T) {
	repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
	repo.createErr = errors.New("boom")
	deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	deps.EmojiRepo = repo

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Imported)
	assert.Equal(t, []string{"old"}, repo.ids(), "消した行が戻っていない")
}

// 上限の値そのものを固定する (#3021)。**at-limit のテストは定数ではなく数字で
// 入力を作る**ので、定数を小さくする変異はここで落ちる。DDL 側との突き合わせは
// `internal/repository/emoji_column_limits_test.go`。
func TestColumnLimitConstants(t *testing.T) {
	assert.Equal(t, 128, nameLimit, "emoji.name は varchar(128)")
	assert.Equal(t, 128, catLimit, "emoji.category は varchar(128)")
	assert.Equal(t, 128, aliasLimit, "emoji.aliases は varchar(128)[]")
	assert.Equal(t, 1024, licLimit, "emoji.license は varchar(1024)")
	assert.Equal(t, 512, urlLimit, "emoji.originalUrl / publicUrl は varchar(512)")
}

// **新しい guard の両方を固定する (#3021 のレビュー H1)。** 読み直しそのものを
// 消す変異は既存テストが捕まえるが、「読み直しは残すが、読み直し自身が失敗した
// ときの分岐だけ消す」形は素通りしていた (実測)。
func TestRun_KeepsExistingWhenLookupsFail(t *testing.T) {
	t.Run("既存行の lookup が DB 障害", func(t *testing.T) {
		repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
		repo.findByNameErr = errors.New("connection reset")
		deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
		deps.EmojiRepo = repo

		res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
		require.NoError(t, err)
		assert.Equal(t, 0, res.Imported)
		assert.Equal(t, []string{"old"}, repo.ids(),
			"同名か分からないのに作っている (同名が 2 つ残る)")
	})

	t.Run("削除の読み直しが DB 障害", func(t *testing.T) {
		repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
		repo.deleteErr = errors.New("boom")
		repo.findByIDErr = errors.New("connection reset")
		deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
		deps.EmojiRepo = repo

		res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
		require.NoError(t, err)
		assert.Equal(t, 0, res.Imported)
		assert.Equal(t, []string{"old"}, repo.ids(),
			"消せたか分からないのに作っている (同名が 2 つ残る)")
	})
}

// 戻す側の `Create` も失敗したらログに残す (握り潰すと、消えた絵文字を追う
// 手がかりが無くなる)。
func TestRun_LogsWhenRestoreFails(t *testing.T) {
	repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
	repo.createErr = errors.New("boom")
	repo.createAlwaysErrs = true
	deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	deps.EmojiRepo = repo

	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Imported)
	assert.Contains(t, logs.String(), "failed to restore the replaced emoji",
		"戻せなかったことがログに残っていない")
	assert.Contains(t, logs.String(), "emojiId=old")
}

// **削除が載っていた直後に `Create` が失敗しても戻す (#3021 のレビュー H1)。**
// 2 つの窓の積 — 削除の ack だけ失われ、続く作成も落ちる形。`replaced` を
// 「削除が成功したときだけ」に閉じると、ここで元の絵文字が消えたままになる。
func TestRun_RestoresWhenDeleteLandedAndCreateFails(t *testing.T) {
	repo := newRowRepo(&model.Emoji{ID: "old", Name: existingName})
	repo.deleteErr = errors.New("boom")
	repo.deleteLands = true
	repo.createErr = errors.New("boom")
	deps, _, _, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
	deps.EmojiRepo = repo

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Imported)
	assert.Equal(t, []string{"old"}, repo.ids(), "消した行が戻っていない")
}

// **弾いたレコードでは drive に何も作らない。** 検証を `replaceEmoji` まで
// 持っていくと、弾くたびに孤児の drive ファイルが残る (レビュー M2)。
func TestRun_RejectedRecordUploadsNothing(t *testing.T) {
	deps, _, _, _ := newDeps(t, importOne(t, map[string]any{
		"name": existingName, "category": strings.Repeat("あ", catLimit+1),
	}))
	// drive の行を数えたいので、uploader をこちらで組み直す。
	files := testutil.NewMockDriveFileRepository()
	folders := testutil.NewMockDriveFolderRepository()
	folders.FilesRef = files
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	deps.Uploader = drive.NewService(files, folders,
		drive.NewLocalStorage(t.TempDir(), "https://example.com/files"), idGen)

	res, err := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	require.Equal(t, 1, res.Skipped)
	assert.Empty(t, files.Files, "弾いたレコードのために drive ファイルを作っている")
}

// storedURL returns a URL of exactly n runes.
func storedURL(n int) string {
	const prefix = "https://s3.example/"
	return prefix + strings.Repeat("a", n-len(prefix))
}

// varyingStorage は Put のたびに別の URL を返す (本体 → webpublic の順)。
//
// **既定の構成では webpublic が作られない**ので `publicUrl` に入るのは `url` その
// もので、publicUrl 側の判定が url 側を包含してしまう。url 側の条件を外した変異を
// 捕まえるために、長さの違う URL を返す保存先を作る (レビュー M1)。
type varyingStorage struct {
	drive.Storage
	urls []string
	n    int
}

func (s *varyingStorage) Put(accessKey string, body io.Reader) (string, error) {
	if _, err := s.Storage.Put(accessKey, body); err != nil {
		return "", err
	}
	u := s.urls[len(s.urls)-1]
	if s.n < len(s.urls) {
		u = s.urls[s.n]
	}
	s.n++
	return u, nil
}

// webpublicOnlyProcessor は webpublic だけを作る (サムネイルは作らない)。
type webpublicOnlyProcessor struct{ drive.ImageProcessor }

func (webpublicOnlyProcessor) GenerateThumbnail([]byte, string) (*drive.ProcessedImage, error) {
	return nil, nil
}

func (webpublicOnlyProcessor) GenerateWebpublic(body []byte, _ string) (*drive.ProcessedImage, error) {
	return &drive.ProcessedImage{Data: body, MimeType: "image/webp"}, nil
}

// 保存先の URL が `emoji.originalUrl` / `publicUrl` に入らないレコードは skip する
// (#3023)。**元の絵文字は消さない** — 取り込みの後・削除の前で弾く。
//
// **上限は数字で作る。** `LocalStorage.Put` が返すのは `<base>/<accessKey>` で、
// `accessKey` は 32 桁の hex 固定なので、base の長さから URL の長さが決まる。
// 512 ちょうどは通り、1 文字超えると skip する形にしてあるので、`emojiURLMaxRunes`
// を動かす変異はここで落ちる (DDL との突き合わせは
// `internal/repository/emoji_column_limits_test.go`)。
func TestRun_ChecksStoredURLAgainstEmojiColumns(t *testing.T) {
	const accessKeyLen = 32 // hex 32 桁 (`newAccessKey`)
	for _, tc := range []struct {
		name     string
		urlRunes int
		imported int
	}{
		{"上限ちょうどは通す", urlLimit, 1},
		{"1 文字超過は skip する", urlLimit + 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, repo, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
			// 長い prefix のオブジェクトストレージ構成を模す (base URL が長い)。
			files := testutil.NewMockDriveFileRepository()
			folders := testutil.NewMockDriveFolderRepository()
			folders.FilesRef = files
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			const prefix = "https://s3.example/"
			base := prefix + strings.Repeat("a", tc.urlRunes-len(prefix)-1-accessKeyLen)
			deps.Uploader = drive.NewService(files, folders,
				drive.NewLocalStorage(t.TempDir(), base), idGen)
			require.NoError(t, repo.Create(&model.Emoji{
				ID: "old", Name: existingName, OriginalURL: "https://example/old.png",
			}))

			res, rerr := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
			require.NoError(t, rerr)
			assert.Equal(t, tc.imported, res.Imported)
			found, ferr := repo.FindByNameAndHost(existingName, nil)
			require.NoError(t, ferr, "既存の絵文字が消えている")
			if tc.imported == 1 {
				assert.NotEqual(t, "old", found.ID, "取り込めるはずの URL で skip している")
				assert.Equal(t, tc.urlRunes, len([]rune(found.OriginalURL)),
					"想定した長さの URL になっていない (テストの前提が崩れている)")
				return
			}
			assert.Equal(t, 1, res.Skipped)
			assert.Equal(t, "old", found.ID, "弾いたのに既存の絵文字が差し替わっている")
		})
	}
}

// **片方だけが超過する形**を両向きとも固定する (#3023 のレビュー M1 / M2)。
//
// 既定の構成では webpublic が作られず `publicUrl` は `url` と同じ値になるので、
// どちらか一方の条件を外した変異が素通りする。`emoji.originalUrl` に入るのは `url`
// そのもの (#722 の不変条件)、`publicUrl` に入るのは webpublic なので両方見る。
func TestRun_SkipsWhenEitherStoredURLDoesNotFit(t *testing.T) {
	for _, tc := range []struct {
		name string
		// 1 回目の Put = 本体、2 回目 = webpublic。
		urls []string
	}{
		{"url だけ超過 (webpublic は収まる)", []string{storedURL(urlLimit + 1), storedURL(urlLimit)}},
		{"webpublic だけ超過 (url は収まる)", []string{storedURL(urlLimit), storedURL(urlLimit + 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, repo, _ := newDeps(t, importOne(t, map[string]any{"name": existingName}))
			files := testutil.NewMockDriveFileRepository()
			folders := testutil.NewMockDriveFolderRepository()
			folders.FilesRef = files
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			svc := drive.NewService(files, folders, &varyingStorage{
				Storage: drive.NewLocalStorage(t.TempDir(), "https://example.com/files"),
				urls:    tc.urls,
			}, idGen)
			svc.SetImageProcessor(webpublicOnlyProcessor{drive.NewDefaultImageProcessor()})
			deps.Uploader = svc
			require.NoError(t, repo.Create(&model.Emoji{
				ID: "old", Name: existingName, OriginalURL: "https://example/old.png",
			}))

			res, rerr := emojiimport.NewImporter(deps).Run(context.Background(), "admin", "f1")
			require.NoError(t, rerr)
			assert.Equal(t, 0, res.Imported)
			assert.Equal(t, 1, res.Skipped)
			found, ferr := repo.FindByNameAndHost(existingName, nil)
			require.NoError(t, ferr, "弾いたのに既存の絵文字が消えている")
			assert.Equal(t, "old", found.ID)
		})
	}
}
