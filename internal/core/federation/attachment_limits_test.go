package federation_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// fkOnceDriveRepo fails the first Create with a foreign_key_violation when the
// row carries a userId, mirroring `drive_file_userId_fkey` against an author
// that was never materialized.
type fkOnceDriveRepo struct {
	*testutil.MockDriveFileRepository
	attempts []*model.DriveFile
}

func (c *fkOnceDriveRepo) Create(f *model.DriveFile) error {
	// 呼び出しごとの値を記録する (retry で userId が落ちたか見るため)。
	snap := *f
	c.attempts = append(c.attempts, &snap)
	if f.UserID != nil {
		return &pgconn.PgError{Code: "23503", Message: `insert or update on table "drive_file" violates foreign key constraint "drive_file_userId_fkey"`}
	}
	return c.MockDriveFileRepository.Create(f)
}

// 代替テキストが長い添付が保存されること (#2717)。
//
// Mastodon は AP の `attachment[].name` に**代替テキスト**を入れてくる。入る先は
// `drive_file.comment` (varchar(512))、切らずに入れると 22001 で落ちて**その添付が
// 丸ごと保存されない**。`name` は URL の basename から作るので、この経路では
// 代替テキストの長さに影響されない (#2723)。
func TestUpsertAttachments_TruncatesLongAltText(t *testing.T) {
	drive := testutil.NewMockDriveFileRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), &stubFetcher{}, idGen)
	r.SetDriveFileRepo(drive)

	// 全角で 700 文字。byte で切ると壊れた UTF-8 になる形にしておく。
	// **先頭と末尾を別の文字にする。** 同じ文字の繰り返しだと「末尾から切る」
	// 実装でも prefix 判定が通ってしまう (#2721 review LOW-2)。
	alt := "さき" + strings.Repeat("あ", 700) + "おわり"
	userID, host := "ru", "remote.example"
	ids := r.UpsertAttachments([]activitypub.Document{{
		URL: "https://media.example/files/long.mp4", MediaType: "video/mp4", Name: alt,
	}}, &userID, &host)

	require.Len(t, ids, 1, "長い alt text で添付が落ちている")
	f := drive.Files[ids[0]]
	require.NotNil(t, f)
	// name は代替テキストではなく URL の basename (#2723)。長い alt text で
	// 列が溢れる経路そのものが無くなっている。
	assert.Equal(t, "long.mp4", f.Name)
	require.NotNil(t, f.Comment)
	assert.Equal(t, 512, len([]rune(*f.Comment)), "comment が列の上限 (rune) で切られていない")
	assert.True(t, strings.HasPrefix(*f.Comment, "さき"), "先頭が落ちている")
	assert.True(t, utf8.ValidString(*f.Comment), "rune 境界で切れていない")
	// **先頭一致も見る。** 長さと UTF-8 妥当性だけだと「末尾から切る」実装を
	// 見逃す (#2721 review LOW-2)。
	assert.True(t, strings.HasPrefix(alt, *f.Comment), "comment を先頭から切っていない")
}

// 著者が materialize されていない (= user 行が無い) 添付でも保存されること
// (#2717)。
//
// リレー由来の投稿は著者の user 行を作らない (#2332) ので `drive_file.userId` の
// FK に当たる。ここで諦めると**その添付が表示されなくなる** — 添付は pack 時に
// DB から引くため、ephemeral note でも行が要る。
func TestUpsertAttachments_FallsBackToUnownedOnForeignKeyViolation(t *testing.T) {
	drive := &fkOnceDriveRepo{MockDriveFileRepository: testutil.NewMockDriveFileRepository()}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), &stubFetcher{}, idGen)
	r.SetDriveFileRepo(drive)

	userID, host := "ghost", "remote.example"
	ids := r.UpsertAttachments([]activitypub.Document{{
		URL: "https://media.example/files/a.webp", MediaType: "image/webp",
	}}, &userID, &host)

	require.Len(t, ids, 1, "FK 違反で添付が落ちている")
	require.Len(t, drive.attempts, 2, "owner 無しで作り直していない")
	require.NotNil(t, drive.attempts[0].UserID)
	assert.Nil(t, drive.attempts[1].UserID, "retry でも userId を付けている")

	saved := drive.Files[ids[0]]
	require.NotNil(t, saved)
	assert.Nil(t, saved.UserID)
	require.NotNil(t, saved.UserHost)
	assert.Equal(t, host, *saved.UserHost, "userHost は残すこと (instance 単位の purge が効く)")
}

// FK 違反**以外**では owner を落として作り直さないこと (#2717)。
//
// error 種別を見ずに retry すると、たまたま owner 無しなら通る別の失敗
// (列長超過など) で**所有者情報を黙って捨てる**。ここは「userId があると失敗し、
// 無ければ成功する」repo を置いて、retry していないことを見る。
type nonFKOnOwnerDriveRepo struct {
	*testutil.MockDriveFileRepository
	attempts int
}

