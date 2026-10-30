package federation

import (
	"errors"
	"strings"

	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elythia-network/elythia/internal/misc/colfit"
	"gorm.io/gorm"
)

// リモート由来の値を書く列の上限 (migration/000001_initial.up.sql)。
//
// **溢れると行ごと落ちる。** PostgreSQL は varchar の超過を 22001 で拒否するので、
// INSERT / UPDATE がまるごと失敗する。失われる範囲は書き込みの単位ごとに違う:
// note なら ingest が error を返し (トップレベル配送ならその inbox job が retry を
// 使い切って dead になる。既定 8 回、`defaultInboxJobMaxAttempts` in
// `internal/server/queue_factory.go`)、actor なら 1 行も作られない。添付は
// 1 件ずつ Create するので**その添付だけ**消える (#2723)。
const (
	driveFileNameMaxRunes    = 256
	driveFileCommentMaxRunes = 512
	driveFileTypeMaxRunes    = 128
	driveFileURLMaxRunes     = 1024
	driveFileThumbMaxRunes   = 512
	driveFileBlurhashMaxRune = 128
	noteCWMaxRunes           = 512
	noteURIMaxRunes          = 512
	// poll.choices は varchar(256)[] (migration/000001_initial.up.sql)。
	pollChoiceMaxRunes = 256
)

// user 側のリモート値を書く列の上限 (migration/000001_initial.up.sql)。
const (
	userURIMaxRunes  = 512
	userHostMaxRunes = 128
)

// 鍵を書く列の上限 (migration/000031_user_publickey.up.sql /
// 000050_user_publickey_extra.up.sql)。upstream の MiUserPublickey も同じ
// 256 / 4096 なので、収まらない鍵は upstream でも連合できない。
const (
	publicKeyKeyIDMaxRunes = 256
	publicKeyPEMMaxRunes   = 4096
)

// fitsColumn reports whether s fits a varchar(max) column and carries no NUL.
//
// PostgreSQL の varchar はコードポイント数で数えるので rune で見る。NUL は
// 長さに関わらず 22021 で弾かれる。
func fitsColumn(s string, max int) bool {
	return colfit.Fits(s, max)
}

// remoteURIList joins remote-supplied URIs for a comma-separated column,
// dropping entries that cannot be stored.
//
// `user.alsoKnownAs` は text 列なので長さは効かないが、**NUL は 22021 で弾かれる**。
// 同じ INSERT / UPDATE に載っている他の列まで巻き添えになるので、ここで落とす。
// 値は URI なので truncate せず**要素ごと捨てる** (#2723)。
func remoteURIList(actorURI, column string, values []string) string {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if strings.ContainsRune(v, 0) {
			slog.Warn("federation: dropping remote value that cannot be stored",
				"actor", truncateRunes(actorURI, userURIMaxRunes), "column", column)
			continue
		}
		kept = append(kept, v)
	}
	return strings.Join(kept, ",")
}

// remoteURIValue returns raw when it fits a varchar(512) URI column, or "" when
// it must be dropped.
//
// **切らない。** 途中で切った URI は別物で、取りに行っても無駄なうえ壊れた参照を
// 保存することになる。`remoteMediaURL` と同じ判断 (#2662 / #2723)。
func remoteURIValue(actorURI, column, raw string) string {
	if raw == "" {
		return ""
	}
	if !fitsColumn(raw, userURIMaxRunes) {
		slog.Warn("federation: dropping remote uri that does not fit its column",
			"actor", truncateRunes(actorURI, userURIMaxRunes), "column", column)
		return ""
	}
	return raw
}

// noteURLMaxRunes は `note.url` の varchar(512) (migration/000001_initial.up.sql)。
const noteURLMaxRunes = 512

