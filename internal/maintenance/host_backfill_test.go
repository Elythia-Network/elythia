package maintenance

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/model"
)

// seedHostUser inserts a remote user with the given (usernameLower, host).
func seedHostUser(t *testing.T, id, username, host string) {
	t.Helper()
	h := host
	require.NoError(t, testDB.Create(&model.User{
		ID: id, Username: username, UsernameLower: username, Host: &h,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
}

func userHost(t *testing.T, id string) string {
	t.Helper()
	var got string
	require.NoError(t, testDB.Raw(`SELECT host FROM "user" WHERE id = ?`, id).Scan(&got).Error)
	return got
}

var userHostColumn = HostColumn{Table: "user", KeysetColumn: "id", Column: "host"}

// 非正規化で保存された host が正規化されること (#2706)。
//
// `url.Parse` は小文字化も punycode 化もしないので、`Mixed.Example` のような表記で
// 入った行が残る。連合ゲートや timeline の instance-mute は完全一致なので取りこぼす。
func TestBackfillHostColumnBatch_Normalizes(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hb_%'`)
	seedHostUser(t, "hb_mixed", "hbmixed", "Mixed.Example")
	seedHostUser(t, "hb_uni", "hbuni", "パイ.example")
	seedHostUser(t, "hb_ok", "hbok", "already.example")

	res, err := BackfillHostColumnBatch(testDB, userHostColumn, "hb_", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned)
	assert.Equal(t, 2, res.Updated, "既に正規化済みの行は touch しない")
	assert.Equal(t, 0, res.Conflicts)
	assert.Equal(t, "hb_uni", res.LastKey)

	assert.Equal(t, "mixed.example", userHost(t, "hb_mixed"))
	assert.Equal(t, "xn--eckve.example", userHost(t, "hb_uni"), "punycode 変換は SQL では書けない")
	assert.Equal(t, "already.example", userHost(t, "hb_ok"))

	// **冪等。** 2 回目は 1 件も更新しない (途中で失敗しても再実行して安全)。
	res, err = BackfillHostColumnBatch(testDB, userHostColumn, "hb_", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned)
	assert.Equal(t, 0, res.Updated)
}

// dry-run は件数だけ数えて書かないこと。
func TestBackfillHostColumnBatch_DryRun(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hd_%'`)
	seedHostUser(t, "hd_1", "hdone", "Mixed.Example")

	res, err := BackfillHostColumnBatch(testDB, userHostColumn, "hd_", 100, true)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	assert.Equal(t, 1, res.Updated)
	assert.Equal(t, "Mixed.Example", userHost(t, "hd_1"), "dry-run で書き換わっている")
}

// 表記違いで 2 行に増えている場合、正規化すると一意制約に当たる。
// **マージせず skip して数える** — 1 行に畳むには note / following / drive_file など
// 多数の FK を張り替える必要があり、バッチでやるには危険すぎる (#2706)。
func TestBackfillHostColumnBatch_ConflictIsSkippedNotFatal(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hc_%'`)
	// 同じ usernameLower で、正規化すると衝突する 2 行。
	seedHostUser(t, "hc_1", "hcdup", "dup.example")
	seedHostUser(t, "hc_2", "hcdup", "Dup.Example")
	// 衝突しない行も混ぜて、1 件の衝突でバッチが止まらないことを見る。
	seedHostUser(t, "hc_3", "hcother", "Other.Example")

	res, err := BackfillHostColumnBatch(testDB, userHostColumn, "hc_", 100, false)
	require.NoError(t, err, "衝突でバッチ全体を落とさないこと")
	assert.Equal(t, 3, res.Scanned)
	assert.Equal(t, 1, res.Conflicts)
	assert.Equal(t, 1, res.Updated, "衝突しない行は進むこと")

	assert.Equal(t, "dup.example", userHost(t, "hc_1"))
	assert.Equal(t, "Dup.Example", userHost(t, "hc_2"), "衝突した行はそのまま残る")
	assert.Equal(t, "other.example", userHost(t, "hc_3"))
}

// keyset で再開できること。LastKey を次の fromKey に渡す。
func TestBackfillHostColumnBatch_Resumable(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hr_%'`)
	seedHostUser(t, "hr_1", "hrone", "One.Example")
	seedHostUser(t, "hr_2", "hrtwo", "Two.Example")
	seedHostUser(t, "hr_3", "hrthree", "Three.Example")

	res, err := BackfillHostColumnBatch(testDB, userHostColumn, "hr_", 2, false)
	require.NoError(t, err)
	require.Equal(t, 2, res.Scanned)
	require.Equal(t, "hr_2", res.LastKey)

	res, err = BackfillHostColumnBatch(testDB, userHostColumn, res.LastKey, 2, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	assert.Equal(t, "hr_3", res.LastKey)

	assert.Equal(t, "one.example", userHost(t, "hr_1"))
	assert.Equal(t, "two.example", userHost(t, "hr_2"))
	assert.Equal(t, "three.example", userHost(t, "hr_3"))

	// 尽きたら Scanned=0 / LastKey="" で止まる (CLI のループ終了条件)。
	res, err = BackfillHostColumnBatch(testDB, userHostColumn, "hr_3", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Scanned)
	assert.Empty(t, res.LastKey)
}

// HostColumns に無い組は拒否すること。整形した SQL に外から識別子が入らない。
func TestBackfillHostColumnBatch_RejectsUnknownColumn(t *testing.T) {
	_, err := BackfillHostColumnBatch(testDB, HostColumn{
		Table: "user", KeysetColumn: "id", Column: `host"; DROP TABLE "user"; --`,
	}, "", 10, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown host column")
}

// 一覧が実在するテーブル・列であること。1 つでも綴りを間違えると、そこだけ
// 非正規化のまま残って同じ症状が別の場所で出る。
func TestHostColumns_AllExist(t *testing.T) {
	require.NotEmpty(t, HostColumns)
	for _, c := range HostColumns {
		t.Run(c.Table+"."+c.Column, func(t *testing.T) {
			var n int64
			// **schema を絞る。** OpenTestDB はパッケージ専用の schema に繋ぐが、
			// information_schema は全 schema を見るので、絞らないと同名テーブルを
			// 数え上げて必ず 1 にならない。
			err := testDB.Raw(`SELECT count(*) FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
				c.Table, c.Column).Scan(&n).Error
			require.NoError(t, err)
			assert.EqualValues(t, 1, n, "列が存在しない")

			err = testDB.Raw(`SELECT count(*) FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
				c.Table, c.KeysetColumn).Scan(&n).Error
			require.NoError(t, err)
			assert.EqualValues(t, 1, n, "keyset 列が存在しない")
		})
	}
}

// SELECT が失敗したら error を返すこと (テーブルが無い schema 等)。
func TestBackfillHostColumnBatch_SelectError(t *testing.T) {
	// 一覧に載っている組だが、テーブルが解決できない search_path で引く。
	// **SET LOCAL はトランザクション内でしか効かない** (外で撃つと黙って無視され、
	// 検査が空振りする)。
	//
	// ここで tx を渡しているのは SELECT を失敗させるためだけ。本番は
	// non-transactional な *gorm.DB を渡す — 23505 で tx が abort すると以降が
	// 25P02 になり、衝突 skip が機能しない (#2714 review LOW-3)。
	err := testDB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Exec(`SET LOCAL search_path TO nonexistent_schema_2706`).Error; e != nil {
			return e
		}
		_, e := BackfillHostColumnBatch(tx, userHostColumn, "", 10, false)
		return e
	})
	require.Error(t, err)
}

// UPDATE が unique violation 以外で失敗したら、握り潰さず error を返すこと。
// 握り潰すと「conflicts が 0 なのに更新されていない」状態になり、運用者が
// backfill 済みだと誤認する。
func TestBackfillHostColumnBatch_UpdateErrorIsFatal(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'he_%'`)
	seedHostUser(t, "he_1", "heone", "Mixed.Example")

	// host 列に 4 文字までの制約を足して、正規化後の値が入らないようにする。
	require.NoError(t, testDB.Exec(
		`ALTER TABLE "user" ADD CONSTRAINT "chk_2706_host_len" CHECK (length(host) <= 4) NOT VALID`).Error)
	t.Cleanup(func() {
		testDB.Exec(`ALTER TABLE "user" DROP CONSTRAINT IF EXISTS "chk_2706_host_len"`)
	})

	_, err := BackfillHostColumnBatch(testDB, userHostColumn, "he_", 10, false)
	require.Error(t, err, "unique violation 以外の error を握り潰している")
	assert.False(t, isUniqueViolation(err))
}

// isUniqueViolation は driver の SQLSTATE と GORM の翻訳済み error の両方を見る。
func TestIsUniqueViolation(t *testing.T) {
	assert.True(t, isUniqueViolation(&pgconn.PgError{Code: "23505"}))
	assert.True(t, isUniqueViolation(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23505"})))
	assert.False(t, isUniqueViolation(&pgconn.PgError{Code: "23503"}), "FK 違反は衝突ではない")
	assert.True(t, isUniqueViolation(gorm.ErrDuplicatedKey))
	assert.False(t, isUniqueViolation(errors.New("boom")))
	assert.False(t, isUniqueViolation(nil))
}

// **schema 側から網羅性を固定する。** `TestHostColumns_AllExist` は「列挙したものが
// 実在するか」しか見ないので、**列を落としても緑のまま**だった。実際
// `note.replyUserHost` / `renoteUserHost` を落としており、それは backfill の動機に
// 挙げた 2 つのフィルタが読む列だった (#2714 review HIGH-1)。
//
// 逆方向 — schema にある host 系の列が、対象一覧か明示的な除外一覧の**どちらかに
// 必ず入っている**ことを見る。新しい列が増えたらここが落ちる。
func TestHostColumns_CoversSchema(t *testing.T) {
	type col struct{ TableName, ColumnName string }
	var found []col
	require.NoError(t, testDB.Raw(`SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND column_name ILIKE '%host%'
		ORDER BY table_name, column_name`).Scan(&found).Error)
	require.NotEmpty(t, found, "host 系の列が 1 つも見つからない (schema が未適用?)")

	covered := map[col]string{}
	for _, c := range HostColumns {
		covered[col{c.Table, c.Column}] = "backfill 対象"
	}
	for _, c := range metaHostColumns {
		covered[col{c.Table, c.Column}] = "意図的に除外"
	}
	for _, c := range found {
		assert.Contains(t, covered, c,
			"schema にある host 列が HostColumns にも除外一覧にも無い: "+c.TableName+"."+c.ColumnName)
	}
	// 逆に、実在しない列を一覧に残していないこと (TestHostColumns_AllExist が
	// HostColumns 側を見るので、ここでは除外一覧を見る)。
	inSchema := map[col]bool{}
	for _, c := range found {
		inSchema[c] = true
	}
	for _, c := range metaHostColumns {
		assert.True(t, inSchema[col{c.Table, c.Column}],
			"除外一覧に実在しない列がある: "+c.Table+"."+c.Column)
	}
}

// **LastKey は skip 判定より前に進める。** 後ろに置くと、batch の末尾が「既に
// 正規化済み」の行だったときに cursor が進まず、CLI は Scanned==0 でしか break
// しないので**同じ batch を読み直して止まらない** (#2714 review MEDIUM-3)。
func TestBackfillHostColumnBatch_LastKeyAdvancesPastSkippedRows(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hk_%'`)
	seedHostUser(t, "hk_1", "hkone", "Mixed.Example")
	seedHostUser(t, "hk_2", "hktwo", "already.example") // 末尾が no-op

	res, err := BackfillHostColumnBatch(testDB, userHostColumn, "hk_", 100, false)
	require.NoError(t, err)
	require.Equal(t, 2, res.Scanned)
	assert.Equal(t, "hk_2", res.LastKey, "skip した行でも cursor を進めること")

	// 進んでいれば次は空になる。進んでいないと同じ行を読み直して止まらない。
	res, err = BackfillHostColumnBatch(testDB, userHostColumn, res.LastKey, 100, false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Scanned)
}

// dry-run では conflicts を数えられないこと (UPDATE を撃たないため) を semantics と
// して固定する。運用者が dry-run の conflicts=0 を「衝突なし」と読まないよう、
// GoDoc と docs にも明記してある (#2714 review MEDIUM-1)。
func TestBackfillHostColumnBatch_DryRunCannotCountConflicts(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hq_%'`)
	seedHostUser(t, "hq_1", "hqdup", "dup.example")
	seedHostUser(t, "hq_2", "hqdup", "Dup.Example")

	res, err := BackfillHostColumnBatch(testDB, userHostColumn, "hq_", 100, true)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated, "本実行なら衝突する行も dry-run では更新見込みに数える")
	assert.Equal(t, 0, res.Conflicts, "dry-run は UPDATE を撃たないので衝突を検出できない")
}

// 衝突した行を呼び出し側へ渡すこと。件数だけだと「対象を見て判断する」ができない
// (#2714 review MEDIUM-2)。
func TestBackfillHostColumnBatch_ReportsConflictKeys(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hj_%'`)
	seedHostUser(t, "hj_1", "hjdup", "dup.example")
	seedHostUser(t, "hj_2", "hjdup", "Dup.Example")

	res, err := BackfillHostColumnBatch(testDB, userHostColumn, "hj_", 100, false)
	require.NoError(t, err)
	require.Equal(t, 1, res.Conflicts)
	require.Len(t, res.ConflictKeys, 1)
	assert.Equal(t, "hj_2", res.ConflictKeys[0].Key)
	assert.Equal(t, "Dup.Example", res.ConflictKeys[0].Host)
	assert.Equal(t, "dup.example", res.ConflictKeys[0].Normalized)
}

// SELECT と UPDATE のあいだに host が変わった行を上書きしないこと。
// 稼働中に流す前提のバッチなので、WHERE から現在値の一致を落としてはいけない
// (#2714 review LOW-4)。
//
// **実経路を通す。** UPDATE 文を手で書いて確かめると実装の写経になり、バッチ側の
// WHERE を緩める変異を検出できない。GORM の callback で SELECT と UPDATE のあいだに
// 割り込んで、行を書き換える。
func TestBackfillHostColumnBatch_DoesNotOverwriteChangedRows(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hw_%'`)
	seedHostUser(t, "hw_1", "hwone", "Mixed.Example")

	sess := testDB.Session(&gorm.Session{NewDB: true})
	fired := false
	require.NoError(t, sess.Callback().Raw().Before("gorm:raw").Register("2706_race", func(tx *gorm.DB) {
		if fired || !strings.HasPrefix(tx.Statement.SQL.String(), "UPDATE") {
			return
		}
		fired = true
		// バッチが読んだ値と食い違わせる (別経路が refresh した状況)。
		testDB.Exec(`UPDATE "user" SET host = 'changed.example' WHERE id = 'hw_1'`)
	}))

	res, err := BackfillHostColumnBatch(sess, userHostColumn, "hw_", 100, false)
	require.NoError(t, err)
	require.True(t, fired, "UPDATE の callback が発火していない (前提が崩れている)")
	assert.Equal(t, "changed.example", userHost(t, "hw_1"),
		"読んだ時点の値と食い違う行を上書きしている")
	assert.Equal(t, 1, res.Updated,
		"戻り値は「撃った」件数。実際に当たったかは上の assert で見る")
}

// UTS#46 の mapping をしない旧 build が保存した行 (`ｅｖｉｌ.example` を
// `xn--qi7ciaj2b.example` にしたもの) も、接続先と同じ `evil.example` に戻すこと。
// 戻さないと、既に入っている行が blockedHosts / 一覧の絞り込みを素通りし続ける。
// 正当な IDN の行は触らない。
func TestBackfillHostColumnBatch_RepairsUnmappedPunycode(t *testing.T) {
	testDB.Exec(`DELETE FROM "user" WHERE id LIKE 'hbm_%'`)
	seedHostUser(t, "hbm_fw", "hbmfw", "xn--qi7ciaj2b.example")
	seedHostUser(t, "hbm_dot", "hbmdot", "xn--evilexample-7e3j")
	seedHostUser(t, "hbm_raw", "hbmraw", "ｅｖｉｌ2.example")
	seedHostUser(t, "hbm_idn", "hbmidn", "xn--eckve.example")

	res, err := BackfillHostColumnBatch(testDB, userHostColumn, "hbm_", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 4, res.Scanned)
	assert.Equal(t, 3, res.Updated)
	assert.Equal(t, "evil.example", userHost(t, "hbm_fw"))
	assert.Equal(t, "evil.example", userHost(t, "hbm_dot"))
	assert.Equal(t, "evil2.example", userHost(t, "hbm_raw"))
	assert.Equal(t, "xn--eckve.example", userHost(t, "hbm_idn"), "正当な IDN の表記は変わらない")
}
