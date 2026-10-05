package repository

import (
	"errors"
	"sync"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// usageDB は集計テスト専用の兄弟 schema (`internal_repository_driveusage`)。
//
// **shared な schema では書けない。** 集計はインスタンス全体の**絶対値**を返すので、
// 同じパッケージの他テストが残した drive_file / user / emoji の行が混ざると期待値が
// 成立しない。かといって shared schema で `DELETE FROM "drive_file"` をすると、行の
// 残り方に依存している他テストを壊す (docs/testing.md「DB を使うテストの分離」の双方向
// 干渉そのもの)。専用 schema を一度だけ作って使い回す。
var (
	usageDB     *gorm.DB
	usageDBOnce sync.Once
	usageDBErr  error
)

func driveUsageDB(t *testing.T) *gorm.DB {
	t.Helper()
	usageDBOnce.Do(func() {
		db, err := testutil.OpenTestDBSchema("driveusage")
		if err != nil {
			usageDBErr = err
			return
		}
		testutil.ApplyMigrations(db)
		usageDB = db
	})
	require.NoError(t, usageDBErr)
	require.NotNil(t, usageDB)
	return usageDB
}

// resetDriveUsageFixtures empties every table the aggregation reads. 自分の schema に
// 閉じているので無条件の DELETE でよい (search_path を跨がない)。
func resetDriveUsageFixtures(t *testing.T, db *gorm.DB) {
	t.Helper()
	// user は drive_file を avatarId / bannerId で指すので、先に参照を外す。
	require.NoError(t, db.Exec(`UPDATE "user" SET "avatarId" = NULL, "bannerId" = NULL`).Error)
	require.NoError(t, db.Exec(`DELETE FROM "drive_file"`).Error)
	require.NoError(t, db.Exec(`DELETE FROM "emoji"`).Error)
	require.NoError(t, db.Exec(`DELETE FROM "user"`).Error)
}

// usageUser inserts a user row. host nil = local.
func usageUser(t *testing.T, db *gorm.DB, id, username string, host *string) *model.User {
	t.Helper()
	u := &model.User{
		ID:                id,
		Username:          username,
		UsernameLower:     username,
		Host:              host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, db.Create(u).Error)
	return u
}

// usageFile inserts a drive_file row. url は id から決まる。
func usageFile(t *testing.T, db *gorm.DB, id string, userID, userHost *string, size int, isLink bool) *model.DriveFile {
	t.Helper()
	f := &model.DriveFile{
		ID:             id,
		UserID:         userID,
		UserHost:       userHost,
		MD5:            id,
		Name:           id + ".bin",
		Type:           "application/octet-stream",
		Size:           size,
		URL:            "https://example.test/files/" + id,
		IsLink:         isLink,
		Properties:     datatypes.JSON([]byte("{}")),
		RequestHeaders: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, db.Create(f).Error)
	return f
}

// usageEmoji inserts an emoji row pointing at the given URLs.
func usageEmoji(t *testing.T, db *gorm.DB, id, originalURL, publicURL string) {
	t.Helper()
	require.NoError(t, db.Create(&model.Emoji{
		ID: id, Name: id, OriginalURL: originalURL, PublicURL: publicURL,
	}).Error)
}

func strptr(s string) *string { return &s }

// bucketOf returns the (origin, kind) cell from the breakdown.
func bucketOf(t *testing.T, b *DriveUsageBreakdown, origin, kind string) DriveUsageBucket {
	t.Helper()
	for _, row := range b.ByKind {
		if row.Origin == origin && row.Kind == kind {
			return row.DriveUsageBucket
		}
	}
	t.Fatalf("%s/%s の行が無い", origin, kind)
	return DriveUsageBucket{}
}

func userIDsOf(rows []DriveUsageUserRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UserID)
	}
	return out
}