// remoteNoteURL validates an AP object's already-decoded `url` and returns the
// value to store in `note.url`, or "" when it must be dropped.
//
// 読み方 (配列なら先頭 / string / object の `href`) は `APLenientHref` が持つ。
// **同じ upstream ヘルパー (`getOneApHrefNullable`) の写しを 2 つ作らない** —
// #2726 で `colfit` に集約したのと同じ理由で、数え方や受理する形が分かれる。
//
// **収まらない / scheme が http(s) でない値は捨てて、note は作る** (#2729)。
// `note.url` は表示用の permalink で、身元は `uri` のほうなので #2723 の
// 「URL / ID 系」の規則に当たる。**upstream は非 https なら note ごと reject する**
// (`checkHttps`、production では `http://` も不可) ので mk-go のほうが寛容だが、
// permalink の scheme が変なだけで本文ごと失うほうが害が大きい。
//
// **`javascript:` を保存しない点は upstream と同等**で、mk-go の優位ではない
// (upstream は throw するので結果として保存されない)。**この検査を通した後でも
// 受理する scheme は mk-go のほうが広い** — `HTTPS://` / `HTTP://` / production の
// `http://` はどれも upstream が reject して mk-go は保存する。parity には戻らない。
// **この scheme 判定が担うのは http(s) 以外の scheme だけ** (長さと NUL は下の
// `fitsColumn` が別に見る。そちらは upstream が 22001 / 22021 で note ごと失う
// 値なので、mk-go だけが保存する形にはならない)。
//
// scheme は case-insensitive に見る (RFC 3986)。`internal/core/urlpreview` の
// `isHTTPScheme` と同じ方針。
func remoteNoteURL(noteURI, href string) string {
	if href == "" {
		return ""
	}
	lower := strings.ToLower(href)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		slog.Warn("federation: dropping remote note url with a non-http scheme",
			"uri", truncateRunes(noteURI, noteURIMaxRunes))
		return ""
	}
	if !fitsColumn(href, noteURLMaxRunes) {
		// `nul` を出す。`fitsColumn` は長さと NUL の両方を見るので、これが無いと
		// NUL 入りの値を落としたときに `len` < `max` のログだけが残って読めない
		// (`remoteMediaURL` は 2 つの判定に分けて別々のログを出している)。
		slog.Warn("federation: dropping remote note url that does not fit its column",
			"uri", truncateRunes(noteURI, noteURIMaxRunes),
			"len", len([]rune(href)), "max", noteURLMaxRunes,
			"nul", strings.ContainsRune(href, 0))
		return ""
	}
	return href
}

// inboxPtr returns nil for an empty inbox so the column stays NULL.
func inboxPtr(inbox string) *string {
	if inbox == "" {
		return nil
	}
	return &inbox
}

// remoteText prepares a remote-supplied **body** string for a column: NUL を落とし、
// max rune で切る。max <= 0 なら切らない (`note.text` のような無制限の列用。リモートの
// note の本文の長さは呼び出し側で clipRemoteNoteText が切る)。
//
// **URL / ID には使わない。** 途中で切った URL は別物で、取りに行っても無駄なうえ
// 壊れた参照を保存することになる。そちらは `remoteMediaURL` と同じく**値ごと捨てる**
// (#2723)。
func remoteText(raw string, max int) string {
	return colfit.Text(raw, max)
}

// remoteNoteTextMaxUTF16 is upstream DB_MAX_NOTE_TEXT_LENGTH (const.ts): the
// length a remote note's text is cut to before it is stored. The checks that
// read the text (prohibited words, federation rules, hashtags) see the full
// text, as upstream does.
const remoteNoteTextMaxUTF16 = 8192

// clipRemoteNoteText cuts s to at most remoteNoteTextMaxUTF16 UTF-16 code
// units, never splitting a surrogate pair.
//
// 本家 NoteCreateService は `data.text.length > DB_MAX_NOTE_TEXT_LENGTH` なら
// `data.text.slice(0, DB_MAX_NOTE_TEXT_LENGTH)` で切る。JavaScript の length は
// UTF-16 の単位なので、rune で数えると BMP 外の文字 (絵文字など) が多い本文で
// 本家の 2 倍まで入る。本家と違ってサロゲートペアの途中では切らない (切ると
// 対になっていない surrogate が残り、保存時に置換文字になる)。
func clipRemoteNoteText(s string) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > remoteNoteTextMaxUTF16 {
			return s[:i]
		}
		units += w
	}
	return s
}

// truncateRunes clips s to at most max runes. byte 単位で切ると壊れた UTF-8 を
// 書くので rune で数える (extractRemoteDescription と同じ方針)。
func truncateRunes(s string, max int) string {
	return colfit.TruncateRunes(s, max)
}

// isForeignKeyViolation reports whether err is a PostgreSQL foreign_key_violation.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503"
	}
	return errors.Is(err, gorm.ErrForeignKeyViolated)
}
