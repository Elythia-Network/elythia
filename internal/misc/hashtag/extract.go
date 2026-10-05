// Package hashtag provides hashtag extraction from note text.
//
// Misskey TS (mfm-js) は MFM パーサで text を構造化してから hashtag
// ノードを取り出す。mk-go も同等の MFM パーサ (internal/activitypub/mfm)
// を持つので、本パッケージはそれに hashtag 抽出を委譲し、note.tags 固有の
// 正規化 (長さ truncate / 大文字小文字 dedup) だけを担う。連合経由で受け
// 取る ActivityPub Object には別途 tag 配列が付くので、本パッケージは
// local note 作成時の text/cw 由来 hashtag だけを担当する。
package hashtag

import (
	"strings"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/activitypub/mfm"
	"github.com/elythia-network/elythia/internal/misc/searchnorm"
)

// MaxUserTags は user.tags に格納する hashtag の最大件数。upstream
// (i/update / ApPersonService) の `.splice(0, 32)` と一致させる。
const MaxUserTags = 32

// MaxNoteTags は note.tags に格納する hashtag の最大件数。upstream
// NoteCreateService の `.splice(0, 32)` と一致させる。
const MaxNoteTags = 32

// MaxTagLength は note.tags / user.tags 列の varchar(128) 制約に合わせた
// tag 長上限。**これを超える tag は drop する** (truncate しない)。判定は
// byte 数ではなく code point 数で行う。multibyte rune を途中で分割すると
// 不正な UTF-8 になるため。
//
// upstream は note-tag 経路だけ `filter(t => Array.from(t).length <= 128)` を
// 持ち、**user-tag 経路には長さ判定が無い** (`extractApHashtags(person.tag)
// .map(normalizeForSearch).splice(0, 32)`)。mk-go は両方で落とす。
//
// 判定は**正規化の前後 2 回**行う。NFKC は合字を展開するので、前だけでは
// 膨らんだ値が列制約を超えて INSERT ごと落ちる (#2662)。
const MaxTagLength = 128

// Extract pulls hashtag names out of text fragments (typically the note's
// body and CW). Extraction goes through the full MFM parser, so hashtags
// inside code blocks, URLs, links and mentions are correctly excluded (the
// previous implementation used a regex scan with code-block / URL masking).
//
// Order is preserved by first occurrence; case-insensitive duplicates collapse
// to a single entry with the first-seen original case kept (Misskey TS も同じ
// 挙動)。Empty / whitespace-only inputs return nil. Tags longer than
// MaxTagLength are dropped.
func Extract(parts ...string) []string {
	tags := mfm.CollectHashtags(parts...)
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		// #2106 L65: byte 単位の truncate は multibyte rune を途中で分割し不正な UTF-8 を
		// 生成する。NormalizeNoteTags と同じく code point 数で判定し、>128 の tag は drop する
		// (upstream の user-tag 経路も truncate せず DB varchar 制約に委ねる)。
		if utf8.RuneCountInString(tag) > MaxTagLength {
			continue
		}
		key := strings.ToLower(tag)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, tag)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ExtractNoteTags extracts hashtags from the given text fragments and returns
// them in the form stored in note.tags: filtered to <=128 code points (dropped,
// not truncated), capped at MaxNoteTags, and normalized via searchnorm.Normalize
// (NFKC + lowercase), de-duplicated. Mirrors upstream NoteCreateService
// (`extractHashtags(...).filter(t => Array.from(t).length <= 128).splice(0, 32)`
// then `tags.map(normalizeForSearch)` on store, #1948-18). search-by-tag は同じ
// normalizeForSearch を query に適用するため、stored 値と一致する。
func ExtractNoteTags(parts ...string) []string {
	return NormalizeNoteTags(mfm.CollectHashtags(parts...))
}

