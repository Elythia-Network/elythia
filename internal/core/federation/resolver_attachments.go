package federation

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/remoteurl"
	"github.com/elythia-network/elythia/internal/model"
)

// maxRemoteAttachments bounds how many AP `attachment` entries one inbound
// document can contribute.
//
// **upstream に上限は無い** (`ApNoteService` は `toArray(note.attachment)` を
// そのまま回す) が、mk-go では 1 件につき `drive_file` の SELECT + INSERT と、
// 未取り込みの添付なら先頭取得の**外向き GET** (#3243) が直列に走る。上限が
// 無いと、署名付き POST 1 通 (inbox の body 上限 64 KiB に添付 900 件が
// 収まる) で inbox worker を数十分占有できる。
//
// 値はローカルの受け入れ上限に揃える — `notes/create` の paramDef は
// `fileIds: maxItems 16` (upstream も同値、`validateCreateInput` 参照)。
// profile fields (maxRemoteFields) を i/update の 16 に揃えたのと同じ決め方。
// 超過分は落とす: 17 枚目以降を持つ note がローカルで作れない以上、リモート
// だけ受け入れる理由が無い。
const maxRemoteAttachments = 16

// maxRemoteEmojiTags bounds how many AP `tag` Emoji entries one inbound
// document (Note or Person) can contribute.
//
// upstream に上限は無いが、1 件につき `emoji` の INSERT / UPDATE が走り、名前は
// `note.emojis` / `user.emojis` (varchar(128)[]) にも載る。無制限だと 1 通で
// 任意件数の書き込みを強制できる。
//
// **hashtag の 32 には揃えない (レビュー M3)。** あちらは upstream 自身が
// `.splice(0, 32)` で切っているが、絵文字は upstream が切っていない。**絵文字を
// 33 種類以上使うノート (いわゆる絵文字アート) は珍しくなく**、32 にすると
// 正当な投稿が目に見えて劣化する (超過分は `:name:` のリテラルで表示される)。
// 実用上まず届かない値まで上げて、無制限の fan-out だけを止める。
const maxRemoteEmojiTags = 128