// seedDriveUsage builds the fixture shared by the breakdown assertions.
//
// 形 (ローカル):
//   - alice: 添付 100 / アイコン 300 / link 行 0 (= 合計 400、3 件、うち実体なし 1)
//   - bob:   添付 50 / バナー 70 / **絵文字が参照している自分のファイル** 77 (= 197、3 件)
//   - c1:    アイコン 11 (**c1 と c2 の両方が同じファイルをアイコンにしている**)
//   - アイコン (c1) とバナー (bob) は**絵文字からも参照させてある** (優先順の検証用)
//   - c2:    ファイルは持たない (c1 のファイルを指すだけ)
//   - c3:    5000 x 2 = 10000、2 件
//   - c4:    9000、1 件
//   - 所有者なし: 絵文字 500 / 絵文字 (originalUrl == publicUrl) 123 / 取り込み残り 900
//
// **上位の 2 人 (c3 / c4) を最後に入れてある。** 上位 N を決めているのは副問い合わせ側の
// ORDER BY で、外側は並べ直すだけ。順序を落とすと LIMIT は「走査順に N 件」を取るので、
// 大きい利用者を後ろに置いておかないと変異が検出できない (実測で先に入れた順に返る)。
//
// 形 (リモート):
//   - a.example: link 行 1 件 (owner あり) + 実体つき 1234 (TS 由来)
//   - b.example: link 行 1 件 (owner なし)
func seedDriveUsage(t *testing.T) *gorm.DB {
	t.Helper()
	db := driveUsageDB(t)
	resetDriveUsageFixtures(t, db)

	alice := usageUser(t, db, "duu_alice", "alice", nil)
	bob := usageUser(t, db, "duu_bob", "bob", nil)
	c1 := usageUser(t, db, "duu_c1", "c1", nil)
	c2 := usageUser(t, db, "duu_c2", "c2", nil)
	usageUser(t, db, "duu_carol", "carol", strptr("a.example"))

	usageFile(t, db, "duf_alice_att", &alice.ID, nil, 100, false)
	avatar := usageFile(t, db, "duf_alice_avatar", &alice.ID, nil, 300, false)
	// ローカル所有の link 行。ByUser の linkCount が定数でないことを見るために要る
	// (TS 製 DB から引き継いだ形)。
	usageFile(t, db, "duf_alice_link", &alice.ID, nil, 0, true)
	usageFile(t, db, "duf_bob_att", &bob.ID, nil, 50, false)
	banner := usageFile(t, db, "duf_bob_banner", &bob.ID, nil, 70, false)
	// **利用者が持っているのに絵文字が参照しているファイル。** #3014 より前に差し替えた
	// 絵文字と TS 由来の DB にある形で、emoji と attachment の優先順がここでだけ効く。
	userEmoji := usageFile(t, db, "duf_bob_emoji", &bob.ID, nil, 77, false)
	shared := usageFile(t, db, "duf_shared_avatar", &c1.ID, nil, 11, false)

	require.NoError(t, db.Model(&model.User{}).Where("id = ?", alice.ID).
		Update("avatarId", avatar.ID).Error)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", bob.ID).
		Update("bannerId", banner.ID).Error)
	// 2 人が同じファイルをアイコンにしている。`DISTINCT` が無いと LEFT JOIN が
	// drive_file の行を複製し、件数も使用量も 2 倍に数える。
	require.NoError(t, db.Model(&model.User{}).Where("id IN ?", []string{c1.ID, c2.ID}).
		Update("avatarId", shared.ID).Error)

	emojiFile := usageFile(t, db, "duf_emoji", nil, nil, 500, false)
	usageEmoji(t, db, "due_1", emojiFile.URL, "")
	// **アイコン / バナーを絵文字も参照している形。** 自分が上げた画像を絵文字に
	// しつつアイコンにも使う運用と、TS 由来の DB にある。優先順が avatar / banner
	// より emoji を上にしていると、この 2 件が emoji 側へ移る。
	usageEmoji(t, db, "due_shared_avatar", shared.URL, shared.URL)
	usageEmoji(t, db, "due_bob_banner", banner.URL, banner.URL)
	// **webpublic variant を持たない絵文字は originalUrl == publicUrl になる。**
	// `UNION` を `UNION ALL` にすると、この形だけが二重計上になる。
	sameURL := usageFile(t, db, "duf_emoji_same", nil, nil, 123, false)
	usageEmoji(t, db, "due_same", sameURL.URL, sameURL.URL)
	usageEmoji(t, db, "due_bob", userEmoji.URL, userEmoji.URL)
	usageFile(t, db, "duf_leftover", nil, nil, 900, false)

	// 上位になる利用者は最後に入れる (上の doc コメント参照)。
	c3 := usageUser(t, db, "duu_c3", "c3", nil)
	c4 := usageUser(t, db, "duu_c4", "c4", nil)
	usageFile(t, db, "duf_c3_a", &c3.ID, nil, 5000, false)
	usageFile(t, db, "duf_c3_b", &c3.ID, nil, 5000, false)
	usageFile(t, db, "duf_c4_a", &c4.ID, nil, 9000, false)

	usageFile(t, db, "duf_remote_link", strptr("duu_carol"), strptr("a.example"), 0, true)
	usageFile(t, db, "duf_remote_cached", nil, strptr("a.example"), 1234, false)
	usageFile(t, db, "duf_remote_b", nil, strptr("b.example"), 0, true)

	return db
}

