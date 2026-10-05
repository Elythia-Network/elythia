package mediaproxy

import (
	"context"
	"strings"
	"testing"

	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skip("test DB not available: " + err.Error())
	}
	testutil.ApplyMigrations(db)
	return db
}

// allowlistテストは共有test DBに行を挿入するため、パッケージ外テスト
// (特にinternal/repository/emoji_test.goのListLocal系) を汚染しないよう
// 必ずt.Cleanupで削除する。
func cleanupRow(t *testing.T, db *gorm.DB, table, id string) {
	t.Helper()
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "`+table+`" WHERE id = ?`, id)
	})
}

func TestDBAllowlistChecker_UserAvatarURL(t *testing.T) {
	db := openTestDB(t)
	checker := NewDBAllowlistChecker(db)

	cleanupRow(t, db, "user", "test-allow-u1")
	err := db.Exec(`INSERT INTO "user" (id, "updatedAt", username, "usernameLower", "avatarUrl", token)
		VALUES ('test-allow-u1', NOW(), 'allowtest1', 'allowtest1', 'https://remote.example/avatar-allow.png', 'tok-allow-1')
		ON CONFLICT (id) DO NOTHING`).Error
	require.NoError(t, err)

	ctx := context.Background()

	ok, err := checker.IsAllowedURL(ctx, "https://remote.example/avatar-allow.png")
	assert.NoError(t, err)
	assert.True(t, ok)

	ok, err = checker.IsAllowedURL(ctx, "https://unknown.example/evil.png")
	assert.NoError(t, err)
	assert.False(t, ok)
}

func TestDBAllowlistChecker_DriveFileURL(t *testing.T) {
	db := openTestDB(t)
	checker := NewDBAllowlistChecker(db)

	cleanupRow(t, db, "drive_file", "test-allow-df1")
	cleanupRow(t, db, "user", "test-allow-u2")
	err := db.Exec(`INSERT INTO "user" (id, "updatedAt", username, "usernameLower", token)
		VALUES ('test-allow-u2', NOW(), 'allowtest2', 'allowtest2', 'tok-allow-2')
		ON CONFLICT (id) DO NOTHING`).Error
	require.NoError(t, err)

	err = db.Exec(`INSERT INTO "drive_file" (id, "userId", "userHost", md5, name, type, size, "storedInternal", url, "isSensitive", "isLink")
		VALUES ('test-allow-df1', 'test-allow-u2', NULL, 'aaa', 'test.png', 'image/png', 1024, false, 'https://s3.example/files/allow-test.png', false, false)
		ON CONFLICT (id) DO NOTHING`).Error
	require.NoError(t, err)

	ctx := context.Background()

	ok, err := checker.IsAllowedURL(ctx, "https://s3.example/files/allow-test.png")
	assert.NoError(t, err)
	assert.True(t, ok)
}

func TestDBAllowlistChecker_EmojiURL(t *testing.T) {
	db := openTestDB(t)
	checker := NewDBAllowlistChecker(db)

	cleanupRow(t, db, "emoji", "test-allow-em1")
	// hostをremoteに設定することでlocal emoji扱いから外し、並行実行される
	// internal/repository/emoji_test.goのListLocal系テストに拾われないようにする
	// (Devin review #259: cross-package race指摘)。allowlistはoriginalUrl/publicUrl
	// のみを見てhostは参照しないので、本テストのassertに影響はない。
	err := db.Exec(`INSERT INTO "emoji" (id, "updatedAt", name, host, "originalUrl", "publicUrl", type)
		VALUES ('test-allow-em1', NOW(), 'allowemoji', 'remote.example', 'https://remote.example/emoji/allow.png', 'https://remote.example/emoji/allow.png', 'image/png')
		ON CONFLICT (id) DO NOTHING`).Error
	require.NoError(t, err)

	ctx := context.Background()

	ok, err := checker.IsAllowedURL(ctx, "https://remote.example/emoji/allow.png")
	assert.NoError(t, err)
	assert.True(t, ok)
}

func TestDBAllowlistChecker_InstanceIconURL(t *testing.T) {
	db := openTestDB(t)
	checker := NewDBAllowlistChecker(db)

	cleanupRow(t, db, "instance", "test-allow-inst1")
	err := db.Exec(`INSERT INTO "instance" (id, host, "firstRetrievedAt")
		VALUES ('test-allow-inst1', 'allow-remote.example', NOW())
		ON CONFLICT (id) DO NOTHING`).Error
	require.NoError(t, err)

	err = db.Exec(`UPDATE "instance" SET "iconUrl" = 'https://allow-remote.example/icon.png' WHERE id = 'test-allow-inst1'`).Error
	require.NoError(t, err)

	ctx := context.Background()

	ok, err := checker.IsAllowedURL(ctx, "https://allow-remote.example/icon.png")
	assert.NoError(t, err)
	assert.True(t, ok)
}

func TestDBAllowlistChecker_UserBannerURL(t *testing.T) {
	db := openTestDB(t)
	checker := NewDBAllowlistChecker(db)

	cleanupRow(t, db, "user", "test-allow-u3")
	err := db.Exec(`INSERT INTO "user" (id, "updatedAt", username, "usernameLower", "bannerUrl", token)
		VALUES ('test-allow-u3', NOW(), 'allowtest3', 'allowtest3', 'https://remote.example/banner-allow.png', 'tok-allow-3')
		ON CONFLICT (id) DO NOTHING`).Error
	require.NoError(t, err)

	ctx := context.Background()

	ok, err := checker.IsAllowedURL(ctx, "https://remote.example/banner-allow.png")
	assert.NoError(t, err)
	assert.True(t, ok)
}

// TestAllowlistQueryUsesIndexes checks that every table in the allowlist query
// can be answered from indexes.
//
// **OR でつないだ列のうち 1 列でも index が無いと、そのテーブルは全件走査になる**
// (#3383。本番で画像 1 枚ごとに 7,383 ページを読んでいた)。テストの DB は小さく、
// planner は放っておくと index があっても seq scan を選ぶので、`enable_seqscan` を
// 切って「index で答えられるか」だけを見る。切っても index が無ければ Seq Scan が
// 残る (コストを大きくするだけで、禁止はしない)。
//
// **見ているのは「index で引けるか」まで。** 列が複合 index の先頭以外にあるだけでも
// Bitmap Index Scan が選ばれ、このテストは通るが実際には index を全部読む。URL 列の
// index は単独の列で張ること。
func TestAllowlistQueryUsesIndexes(t *testing.T) {
	db := openTestDB(t)

	var plan []string
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SET LOCAL enable_seqscan = off`).Error; err != nil {
			return err
		}
		return tx.Raw(`EXPLAIN `+allowlistSQL, allowlistArgs("https://remote.example/a.png")...).Scan(&plan).Error
	})
	require.NoError(t, err)
	joined := strings.Join(plan, "\n")
	require.NotEmpty(t, plan, "EXPLAIN が空")
	// 4 テーブル全部を見ていることの下限。抽出が空振りして緑にならないように。
	for _, table := range []string{`"user"`, "drive_file", "emoji", "instance"} {
		assert.Containsf(t, joined, table, "実行計画に %s が無い:\n%s", table, joined)
	}
	assert.NotContainsf(t, joined, "Seq Scan",
		"許可確認に index の無い列がある (OR の列は全部 index が要る):\n%s", joined)
}