// extractAttachments parses the AP `attachment` array (heterogeneous []any
// after JSON unmarshal) and returns Document entries. type が upstream の
// `validDocumentTypes` ("Audio" / "Document" / "Image" / "Page" / "Video") の
// いずれかで `url` を持つもののみ採用する。#378 / #2662。
// noteSensitive は upstream の `attach.sensitive ??= note.sensitive` に対応する。
// 添付側に `sensitive` が無いとき note レベルの値を継ぐ。
//
// **採用件数は maxRemoteAttachments で打ち切る。** 打ち切りは採用側で数える
// (raw の要素数ではない) ので、type が合わない要素が前に並んでいても有効な
// 添付が削られない。
func extractAttachments(rawAttachments []any, noteSensitive bool) []activitypub.Document {
	if len(rawAttachments) == 0 {
		return nil
	}
	out := make([]activitypub.Document, 0, min(len(rawAttachments), maxRemoteAttachments))
	for _, raw := range rawAttachments {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := apTypeOf(m)
		switch typ {
		// upstream の validDocumentTypes は Page も含む (type.ts:263)。
		case "Document", "Image", "Audio", "Video", "Page":
			// ok
		default:
			continue
		}
		urlStr, _ := m["url"].(string)
		if urlStr == "" {
			continue
		}
		// 画面に出すときにプロキシで包めない形の URL の添付は取り込まない。
		// `url` は dedup の鍵 (`drive_file.uri`) にも使うので、ここでは符号化した
		// 値に置き換えず、保存する表示用の値は upsertAttachments で作る。
		if _, ok := remoteurl.Media(urlStr); !ok {
			continue
		}
		mediaType, _ := m["mediaType"].(string)
		name, _ := m["name"].(string)
		// upstream が実際に NSFW 判定に使うのは添付単位の `sensitive`
		// (`attach.sensitive ??= note.sensitive` → `DriveFile.isSensitive`)。
		// note レベルだけ寛容にして file レベルを `.(bool)` のままにすると、
		// `"sensitive": "true"` の添付が非 NSFW になる (#2662)。
		// 添付側に読める値が無ければ note レベルを継ぐ (upstream の `??=`)。
		// `??=` は null / undefined のときだけ代入するので、**欠落と明示 null は
		// 継承、読めた値は明示 false でも優先**する。
		sensitive := noteSensitive
		if raw, present := m["sensitive"]; present && raw != nil {
			if v, known := activitypub.ParseBoolish(raw); known {
				sensitive = v
			}
		}
		// width / height / blurhash / icon は省略実装が多いので欠落時は
		// zero value のまま。upsertAttachments 側で 0/空チェックして
		// DriveFile に書き込むかを判断する (#460/#461)。
		width := numberAsInt(m["width"])
		height := numberAsInt(m["height"])
		blurhash, _ := m["_misskey_blurhash"].(string)
		var icon *activitypub.Image
		if iconMap, ok := m["icon"].(map[string]any); ok {
			iconURL, _ := iconMap["url"].(string)
			iconType := apTypeOf(iconMap)
			// サムネイルは表示の補助でしかないので、崩れた形なら添付は残して
			// サムネイルだけ捨てる。
			if clean, ok := remoteurl.Media(iconURL); ok {
				icon = &activitypub.Image{Type: activitypub.APType(iconType), URL: activitypub.APLenientHref(clean)}
			}
		}
		out = append(out, activitypub.Document{
			Type:      typ,
			MediaType: mediaType,
			URL:       urlStr,
			Name:      name,
			Sensitive: sensitive,
			Width:     width,
			Height:    height,
			Icon:      icon,
			Blurhash:  blurhash,
		})
		if len(out) >= maxRemoteAttachments {
			break
		}
	}
	return out
}

// numberAsInt は JSON unmarshal 後の `any` を int に丸めて返す。
// encoding/json は数値を float64 でデコードするため、width/height の
// ような integer 想定 field を取り出すときに毎回 type switch するのを
// 避けるためのヘルパー。NaN / 負値は 0 として扱い、後段の `> 0`
// ガードで弾く。
func numberAsInt(v any) int {
	switch x := v.(type) {
	case float64:
		if x <= 0 {
			return 0
		}
		return int(x)
	case int:
		if x <= 0 {
			return 0
		}
		return x
	case int64:
		if x <= 0 {
			return 0
		}
		return int(x)
	default:
		return 0
	}
}

// upsertAttachments persists each AP Document as a drive_file row (link
// 形式、isLink=true、実体は保存せず先頭だけ取得する #3243) and returns the
// resulting drive_file IDs in original order. URI による dedup を行うので、同じ remote attachment が
// 複数の note に紐付いても drive_file は 1 行のみ。
//
// driveFileRepo が未設定なら空 (model.StringArray{}) を返す (旧挙動)。userID は
// リモート user の ID (note.UserID 相当)、host はリモート host (nil =
// ローカル、attachment 文脈ではほぼ常に non-nil)。

