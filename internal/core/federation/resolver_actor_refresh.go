package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/hashtag"
	"github.com/elythia-network/elythia/internal/model"
)

// ForceResolveActor resolves an actor and always re-fetches the profile,
// bypassing the TTL cache. Move activityなどプロフィール更新が確実に必要な場合に使う。
func (r *Resolver) ForceResolveActor(uri string) (*model.User, error) {
	if existing, err := r.userRepo.FindByURI(uri); err == nil {
		_ = r.refreshActor(existing, uri, false, nil)
		return existing, nil
	}
	return r.ResolveActor(uri)
}

// RefreshActor re-fetches the stored remote actor at uri and updates its row,
// like upstream ApPersonService.updatePerson. Unlike ForceResolveActor it does
// not create a missing row and it reports a failed fetch.
//
// 本家 RemoteUserResolveService.resolveUser の再同期は updatePerson の失敗を
// そのまま投げる (users/show は FAILED_TO_RESOLVE_REMOTE_USER になる) ので、
// 取得の失敗を握らずに返す。
func (r *Resolver) RefreshActor(uri string) (*model.User, error) {
	if r.isSelfHostURI(uri) {
		return nil, ErrLocalActor
	}
	existing, err := r.userRepo.FindByURI(uri)
	if err != nil {
		return nil, fmt.Errorf("refresh actor: %w", err)
	}
	if err := r.refreshActor(existing, uri, false, nil); err != nil {
		return nil, fmt.Errorf("refresh actor: %w", err)
	}
	return existing, nil
}

// notifyInstance is a best-effort hook into the instance tracker. ベスト
// エフォートのため、失敗してもエラーは伝搬しない。
func (r *Resolver) notifyInstance(host string) {
	if r.instanceTracker == nil || host == "" {
		return
	}
	_, _ = r.instanceTracker.RegisterFromHost(host)
}

// shouldRefreshActor reports whether a cached user row is past its TTL and
// should be refetched.
func (r *Resolver) shouldRefreshActor(u *model.User) bool {
	if u == nil || u.LastFetchedAt == nil {
		return true
	}
	return r.clock().Sub(*u.LastFetchedAt) > r.actorTTL
}