func TestDriveUsageRepository_BreakdownByKind(t *testing.T) {
	db := seedDriveUsage(t)
	repo := NewDriveUsageRepository(db)

	b, err := repo.Breakdown(10)
	require.NoError(t, err)

	// 種類 x origin は 0 埋めで必ず全件返る。
	require.Len(t, b.ByKind, 10)

	// attachment = 利用者所有で avatar / banner / emoji のいずれでもないもの。
	// alice 2 (100 + link 0) + bob 1 (50) + c3 2 (10000) + c4 1 (9000)。
	assert.Equal(t, DriveUsageBucket{Count: 6, Size: 19150, LinkCount: 1},
		bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindAttachment))
	// **どちらも絵文字からも参照されているが avatar / banner 側に数える。**
	// emoji を上に置くと 2 件とも emoji へ移り、ここが落ちる。
	assert.Equal(t, DriveUsageBucket{Count: 2, Size: 311},
		bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindAvatar))
	assert.Equal(t, DriveUsageBucket{Count: 1, Size: 70},
		bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindBanner))
	// 所有者なし 2 件 (500 + 123) と、bob が持っている 77。
	assert.Equal(t, DriveUsageBucket{Count: 3, Size: 700},
		bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindEmoji))
	assert.Equal(t, DriveUsageBucket{Count: 1, Size: 900},
		bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindOther))

	// リモートの owner ありは attachment、owner 無しは other。
	assert.Equal(t, DriveUsageBucket{Count: 1, Size: 0, LinkCount: 1},
		bucketOf(t, b, DriveUsageOriginRemote, DriveUsageKindAttachment))
	assert.Equal(t, DriveUsageBucket{Count: 2, Size: 1234, LinkCount: 1},
		bucketOf(t, b, DriveUsageOriginRemote, DriveUsageKindOther))
	assert.Equal(t, DriveUsageBucket{}, bucketOf(t, b, DriveUsageOriginRemote, DriveUsageKindAvatar))
	assert.Equal(t, DriveUsageBucket{}, bucketOf(t, b, DriveUsageOriginRemote, DriveUsageKindBanner))
	assert.Equal(t, DriveUsageBucket{}, bucketOf(t, b, DriveUsageOriginRemote, DriveUsageKindEmoji))

	// 合計は内訳の和で、全行をちょうど 1 回ずつ数えている。
	assert.Equal(t, DriveUsageBucket{Count: 13, Size: 21131, LinkCount: 1}, b.LocalTotal())
	assert.Equal(t, DriveUsageBucket{Count: 3, Size: 1234, LinkCount: 2}, b.RemoteTotal())
	assert.Equal(t, DriveUsageBucket{Count: 16, Size: 22365, LinkCount: 3}, b.Total())

	// **行数との突き合わせがこのテストの背骨。** 重複排除 (emoji の UNION、
	// avatar / banner の DISTINCT) が落ちると、ここだけが破れる。
	var rows int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM "drive_file"`).Scan(&rows).Error)
	assert.Equal(t, rows, b.Total().Count, "内訳の合計が drive_file の行数と一致しない")
}

// アバターとバナーが同じ file を指しているときは avatar 側にだけ数える。
// 優先順が無いと 1 つの file が 2 箇所に乗り、合計が行数を超える。
func TestDriveUsageRepository_BreakdownKindPriority(t *testing.T) {
	db := driveUsageDB(t)
	resetDriveUsageFixtures(t, db)

	u := usageUser(t, db, "dup_u", "dup", nil)
	f := usageFile(t, db, "dup_f", &u.ID, nil, 42, false)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", u.ID).
		Updates(map[string]any{"avatarId": f.ID, "bannerId": f.ID}).Error)

	b, err := NewDriveUsageRepository(db).Breakdown(10)
	require.NoError(t, err)

	assert.Equal(t, DriveUsageBucket{Count: 1, Size: 42},
		bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindAvatar))
	assert.Equal(t, DriveUsageBucket{}, bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindBanner))
	assert.Equal(t, DriveUsageBucket{Count: 1, Size: 42}, b.Total())
}

// 絵文字は publicUrl 側の一致でも拾う。あわせて **空の URL が「url が空の
// drive_file」と結合しない**ことを両方の列で見る (`emoji.publicUrl` は DEFAULT ”、
// `originalUrl` は NOT NULL だが空文字自体は入れられる)。
func TestDriveUsageRepository_BreakdownEmojiMatchesPublicURL(t *testing.T) {
	db := driveUsageDB(t)
	resetDriveUsageFixtures(t, db)

	viaPublic := usageFile(t, db, "due_pub", nil, nil, 11, false)
	usageEmoji(t, db, "due_p", "https://example.test/other", viaPublic.URL)

	// url が空の行 + 空の originalUrl / publicUrl を持つ絵文字。空同士で結合すると
	// この行が emoji に化ける。
	blank := &model.DriveFile{
		ID: "due_blank", MD5: "b", Name: "b", Type: "application/octet-stream",
		Size: 7, URL: "", Properties: datatypes.JSON([]byte("{}")),
		RequestHeaders: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, db.Create(blank).Error)
	usageEmoji(t, db, "due_blankpublic", "https://example.test/x", "")
	usageEmoji(t, db, "due_blankoriginal", "", "https://example.test/y")

	b, err := NewDriveUsageRepository(db).Breakdown(10)
	require.NoError(t, err)

	assert.Equal(t, DriveUsageBucket{Count: 1, Size: 11},
		bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindEmoji))
	assert.Equal(t, DriveUsageBucket{Count: 1, Size: 7},
		bucketOf(t, b, DriveUsageOriginLocal, DriveUsageKindOther))
}

func TestDriveUsageRepository_BreakdownRankings(t *testing.T) {
	db := seedDriveUsage(t)
	repo := NewDriveUsageRepository(db)

	b, err := repo.Breakdown(10)
	require.NoError(t, err)

	require.Len(t, b.ByHost, 2)
	assert.Equal(t, DriveUsageHostRow{
		Host: "a.example", DriveUsageBucket: DriveUsageBucket{Count: 2, Size: 1234, LinkCount: 1},
	}, b.ByHost[0])
	assert.Equal(t, DriveUsageHostRow{
		Host: "b.example", DriveUsageBucket: DriveUsageBucket{Count: 1, LinkCount: 1},
	}, b.ByHost[1])

	require.Len(t, b.ByUser, 5)
	assert.Equal(t, []string{"duu_c3", "duu_c4", "duu_alice", "duu_bob", "duu_c1"}, userIDsOf(b.ByUser))
	assert.Equal(t, DriveUsageUserRow{
		UserID: "duu_c3", Username: "c3",
		DriveUsageBucket: DriveUsageBucket{Count: 2, Size: 10000},
	}, b.ByUser[0])
	// 利用者ごとの件数は種類を問わず自分の file 全部。alice は link 行を 1 つ持つ。
	assert.Equal(t, DriveUsageUserRow{
		UserID: "duu_alice", Username: "alice",
		DriveUsageBucket: DriveUsageBucket{Count: 3, Size: 400, LinkCount: 1},
	}, b.ByUser[2])
	// 所有していない利用者 (c2) は並ばない。
	assert.NotContains(t, userIDsOf(b.ByUser), "duu_c2")
}

// **上位 N が「並べてから切った」結果であること。** 副問い合わせの ORDER BY を落とすと、
// 走査順に N 件を取ったうえで外側が整列するので、順序だけを見るアサートは素通りする。
// 選ばれた**集合**を見る。
func TestDriveUsageRepository_BreakdownPicksTheLargestUsers(t *testing.T) {
	db := seedDriveUsage(t)
	repo := NewDriveUsageRepository(db)

	b, err := repo.Breakdown(2)
	require.NoError(t, err)
	assert.Equal(t, []string{"duu_c3", "duu_c4"}, userIDsOf(b.ByUser))

	b, err = repo.Breakdown(3)
	require.NoError(t, err)
	assert.Equal(t, []string{"duu_c3", "duu_c4", "duu_alice"}, userIDsOf(b.ByUser))
}

// 上位 N は実際に効く。効いていないと大きいインスタンスで応答が青天井になる。
func TestDriveUsageRepository_BreakdownTopNCaps(t *testing.T) {
	db := seedDriveUsage(t)
	repo := NewDriveUsageRepository(db)

	b, err := repo.Breakdown(1)
	require.NoError(t, err)
	require.Len(t, b.ByHost, 1)
	assert.Equal(t, "a.example", b.ByHost[0].Host)
	require.Len(t, b.ByUser, 1)
	assert.Equal(t, "duu_c3", b.ByUser[0].UserID)

	// 0 以下でも 1 件は返す (0 件だと「ホストが無い」と読めてしまう)。
	b, err = repo.Breakdown(0)
	require.NoError(t, err)
	assert.Len(t, b.ByHost, 1)
	assert.Len(t, b.ByUser, 1)

	// 上限を超える指定は黙って丸める。内訳は変わらない。
	b, err = repo.Breakdown(DriveUsageMaxTopN + 1000)
	require.NoError(t, err)
	assert.Len(t, b.ByHost, 2)
	assert.Len(t, b.ByUser, 5)
}

// size が同点 (mk-go のリモート行は全部 0) のとき、件数の多いホストを先に出す。
// tiebreaker が host 名だけだと、実体を持たない行を大量に抱えたホストが名前順に埋もれる。
func TestDriveUsageRepository_BreakdownOrderIsDeterministic(t *testing.T) {
	db := driveUsageDB(t)
	resetDriveUsageFixtures(t, db)

	usageFile(t, db, "duo_a", nil, strptr("a.example"), 0, true)
	usageFile(t, db, "duo_b", nil, strptr("b.example"), 0, true)
	usageFile(t, db, "duo_c1", nil, strptr("c.example"), 0, true)
	usageFile(t, db, "duo_c2", nil, strptr("c.example"), 0, true)

	repo := NewDriveUsageRepository(db)
	for i := 0; i < 3; i++ {
		b, err := repo.Breakdown(10)
		require.NoError(t, err)
		require.Len(t, b.ByHost, 3)
		assert.Equal(t, []string{"c.example", "a.example", "b.example"},
			[]string{b.ByHost[0].Host, b.ByHost[1].Host, b.ByHost[2].Host})
	}
}

// 利用者側も同じ。size が同点なら件数の多い利用者が先。
//
// **上位 N で切れる形で見る。** 並び順だけを見ると外側の ORDER BY が同じ並べ替えを
// するので、副問い合わせ側の tiebreaker を落としても通ってしまう。件数の少ない方の
// ID を若くしておくと、tiebreaker が無いときに**選ばれる利用者そのもの**が変わる。
func TestDriveUsageRepository_BreakdownUserTiebreakIsCount(t *testing.T) {
	db := driveUsageDB(t)
	resetDriveUsageFixtures(t, db)

	top := usageUser(t, db, "dut_top", "top", nil)
	few := usageUser(t, db, "dut_a_few", "few", nil)
	many := usageUser(t, db, "dut_b_many", "many", nil)
	usageFile(t, db, "dut_top_a", &top.ID, nil, 900, false)
	usageFile(t, db, "dut_few_a", &few.ID, nil, 50, false)
	usageFile(t, db, "dut_many_a", &many.ID, nil, 25, false)
	usageFile(t, db, "dut_many_b", &many.ID, nil, 25, false)

	repo := NewDriveUsageRepository(db)
	b, err := repo.Breakdown(10)
	require.NoError(t, err)
	assert.Equal(t, []string{"dut_top", "dut_b_many", "dut_a_few"}, userIDsOf(b.ByUser))

	b, err = repo.Breakdown(2)
	require.NoError(t, err)
	assert.Equal(t, []string{"dut_top", "dut_b_many"}, userIDsOf(b.ByUser))
}

// 行が 1 つも無くても 0 埋めの内訳が返り、集計は 0 になる。
func TestDriveUsageRepository_BreakdownEmpty(t *testing.T) {
	db := driveUsageDB(t)
	resetDriveUsageFixtures(t, db)

	b, err := NewDriveUsageRepository(db).Breakdown(10)
	require.NoError(t, err)
	require.Len(t, b.ByKind, 10)
	assert.Empty(t, b.ByHost)
	assert.Empty(t, b.ByUser)
	assert.Equal(t, DriveUsageBucket{}, b.Total())
}

// 上位 N の丸め。上限側は fixture の件数では観測できないので、丸めそのものを直接見る
// (**丸めが無いと大きいインスタンスで応答が青天井**)。
func TestClampDriveUsageTopN(t *testing.T) {
	assert.Equal(t, 1, clampDriveUsageTopN(0))
	assert.Equal(t, 1, clampDriveUsageTopN(-5))
	assert.Equal(t, 1, clampDriveUsageTopN(1))
	assert.Equal(t, 30, clampDriveUsageTopN(30))
	assert.Equal(t, DriveUsageMaxTopN, clampDriveUsageTopN(DriveUsageMaxTopN))
	assert.Equal(t, DriveUsageMaxTopN, clampDriveUsageTopN(DriveUsageMaxTopN+1))
	assert.Equal(t, DriveUsageMaxTopN, clampDriveUsageTopN(1_000_000))
}

// SQL が壊れていれば err で返る (nil の内訳を返さない)。
//
// **3 本それぞれの包み方を見る。** Breakdown 経由だと最初のクエリが落ちた時点で返るので、
// 2 本目・3 本目の `fmt.Errorf` を取り違えていても気付けない。
func TestDriveUsageRepository_ScansPropagateError(t *testing.T) {
	db := driveUsageDB(t)
	// search_path を空にすると `drive_file` が解決できず、どのクエリも落ちる。
	// トランザクション内の SET LOCAL なので rollback で元に戻る。
	tx := db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	require.NoError(t, tx.Exec(`SET LOCAL search_path TO pg_temp`).Error)
	repo := &driveUsageRepository{db: tx}

	b, err := repo.Breakdown(10)
	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), "drive usage by kind")

	kinds, err := repo.scanKinds()
	require.Error(t, err)
	assert.Nil(t, kinds)
	assert.Contains(t, err.Error(), "drive usage by kind")

	hosts, err := repo.scanHosts(10)
	require.Error(t, err)
	assert.Nil(t, hosts)
	assert.Contains(t, err.Error(), "drive usage by host")

	users, err := repo.scanUsers(10)
	require.Error(t, err)
	assert.Nil(t, users)
	assert.Contains(t, err.Error(), "drive usage by user")
}

// failingScanner fails one of the three queries and succeeds for the others.
type failingScanner struct {
	failKinds, failHosts, failUsers bool
	sawTopN                         int
}

var errScan = errors.New("scan boom")

func (f *failingScanner) scanKinds() ([]driveUsageKindScan, error) {
	if f.failKinds {
		return nil, errScan
	}
	return nil, nil
}

func (f *failingScanner) scanHosts(topN int) ([]driveUsageHostScan, error) {
	f.sawTopN = topN
	if f.failHosts {
		return nil, errScan
	}
	return nil, nil
}

func (f *failingScanner) scanUsers(topN int) ([]driveUsageUserScan, error) {
	f.sawTopN = topN
	if f.failUsers {
		return nil, errScan
	}
	return nil, nil
}

// **3 本のどれが落ちても err で返る。** 実 DB では「2 本目だけ失敗」を作れない
// (同じテーブルを読むので 1 本目が先に落ちる) ので、ここだけ seam を使う。落とすと
// DB 障害が「ホスト 0 件 / 利用者 0 件」の 200 に化ける (#2792)。
func TestDriveUsageBreakdownFrom_PropagatesEveryScanError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scanner *failingScanner
	}{
		{"kinds", &failingScanner{failKinds: true}},
		{"hosts", &failingScanner{failHosts: true}},
		{"users", &failingScanner{failUsers: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := driveUsageBreakdownFrom(tc.scanner, 10)
			require.ErrorIs(t, err, errScan)
			assert.Nil(t, b)
		})
	}

	// 全部成功すれば 0 埋めの内訳が返り、丸めた topN が下へ渡る。
	ok := &failingScanner{}
	b, err := driveUsageBreakdownFrom(ok, DriveUsageMaxTopN+1)
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.Len(t, b.ByKind, 10)
	assert.Equal(t, DriveUsageMaxTopN, ok.sawTopN)
}

// 分類の CASE が定数に無い値を返したら、その行を黙って捨てずに落ちる。捨てると
// 「内訳の合計だけが行数より少ない表」になり、画面からは気付けない。
func TestAssembleDriveUsage_RejectsUnknownKind(t *testing.T) {
	b, err := assembleDriveUsage([]driveUsageKindScan{{Origin: "local", Kind: "brandnew", Cnt: 1}}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), "brandnew")

	b, err = assembleDriveUsage([]driveUsageKindScan{{Origin: "elsewhere", Kind: DriveUsageKindAvatar, Cnt: 1}}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), "elsewhere")
}

// driveUsageKindIndex が知らない組み合わせを弾くこと。
func TestDriveUsageKindIndex_RejectsUnknown(t *testing.T) {
	assert.Equal(t, -1, driveUsageKindIndex("local", "nope"))
	assert.Equal(t, -1, driveUsageKindIndex("nope", DriveUsageKindAvatar))
	assert.Equal(t, 0, driveUsageKindIndex(DriveUsageOriginLocal, DriveUsageKindAttachment))
	assert.Equal(t, len(driveUsageKinds),
		driveUsageKindIndex(DriveUsageOriginRemote, DriveUsageKindAttachment))
}
