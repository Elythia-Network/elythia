package admin

import (
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// 列幅は migration/000001_initial の `emoji` テーブル定義に対応する。
// 変えるときは DDL と揃えること。
const (
	emojiNameMaxRunes     = 128
	emojiCategoryMaxRunes = 128
	emojiAliasMaxRunes    = 128
	emojiLicenseMaxRunes  = 1024
	emojiRoleIDMaxRunes   = 128
	// `emoji.originalUrl` / `publicUrl` は varchar(512)。利用者の文字列が直接入る
	// のは `admin/emoji/add` の `url` 直接指定 (mk-go 独自の legacy 経路) だけだが、
	// drive が作った URL も同じ列に入るので #3023 でそちらにも掛けている。
	emojiURLMaxRunes = 512
)

// emojiBodyFits reports whether a locally entered body value can be stored in
// its column. A nil pointer (= 省略) は常に真。
//
// **弾く。切らない (#3018)。** ここを通るのは管理画面で人がその場で打った値で、
// 黙って切ると保存した本人にも分からない形で別の値になる。とくに `license` は
// 権利表示なので、切った結果は**嘘になる**。申請経路
// (`emojiapplication.Service.Create`) が利用者入力に対して既に `ErrTooLong` で
// 弾いており、そちらに揃える (あちらが守るのは `emoji_application` の同じ幅の列で、
// 別テーブル。**NUL も #3022 で同じ述語に揃えた**)。
//
// **切るのはリモート由来の値だけ** — `admin/emoji/copy` と AP 経路は相手サーバーが
// 決めた値を入れるので、弾くと取り込みそのものができなくなる (docs/divergence.md の
// 「リモート由来の文字列を列に入れるときの規則」、#2726)。
//
// NUL も `colfit.Fits` が落とす。PostgreSQL の text 系列は長さに関わらず NUL を
// SQLSTATE 22021 で弾くので、通すと同じく 500 になる。
func emojiBodyFits(v *string, max int) bool {
	return v == nil || colfit.Fits(*v, max)
}

// emojiFileURLFits reports whether the drive file's URLs fit `emoji.originalUrl`
// and `publicUrl` (#3023)。
//
// **`drive_file.url` は varchar(1024) で、`emoji` 側は varchar(512)。** 保存先の
// URL は `objectStorageBaseUrl` (+ prefix) + `/` + `accessKey` で決まる (`accessKey` は
// 32 桁の hex 固定なので、長さを決めるのは設定のほう) ので、長い prefix の
// オブジェクトストレージ構成では超えうる。通すと `Create` /
// `UpdateFields` が SQLSTATE 22001 で落ち、**操作者には直しようのない 5xx** になる。
//
// **列は広げない。** upstream も 512 (`models/Emoji.ts`) なので広げると TS が
// 保存できない値が入り、`emoji` は共有テーブルなので upstream 由来の列を `ALTER`
// すると復路が壊れる (down で narrow できない)。**URL は切らない** — 途中で切った
// URL は別物で、取りに行っても無駄なうえ壊れた参照を保存することになる
// (#3018 の `url` 直接指定と同じ判断)。
//
// **これで 5xx が全部消えるわけではない。** `Storage.Put` が返す URL は
// `base (+ prefix) + accessKey` で、`accessKey` は 32 桁の hex 固定なので、
// `url` / `thumbnailUrl` / `webpublicUrl` の長さは**必ず同じ**になる。
// `drive_file` 側の `thumbnailUrl` / `webpublicUrl` は varchar(512) なので、
// **`url` が 512 を超える構成ではサムネイルを作る画像は複製の INSERT が先に
// 22001 で落ちる** (= `Upload` の失敗として 500)。サムネイルは decode できる
// 画像なら必ず作られる (`isMimeImage` + `imagedecode.Decode`) ので、ここが結果を
// 変えるのは**サムネイルも webpublic も作られなかった行 (decode に失敗した画像) と、
// 複製を作らない経路 (元が既に system 所有 / fetcher 未配線)**。`drive_file` 自身の
// 列の食い違い (`url` は 1024 なのに派生 URL は 512) は別 issue。
//
// **`emoji.type` も見ていない。** varchar(64) に入るのは `webpublicType ?? type` で、
// `drive_file` 側はどちらも varchar(128)。**フォールバックする `type` のほうが本命** —
// `GenerateWebpublic` は EXIF が無く `webpublicMax` 以下の画像に nil を返すので、
// webpublic が作られないことのほうが多い。入りうる値は絵文字の MIME allowlist
// (最長は `image/vnd.mozilla.apng` の 22 文字) なので 64 には収まるが、上の
// `drive_file` の食い違いと同じ粒度で残っている。
//
// **`publicUrl` 側も見るが、現状は冗長。** 上の「長さが同じ」から `url` が入れば
// `publicUrl` も入る。導出を変えたときに素通りさせないために両方見る。
// **`f == nil` は呼び出し元が手前で弾いているので到達しない。** false を返すのは
// fail-closed のため (到達したら「URL が長すぎる」として 400 になる)。
func emojiFileURLFits(f *model.DriveFile) bool {
	return f != nil &&
		colfit.Fits(f.URL, emojiURLMaxRunes) &&
		colfit.Fits(preferWebpublicURL(f), emojiURLMaxRunes)
}

// emojiFileURLTooLong renders the 400 for a drive file whose URL cannot be
// stored on an emoji.
//
// **`INVALID_PARAM` を共有する** (#3018 と同じ理由)。
//
// **「別のファイルを選べば済む」ではない。** URL の長さは
// `base (+ prefix) + accessKey (32 桁固定)` で決まるので、同じ保存先の設定で選び直せば
// どのファイルも同じ長さになる (選び直しで直るのは、元が既に system 所有で、その行が
// 別の設定だったころに作られていた場合だけ)。**message では対処を指示せず、長さが
// どこで決まるかを書く** — 保存先はオブジェクトストレージとは限らず、モデレーターが
// 設定を変えられるとも限らないため。
func emojiFileURLTooLong(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
		"The drive file URL is too long to store on an emoji (max 512 characters). Its length is determined by the file storage base URL and prefix.",
		"3d81ceae-475f-4600-b2a8-2bc116157532"))
}