// refreshActor refetches the remote actor document and updates mutable fields
// on the local user row. 失敗してもエラーは返さず (呼び出し側はベストエフォート
// で既存値を使う)、ログは呼び出し元側で残す。
//
// 戻り値は actor document の取得・検証の error だけ。DB への書き込みの失敗は
// 従来どおり握る (次の refresh で再試行される)。
func (r *Resolver) refreshActor(existing *model.User, uri string, skipFeatured bool, chain *resolveChain) error {
	// background refresh は federation-loop 扱いで Strict (request host binding 有効)。
	actor, err := r.fetchActor(uri, false)
	if err != nil {
		// **document が恒久的に不正なら lastFetchedAt だけ進める。**
		// 進めないと shouldRefreshActor が永久に true のままで、その actor から
		// inbound activity が 1 件来るたびに outbound fetch が 1 回走り続ける。
		// 相手は同じ document を返すので自然回復しない (#2662 で
		// preferredUsername の検証を足したことで、検証前に取り込まれた既存行が
		// この状態になりうる)。
		//
		// 対象は **document の内容起因**のエラーだけ。ネットワーク断や 5xx で
		// 進めると、一時的な障害の間に TTL を空振りさせて更新が遅れる。
		// `ErrObjectHostMismatch` (inbox / publicKey.id の host 不一致など) も
		// 相手が document を直さない限り変わらないので同じ扱いにする。
		//
		// **副作用**: lastFetchedAt を進めるので、`DeleteOrphanRemoteUsers` の
		// 「graceDays 以上 fetch されていない」条件から外れ続ける。掃除対象は
		// relay 由来の孤児行だけなので影響は小さいが、恒久的に壊れている
		// actor はまさに掃除したい対象ではある。
		if errors.Is(err, ErrInvalidActor) || errors.Is(err, ErrObjectHostMismatch) {
			now := r.clock()
			if uerr := r.userRepo.UpdateUser(existing.ID, map[string]any{"lastFetchedAt": &now}); uerr != nil {
				slog.Warn("federation: lastFetchedAt backoff update failed",
					"userID", existing.ID, "error", uerr)
			} else {
				existing.LastFetchedAt = &now
			}
		}
		return err
	}
	now := r.clock()
	fields := map[string]any{
		"lastFetchedAt": &now,
	}
	// 書き込みに失敗したとき、凍結の値だけは DB と同じに戻すために覚えておく。
	wasSuspended := existing.IsSuspended
	// actor type が変わるケース (Person ↔ Service など) を反映する。常に上書きする
	// (TS Misskey も同様)。
	isBot := activitypub.IsBotActorType(actor.Type.String())
	fields["isBot"] = isBot
	existing.IsBot = isBot
	// manuallyApprovesFollowers の切り替えも追従する (リモート側でlock/unlock
	// された場合にローカルの判定もずれないように)。
	fields["isLocked"] = actor.ManuallyApproves.Bool()
	existing.IsLocked = actor.ManuallyApproves.Bool()
	// **由来 (#2973) を見て決める。** `local` = モデレーターの判断には触らず、
	// `remote` = こちらが actor を見て凍結した行だけが発信元の解除に追従する。
	// 記録が無い行は解除方向では `local` 扱い (安全側)。
	if suspend, ok := r.resolveSuspensionChange(existing, actor.Suspended.Bool()); ok {
		fields["isSuspended"] = suspend
		existing.IsSuspended = suspend
		// **痕跡を残す。** これはモデレーターの操作を伴わない変更で、
		// moderation log には載らない。
		slog.Info("federation: updating remote suspension per toot:suspended",
			"uri", actor.ID, "userId", existing.ID, "suspended", suspend)
		if err := r.suspensionOriginRepo.Set(existing.ID, model.SuspensionOriginRemote); err != nil {
			// **由来を記録できなかったら凍結もしない。** 記録が無いまま
			// 凍結すると、次の refresh で「由来不明」として扱われ、モデレーターが
			// 解除しても戻せるかどうかが運任せになる。
			slog.Warn("federation: failed to record suspension origin; skipping",
				"uri", actor.ID, "userId", existing.ID, "err", err)
			delete(fields, "isSuspended")
			existing.IsSuspended = !suspend
		}
	}
	if name := remoteDisplayName(actor.Name.String()); name != "" {
		fields["name"] = &name
		existing.Name = &name
	}
	if inbox := remoteURIValue(actor.ID, "user.inbox", actor.Inbox); inbox != "" {
		fields["inbox"] = &inbox
		existing.Inbox = &inbox
	}
	if shared := remoteURIValue(actor.ID, "user.sharedInbox", remoteSharedInbox(actor)); shared != "" {
		fields["sharedInbox"] = &shared
		existing.SharedInbox = &shared
	}
	// 本家 updatePerson と同じく、actor が followers を出していなければ既存値を残す
	// (`person.followers ? getApId(...) : undefined`)。
	if followers := remoteURIValue(actor.ID, "user.followersUri", actor.Followers.String()); followers != "" {
		fields["followersUri"] = &followers
		existing.FollowersURI = &followers
	}
	if featured := remoteURIValue(actor.ID, "user.featured", actor.Featured.String()); featured != "" {
		fields["featured"] = &featured
		existing.Featured = &featured
	}
	// movedAt は移行が「起きた」瞬間だけ打刻する。upstream
	// (ApPersonService.updatePerson の `moving` フラグ) と同じ判定で、
	// 無→有 と 有→別の値 のときだけ更新し、同じ移行先を宣言し続けている
	// actor の再取得では触らない。
	//
	// 以前は actor が movedTo を持っていれば毎回 now で打ち直していた。
	// movedAt はまだどこからも読まれていないので実害は出ていなかったが、
	// upstream が movedAt を時間窓の基準に使う以上 (移行直後 2h の import
	// 上限緩和 / 14 日の移行クールダウン)、打ち直すと窓が開き続けて機能
	// しなくなる。時間窓を実装する前にここを直しておく (#2412)。
	// 移行の検知に使う「更新前」の状態。existing はこの後 in-place で
	// 書き換わるので、判定に必要な値をここで退避する。
	prevMovedAt := existing.MovedAt
	movedThisRefresh := false
	// create 経路と同じく、収まらない URI は値ごと捨てる (#2723)。捨てないと
	// **atomic UPDATE ごと失敗して `lastFetchedAt` が進まず**、inbound activity
	// 1 件につき outbound fetch が 1 回走り続ける。
	if movedTo := remoteURIValue(actor.ID, "user.movedToUri", actor.MovedTo.String()); movedTo != "" {
		moving := existing.MovedToURI == nil || *existing.MovedToURI != movedTo
		fields["movedToUri"] = &movedTo
		existing.MovedToURI = &movedTo
		if moving {
			fields["movedAt"] = &now
			existing.MovedAt = &now
			movedThisRefresh = true
		}
	}
	// 移行先が消えた (actor.MovedTo == "") ケースは追わない。他フィールドと
	// 同じ「空値・欠落では既存値を温存する」規約に従う。
	//
	// upstream は `movedToUri: person.movedTo ?? null` で null に戻すが、
	// mk-go はここで意図的に divergence する。リモートが一時的に movedTo を
	// 欠いた actor JSON を返しただけでクリアすると、次の取得が「無→有」の
	// 遷移に見えて movedAt が打ち直され、上の修正が骨抜きになるため。
	// 移行の取り消しに追従できない代わりに、時間窓の基準を安定させる。
	if akaStr := remoteURIList(actor.ID, "user.alsoKnownAs", actor.AlsoKnownAs); akaStr != "" {
		fields["alsoKnownAs"] = &akaStr
		existing.AlsoKnownAs = &akaStr
	}
	// アカウントの作成日時 (#3465)。送られてくるたびに最新の値で上書きする。
	// 無い・読めないときは保存済みの値を残す (他のフィールドと同じ規約。
	// `/api/users/show` から埋めた値を、`published` を送らない相手の refresh で
	// 消さないため)。
	if createdAt := parseAccountPublished(actor.Published.String(), now); createdAt != nil {
		fields["accountCreatedAt"] = createdAt
		existing.AccountCreatedAt = createdAt
	}
	// アバター / バナー画像のURLリモート側で変更された場合に追従する。
	// アイコンは、他フィールドと同様、空値や欠落時は既存値を温存する (削除は追わない。
	// 本家は消すが、#3526 より前からの扱いのまま)。バナーは下のとおり消える。
	if avatarURL := remoteMediaURL(actor.ID, "user.avatarUrl", actor.Icon.URLOrEmpty(), maxRemoteAvatarURLLen); avatarURL != "" {
		fields["avatarUrl"] = &avatarURL
		existing.AvatarURL = &avatarURL
	}
	// バナーは、actor から image が消えたらこちらも消す (本家 resolveAvatarAndBanner は
	// image が無ければ bannerId / bannerUrl / bannerBlurhash を null にする)。リモートの
	// 人のバナーは表示されるので (#3526)、消えたものを出し続けない。image はあるが値を
	// 使えないとき (長すぎる・NUL を含む・形が崩れている・url が空や文字列の形) は、
	// 本家が取得に失敗して今の値を残すのと同じく、今の値を残す。
	if bannerURL := remoteMediaURL(actor.ID, "user.bannerUrl", actor.Image.URLOrEmpty(), maxRemoteBannerURLLen); bannerURL != "" {
		fields["bannerUrl"] = &bannerURL
		existing.BannerURL = &bannerURL
	} else if actor.Image == nil && (existing.BannerURL != nil || existing.BannerID != nil || existing.BannerBlurhash != nil) {
		fields["bannerId"] = nil
		fields["bannerUrl"] = nil
		fields["bannerBlurhash"] = nil
		existing.BannerID = nil
		existing.BannerURL = nil
		existing.BannerBlurhash = nil
	}
	// `_misskey_canChat` の更新を chatScope に反映 (#692)。flag 欠落時は
	// 既存値を保持する (連合先がフィールドを export していない場合に上書き
	// しないため)。
	if actor.MisskeyCanChat != nil {
		newScope := "everyone"
		if !*actor.MisskeyCanChat {
			newScope = "none"
		}
		fields["chatScope"] = newScope
		existing.ChatScope = newScope
	}
	// AP Person Tag配列からカスタム絵文字を抽出してDBにupsert
	if existing.Host != nil {
		emojis := r.upsertEmojis(extractEmojiTags(actor.Tag), *existing.Host)
		fields["emojis"] = emojis
		existing.Emojis = emojis
	}
	// person.tag の Hashtag entry を user.tags に追従させる (actor 更新時に
	// 自己紹介の hashtag が変わったら反映する。新規取り込みと同じ正規化)。
	oldTags := []string(existing.Tags)
	// ExtractUserTags は hashtag が無いと nil を返すが、nil の model.StringArray は
	// Updates() map 経由で SQL NULL になり user.tags (NOT NULL) 制約に違反する。
	// その場合 actor 更新 (emojis / name / lastFetchedAt 等を含む atomic UPDATE)
	// 全体が失敗するため空配列に倒して '{}' を書く。
	tags := model.StringArray(hashtag.ExtractUserTags(extractHashtagTagNames(actor.Tag)...))
	if tags == nil {
		tags = model.StringArray{}
	}
	fields["tags"] = tags
	existing.Tags = tags
	existing.LastFetchedAt = &now
	// UpdateUserエラーはベストエフォートで無視 (次回再試行される)
	if err := r.userRepo.UpdateUser(existing.ID, fields); err != nil {
		// **凍結の値は DB に書けた値に戻す (#3610)。** inbox はこの行 (署名者) で
		// 凍結を判定し、DB を引き直さない。書けなかった解除を手元の行だけに
		// 残すと、DB では凍結のままの actor の activity を処理してしまう。
		existing.IsSuspended = wasSuspended
	}
	// hashtag 集計を tags 差分で追従させる。remote なので isLocal=false。
	// non-blocking hook (#1362)。
	if r.hashtagHook != nil {
		r.hashtagHook.UpdateUsertags(existing.ID, false, oldTags, []string(tags))
	}
	// UserProfile の description (#1022) と location / birthday / fields (#2661)。
	// actor 側が変わった場合に追従する。profile 行が無いケース (= それぞれの
	// fix 以前に取り込まれた remote user) は back-fill する。Update / Create の
	// いずれも best-effort で fail しても user 更新は維持する。
	desc := extractRemoteDescription(actor)
	extras := extractRemoteProfileExtras(actor)
	// フォロー / フォロワー一覧の公開範囲 (upstream updatePerson と同じ規則)。
	// 一時的な失敗 (ネットワーク / 5xx / 429 / 文書の不備) では保存済みの値を
	// 変えず、4xx なら非公開にする。
	followingVis, followingOK, followersVis, followersOK := r.followVisibilitiesOnUpdate(actor.ID, actor.FollowingRaw, actor.FollowersRaw)
	if _, err := r.userRepo.FindProfileByUserID(existing.ID); err != nil {
		// back-fill では「保存済みの値」が無い。一時的な失敗で列の既定値
		// (public) に任せると、非公開の一覧を公開扱いにしうるので、作成時と
		// 同じく非公開に倒す。
		if !followingOK {
			followingVis = model.FollowingVisibilityPrivate
		}
		if !followersOK {
			followersVis = model.FollowingVisibilityPrivate
		}
		_ = r.userRepo.CreateProfile(&model.UserProfile{
			UserID:              existing.ID,
			Description:         desc,
			Location:            extras.location,
			Birthday:            extras.birthday,
			Fields:              extras.fields,
			UserHost:            existing.Host,
			FollowingVisibility: followingVis,
			FollowersVisibility: followersVis,
		})
	} else {
		// fields は jsonb 列に string で渡す。**素の []byte を渡してはいけない**
		// (実測 SQLSTATE 22P02 で UPDATE ごと落ち、同じ書き込みの description も
		// 巻き添えになる)。datatypes.JSON は driver.Valuer を実装しているので
		// そのままでも書けるが、値の形を呼び出し側で確定させておく。
		profileFields := map[string]any{
			"description": desc,
			"location":    extras.location,
			"birthday":    extras.birthday,
			"fields":      string(extras.fields),
		}
		if followingOK {
			profileFields["followingVisibility"] = string(followingVis)
		}
		if followersOK {
			profileFields["followersVisibility"] = string(followersVis)
		}
		_ = r.userRepo.UpdateProfile(existing.ID, profileFields)
	}
	r.cachePublicKey(existing.ID, actor.PublicKey.ID, actor.PublicKey.PublicKeyPEM.String())
	r.cacheAssertionMethods(existing.ID, actor.ID, actor.AssertionMethod)
	if existing.Host != nil {
		r.notifyInstance(*existing.Host)
	}
	// 移行を検知したらフォロワー等の引き継ぎを起動する。DB 更新の後に置くのは、
	// 引き継ぎ処理が movedToUri の永続化済みを前提にするため。
	if movedThisRefresh {
		r.processRemoteMove(existing, prevMovedAt, nil, chain)
	}
	// 既に観測済みのユーザーはここでピン留めが埋まる (#2552)。actor の TTL が
	// 切れるたびに引き直すので、featured を後から公開した相手にも追従する。
	if !skipFeatured {
		r.updateFeatured(existing, chain)
	}
	return nil
}