func (c *nonFKOnOwnerDriveRepo) Create(f *model.DriveFile) error {
	c.attempts++
	if f.UserID != nil {
		return &pgconn.PgError{Code: "22001", Message: "value too long"}
	}
	return c.MockDriveFileRepository.Create(f)
}

func TestUpsertAttachments_OtherErrorsDoNotDropOwner(t *testing.T) {
	drive := &nonFKOnOwnerDriveRepo{MockDriveFileRepository: testutil.NewMockDriveFileRepository()}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), &stubFetcher{}, idGen)
	r.SetDriveFileRepo(drive)

	userID, host := "ru", "remote.example"
	ids := r.UpsertAttachments([]activitypub.Document{{
		URL: "https://media.example/files/b.webp", MediaType: "image/webp",
	}}, &userID, &host)

	assert.Empty(t, ids, "FK 違反でないのに owner を落として作り直している")
	assert.Equal(t, 1, drive.attempts, "retry してはいけない")
}

// NUL を含む alt text でも添付が落ちないこと (#2721 review MEDIUM-1)。
//
// 長さだけ直しても、制御文字が混じると PostgreSQL が 22021 で弾いて同じく
// 添付が丸ごと消える。
func TestUpsertAttachments_StripsNULFromAltText(t *testing.T) {
	drive := testutil.NewMockDriveFileRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), &stubFetcher{}, idGen)
	r.SetDriveFileRepo(drive)

	userID, host := "ru", "remote.example"
	ids := r.UpsertAttachments([]activitypub.Document{{
		URL: "https://media.example/files/nul.webp", MediaType: "image/webp", Name: "al\x00t",
	}}, &userID, &host)

	require.Len(t, ids, 1)
	f := drive.Files[ids[0]]
	require.NotNil(t, f)
	assert.NotContains(t, f.Name, "\x00", "NUL が残っている")
	require.NotNil(t, f.Comment)
	assert.NotContains(t, *f.Comment, "\x00")
}

// 添付の他の列も溢れさせないこと (#2723)。
//
// #2717 で `name` を直したが、**同じ INSERT に載る他の列**が無検査だと同じ症状
// (その添付が丸ごと消える) が残る。列ごとに扱いが違う:
//   - `url` (varchar(1024) NOT NULL) — 実体そのもの。入らなければ添付ごと諦める
//   - `type` (varchar(128)) — 切ると別の MIME type になるので既定値に倒す
//   - `thumbnailUrl` (512) / `blurhash` (128) — 表示の補助なので値ごと捨てる
func TestUpsertAttachments_DropsOversizedColumns(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	newResolver := func(drive *testutil.MockDriveFileRepository) *federation.Resolver {
		r := federation.NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
			activitypub.NewURLBuilder("https://example.com"), &stubFetcher{}, idGen)
		r.SetDriveFileRepo(drive)
		return r
	}
	userID, host := "ru", "remote.example"

	t.Run("url over 1024 は添付ごと落とす", func(t *testing.T) {
		drive := testutil.NewMockDriveFileRepository()
		longURL := "https://media.example/f/" + strings.Repeat("a", 1024)
		ids := newResolver(drive).UpsertAttachments([]activitypub.Document{{
			URL: longURL, MediaType: "image/png",
		}}, &userID, &host)
		assert.Empty(t, ids, "列に入らない url の添付を保存している")
		assert.Empty(t, drive.Files)
	})

	t.Run("type over 128 は既定値に倒す", func(t *testing.T) {
		drive := testutil.NewMockDriveFileRepository()
		ids := newResolver(drive).UpsertAttachments([]activitypub.Document{{
			URL: "https://media.example/f/a.bin", MediaType: "image/" + strings.Repeat("x", 200),
		}}, &userID, &host)
		require.Len(t, ids, 1, "長い mediaType で添付が落ちている")
		// **切った値を入れない。** 切ると存在しない MIME type になる。
		assert.Equal(t, "application/octet-stream", drive.Files[ids[0]].Type)
	})

	t.Run("thumbnailUrl / blurhash は値ごと捨てる", func(t *testing.T) {
		drive := testutil.NewMockDriveFileRepository()
		thumb := "https://media.example/t/" + strings.Repeat("a", 512)
		ids := newResolver(drive).UpsertAttachments([]activitypub.Document{{
			URL:       "https://media.example/f/b.png",
			MediaType: "image/png",
			Icon:      &activitypub.Image{URL: activitypub.APLenientHref(thumb)},
			Blurhash:  strings.Repeat("b", 200),
		}}, &userID, &host)
		require.Len(t, ids, 1, "長い thumbnail / blurhash で添付が落ちている")
		f := drive.Files[ids[0]]
		assert.Nil(t, f.ThumbnailURL, "列に入らない thumbnailUrl を保存している")
		assert.Nil(t, f.Blurhash, "列に入らない blurhash を保存している")
	})
}