// emojiValueTooLong renders the 400 for a body value that does not fit.
//
// **`INVALID_PARAM` を共有する。** upstream の paramDef に maxLength は無いので
// 対応する error code / id が存在せず、新しい id を作ると misskey-js の型にも
// 載せることになる (`admin/emoji/copy` の名前検証 #2998 と同じ判断)。
func emojiValueTooLong(c echo.Context, field string) error {
	return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
		field+" is too long or contains an invalid character.",
		"3d81ceae-475f-4600-b2a8-2bc116157532"))
}

// normalizeEmojiAliases drops alias elements that cannot be stored.
//
// **要素ごとに落とす。切らない。** 1 つが長すぎるだけで他の alias まで捨てないが、
// 切ると別の名前になってリアクションの照合に使えないので、その要素は落とす。
// `admin/emoji/copy` (#2998) と申請経路の remote 取り込みも同じ関数を通る。
//
// **空要素も落とす。** 列には入るが、空の alias は照合に使えないうえ、NUL だけの
// 要素が `ToStorable` で空になったものと区別できない。`admin/emoji/copy` は #2998 から
// この挙動で、`add` / `update` / 一括編集は #3018 で揃えた (upstream は `[""]` を
// そのまま保存する)。
//
// 申請の作成側 (`internal/core/emojiapplication` の `normalizeAliases`) は**別物**で、
// あちらは `emoji_application` の列に対して trim / 重複排除 / 件数上限も掛ける。
// **NUL の扱いも違う** — あちらは NUL を含む要素を丸ごと落とし (#3022)、こちらは
// 下記のとおり NUL を除去して残す。共有していないのは書き込む先のテーブルが違うため。
//
// **NUL を含む要素だけは値が変わる** (`a\x00b` → `ab`)。「切らない」原則の例外で、
// NUL を含んだままでは長さに関わらず SQLSTATE 22021 で落ちるため。#2998 からの挙動。
func normalizeEmojiAliases(in []string) []string {
	return dropUnstorableElements(in, emojiAliasMaxRunes)
}

// fitEmojiAliases / fitEmojiRoleIDs normalize a locally entered array and report
// whether the request still means what the operator asked for (#3018)。
//
// **送った要素が全部落ちたら成功にしない。** 空配列を書くのは「全消去」なので、
// 「送ったのに 1 つも入らなかった」を 204 で返すと**既存の値を黙って消す**方向へ
// 倒れる (同梱 frontend の一括タグ付けは入力をそのまま `split(' ')` するだけなので、
// 長すぎる文字列を 1 つ貼れば選択中の全絵文字の alias が消える)。#3018 より前は
// NOT NULL 違反で 500 になっており、**うるさいがデータは無事**だった。
//
// **明示的な `[]` は通す** — あちらは「全消去」という意思表示で、落ちた結果ではない。
//
// **リモート由来の経路 (`admin/emoji/copy` / 申請の remote 取り込み) では使わない。**
// あちらは相手サーバーが決めた値なので、全部落ちたからといって取り込み自体を
// 失敗させる理由が無い (`normalizeEmojiAliases` をそのまま使う)。
func fitEmojiAliases(in []string) ([]string, bool) {
	return fitEmojiArray(in, emojiAliasMaxRunes)
}