// remoteMoveCooldown is how long a remote account must wait between moves
// before mk-go will process another one. upstream ApPersonService は 14 日
// (「Mastodon のクールダウン期間は 30 日だが若干緩めに設定しておく」)。
const remoteMoveCooldown = 14 * 24 * time.Hour

// maxRemoteMoveChain bounds how many hops a move chain may traverse before we
// give up. upstream の `movePreventUris.length > 10` と同じ上限。
const maxRemoteMoveChain = 10

// processRemoteMove carries local followers (and blocks / mutes / roles / lists
// / antennas) of a moved remote account over to its destination.
//
// upstream ApPersonService.processRemoteMove 相当 (#2414)。src は movedToUri を
// 書き終えた直後の行で、prevMovedAt は今回の更新より前の movedAt。
//
// visited は移行チェーンを辿った URI の集合で、A→B→A のような循環と、無限に
// 続く連鎖を止める。最初の呼び出しでは nil を渡す。
//
// **best-effort。** 移行の検知は actor 更新の副作用なので、ここでの失敗が
// プロフィール更新そのものを巻き戻すことは無い。
func (r *Resolver) processRemoteMove(src *model.User, prevMovedAt *time.Time, visited map[string]bool, chain *resolveChain) {
	if r.moveProcessor == nil || src == nil {
		return
	}
	if src.MovedToURI == nil || *src.MovedToURI == "" {
		return
	}
	dstURI := *src.MovedToURI
	srcURI := ""
	if src.URI != nil {
		srcURI = *src.URI
	}
	// 自分自身への移行は無意味。upstream も同じ位置で弾く。
	if srcURI != "" && srcURI == dstURI {
		return
	}
	// クールダウン。前回の移行から 14 日経っていなければ処理しない。
	// **#2412 で movedAt を遷移時だけ打刻するようにしたのが前提。** 再取得の
	// たびに打ち直していた頃はこの判定が常に成立してしまう。
	if prevMovedAt != nil && src.MovedAt != nil &&
		src.MovedAt.Sub(*prevMovedAt) < remoteMoveCooldown {
		slog.Info("federation: skip remote move (cooldown)",
			"srcURI", srcURI, "dstURI", dstURI)
		return
	}
	if visited == nil {
		visited = map[string]bool{}
	}
	if len(visited) > maxRemoteMoveChain {
		slog.Warn("federation: skip remote move (chain too long)",
			"srcURI", srcURI, "dstURI", dstURI, "hops", len(visited))
		return
	}
	if visited[dstURI] {
		slog.Warn("federation: skip remote move (circular)",
			"srcURI", srcURI, "dstURI", dstURI)
		return
	}
	visited[srcURI] = true

	dst, err := r.userRepo.FindByURI(dstURI)
	if err != nil || dst == nil {
		// ローカルユーザーは user.uri を持たない (NULL) ので URI では引けない。
		// upstream fetchPerson と同じく、自インスタンスを指す URI は id に
		// 分解して DB を引く。**移行先が自インスタンス (= move-in 本来の形)
		// はこの経路でしか解決できない。**
		if r.urls != nil {
			if localID := r.urls.LocalUserIDFromURI(dstURI); localID != "" {
				if u, ferr := r.userRepo.FindByID(localID); ferr == nil && u != nil {
					dst = u
				}
			}
		}
	}
	if dst == nil {
		// 未知の移行先は取りに行く。ローカルを名乗る URI が DB に無い場合は
		// movedToUri が間違っているので、取りに行かず打ち切る (upstream の
		// 'failed: movedTo is local but not found')。
		if r.urls != nil && r.urls.IsLocalURI(dstURI) {
			slog.Warn("federation: remote move points at an unknown local URI",
				"srcURI", srcURI, "dstURI", dstURI)
			return
		}
		// **チェーンを引き継ぐ。** 落とすと移行先の featured 解決が、この
		// goroutine が既に握っている鍵を「他人のもの」と見なす。いまは待たずに
		// 諦めるのでピンを 1 件落とすだけで済むが、以前は待って止まっていた
		// (#2684 と同じ形)。
		//
		// **この待ちがあるので actor 側もグラフに載せている。** 互いを movedTo に
		// 指す 2 つの actor を 2 worker が同時に取得すると、ここで actor どうしの
		// 循環になる。
		dst, err = r.resolveActor(dstURI, false, false, chain)
		if err != nil || dst == nil {
			slog.Warn("federation: resolve remote move destination failed",
				"srcURI", srcURI, "dstURI", dstURI, "err", err)
			return
		}
	}

	if !r.moveDestinationAccepts(src, dst, srcURI, dstURI) {
		return
	}
	r.moveProcessor.PostMoveProcess(src, dst)
}