// TestAllowlistIndexesAcceptLongMultibyteURLs checks that the allowlist indexes
// do not reject long remote URLs.
//
// **btree は 1 行 2704 bytes を超える値を入れられない** (#3383 のレビューで実測)。
// user."avatarUrl" / drive_file.src は varchar(1024) で、リモートの actor は多バイトの
// 文字を 1024 字まで送れる (resolver.go の remoteMediaURL は rune 数で切る)。btree に
// すると INSERT / UPDATE ごと落ち、actor を作れない・更新できない (#2662 と同じ形で
// 受信のたびに fetch し直す) ので、この 2 列は hash index にしている。
func TestAllowlistIndexesAcceptLongMultibyteURLs(t *testing.T) {
	db := openTestDB(t)
	checker := NewDBAllowlistChecker(db)
	// 1 字 3 bytes の漢字を 1000 字 (3000 bytes 超)。**同じ字を並べない** — index の
	// 値は圧縮されるので、繰り返しだと上限に収まってしまい、btree でも通る (実測)。
	var b strings.Builder
	b.WriteString("https://remote.example/")
	for i := 0; i < 1000; i++ {
		b.WriteRune(rune(0x4E00 + (i*7919)%20000))
	}
	long := b.String()

	cleanupRow(t, db, "user", "test-allow-long1")
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, "updatedAt", username, "usernameLower", "avatarUrl", token)
		VALUES ('test-allow-long1', NOW(), 'allowlong1', 'allowlong1', ?, 'tok-allow-long1')`, long).Error)

	ok, err := checker.IsAllowedURL(context.Background(), long)
	require.NoError(t, err)
	assert.True(t, ok)

	cleanupRow(t, db, "drive_file", "test-allow-long-df1")
	require.NoError(t, db.Exec(`INSERT INTO "drive_file" (id, "userId", md5, name, type, size, comment, properties, "storedInternal", url, "accessKey", src, "isLink")
		VALUES ('test-allow-long-df1', 'test-allow-long1', 'x', 'x', 'image/png', 0, NULL, '{}', false, 'https://remote.example/long-df.png', NULL, ?, true)`, long).Error)
}