func fitEmojiRoleIDs(in []string) ([]string, bool) {
	return fitEmojiArray(in, emojiRoleIDMaxRunes)
}

func fitEmojiArray(in []string, max int) ([]string, bool) {
	out := dropUnstorableElements(in, max)
	return out, len(in) == 0 || len(out) > 0
}

// normalizeEmojiRoleIDs drops role ids that cannot be stored (#3018).
//
// `emoji.roleIdsThatCanBeUsedThisEmojiAsReaction` も varchar(128)[] で、`aliases` と
// 同じ request struct から無検証で書かれていた。**実在するロールかは見ない** —
// それは別の検証で、ここは列に入るかだけを見る。id は aidx (16 文字) なので、
// 落ちるのは最初から存在しえない値だけ。
func normalizeEmojiRoleIDs(in []string) []string {
	return dropUnstorableElements(in, emojiRoleIDMaxRunes)
}

// dropUnstorableElements returns the elements of in that fit a varchar(max)[]
// column, with NUL stripped first.
//
// 返り値は必ず非 nil。呼び出し側は `req.Aliases != nil` で「フィールドを明示送信
// したか」を判定するし、`model.StringArray(nil)` は SQL の NULL になって
// NOT NULL 制約で落ちる (#729)。
func dropUnstorableElements(in []string, max int) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = colfit.ToStorable(v)
		if v == "" || !colfit.Fits(v, max) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// EmojiAddAliasesBulk handles POST /api/admin/emoji/add-aliases-bulk.
//
// 旧実装は id ごとに FindByID→UpdateFields を直列に呼んでいたが、件数が多いと
// DB 往復が線形に増える。FindManyByIDs で一括取得 → 個別に alias を merge →
// UpdateFields で書き戻す (alias はレコード毎に異なるため UpdateFieldsMany
// では一括化できない)。
func (h *Handler) EmojiAddAliasesBulk(c echo.Context) error {
	var req struct {
		IDs     []string `json:"ids"`
		Aliases []string `json:"aliases"`
	}
	if err := c.Bind(&req); err != nil {
		// 本家は paramDef の ajv 検査で 400 にする (#3330)。空の ids は本家でも
		// 通るので、下で従来どおり何もせず 204 を返す。
		return apierr.JSONInvalidParam(c)
	}
	if len(req.IDs) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if h.emojiRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	rows, err := h.emojiRepo.FindManyByIDs(req.IDs)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// **列に入らない alias を落としてから混ぜる (#3018)。** そのまま渡すと
	// `emoji.aliases` varchar(128)[] を超えて SQLSTATE 22001 になり、per-emoji の
	// log だけ残して全件が黙って失敗する。**全部落ちたら弾く** — 足すつもりの値が
	// 1 つも入らないのに 204 を返すと、成功したように見えて何も起きない。
	adding, ok := fitEmojiAliases(req.Aliases)
	if !ok {
		return emojiValueTooLong(c, "aliases")
	}
	for _, e := range rows {
		merged := dedupe(append(append([]string{}, e.Aliases...), adding...))
		// model.Emoji.Aliases は model.StringArray なので plain []string で
		// 渡すと GORM が record literal `('a','b')` を生成して
		// SQLSTATE 42804 (column type mismatch) になる drift がある
		// (#896 と同 pattern)。model.StringArray で wrap して
		// `'{a,b}'` array literal として書き込ませる。
		if err := h.emojiRepo.UpdateFields(e.ID, map[string]any{"aliases": model.StringArray(merged)}); err != nil {
			// per-emoji 失敗は bulk 操作全体を中断せず log で観測する
			// (#882 で発覚した silent failure 防止)。複数件のうち一部が
			// 失敗しても他の emoji は更新したい意図。
			slog.WarnContext(c.Request().Context(), "admin/emoji/add-aliases-bulk: per-emoji update failed",
				"emojiId", e.ID, "err", err)
		}
	}
	h.publishEmojiUpdatedByIDs(req.IDs) // #2046
	return c.NoContent(http.StatusNoContent)
}