// moveDestinationAccepts reports whether dst really claims src as a previous
// identity, i.e. whether the migration is bidirectionally confirmed.
//
// **ここは security boundary。** `alsoKnownAs` の相互確認を省くと、任意の actor
// が movedTo で他人を指すだけでそのフォロワーを奪える。確認が取れない限り
// 引き継ぎに入らない。
func (r *Resolver) moveDestinationAccepts(src, dst *model.User, srcURI, dstURI string) bool {
	dstCanonical := ""
	if dst.URI != nil {
		dstCanonical = *dst.URI
	}
	// ローカルユーザーが移行先の場合、user.uri は空なので正規 URI を組み立てる。
	if dstCanonical == "" && dst.IsLocal() && r.urls != nil {
		dstCanonical = r.urls.UserURI(dst.ID)
	}
	if dstCanonical == "" || dstCanonical != dstURI {
		slog.Warn("federation: remote move destination URI mismatch",
			"srcURI", srcURI, "declared", dstURI, "resolved", dstCanonical)
		return false
	}
	// 移行先がさらに移行済みで、それが自分自身や移行元を指す場合は打ち切る。
	if dst.MovedToURI != nil && *dst.MovedToURI != "" {
		if *dst.MovedToURI == dstCanonical || (srcURI != "" && *dst.MovedToURI == srcURI) {
			slog.Warn("federation: remote move destination points back",
				"srcURI", srcURI, "dstURI", dstURI, "dstMovedTo", *dst.MovedToURI)
			return false
		}
	}
	if srcURI == "" {
		return false
	}
	if !alsoKnownAsContains(dst.AlsoKnownAs, srcURI) {
		slog.Warn("federation: remote move rejected (alsoKnownAs does not claim the source)",
			"srcURI", srcURI, "dstURI", dstURI)
		return false
	}
	return true
}

