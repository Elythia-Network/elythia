package federation

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/remoteurl"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// mergeMentionIDs merges two ordered ID slices preserving order and removing
// duplicates. text 由来 mention と AP `tag` Mention 由来 mention を合算する
// (#397) ために使う。両方空なら nil ではなく非 nil 空 (pq の '{}' 既定値と
// 整合) を返す。
func mergeMentionIDs(a, b []string) model.StringArray {
	out := make(model.StringArray, 0, len(a)+len(b))
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, id := range a {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range b {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// extractMentionTags parses the Tag array of a Note and returns the `href`
// of every Mention entry (type="Mention"). AP では宛先 actor URI が tag 配列
// の Mention に乗ってくるため、本文 @mention 抽出だけでは specified DM の
// 受信者を取りこぼす (#397)。href が空のものはスキップ。
func extractMentionTags(tags []any) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		m, ok := tag.(map[string]any)
		if !ok {
			continue
		}
		if apTypeOf(m) != "Mention" {
			continue
		}
		href, _ := m["href"].(string)
		if href == "" {
			continue
		}
		out = append(out, href)
	}
	return out
}

// inboundMentionFetchBudget is the wall-clock budget of the unknown-actor
// fetches of one inbound note. 予算を過ぎたら次の取得を始めない。本家には全体の
// 締め切りが無い。notes/create のメンションの取得 (defaultRemoteMentionFetchTimeout)
// と同じ 20 秒。
//
// **始めた 1 件は止めない。** その 1 件は actor の取得 (fetcher の timeout) に
// 加えて、新しい actor を作る経路では notifyInstance → RegisterFromHost が
// インスタンスのメタデータ (nodeinfo など) の取得を同期で行うので、予算の 20 秒に
// それらの timeout ぶん (最悪 1〜2 分程度) が足されうる。
var inboundMentionFetchBudget = 20 * time.Second

// fetchUnknownMentionActors resolves (fetches and stores) the remote actors
// among an inbound note's Mention hrefs and specified recipients that are not
// in the DB yet, like upstream extractApMentions / parseAudience, which call
// ApPersonService.resolvePerson on each and ignore failures.
//
// 本家との違い (docs/divergence.md):
//
//   - **直列に取る。** 本家は 2 並列。resolveChain は 1 つの木を 1 つの goroutine
//     が触る前提 (resolve_waits.go) なので、同じ木の中で並列にすると待ちの
//     循環を検出できなくなる
//   - **件数と時間に上限を置く**。件数は投稿者の mentionLimit ポリシー
//     (mentionLimitFor。取り込みの上限判定と同じ値) で、それより多く解決できたら
//     どのみち上限で弾かれる。時間は inboundMentionFetchBudget。到達できなかったホストの残りは取りに行かない
//   - **同時に取りに行くノートの数を inbox の worker 全体で絞る**
//     (inboundMentionFetchSlots)。枠が取れなければ取りに行かず、DB の照合だけに
//     する。応答しないホスト (ワイルドカード DNS の別サブドメインなど) への
//     メンションを並べたノートを送り続けても、止まる worker は枠の数まで
//   - **取得に失敗した URI と到達できなかったホストを短い間覚えて飛ばす**
//     (inboundMentionFailureTTL)
//   - **新しく取り込んだ actor の featured は取らない。** 取ると 1 件の配送から
//     メンションの数 × ピン留めの数の取得が起きる。次に actor を取り直したとき
//     (TTL 切れ) に埋まる
//   - 連合しないホストは、取りに行く前に飛ばす (どのみち fetchActor が拒否する)
//   - mentionLimit を超える数の Mention を持つ note は、取得しても数えた時点で
//     弾かれるので、何も取りに行かない
//
// DB の照合に失敗したら、そこで止める。続く resolveMentionedUserIDs が同じ
// 障害を error にして、inbox job が retry される。
//
// mentionLimit は投稿者の mentionLimit ポリシーを遅延で返す (lazyMentionLimit)。
// 取りに行く候補が無ければ呼ばない (メンションの無い note で role policy を
// 引かない)。
func (r *Resolver) fetchUnknownMentionActors(mentionLimit func() int, tagHrefs, audience []string, chain *resolveChain) {
	if r.userRepo == nil {
		return
	}
	rawTags := make(map[string]struct{}, len(tagHrefs))
	for _, h := range tagHrefs {
		rawTags[h] = struct{}{}
	}
	if len(rawTags) > 0 && len(rawTags) > mentionLimit() {
		return
	}
	guard := r.mentionFetch
	if guard == nil {
		return
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	start := time.Now()
	seen := make(map[string]struct{})
	attempts := 0
	for _, list := range [][]string{tagHrefs, audience} {
		for _, href := range list {
			if _, dup := seen[href]; dup {
				continue
			}
			seen[href] = struct{}{}
			if href == "" || r.isSelfHostURI(href) {
				continue
			}
			// http(s) 以外は取りに行かない。`ftp://` などは通信せずに失敗するので、
			// 取りに行っても何も得られない。
			if !isHTTPURI(href) {
				continue
			}
			host, err := hostFromURI(href)
			if err != nil || !r.hostPolicyAllowed(host) {
				continue
			}
			if guard.failedRecently(href, host, r.clock()) {
				continue
			}
			switch _, ferr := r.userRepo.FindByURI(href); {
			case ferr == nil:
				continue
			case !repository.IsNotFound(ferr):
				return
			}
			// 受信を拒否するソフトウェアの判定は instance の行を引くので、既知の利用者
			// (上の FindByURI で当たったもの) を除いてから見る (#3563)。既知の利用者は
			// どのみち取りに行かないので、結果は変わらない。
			if !r.hostSoftwareAllowed(host) {
				continue
			}
			if attempts >= mentionLimit() || time.Since(start) > inboundMentionFetchBudget {
				slog.Info("federation: stopped fetching unknown mentioned actors",
					"attempts", attempts, "elapsed", time.Since(start))
				return
			}
			if release == nil {
				var ok bool
				if release, ok = guard.tryAcquire(); !ok {
					slog.Debug("federation: skipped fetching unknown mentioned actors: all slots busy")
					return
				}
			}
			attempts++
			if _, err := r.resolveActor(href, false, true, chain); err != nil {
				// DB の障害と、解決の待ち合わせの都合で降りたものは覚えない (相手の
				// 不備ではない)。
				if !errors.Is(err, ErrLookupUnavailable) && !errors.Is(err, ErrResolveWouldDeadlock) &&
					!errors.Is(err, ErrResolveJoinTimeout) && !errors.Is(err, ErrResolveWouldBlock) {
					guard.recordFailure(href, host, isUnreachableHostError(err, host), r.clock())
				}
				slog.Debug("federation: could not resolve mentioned actor",
					"uri", truncateRunes(href, userURIMaxRunes), "err", err)
			}
		}
	}
}

// resolveMentionedUserIDs maps actor URIs (typically from AP Mention tags or
// the `to` array) to local model.User IDs. ローカル URI は ExtractLocalUserID
// で安価に変換し、リモート URI は DB に取り込み済みのものを
// userRepo.FindByURI でルックアップする。未知のリモート URI は ingest の前段の
// fetchUnknownMentionActors が取り込んでおく (ここでは取りに行かない)。
// 返り値は入力順を保ち重複排除する。
//
// **引けなかったものを「未知」に潰さない** (#3121)。潰すと `mentions` や
// specified note の `visibleUserIds` が空のまま note が確定し、**URI は DB に
// 入るので retry は dedup にヒットして二度と直らない**。DM なら受信者本人が
// 本文を読めなくなる (`core/note/visibility.go` の `CanView` は空配列を見る)。
func (r *Resolver) resolveMentionedUserIDs(hrefs []string, chains ...*resolveChain) ([]string, error) {
	if len(hrefs) == 0 {
		return nil, nil
	}
	existingLocal, err := r.existingLocalUserIDs(hrefs)
	if err != nil {
		if len(chains) > 0 && chains[0] != nil && chains[0].inboundLocalReferences {
			return nil, fmt.Errorf("%w: resolve existing local mentioned users: %w", ErrInboundLocalReferenceFailure, err)
		}
		return nil, err
	}
	seen := make(map[string]struct{}, len(hrefs))
	out := make([]string, 0, len(hrefs))
	for _, href := range hrefs {
		var id string
		if local := r.ExtractLocalUserID(href); local != "" {
			if _, ok := existingLocal[local]; ok || existingLocal == nil {
				id = local
			}
		} else if r.userRepo != nil {
			u, err := r.userRepo.FindByURI(href)
			switch {
			case err == nil && u != nil:
				id = u.ID
			case err != nil && !repository.IsNotFound(err):
				return nil, fmt.Errorf("resolve mentioned user %q: %w", truncateRunes(href, userURIMaxRunes), err)
			}
		}
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(chains) > 0 && chains[0] != nil && chains[0].inboundLocalReferences {
		if err := r.ObserveLocalInboxBodyReferences(out, nil, chains[0].inboundSourceHost); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// extractHashtagTagNames parses the Tag array of a Note and returns the
// `name` of every Hashtag entry (type="Hashtag"). upstream Misskey の
// renderHashtag は `name: "#" + tag` 形式で出すので、戻り値も `#tag`
// プレフィクス付き。呼び出し側で hashtag.Extract に渡せば本文由来の
// 抽出と統一的に dedup / 正規化される (#679)。
//
// AP spec は `name` の format を厳密に規定していないため、`#` 無しで
// 来る実装 (稀) のために defensive に prefix を補う。よって戻り値は
// 必ずしも入力 `name` と完全一致しない (`"tag"` → `"#tag"` に書き換え
// られる場合がある)。`name` が空のものはスキップ。type 違いも skip。
func extractHashtagTagNames(tags []any) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		m, ok := tag.(map[string]any)
		if !ok {
			continue
		}
		if apTypeOf(m) != "Hashtag" {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		// `#` prefix 欠落は upstream の renderHashtag では起きないが、
		// 他実装が `name: "tag"` だけ送ってきた場合に hashtag.Extract の
		// 正規表現にマッチさせるため補完する。
		if !strings.HasPrefix(name, "#") {
			name = "#" + name
		}
		out = append(out, name)
	}
	return out
}

// extractEmojiTags parses the Tag array of a Person or Note and returns
// any elements with type "Emoji". Tag配列はJSON unmarshal後に []any
// (各要素が map[string]any) として届くため、型アサーションで抽出する。
//
// **採用件数は maxRemoteEmojiTags で打ち切る。** upsertEmojis は 1 件ごとに
// `emoji` 行を書くので、上限が無いと 1 document で任意件数の書き込みを起こせる。
// 打ち切りは採用側で数える (Emoji 以外の tag は勘定に入れない)。
func extractEmojiTags(tags []any) []activitypub.EmojiTag {
	if len(tags) == 0 {
		return nil
	}
	var out []activitypub.EmojiTag
	for _, tag := range tags {
		m, ok := tag.(map[string]any)
		if !ok {
			continue
		}
		typ := apTypeOf(m)
		if typ != "Emoji" {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		var iconURL string
		if icon, ok := m["icon"].(map[string]any); ok {
			iconURL, _ = icon["url"].(string)
		}
		if iconURL == "" {
			continue
		}
		id, _ := m["id"].(string)
		updated, _ := m["updated"].(string)
		// upstream Misskey TS の renderEmoji は license を `_misskey_license`
		// オブジェクト (`{freeText: string|null}`) で federate する (#731)。
		// 3 状態を区別して保存する:
		//   - wrapper 欠落 → License = nil ("license 情報が federate されて
		//     いない" = 上書きしない)
		//   - wrapper あり + freeText=null → FreeText = nil ("license は明示的
		//     に未設定" = NULL 上書き OK)
		//   - wrapper あり + freeText=string → FreeText = &string (具体値)
		var license *activitypub.MisskeyLicense
		if lic, ok := m["_misskey_license"].(map[string]any); ok {
			license = &activitypub.MisskeyLicense{}
			if free, ok := lic["freeText"].(string); ok {
				license.FreeText = &free
			}
		}
		out = append(out, activitypub.EmojiTag{
			Type:    "Emoji",
			Name:    name,
			Icon:    activitypub.Image{Type: "Image", URL: activitypub.APLenientHref(iconURL)},
			ID:      id,
			Updated: updated,
			License: license,
		})
		if len(out) >= maxRemoteEmojiTags {
			break
		}
	}
	return out
}

// upsertEmojis persists remote emoji from AP tags and returns the list of
// short names (without colons). 同一hostに対して FindManyByNamesAndHost で
// バッチ取得し、その結果を元にcreate/updateを判定することでN+1を回避する。
// Create/UpdateFieldsが失敗した場合はその名前を結果から除外し、後段で
// 「名前は載っているが行が存在しない」状態を防ぐ。emojiRepoが未設定なら
// 空配列を返す。
func (r *Resolver) upsertEmojis(tags []activitypub.EmojiTag, host string) model.StringArray {
	if r.emojiRepo == nil || len(tags) == 0 {
		return model.StringArray{}
	}
	// 同名タグが重複して来るケースに備えて重複排除しつつ name → tag を保持
	tagByName := make(map[string]activitypub.EmojiTag, len(tags))
	order := make([]string, 0, len(tags))
	for _, tag := range tags {
		name := strings.Trim(tag.Name, ":")
		if name == "" {
			continue
		}
		// **name は行の身元** (`emoji` の UNIQUE は name+host) で、そのまま
		// `note.emojis` / `user.emojis` (varchar(128)[]) にも載る。つまり
		// ここ 1 箇所の判定で 3 列を守る。切ると別の絵文字を指すので、
		// 収まらない tag は丸ごと落とす (#2726)。
		//
		// **下の FindManyByNamesAndHost より前で落とす。** NUL 入りの name を
		// query に渡すと 22021 でバッチ取得ごと落ち、その note の絵文字が
		// 全部消える。
		if !fitsColumn(name, emojiNameMaxRunes) {
			slog.Warn("upsertEmojis: emoji name does not fit its column", "host", host)
			continue
		}
		if _, ok := tagByName[name]; ok {
			continue
		}
		tagByName[name] = tag
		order = append(order, name)
	}
	if len(order) == 0 {
		return model.StringArray{}
	}

	existingRows, err := r.emojiRepo.FindManyByNamesAndHost(order, &host)
	if err != nil {
		return model.StringArray{}
	}
	existingByName := make(map[string]*model.Emoji, len(existingRows))
	for _, e := range existingRows {
		existingByName[e.Name] = e
	}

	names := make(model.StringArray, 0, len(order))
	for _, name := range order {
		tag := tagByName[name]
		// 画面に出すときにプロキシで包めない形の URL は使わない。括弧などは
		// 符号化した値を保存する。収まらない URL と同じ扱いにする。
		iconURL, iconOK := remoteurl.Media(tag.Icon.URL.String())
		// icon URL は `originalUrl` / `publicUrl` (varchar(512) NOT NULL)。
		// 切った URL は別物なので**値ごと捨てる** (#2726)。符号化で長くなる
		// ことがあるので、符号化した後の値で測る。
		iconFits := iconOK && fitsColumn(iconURL, emojiURLMaxRunes)
		if existing, ok := existingByName[name]; ok {
			// 既存絵文字: URL/URIが変わっていればまとめてupdate
			updates := map[string]any{}
			// 収まらない URL では**既存の値を残す**。行は既にあるので、
			// 壊れた URL で上書きするより古い URL のほうが役に立つ。
			if iconFits && existing.OriginalURL != iconURL {
				updates["originalUrl"] = iconURL
				updates["publicUrl"] = iconURL
			}
			if !iconFits {
				slog.Warn("upsertEmojis: keeping stored url; new url is malformed or does not fit its column",
					"name", name, "host", host)
			}
			// uri は URL なので切らない。収まらなければ**値だけ捨てて**行は残す
			// (dedup / 更新判定に使うだけで、無くても絵文字は表示できる、#2726)。
			if tag.ID != "" && fitsColumn(tag.ID, emojiURLMaxRunes) &&
				(existing.URI == nil || *existing.URI != tag.ID) {
				updates["uri"] = &tag.ID
			}
			// AP `_misskey_license.freeText` の差分も追従する (#731)。
			// nil wrapper = AP tag に license フィールド無し → 既存値を温存
			// (連合先が一時的に license export を停止した場合に上書きしない)。
			// FreeText nil 内部 = wrapper はあるが空 → 明示的に空 license で上書き。
			if tag.License != nil {
				newLicense := remoteEmojiLicense(tag.License.FreeText)
				existingLicense := existing.License
				if !pointerStringsEqual(newLicense, existingLicense) {
					updates["license"] = newLicense
				}
			}
			if len(updates) > 0 {
				if err := r.emojiRepo.UpdateFields(existing.ID, updates); err != nil {
					// updateに失敗してもrow自体は存在するので名前は返してよい
					slog.Warn("upsertEmojis: update failed",
						"name", name, "host", host, "err", err)
				}
			}
			names = append(names, name)
			continue
		}
		// 新規絵文字: create
		//
		// URL が収まらないなら**この tag を丸ごと落とす**。`originalUrl` は
		// NOT NULL で、空の行を作っても壊れた画像になるだけ。名前だけ
		// `note.emojis` に載るほうが悪い (クライアントは未解決の絵文字を
		// `:name:` のまま出すので、そちらのほうが読める、#2726)。
		if !iconFits {
			slog.Warn("upsertEmojis: emoji url is malformed or does not fit its column",
				"name", name, "host", host)
			continue
		}
		now := r.clock()
		uri := tag.ID
		emoji := &model.Emoji{
			ID:          r.idGen.Generate(now),
			Name:        name,
			Host:        &host,
			OriginalURL: iconURL,
			PublicURL:   iconURL,
		}
		// #731: AP `_misskey_license` 経由で federation 直後に保存。
		// wrapper があれば FreeText (nil 含む) を取り込む、wrapper 自体が
		// nil なら model.Emoji.License は nil のまま。
		if tag.License != nil {
			emoji.License = remoteEmojiLicense(tag.License.FreeText)
		}
		if uri != "" && fitsColumn(uri, emojiURLMaxRunes) {
			emoji.URI = &uri
		}
		if err := r.emojiRepo.Create(emoji); err != nil {
			// createに失敗した場合、行が存在しないままになる可能性があるため
			// 結果から除外する。EmojiResolverは欠損nameを単に解決しないだけで
			// クライアント側のフォールバック表示が利く。
			slog.Warn("upsertEmojis: create failed",
				"name", name, "host", host, "err", err)
			continue
		}
		names = append(names, name)
	}
	return names
}

// AP tag 由来の emoji が入る列の上限 (migration/000001_initial.up.sql)。
const (
	emojiNameMaxRunes    = 128
	emojiURLMaxRunes     = 512
	emojiLicenseMaxRunes = 1024
)

// remoteEmojiLicense prepares `_misskey_license.freeText` for `emoji.license`
// (`varchar(1024)`).
//
// license は表示される本文なので切って NUL を落とす。nil (= wrapper はあるが
// 未設定) はそのまま nil を返す — 「明示的に未設定」を空文字に潰すと、
// 既存の license を空で上書きしたのか未設定なのかが区別できなくなる (#2726)。
func remoteEmojiLicense(v *string) *string {
	if v == nil {
		return nil
	}
	out := remoteText(*v, emojiLicenseMaxRunes)
	return &out
}

// withImplicitMentions appends to mentions the users upstream
// NoteCreateService.create adds to mentionedUsers besides the Mention tags: the
// reply target's author (unless it is the note's author), then for a specified
// note its recipients. n must carry UserID, ReplyUserID, Visibility and
// VisibleUserIDs. The result is dedup'd and never nil.
func withImplicitMentions(n *model.Note, mentions model.StringArray) model.StringArray {
	var extra []string
	if n.ReplyUserID != nil && *n.ReplyUserID != "" && *n.ReplyUserID != n.UserID {
		extra = append(extra, *n.ReplyUserID)
	}
	if n.Visibility == model.NoteVisibilitySpecified {
		extra = append(extra, n.VisibleUserIDs...)
	}
	return mergeMentionIDs(mentions, extra)
}

// specifiedAudience returns the addressees of a specified note: `to` then `cc`,
// without the public collection and the author's followers collection,
// de-duplicated. Mirrors upstream ApAudienceService.parseAudience
// (`unique(concat([toGroups.other, ccGroups.other]))`).
func specifiedAudience(actor *model.User, to, cc []string) []string {
	followers := authorFollowersURI(actor)
	var out []string
	seen := make(map[string]struct{})
	for _, list := range [][]string{to, cc} {
		for _, id := range list {
			if hasPublicAudience([]string{id}) || (followers != "" && id == followers) {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// hasPublicAudience reports whether list contains the AS public collection in
// any of the forms upstream ApAudienceService.isPublic accepts (#1864)。
func hasPublicAudience(list []string) bool {
	for _, v := range list {
		if v == activitypub.Public || v == "as:Public" || v == "Public" {
			return true
		}
	}
	return false
}