// EmojiCopy handles POST /api/admin/emoji/copy.
//
// Misskey TS 本家 (admin/emoji/copy.ts) は元の name のまま local emoji を
// 追加し、同名の local emoji が既に存在する場合のみ DUPLICATE_NAME を
// 返す。以前の mk-go 実装は `_copy` suffix を勝手に付与していたが、
// これは TS 仕様違反 (#650 問題 1) なのでサフィックスを除去した。
//
// 画像については upstream の driveService.uploadFromUrl 相当に対応するため、
// emojiImageFetcher が wire 済みなら src.OriginalURL を local drive に
// 保存し、新 emoji の OriginalURL/PublicURL を drive 経由の URL に差し替える
// (#670)。fetcher 未配線時は src の URL をそのまま継承する legacy 挙動を
// 維持する (テスト容易性 + 未配線環境での graceful degradation 用)。
//
// ref: third_party/misskey/packages/backend/src/server/api/endpoints/admin/emoji/copy.ts
func (h *Handler) EmojiCopy(c echo.Context) error {
	// **上書き項目は mk-go 独自の additive パラメータ** (#2698)。upstream の
	// paramDef は `emojiId` のみ必須なので、足しても既存の呼び出しは通る。
	//
	// リモート絵文字をインポートするとき、AP では運ばれないカテゴリ・エイリアス・
	// センシティブを `admin/emoji/fetch-remote-meta` で取ってきて、確認・編集した値を
	// ここに渡す。**ポインタで受けるのは「指定なし」と「空を指定」を区別するため** —
	// 前者は src の値を保つ、後者は空にする。
	var req struct {
		EmojiID     string    `json:"emojiId"`
		Name        *string   `json:"name"`
		Category    *string   `json:"category"`
		Aliases     *[]string `json:"aliases"`
		License     *string   `json:"license"`
		IsSensitive *bool     `json:"isSensitive"`
	}
	if err := c.Bind(&req); err != nil || req.EmojiID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("emojiId is required."))
	}
	if h.emojiRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	src, err := h.emojiRepo.FindByID(req.EmojiID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_EMOJI", "No such emoji.", "e2785b66-dca3-4087-9cac-b93c541cc425"))
	}
	// **名前は検証してから使う (#2998)。** リモート絵文字の `name` は相手サーバーが
	// 決める値で、upstream の `admin/emoji/add` が強制する `^[a-zA-Z0-9_]+$` を
	// 満たすとは限らない (2026-09-15 の実測ではリモート 21,505 件のうち 10 件が制約外で、
	// `+_+` や `ablobcatnodmeltcry@3.5mbps.net` のような名前がある)。そのまま
	// コピーすると **MFM の `:name:` から参照できないローカル絵文字**ができ、しかも
	// 同じ名前を `add` で作ろうとすると 400 で弾かれるので、**経路によって通ったり
	// 通らなかったりする**。
	//
	// **upstream は検証しない** (`copy.ts` は `emoji.name` をそのまま
	// `customEmojiService.add` へ渡す) ので意図的な差分。弾くだけだと制約外の絵文字を
	// 取り込む手段が無くなるため、`name` の上書きを受けて人が決められるようにしてある
	// (`category` などと同じ additive なパラメータ)。
	name := src.Name
	if req.Name != nil {
		name = *req.Name
	}
	//
	// **長さも見る。** `^[a-zA-Z0-9_]+$` は文字種しか縛らないので、`emoji.name`
	// varchar(128) を超える名前が素通りして `Create` が SQLSTATE 22001 で落ちる
	// (リモートへ 1 往復して drive ファイルを作った後に 500)。兄弟の上書きは
	// `colfit` で切るが、**名前は切ると別物になる**ので弾く (AP 経路も
	// `emojiNameMaxRunes` で同じ値を見ている)。
	if !emojiNamePattern.MatchString(name) || utf8.RuneCountInString(name) > emojiNameMaxRunes {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid emoji name.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// **重複チェックを DB 障害で skip しない** (#2792)。`err == nil` だけを見ると、
	// 接続断のときに「重複していない」と判断して同名の絵文字を作ってしまう。
	// not-found のときだけ「重複なし」と扱う。
	existing, dupErr := h.emojiRepo.FindByNameAndHost(name, nil)
	if dupErr != nil && !repository.IsNotFound(dupErr) {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if dupErr == nil && existing != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("DUPLICATE_NAME", "Duplicate name.", "f7a3462c-4e6e-4069-8421-b9bd4f4c3975"))
	}
	copied := *src
	copied.ID = h.idGen.Generate(time.Now())
	copied.Host = nil
	copied.Name = name
	// **`uri` は元のまま継承する (#2998)。** 名前を変えても出自は変わらないので、
	// 承認経路 (`emoji_application.go`) と同じ扱いにしてある。ただし AP の tag には
	// `id` として出る (`renderer.go`) ので、名前を変えた絵文字は
	// `{id: <元の URI>, name: ":<新しい名前>:"}` になり両者が食い違う。受け手は
	// name + host で照合するので実害は見つかっていないが、`tag.id` を辿る実装には
	// 別の名前のオブジェクトが返る。

	// 指定された項目だけ上書きする (#2698)。
	//
	// **列に収まる形に整えてから入れる。** 値の出どころは相手サーバーの
	// `/api/emoji` (= 相手が決める値) で、そのまま渡すと `emoji.category`
	// varchar(128) / `license` varchar(1024) / `aliases` varchar(128)[] を
	// 超えて SQLSTATE 22001 になり、インポートが 500 で落ちる。AP 経路は
	// `internal/core/federation` が同じ 3 列に対して既に同じ規則を持っており
	// (#2726、docs/divergence.md の「リモート由来の文字列を列に入れるときの
	// 規則」)、REST 経路だけ素通しにすると非対称になる。
	//
	// **本文は切り、要素は落とす。** category / license は本文なので切る
	// (`colfit.Text`)。alias は 1 つが長すぎるだけなので、その要素だけ落として
	// 他は残す (切ると別の名前になり、リアクションの照合に使えない)。
	if req.Category != nil {
		v := colfit.Text(*req.Category, emojiCategoryMaxRunes)
		copied.Category = &v
	}
	if req.Aliases != nil {
		copied.Aliases = model.StringArray(normalizeEmojiAliases(*req.Aliases))
	}
	if req.License != nil {
		v := colfit.Text(*req.License, emojiLicenseMaxRunes)
		copied.License = &v
	}
	if req.IsSensitive != nil {
		copied.IsSensitive = *req.IsSensitive
	}

	// fetcher が wire され src.OriginalURL があれば drive に保存して URL
	// を切り替える。drive file は upstream Misskey TS の uploadFromUrl
	// ({user: null}) と同じく **system 所有 (user nil)** で作成する。
	// custom emoji はインスタンス管理アセットであり、操作者個人の drive に
	// 紐付けるとロール変更や削除で巻き込まれて表示が壊れる。失敗時は
	// INTERNAL_ERROR を返して emoji 作成自体を中止する (URL 引き継ぎだけで
	// 作成すると #670 の症状に逆戻りするため)。
	systemFileID := ""
	if h.emojiImageFetcher != nil && src.OriginalURL != "" {
		df, err := h.emojiImageFetcher.FetchAndStore(c.Request().Context(), src.OriginalURL, nil, name)
		if err != nil {
			slog.WarnContext(c.Request().Context(), "emoji copy: drive fetch failed",
				"srcId", src.ID, "url", src.OriginalURL, "err", err)
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Failed to fetch emoji image.", "0a4e0b9e-2d7c-4d6f-8f6b-1f9c2e9b4d83"))
		}
		systemFileID = df.ID
		// **取り込んだものの MIME を見る (#2998)。** 承認経路は #2966 で既に見て
		// いるので、こちらだけ無検査だと**承認で弾かれるものが copy なら通る**という
		// 迂回路になる (`emoji_application.go` のコメントが「EmojiCopy も同じ穴」と
		// 書いていたのがここ)。相手が `originalUrl` に非画像を置くと、それがそのまま
		// 絵文字として登録される。`admin/emoji/add` も fileId 経路で同じ検証をする。
		//
		// **弾いたら片付ける。** その時点で誰からも参照されないので、残すと孤児になる。
		if !isAllowedEmojiImageType(df.Type) {
			h.deleteSystemEmojiFile(c.Request().Context(), systemFileID)
			return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
		}
		// **URL が列に入るかを見る (#3023)。** 取り込んだ実体の URL は保存先が
		// 決めるので、作ってからでないと分からない。弾いたら片付ける。
		if !emojiFileURLFits(df) {
			h.deleteSystemEmojiFile(c.Request().Context(), systemFileID)
			return emojiFileURLTooLong(c)
		}
		// 不変条件 (#722): emoji.originalUrl は必ず drive_file.url と一致
		// させる。`DriveFileRepository.DeleteOrphans` の cleanup guard が
		// `NOT EXISTS (emoji.originalUrl = drive_file.url ...)` で system 所有
		// emoji 画像を保護しているので、ここで df.URL 以外を入れると guard を
		// すり抜けて cleanup で消える。webpublic 系列を直接 originalUrl に
		// セットする変更を入れる際は cleanup 側も同時更新すること。
		copied.OriginalURL = df.URL
		if df.WebpublicURL != nil && *df.WebpublicURL != "" {
			copied.PublicURL = *df.WebpublicURL
		} else {
			copied.PublicURL = df.URL
		}
		// **導出は共有ヘルパーに寄せる (#2998)。** `EmojiAdd` も承認経路の 2 つも
		// `preferWebpublicType` を通しており、ここだけ手で書くと片方だけ直す事故に
		// なる (実際、両方が空のときの戻りが違っていた)。
		copied.Type = preferWebpublicType(df)
	}

	if err := h.emojiRepo.Create(&copied); err != nil {
		// **取り込んだものを片付ける (#2998)。** ここまで来た drive ファイルは
		// 誰からも参照されないので、残すと孤児になる。`DeleteOrphans` の対象では
		// あるが自動では走らないので、掃除するまで実体ストレージを食い続ける。
		// 承認経路 (`emoji_application.go`) は #2966 で同じ形にしてある。
		//
		// **ただし載ったかを読み直してから (#3019)。** INSERT が commit 済みで
		// ack だけ失われた場合に消すと、名前が使用中のまま画像だけ無い状態になり、
		// 同じ名前で取り込み直しても `DUPLICATE_NAME` で弾かれる。
		h.cleanupUnreferencedEmojiCopy(c.Request().Context(), copied.ID, systemFileID, copied.OriginalURL)
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.logModeration(c, moderationlog.LogAddCustomEmoji, map[string]any{
		"emojiId": copied.ID,
		"emoji":   &copied,
	})
	h.publishEmojiAdded(&copied) // #2046
	return c.JSON(http.StatusOK, map[string]any{"id": copied.ID})
}

