package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/require"
)

// TestNULBindParamIsRejectedByPostgres pins the reason the cursor / id / search
// guards exist (#3025).
//
// **NUL は列に入らないだけでなく、SELECT の bind parameter にも載せられない。**
// 比較の右辺に置いただけで落ちるので、「存在しない id を引いたら 0 行」ではなく
// **クエリそのものがエラーになる**。`IsNotFound` でもないので handler は
// `JSONInternalError` へ倒し、認証済みの一般利用者がパラメータ 1 文字で 500 を
// 起こせていた。
//
// SQLSTATE は protocol mode で変わる (#2726)。ここ (`lib/pq` 相当の simple
// protocol) では 08P01 (protocol_violation)、本番の pgx extended protocol では
// 22021 (character_not_in_repertoire)。**どちらの番号にも依存しない** — 見るのは
// 「引く前に弾かないと落ちる」ことだけ。
func TestNULBindParamIsRejectedByPostgres(t *testing.T) {
	db := testutil.MustOpenTestDB()

	for _, tt := range []struct {
		name string
		sql  string
		arg  string
	}{
		// カーソル (`untilId` / `sinceId`)。
		{"cursor 比較", `SELECT count(*) FROM "user" WHERE id < ?`, "a\x00b"},
		// 単体の id (`userId` / `noteId` など)。
		{"id の等価比較", `SELECT count(*) FROM "user" WHERE id = ?`, "a\x00b"},
		// 検索文字列 (`query` / `host` など)。
		{"LIKE のパターン", `SELECT count(*) FROM "user" WHERE "usernameLower" LIKE ?`, "%a\x00b%"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var n int64
			err := db.Raw(tt.sql, tt.arg).Scan(&n).Error
			require.Error(t, err, "NUL を含む bind parameter が通っている (guard が要らなくなったのなら #3025 の判断ごと見直すこと)")
			require.False(t, IsNotFound(err), "not-found ではない = handler は 500 に倒す")
		})
	}

	// **接続は生き残る。** 落ちるのはそのクエリだけなので、後続が巻き添えに
	// なっていないことも見ておく (巻き添えになるなら guard の緊急度が変わる)。
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM "user"`).Scan(&n).Error,
		"NUL のクエリの後で接続が使えなくなっている")
}
