package maintenance

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/misc/idnhost"
)

// HostColumn identifies one remote-host column to normalize.
//
// KeysetColumn is what the batch paginates on. It is usually the primary key;
// `instance_signature_capability` has no separate id so it paginates on the
// host column itself (see HostColumns).
type HostColumn struct {
	Table        string
	KeysetColumn string
	Column       string
}

// HostColumns lists every per-row column that stores a remote host.
//
// **1 つ漏らすとそこだけ非正規化のまま残り、同じ症状が別の場所で出る。**
// `note.replyUserHost` / `renoteUserHost` は実際に落としていて、
// **backfill の動機に挙げた 2 つのフィルタが読む列**だった
// (instance-mute の SQL は `repository/note.go` で 3 列を並べて見るし、
// blocked-host filter は `notesfilter/blockedhost.go` で 3 列を or で見る)。
// 網羅性は `TestHostColumns_CoversSchema` が schema 側から固定する
// (#2714 review HIGH-1)。
//
// **現時点で host 完全一致に使われていない列も入れる** (`poll.userHost` /
// `follow_request.*Host` / `abuse_user_report.*Host` は今は `IS NULL` 判定だけ)。
// どれも `user.host` の写しで、正規化は冪等な no-op なので入れて損が無い。逆に
// 外すと「今は使われていない」という前提が変わった瞬間に穴になる。
//
// **`meta` の host 列は対象外** — `blockedHosts` / `silencedHosts` /
// `mediaSilencedHosts` / `federationHosts` は運用者が入力する一覧で、リモートから
// 受け取った値ではない。`smtpHost` はそもそもリモートインスタンスではない。
// upstream も同じ扱い (`update-meta.ts` は lowercase しかしない)。Unicode IDN 表記で
// 登録している場合の注意は docs/deployment.md に書いてある。
//
// 順序は依存関係ではなく、影響の大きい順 (acct 解決 → 連合ゲート → 表示)。
var HostColumns = []HostColumn{
	{Table: "user", KeysetColumn: "id", Column: "host"},
	{Table: "instance", KeysetColumn: "id", Column: "host"},
	{Table: "instance_signature_capability", KeysetColumn: "host", Column: "host"},
	{Table: "emoji", KeysetColumn: "id", Column: "host"},
	{Table: "following", KeysetColumn: "id", Column: "followerHost"},
	{Table: "following", KeysetColumn: "id", Column: "followeeHost"},
	{Table: "follow_request", KeysetColumn: "id", Column: "followerHost"},
	{Table: "follow_request", KeysetColumn: "id", Column: "followeeHost"},
	{Table: "note", KeysetColumn: "id", Column: "userHost"},
	{Table: "note", KeysetColumn: "id", Column: "replyUserHost"},
	{Table: "note", KeysetColumn: "id", Column: "renoteUserHost"},
	{Table: "poll", KeysetColumn: "noteId", Column: "userHost"},
	{Table: "drive_file", KeysetColumn: "id", Column: "userHost"},
	{Table: "user_profile", KeysetColumn: "userId", Column: "userHost"},
	{Table: "abuse_user_report", KeysetColumn: "id", Column: "targetUserHost"},
	{Table: "abuse_user_report", KeysetColumn: "id", Column: "reporterHost"},
	// 取り込んだ chat room の出どころ (#2994)。**対象一覧に入れる** — 値は正規形の
	// room URI の authority (実行時) か owner の `user.host` (migration) から入るので
	// 普通は書き換えが起きないが、一覧から外すと「host 列なのに誰も見ていない」
	// 状態になり、別経路で入った非正規化の値が残っても気付けない。
	{Table: "chat_room", KeysetColumn: "id", Column: "host"},
	// 絵文字のインポート申請が指すリモートの host (#2934 / #2935)。**対象一覧に
	// 入れる** — 申請は host で相手を特定するので、正規化されていないと同じ
	// サーバーが別物として並ぶ。本 migration の時点では kind='own' しか
	// 発行されないので行は空だが、#2935 で書かれ始める。
	{Table: "emoji_application", KeysetColumn: "id", Column: "remoteHost"},
}

// metaHostColumns are the host-ish columns deliberately left out of HostColumns.
// TestHostColumns_CoversSchema はこの 2 つの集合で schema を覆えることを見る。
var metaHostColumns = []HostColumn{
	{Table: "meta", KeysetColumn: "id", Column: "blockedHosts"},
	{Table: "meta", KeysetColumn: "id", Column: "silencedHosts"},
	{Table: "meta", KeysetColumn: "id", Column: "mediaSilencedHosts"},
	{Table: "meta", KeysetColumn: "id", Column: "federationHosts"},
	{Table: "meta", KeysetColumn: "id", Column: "smtpHost"},
}

// HostBackfillResult reports the outcome of one keyset batch.
type HostBackfillResult struct {
	Scanned   int    // rows inspected this batch
	Updated   int    // rows whose host changed (dryRun では更新見込みの件数)
	Conflicts int    // rows skipped because the normalized value already exists
	LastKey   string // greatest keyset value seen; "" when no rows remained
	// ConflictKeys は衝突した行。**件数だけだと運用者が対象を特定できない**ので、
	// 呼び出し側がログに出せるように返す (#2714 review MEDIUM-2)。
	ConflictKeys []HostConflict
}

// HostConflict identifies one row whose normalized host collided.
type HostConflict struct {
	Key        string // keyset column value
	Host       string // the value still stored
	Normalized string // what it would have become
}