// EmojiDeleteBulk handles POST /api/admin/emoji/delete-bulk.
//
// Misskey TS の CustomEmojiService.deleteBulk() は削除対象を for ループで
// 一件ずつ処理し、絵文字ごとに deleteCustomEmoji moderation log を書く。
// 互換性のため mk-go も削除前の snapshot を取得して per-emoji log を出力
// する。bulk 削除自体は 1 SQL (DeleteMany) で完結。
func (h *Handler) EmojiDeleteBulk(c echo.Context) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := c.Bind(&req); err != nil {
		// 本家は paramDef の ajv 検査で 400 にする (#3330)。空の ids は本家でも
		// 通るので、下で従来どおり何もせず 204 を返す。
		return apierr.JSONInvalidParam(c)
	}
	if len(req.IDs) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if h.emojiRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// per-emoji log のため削除前に snapshot を取得。失敗しても削除自体は
	// 続行する (snapshot 失敗で監査が落ちるより操作完遂が優先)。エラー時は
	// log が出ない理由を debug-level で残して運用時の追跡を可能にする。
	snapshots, err := h.emojiRepo.FindManyByIDs(req.IDs)
	if err != nil {
		slog.DebugContext(c.Request().Context(), "moderation log: snapshot lookup failed before delete-bulk",
			"ids", req.IDs, "err", err)
	}
	if err := h.emojiRepo.DeleteMany(req.IDs); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// per-emoji log を 1 batch にまとめる (#671)。Misskey TS 互換は per-emoji
	// row 単位なので結果として永続化される行数は同じだが、N goroutine + N
	// INSERT を 1 goroutine + 1 multi-row INSERT に圧縮する。
	if len(snapshots) > 0 {
		entries := make([]moderationlog.Entry, 0, len(snapshots))
		for _, e := range snapshots {
			entries = append(entries, moderationlog.Entry{
				Type: moderationlog.LogDeleteCustomEmoji,
				Info: map[string]any{
					"emojiId": e.ID,
					"emoji":   e,
				},
			})
		}
		h.logModerationMany(c, entries)
	}
	// 削除前 snapshot を broadcast (#2046)。snapshot 取得失敗時は空で no-op。
	h.publishEmojiDeleted(snapshots...)
	return c.NoContent(http.StatusNoContent)
}