// attachmentFileName derives the drive file name from the attachment URL, the
// way upstream's uploadFromUrl does (`urlObj.pathname.split('/').pop()`, then
// `validateFileName` の不合格は `untitled`)。
//
// **upstream は実体を download して名前を決める** (Content-Disposition があれば
// それを優先する)。mk-go は実体を保存しない (docs/divergence/operations.md 5.5) が、先頭を
// 取得して Content-Disposition を見る (probeAttachment、#3243)。これは
// Content-Disposition から名前が取れなかったときの fallback — 取得の失敗、
// ヘッダ無し、名前の不合格 (制御文字・bidi 制御文字を含む)、エラーページと
// 判断して取得結果を捨てた場合。**拡張子の補完はしない** — upstream が付けるのは
// 実体を sniff した型であって、相手の申告した mediaType ではないため。
//
// 結果が upstream とずれるのは 4 つ。(1) この fallback に落ちたとき。取得に
// 失敗した添付では Content-Disposition の名前を使えない (**Misskey 同士では
// ここでずれる** — upstream は自分が配信するファイルに
// `Content-Disposition: inline; filename=...` を付けるので、upstream 側は原ファイル
// 名を採る)。名前が不合格のとき upstream は `untitled` にする。Latin-1 の名前は
// upstream だけが decode する (詳細は docs/divergence.md)。(2) 拡張子の補完。
// (3) Go の `net/url` は WHATWG URL の正規化をしないので `/a/%2e%2e` (upstream は
// 畳んで `untitled`) と `/a\b.png` (upstream は `\` を区切り扱いにして `b.png`) が
// ずれる。(4) upstream は `name === comment` のとき comment を落とすが mk-go は
// 残す。Mastodon 系はいずれにも当たらないので一致する。
//
// pathname と同じく percent-encoding は解かない (解くと `%2F` が `/` になり、
// upstream が弾く名前を作ってしまう)。
//
// **NUL の除去はしない。** `url.Parse` は制御文字を弾き、**唯一の例外である
// fragment は `EscapedPath()` に入らない**ので、basename に NUL は来ない。加えて
// 呼び出し元が `fitsColumn(doc.URL, ...)` で先に弾く。重ねると届かない検査が増える
// だけになる。
func attachmentFileName(rawURL string) string {
	const fallback = "untitled"
	u, err := url.Parse(rawURL)
	if err != nil {
		return fallback
	}
	// **`path.Base` は使わない。** 空文字に "."、末尾スラッシュに手前の segment を
	// 返すので、upstream が `untitled` にする入力で名前を作ってしまう。
	segs := strings.Split(u.EscapedPath(), "/")
	base := segs[len(segs)-1]
	if !coredrive.ValidateFileName(base) {
		return fallback
	}
	// ここで truncate はしない。`ValidateFileName` が 200 rune で止めるので列
	// (varchar(256)) には必ず入る。重ねると届かない検査が増えるだけになるので、
	// 収まることは `TestAttachmentFileName_FitsColumn` で固定する。
	return base
}