// alsoKnownAsContains reports whether the csv alsoKnownAs column lists uri.
// mk-go は upstream の配列を text csv として保持している (core/move と同じ表現)。
func alsoKnownAsContains(csvPtr *string, uri string) bool {
	if csvPtr == nil || *csvPtr == "" || uri == "" {
		return false
	}
	for _, part := range strings.Split(*csvPtr, ",") {
		if strings.TrimSpace(part) == uri {
			return true
		}
	}
	return false
}

// fetchActor fetches and decodes a remote actor document. Reject any
// document whose `type` is not in activitypub.ValidActorTypes — this guards
// against a non-Actor object (e.g. a Note) being interpreted as a Person.
func (r *Resolver) fetchActor(uri string, allowCrossHost bool) (*activitypub.Person, error) {
	// **自ホストの actor は取りに行かない。** 取りに行くと自分の actor document が
	// 返り、`host = 自ホスト` の「リモート扱いの」user / user_profile 行と、
	// `notifyInstance` 経由で自ホストの instance 行 + chart まで出来る。upstream は
	// ApPersonService.createPerson で明示的に弾く (`cannot resolve local user`)。
	// **到達経路は未認証** — inbox の署名検証は keyId の base URL を
	// ResolveActor に渡すのが先なので、誰でも任意のローカル利用者の URI を送れる。
	if r.isSelfHostURI(uri) {
		return nil, ErrLocalActor
	}
	// federation policy gate: ホワイトリスト連合 (federation: specified) や
	// blockedHosts 設定下では、対象 URI の host が許可されていなければ HTTP
	// fetch 自体を抑止する。refreshActor / refreshPublicKey / resolveActorOnce
	// すべてここを経由するため、actor 側はこの 1 箇所で塞げる。
	if !r.hostAllowedForURI(uri) {
		return nil, ErrHostNotAllowed
	}
	body, finalURL, err := r.fetchObjectWithFinalURL(uri)
	if err != nil {
		return nil, err
	}
	var actor activitypub.Person
	if err := json.Unmarshal(body, &actor); err != nil {
		return nil, ErrInvalidActor
	}
	actor.FollowersRaw, actor.FollowingRaw = rawFollowCollections(body)
	// fetch した document が AS @context を持つことを要求する (本家 resolve の
	// invalid-response guard、#1828)。誤設定 endpoint が返す non-AP JSON を
	// actor として取り込むのを防ぐ。
	if !hasActivityStreamsContext(actor.Context) {
		slog.Warn("federation: actor missing ActivityStreams @context", "uri", uri)
		return nil, ErrInvalidActor
	}
	// **`id` も正規化する。** upstream は `assertActivityMatchesUrl` が
	// `new URL(activity.id)` を通すので、末尾改行のような形でも通る。ここで
	// 落とすと **その actor がまったく取り込めない** (#2662)。`id` は全 field の
	// 中で最も必ず存在する URL なので、inbox に改行を付ける実装は id にも付ける。
	actor.ID = trimWHATWGURL(actor.ID)
	if actor.ID == "" {
		return nil, ErrInvalidActor
	}
	// actor.ID host が body を返した host と一致するか検証 (object-spoofing 防止、
	// #1820)。以降 resolveActorOnce は actor.ID から host/URI を導出するため、
	// ここで bind しておかないと別 host になりすました actor が作られる。
	if err := assertResponseHostMatches(finalURL, actor.ID); err != nil {
		slog.Warn("federation: actor id host mismatch", "uri", uri, "finalURL", finalURL, "id", actor.ID)
		return nil, err
	}
	// request URI host == id host も要求する (Strict、#1828)。attacker.example の
	// entry URI から 302 で victim.example の actor 解決を誘発するのを防ぐ。
	// user-initiated ap/show (allowCrossHost) は upstream 同様 cross-host を許容。
	if !allowCrossHost {
		if err := assertRequestHostMatches(uri, actor.ID); err != nil {
			slog.Warn("federation: actor request host mismatch", "uri", uri, "id", actor.ID)
			return nil, err
		}
	}
	// **型の検査を username より先に置く。** upstream validateActor も
	// `isActor(x)` を最初に見る。逆順だと Note を actor として resolve した
	// ときに「preferredUsername が不正」と報告され、型ミスマッチを username
	// 問題と読み違える (#2662)。host 検証よりは後ろに置く。そちらは spoofing
	// 対策で、専用のエラーを返せなくなると調査に効く情報が減る。
	if !activitypub.IsValidActorType(actor.Type.String()) {
		// Note/Activity 等 Actor でない object が actor として参照された場合の
		// 防衛策。デバッグのため URI と type を残してから拒否する。
		slog.Warn("federation: rejecting non-actor type", "uri", uri, "type", actor.Type.String())
		return nil, ErrInvalidActor
	}
	// **配送に使う URL は正規化してから保存する。** 検査だけ `trimWHATWGURL` で
	// 緩めて生値を保存すると、actor は取り込めるのに `http.NewRequest` が毎回
	// 落ちて**配送が永久に成立しない** (末尾空白なら `%20` 付きの URL を叩いて
	// 相手が 404)。upstream は `new URL()` を通した値を使うので、そこに揃える。
	// deliver 側は ErrSkipRetry を付けないので、1 人いるだけで全 activity が
	// MaxAttempts 回空振りする (#2662)。
	actor.Inbox = trimWHATWGURL(actor.Inbox)
	actor.SharedInbox = activitypub.APLenientID(trimWHATWGURL(actor.SharedInbox.String()))
	actor.Endpoints.SharedInbox = activitypub.APLenientID(trimWHATWGURL(actor.Endpoints.SharedInbox.String()))
	actor.PublicKey.ID = trimWHATWGURL(actor.PublicKey.ID)
	// inbox / sharedInbox の host を actor に縛る。upstream validateActor と同型
	// (前者は Error、後者は破棄)。ここを見ないと、任意のリモート actor が
	// 第三者ホストを配送先として宣言できてしまう (deliver は blocklist しか
	// 見ない)。
	if !sameDeliveryHost(actor.Inbox, actor.ID) {
		slog.Warn("federation: actor inbox host mismatch",
			"uri", uri, "inbox", actor.Inbox)
		return nil, ErrInvalidActor
	}
	if actor.SharedInbox != "" && !sameDeliveryHost(actor.SharedInbox.String(), actor.ID) {
		slog.Warn("federation: dropping shared inbox with different host",
			"uri", uri, "sharedInbox", actor.SharedInbox.String())
		actor.SharedInbox = ""
	}
	if actor.Endpoints.SharedInbox != "" && !sameDeliveryHost(actor.Endpoints.SharedInbox.String(), actor.ID) {
		slog.Warn("federation: dropping endpoints.sharedInbox with different host",
			"uri", uri, "sharedInbox", actor.Endpoints.SharedInbox.String())
		actor.Endpoints.SharedInbox = ""
	}
	if name, ok := actorCollectionsOnActorHost(&actor); !ok {
		slog.Warn("federation: actor collection host mismatch", "uri", uri, "collection", name)
		return nil, ErrInvalidActor
	}
	// preferredUsername は user.username / usernameLower (varchar(128) NOT NULL)
	// にそのまま入る。upstream validateActor と同じ条件で弾く (#2662)。素通しすると
	// userRepo.Create が落ちて actor がまったく作られない。
	if !validRemoteUsername(actor.PreferredUsername) {
		slog.Warn("federation: actor has unusable preferredUsername", "uri", uri)
		return nil, ErrInvalidActor
	}
	// publicKey.id (= HTTP Signature keyId) host を actor.ID host に縛る (Fix A、
	// upstream validateActor の publicKey.id host check と同型)。**判定は
	// `sameDeliveryHost`**: upstream は `punyHost` で比較しており `www.` を
	// 同一視しない。`normalizeMatchHost` (object-host binding 用) を使うと `www.`
	// サブドメインを名乗る actor が親ドメインの keyId を宣言できてしまう
	// (#2662)。これが無いと攻撃者 actor が publicKey.id に victim
	// ドメインの keyId を載せて自分の RSA 鍵を植え込み、LD-Signature 経路の
	// global FindByKeyID 解決で victim を名乗る活動を verify 通過させられる
	// (key confusion / 連合認証バイパス)。allowCrossHost (ap/show) でも keyId は
	// actor 自身の host に縛る (request host とは独立、upstream expectHost と同じ)。
	if actor.PublicKey.ID != "" && !sameDeliveryHost(actor.PublicKey.ID, actor.ID) {
		slog.Warn("federation: actor publicKey.id host mismatch",
			"uri", uri, "id", actor.ID, "publicKeyID", actor.PublicKey.ID)
		return nil, ErrObjectHostMismatch
	}
	return &actor, nil
}