// dedupe returns the slice with duplicates removed while preserving order.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// EmojiImportZip handles POST /api/admin/emoji/import-zip.
//
// fileId で Drive にアップロード済みの ZIP を指定し、非同期ジョブで展開して
// ローカルカスタム絵文字として登録する。本家 Misskey 互換
// (QueueService.createImportCustomEmojisJob 相当)。
func (h *Handler) EmojiImportZip(c echo.Context) error {
	var req struct {
		FileID string `json:"fileId"`
	}
	if err := c.Bind(&req); err != nil || req.FileID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "fileId is required.", "5f4c9d8a-7c39-4bfa-9dcb-09f17e0f7a25"))
	}
	// upstream import-zip.ts:30 は drive file の存在確認をせず無条件で
	// createImportCustomEmojisJob を enqueue する (存在しなければ job が後で失敗)。
	// mk-go の同期 NO_SUCH_FILE check は upstream に無い divergence なので外す
	// (= 不正 fileId でも 204 を返し job 側に委ねる、#1999)。
	if h.emojiEnqueuer == nil {
		return c.NoContent(http.StatusNoContent)
	}
	user := middleware.GetUser(c)
	if user == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.emojiEnqueuer.EnqueueImportCustomEmojis(queue.ImportCustomEmojisPayload{
		UserID: user.ID,
		FileID: req.FileID,
	}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Failed to enqueue emoji import.", "89a6d9fd-0fe6-4c3c-9daa-7c6b1f29f1a4"))
	}
	return c.NoContent(http.StatusNoContent)
}