// BackfillHostColumnBatch normalizes one keyset batch of a remote-host column
// to the form hostFromURI now produces (UTS#46 mapping + punycode, lowercase).
//
// **mapping 無しで punycode 化した旧形式も畳み直す** (`idnhost.RepairLegacyPuny`)。
// 以前の `Puny` は UTS#46 の文字対応付けをしなかったので、`https://ｅｖｉｌ.example/`
// の actor が `xn--qi7ciaj2b.example` として保存されていた。接続先は
// `evil.example` なので、この行は blockedHosts の `evil.example` に当たらない。
// 正当な IDN の行 (`xn--eckve.example`) は同じ値に戻るので触らない。
//
// **`hostFromURI` が保存する形と完全には一致しない。** あちらは既定ポート
// (`https://h:443` の `:443`) も剥がすようになったが、この backfill は
// host の正規化しか掛けないのでポートを落とさない。過去に `h:443` の形で
// 保存された行は、これを流しても `h` にはならない。
//
// 既存行は `url.Parse` の生の host で保存されており、`Mixed.Example` のような
// 表記のまま残る。読み取り側の両当たりは #2996 で撤去したので acct 解決からも引けず、
// 連合ゲートや timeline の instance-mute は完全一致なので取りこぼす (#2706)。
//
// **PostgreSQL に IDNA 変換が無い**ので SQL migration では書けない。`lower()` だけ
// では `パイ.example` → `xn--eckve.example` を作れないため、app-level のバッチにする
// (`cmd/backfill-note-tags` と同じ理由)。
//
// **衝突はマージせず skip して数える。** 同じリモートが表記違いで 2 行に増えている
// 場合、正規化すると一意制約 (`user` の (usernameLower, host) / `instance` の host /
// `emoji` の (name, host)) に当たる。1 行に畳むには note / following / drive_file
// など多数の FK を張り替える必要があり、バッチでやるには危険すぎる。運用者が
// Conflicts の件数を見て手当てを判断する。
//
// 戻り値の LastKey を次回の fromKey に渡せば再開できる。**LastKey は skip した行でも
// 進める** — 進めないと、batch の末尾が既に正規化済みの行だったときに cursor が動かず、
// 呼び出し側が同じ batch を読み直して止まらない (#2714 review MEDIUM-3)。
//
// 冪等なので、途中で失敗しても再実行して安全。**dryRun は UPDATE を撃たないので
// Conflicts を数えられない** (常に 0)。衝突は本実行で初めて分かる
// (#2714 review MEDIUM-1)。
//
// **transaction を渡さないこと。** 23505 が起きると PostgreSQL の transaction は
// abort し、以降の文が 25P02 になるので衝突 skip が機能しない
// (#2714 review LOW-3)。
func BackfillHostColumnBatch(db *gorm.DB, col HostColumn, fromKey string, batchSize int, dryRun bool) (HostBackfillResult, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	if err := col.validate(); err != nil {
		return HostBackfillResult{}, err
	}
	type row struct {
		Key  string
		Host string
	}
	var rows []row
	// 列名は HostColumns の固定値しか通らない (validate 済み) が、識別子として
	// quote しておく。
	sel := fmt.Sprintf(`SELECT %s AS key, %s AS host FROM %s WHERE %s > ? AND %s IS NOT NULL AND %s <> '' ORDER BY %s ASC LIMIT ?`,
		quoteIdent(col.KeysetColumn), quoteIdent(col.Column), quoteIdent(col.Table),
		quoteIdent(col.KeysetColumn), quoteIdent(col.Column), quoteIdent(col.Column),
		quoteIdent(col.KeysetColumn))
	if err := db.Raw(sel, fromKey, batchSize).Scan(&rows).Error; err != nil {
		return HostBackfillResult{}, err
	}

	res := HostBackfillResult{}
	upd := fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ? AND %s = ?`,
		quoteIdent(col.Table), quoteIdent(col.Column),
		quoteIdent(col.KeysetColumn), quoteIdent(col.Column))
	for _, r := range rows {
		res.Scanned++
		res.LastKey = r.Key
		normalized := idnhost.RepairLegacyPuny(r.Host)
		if normalized == r.Host {
			continue
		}
		if dryRun {
			res.Updated++
			continue
		}
		// **1 行ずつ独立させる。** 衝突した 1 行でバッチ全体を巻き戻さない。
		err := db.Exec(upd, normalized, r.Key, r.Host).Error
		if err != nil {
			if isUniqueViolation(err) {
				res.Conflicts++
				res.ConflictKeys = append(res.ConflictKeys,
					HostConflict{Key: r.Key, Host: r.Host, Normalized: normalized})
				continue
			}
			return res, err
		}
		res.Updated++
	}
	return res, nil
}

// validate rejects anything not in HostColumns so the formatted SQL can only
// ever contain identifiers this package chose.
func (c HostColumn) validate() error {
	for _, known := range HostColumns {
		if known == c {
			return nil
		}
	}
	return fmt.Errorf("maintenance: unknown host column %s.%s", c.Table, c.Column)
}

// quoteIdent quotes a SQL identifier. 入力は HostColumns の固定値だけなので
// エスケープは届かない防御だが、`user` のような予約語を含むので quote 自体は要る。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// isUniqueViolation reports whether err is a PostgreSQL unique_violation.
//
// GORM の TranslateError は有効化していないので、driver の error を直接見る。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return errors.Is(err, gorm.ErrDuplicatedKey)
}