// NormalizeNoteTags applies the note.tags store normalization to a pre-extracted
// tag list (used for AP-received apHashtags where the tags come from the remote
// Object rather than MFM extraction). >128 code-point tags are dropped (before
// and after normalization), the list is capped at MaxNoteTags, each is
// normalized, and duplicates collapse (#1948-18).
func NormalizeNoteTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	// upstream の順序 (NoteCreateService.ts:581,632) は raw dedup(extractHashtags の
	// unique) → filter(<=128 code point) → splice(0,32) → map(normalizeForSearch)。
	// 順序が重要: cap を normalize より**前**に効かせないと、case/width 衝突で空いた
	// cap slot に upstream が落とす tag が残り、>32 tag note で stored tags がずれる
	// (#1948-18)。よって RAW のまま dedup + 128filter + 32cap してから normalize する。
	rawSeen := make(map[string]struct{}, len(tags))
	capped := make([]string, 0, MaxNoteTags)
	for _, tag := range tags {
		if utf8.RuneCountInString(tag) > MaxTagLength {
			continue
		}
		if _, dup := rawSeen[tag]; dup {
			continue
		}
		rawSeen[tag] = struct{}{}
		capped = append(capped, tag)
		if len(capped) >= MaxNoteTags {
			break
		}
	}
	// normalize して post-normalize の clean dedup を行う。upstream は normalize 後の
	// duplicate (例 'Misskey'/'misskey' → ['misskey','misskey']) をそのまま格納するが、
	// `@> ARRAY[]` 検索・trends 集計は duplicate に非感応なので clean array にする
	// (cap の**順序**は upstream と一致させてある。ただし下の post-normalize
	// フィルタで落ちる tag があると surviving 集合自体は upstream とずれる。
	// upstream は同じ入力で INSERT ごと落ちるので mk-go が安全側)。
	normSeen := make(map[string]struct{}, len(capped))
	out := make([]string, 0, len(capped))
	for _, tag := range capped {
		n := searchnorm.Normalize(tag)
		if n == "" {
			continue
		}
		// NUL 入りの tag は落とす。`note.tags` / `user.tags` は
		// varchar(128)[] で NUL を受け付けず (SQLSTATE 22021)、INSERT ごと
		// 落ちて **その actor / Note が 1 行も作られない** (#2662)。
		// 正規化は NUL を除去しないのでここで見る。
		if strings.ContainsRune(n, 0) {
			continue
		}
		// ExtractUserTags と同じ理由で正規化のあとにもう一度長さを見る。
		// 上の 128 rune フィルタは正規化前の値に効くが、NFKC は合字を展開する
		// ので通過した tag が膨らむ (`㍿` x100 = 100 rune が 400 rune になる)。
		// `note.tags` は varchar(128)[] なので、超過値を渡すと note の
		// INSERT が SQLSTATE 22001 で落ち、**リモートの Note が取り込めない /
		// ローカルの投稿が失敗する** (#2662)。upstream も同じ穴を持つが
		// (`filter(<=128)` → `map(normalizeForSearch)` の順)、128 rune を
		// 超えた tag は検索にも使えないので落とす。
		if utf8.RuneCountInString(n) > MaxTagLength {
			continue
		}
		if _, ok := normSeen[n]; ok {
			continue
		}
		normSeen[n] = struct{}{}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ExtractUserTags extracts hashtags from the given text fragments and returns
// them in the form stored in user.tags: normalized via searchnorm.Normalize
// (NFKC + lowercase), de-duplicated, and capped at MaxUserTags. This mirrors
// Misskey's i/update / ApPersonService
// (`extractHashtags(...).map(normalizeForSearch).splice(0, 32)`), so the stored
// values match the normalized tag used by the hashtags/users containment query.
//
// local の profile description / remote の AP person.tag (Hashtag entry を
// "#tag" 文字列化したもの) のどちらを渡しても同じ正規化結果になる。
func ExtractUserTags(parts ...string) []string {
	raw := Extract(parts...)
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, tag := range raw {
		n := searchnorm.Normalize(tag)
		if n == "" {
			continue
		}
		// NormalizeNoteTags と同じ理由で NUL 入りの tag を落とす。
		if strings.ContainsRune(n, 0) {
			continue
		}
		// **正規化のあとにもう一度長さを見る。** Extract の 128 rune フィルタは
		// 正規化前の値に効くが、NFKC は合字を展開するので通過した tag が
		// 膨らむ (`㍿` x100 = 100 rune が 400 rune になる)。`user.tags` は
		// varchar(128)[] なので、超過値を渡すと `INSERT INTO "user"` が
		// SQLSTATE 22001 で落ち、**その actor が 1 行も作られない** (#2662)。
		// upstream も同じ穴を持つが (`extractApHashtags(...).map(normalizeForSearch)`
		// に長さ判定が無い)、128 rune を超えた tag は検索にも使えないので落とす。
		if utf8.RuneCountInString(n) > MaxTagLength {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
		if len(out) >= MaxUserTags {
			break
		}
	}
	return out
}