func (r *Resolver) upsertAttachments(docs []activitypub.Document, userID, host *string) model.StringArray {
	if r.driveFileRepo == nil || len(docs) == 0 {
		return model.StringArray{}
	}
	ids := make(model.StringArray, 0, len(docs))
	// 先頭取得 (probe) の予算は**この呼び出し全体**で 1 つ。添付ごとの
	// `attachmentFetchTimeout` は 1 本の GET しか縛らないので、直列に積まれると
	// 件数分だけ待たされる (attachmentProbeBudget の説明を参照)。
	//
	// **ジョブの ctx は届かない。** IngestNote / upsertAttachments は ctx を
	// 引き回していない (queue processor から resolver までの経路が ctx を取らない)
	// ので、ここで根を作る。ctx を通す改修が入ったらこの Background を差し替える。
	budget := r.probeBudget
	if budget <= 0 {
		budget = attachmentProbeBudget
	}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), budget)
	defer cancelProbe()
	for _, doc := range docs {
		// **URL が列に入らないなら添付ごと諦める。** `drive_file.url` は
		// varchar(1024) NOT NULL で、この添付の実体そのもの。切ると別の場所を
		// 指すし、捨てると表示できない (#2723)。`uri` も同じ値を入れる。
		//
		// **DB を引く前に見る。** NUL 入りの値は下の `FindByURI` の SELECT 自体が
		// 22021 で落ちる (機能は壊れないが、添付ごとに DB エラーが出る)。
		//
		// **長さ検査をここに置けるのは `drive_file.uri` も 1024 だから。** dedup に
		// 当たりうる値は必ずこの長さに収まる。`driveFileURLMaxRunes` を下げるか
		// `uri` を広げるなら、**長さ検査だけ** dedup の後ろへ移すこと (でないと DB に
		// 在る添付を捨てる)。NUL 検査はここに残す — 後ろに回すと上の 22021 が戻る。
		if !fitsColumn(doc.URL, driveFileURLMaxRunes) {
			slog.Warn("federation: skipping attachment whose url does not fit drive_file.url",
				"url", truncateRunes(doc.URL, driveFileURLMaxRunes))
			continue
		}
		// 表示に使う `url` は、形を確かめて符号化した値にする。extractAttachments
		// を通らずに来た Document もあるので、ここでも確かめる。符号化で長く
		// なって列に入らなければ、長すぎる値と同じく添付ごと諦める。
		mediaURL, ok := remoteurl.Media(doc.URL)
		if !ok || !fitsColumn(mediaURL, driveFileURLMaxRunes) {
			slog.Warn("federation: skipping attachment whose url is malformed",
				"url", truncateRunes(doc.URL, driveFileURLMaxRunes))
			continue
		}
		// URI ベースで dedup。既存ならその ID を再利用する。
		if existing, err := r.driveFileRepo.FindByURI(doc.URL); err == nil && existing != nil {
			ids = append(ids, existing.ID)
			continue
		}
		now := r.clock()
		// 先頭を 1 回だけ取得して、形式・名前・画像の寸法をまとめて読む (#3243)。
		// AP の申告だけで決めると、`mediaType` を持たない実装 (画像なのに
		// octet-stream になる) や、URL にファイル名を持たない実装 (名前が UUID や
		// API のパスになる) で壊れる。取得できなければ申告と推測に倒す。
		var probed attachmentProbe
		if r.attachmentProbeClient != nil {
			probed, _ = probeAttachment(probeCtx, r.attachmentProbeClient, doc.URL)
		}
		// メディアを名乗るのに文書が返ったら、200 のエラーページを読んだと
		// みなして取得結果ごと捨てる (形式だけでなく名前もエラーページのもの)。
		if claimsMedia(doc.MediaType, doc.Type) && isDocumentMIME(probed.MIME) {
			probed = attachmentProbe{}
		}
		// `drive_file.type` は varchar(128)。**切ると別の MIME type になる**ので、
		// 収まらない申告は採らない (#2723)。
		mediaType := resolveAttachmentMIME(probed.MIME, doc.MediaType, doc.Type, doc.URL)
		// AP の `name` は**代替テキスト**なので `comment` (varchar(512)) に入れる。
		// `name` (varchar(256)) は Content-Disposition か URL から作る — upstream の
		// `uploadFromUrl` と同じ置き場・同じ順にする (#2723)。
		//
		// **comment は列の上限で切る。** 説明が長い添付は入らず 22001 で落ち、
		// **その添付が丸ごと保存されない** (#2717)。rune 単位で切る — byte で
		// 切ると壊れた UTF-8 を書く。
		// **NUL も落とす。** 長さだけ直しても、制御文字が混じると 22021 で
		// 同じく添付が丸ごと落ちる (#2721 review MEDIUM-1)。
		safeName := sanitizeRemoteText(doc.Name)
		name := probed.FileName
		if name == "" {
			name = attachmentFileName(doc.URL)
		}
		var comment *string
		if safeName != "" {
			cn := truncateRunes(safeName, driveFileCommentMaxRunes)
			comment = &cn
		}
		uri := doc.URL
		f := &model.DriveFile{
			ID:             r.idGen.Generate(now),
			UserID:         userID,
			UserHost:       host,
			MD5:            "", // link 形式のため content hash 取得不可、placeholder
			Name:           name,
			Type:           mediaType,
			Size:           0, // 未 fetch のため未知
			Comment:        comment,
			URL:            mediaURL,
			URI:            &uri,
			IsLink:         true,
			IsSensitive:    doc.Sensitive,
			MaybeSensitive: doc.Sensitive,
			StoredInternal: false,
		}
		// AP Document に乗ってきた metadata を可能な限り永続化する
		// (#460 thumbnail / #461 properties)。link-format なので実体は
		// 保存しない。width/height は宣言を優先し、欠けていれば先頭取得で
		// 読んだ画像ヘッダの値で埋める。icon/blurhash は宣言をそのまま使う。
		// thumbnail / blurhash は**表示の補助でしかない**ので、列
		// (varchar(512) / varchar(128)) に入らなければ値ごと捨てて添付は残す。
		if doc.Icon != nil && doc.Icon.URL != "" {
			if thumb, ok := remoteurl.Media(doc.Icon.URL.String()); ok && fitsColumn(thumb, driveFileThumbMaxRunes) {
				f.ThumbnailURL = &thumb
			}
		}
		width := doc.Width
		height := doc.Height
		// 上流 Misskey TS の renderDocument は width/height を AP に
		// 載せないため、先頭取得で読めた画像ヘッダの寸法で埋める。取得
		// できなければ 0/0 のまま属性 JSON を空にしておき、表示側の
		// フォールバックに任せる。
		if width == 0 {
			width = probed.Width
		}
		if height == 0 {
			height = probed.Height
		}
		if width > 0 || height > 0 {
			// upstream Misskey は properties JSON を `{width, height}` で
			// 持つ。orientation は EXIF 由来で AP には来ないので省略。
			props := map[string]int{}
			if width > 0 {
				props["width"] = width
			}
			if height > 0 {
				props["height"] = height
			}
			if encoded, err := json.Marshal(props); err == nil {
				f.Properties = encoded
			}
		}
		if doc.Blurhash != "" && fitsColumn(doc.Blurhash, driveFileBlurhashMaxRune) {
			bh := doc.Blurhash
			f.Blurhash = &bh
		}
		if err := r.driveFileRepo.Create(f); err != nil {
			// **著者が materialize されていない場合は owner 無しで作り直す。**
			// リレー由来の投稿は著者の user 行を作らない (#2332) ので、
			// `drive_file.userId` の FK に当たる。ここで諦めると**その添付が
			// 表示されなくなる** — 添付は pack 時に DB から引くため、ephemeral
			// note でも行が要る。`userId` は NULL 可 (`ON DELETE SET NULL`) で、
			// 所有者不明の行はスキーマ上も想定済み。`userHost` は残すので
			// instance 単位の purge は効く (#2717)。
			if f.UserID != nil && isForeignKeyViolation(err) {
				f.UserID = nil
				retryErr := r.driveFileRepo.Create(f)
				if retryErr == nil {
					ids = append(ids, f.ID)
					continue
				}
				err = retryErr
			}
			slog.Warn("upsertAttachments: create failed",
				"url", doc.URL, "err", err)
			continue
		}
		ids = append(ids, f.ID)
	}
	return ids
}

// collectAttachedFileTypes returns the MIME type list for the given drive
// file IDs. note.AttachedFileTypes は TS 互換のためノート行に冗長保持される。
func (r *Resolver) collectAttachedFileTypes(fileIDs []string) model.StringArray {
	if r.driveFileRepo == nil || len(fileIDs) == 0 {
		return model.StringArray{}
	}
	files, err := r.driveFileRepo.FindByIDs(fileIDs)
	if err != nil {
		return model.StringArray{}
	}
	// FindByIDs の戻り順は不定なので id → type のマップで再整列する。
	byID := make(map[string]string, len(files))
	for _, f := range files {
		byID[f.ID] = f.Type
	}
	out := make(model.StringArray, 0, len(fileIDs))
	for _, id := range fileIDs {
		if t, ok := byID[id]; ok {
			out = append(out, t)
		}
	}
	return out
}