// EmojiListRemote handles POST /api/admin/emoji/list-remote.
func (h *Handler) EmojiListRemote(c echo.Context) error {
	if h.emojiRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	var req struct {
		Query     string `json:"query"`
		Host      string `json:"host"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		Limit     *int   `json:"limit"`
		Offset    int    `json:"offset"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// upstream list-remote.ts:69 は host を toPuny (lowercase + IDN punycode) して
	// から equality 突合する。保存側も #2706 で同じ正規化を掛けるようになったので、
	// IDN / 大文字混在の host param を正規化しないと match しない (#1948-13)。
	// **非正規化のまま保存された行は引けない** — 完全一致なので、`Mixed.Example` の
	// ような表記で入った emoji は `cmd/backfill-remote-host` を流すまで出てこない
	// (user の acct 解決も #2996 で同じになった)。
	host := req.Host
	if host != "" {
		host = toPunyHost(host)
	}
	emojis, err := h.emojiRepo.ListRemoteWithFilter(req.Query, host, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if emojis == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream Misskey の admin/emoji/list-remote は EmojiDetailed schema を
	// 返す (= `url` field、`publicUrl` ではない)。frontend の旧
	// custom-emojis-manager.vue は emoji.url を読み込んで <img> を組み立てる
	// ため、raw model.Emoji (publicUrl / originalUrl 持ち、url 無し) を返すと
	// 画像が壊れて表示そのものが空に見える (#466)。entity.PackEmojiDetailedList
	// で publicUrl → url 変換した shape にする。
	return c.JSON(http.StatusOK, entity.PackEmojiDetailedList(emojis))
}

// EmojiRemoveAliasesBulk handles POST /api/admin/emoji/remove-aliases-bulk.
func (h *Handler) EmojiRemoveAliasesBulk(c echo.Context) error {
	var req struct {
		IDs     []string `json:"ids"`
		Aliases []string `json:"aliases"`
	}
	if err := c.Bind(&req); err != nil {
		// 本家は paramDef の ajv 検査で 400 にする (#3330)。空の ids は本家でも
		// 通るので、下で従来どおり何もせず 204 を返す。
		return apierr.JSONInvalidParam(c)
	}
	if len(req.IDs) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if h.emojiRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	rows, err := h.emojiRepo.FindManyByIDs(req.IDs)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	removeSet := make(map[string]bool, len(req.Aliases))
	for _, a := range req.Aliases {
		removeSet[a] = true
	}
	for _, e := range rows {
		filtered := make([]string, 0, len(e.Aliases))
		for _, a := range e.Aliases {
			if !removeSet[a] {
				filtered = append(filtered, a)
			}
		}
		// model.StringArray wrap (#896 と同 pattern) — add-aliases-bulk と
		// 同じ理由で plain []string では SQLSTATE 42804 になる。
		if err := h.emojiRepo.UpdateFields(e.ID, map[string]any{"aliases": model.StringArray(filtered)}); err != nil {
			// per-emoji 失敗は他 emoji への影響を避けるため warn log で
			// 観測しつつ continue する (add-aliases-bulk と同 policy)。
			slog.WarnContext(c.Request().Context(), "admin/emoji/remove-aliases-bulk: per-emoji update failed",
				"emojiId", e.ID, "err", err)
		}
	}
	h.publishEmojiUpdatedByIDs(req.IDs) // #2046
	return c.NoContent(http.StatusNoContent)
}

// EmojiSetAliasesBulk handles POST /api/admin/emoji/set-aliases-bulk.
//
// 全 id で同じ aliases 値を設定するため UpdateFieldsMany (WHERE IN UPDATE) で
// 1 文に集約できる。
func (h *Handler) EmojiSetAliasesBulk(c echo.Context) error {
	var req struct {
		IDs     []string `json:"ids"`
		Aliases []string `json:"aliases"`
	}
	if err := c.Bind(&req); err != nil {
		// 本家は paramDef の ajv 検査で 400 にする (#3330)。空の ids は本家でも
		// 通るので、下で従来どおり何もせず 204 を返す。
		return apierr.JSONInvalidParam(c)
	}
	if len(req.IDs) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if h.emojiRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// **省略を「全消去」にしない (#3018)。** upstream の paramDef は `aliases` を
	// required にしているので、落として送るのは schema validator で 400 になる形。
	// mk-go は #3018 まで `model.StringArray(nil)` を書いており、`aliases` は
	// NOT NULL なので**書き込みが落ちて 500** = データは無事だった。正規化を通すと
	// nil が `{}` になり、**黙って全件の alias を消す**方向へ倒れる。
	// `add-aliases-bulk` / `remove-aliases-bulk` は省略しても merge / filter が
	// no-op になるだけなので、この判定は要らない。
	if req.Aliases == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "aliases is required.",
			"3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 列に入らない要素は落とす。**全部落ちたら弾く** — 空配列を書くと全消去になる (#3018)。
	aliases, ok := fitEmojiAliases(req.Aliases)
	if !ok {
		return emojiValueTooLong(c, "aliases")
	}
	// model.StringArray wrap (#896 と同 pattern) — UpdateFieldsMany 経由でも
	// 同 drift が発生するので caller 側で wrap する。
	if err := h.emojiRepo.UpdateFieldsMany(req.IDs, map[string]any{"aliases": model.StringArray(aliases)}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.publishEmojiUpdatedByIDs(req.IDs) // #2046
	return c.NoContent(http.StatusNoContent)
}

// EmojiSetCategoryBulk handles POST /api/admin/emoji/set-category-bulk.
func (h *Handler) EmojiSetCategoryBulk(c echo.Context) error {
	var req struct {
		IDs      []string `json:"ids"`
		Category *string  `json:"category"`
	}
	if err := c.Bind(&req); err != nil {
		// 本家は paramDef の ajv 検査で 400 にする (#3330)。空の ids は本家でも
		// 通るので、下で従来どおり何もせず 204 を返す。
		return apierr.JSONInvalidParam(c)
	}
	if len(req.IDs) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if h.emojiRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// 列に入らない値は 400 で返す (#3018)。渡すと SQLSTATE 22001 が生のまま 500。
	if !emojiBodyFits(req.Category, emojiCategoryMaxRunes) {
		return emojiValueTooLong(c, "category")
	}
	// upstream set-category-bulk は category nullable ('Use null to reset') で
	// ps.category ?? null を書く。*string にして JSON null を SQL NULL に落とす
	// (旧 non-pointer string だと null が "" になっていた、#1948-13)。
	if err := h.emojiRepo.UpdateFieldsMany(req.IDs, map[string]any{"category": nullableString(req.Category)}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.publishEmojiUpdatedByIDs(req.IDs) // #2046
	return c.NoContent(http.StatusNoContent)
}

// nullableString returns the dereferenced string for a non-nil pointer, else nil
// so a GORM map Updates writes SQL NULL (matching upstream's `?? null` reset
// semantics for nullable emoji fields, #1948-13).
func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// EmojiSetLicenseBulk handles POST /api/admin/emoji/set-license-bulk.
func (h *Handler) EmojiSetLicenseBulk(c echo.Context) error {
	var req struct {
		IDs     []string `json:"ids"`
		License *string  `json:"license"`
	}
	if err := c.Bind(&req); err != nil {
		// 本家は paramDef の ajv 検査で 400 にする (#3330)。空の ids は本家でも
		// 通るので、下で従来どおり何もせず 204 を返す。
		return apierr.JSONInvalidParam(c)
	}
	if len(req.IDs) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if h.emojiRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// 列に入らない値は 400 で返す (#3018)。
	if !emojiBodyFits(req.License, emojiLicenseMaxRunes) {
		return emojiValueTooLong(c, "license")
	}
	// upstream set-license-bulk も license nullable で ps.license ?? null を書く (#1948-13)。
	if err := h.emojiRepo.UpdateFieldsMany(req.IDs, map[string]any{"license": nullableString(req.License)}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.publishEmojiUpdatedByIDs(req.IDs) // #2046
	return c.NoContent(http.StatusNoContent)
}
