package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/activitypub/mfm"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/hashtag"
	"github.com/elythia-network/elythia/internal/misc/remoteurl"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/datatypes"
)

// ErrLookupUnavailable marks "we could not look it up", as opposed to "we
// looked and it is not there" (#3121).
//
// **署名検証の失敗は既定で ack する** — 署名が合わない body を retry しても
// 結果は変わらないため。しかし検証は DB も読むので、**DB 障害のあいだに届いた
// activity まで同じ ack に落ちる**。落ちた側は相手が再送してくれるとは限らない
// (inbox は enqueue した時点で 202 を返す) ので、確かめられなかったぶんだけは
// 区別して job に retry させる。
var ErrLookupUnavailable = errors.New("lookup unavailable")

// PublicKeyForActor returns the cached public key PEM for an actor ID.
// in-memory → DB → miss の順で探索する。TTL超過は miss として扱い、呼び出し
// 側が ResolveActor を再実行することで refresh をトリガできる。
func (r *Resolver) PublicKeyForActor(actorID string) (string, error) {
	// 1. in-memory cache (TTL内)
	r.keysMu.RLock()
	entry, ok := r.keys[actorID]
	gen := r.keysGen
	r.keysMu.RUnlock()
	if ok {
		if r.clock().Sub(entry.fetchedAt) <= r.actorTTL {
			return entry.pem, nil
		}
		r.keysMu.Lock()
		delete(r.keys, actorID)
		r.keysMu.Unlock()
	}
	// 2. DB fallback
	if r.publickeyRepo != nil {
		pk, err := r.publickeyRepo.FindByUserID(actorID)
		switch {
		case err == nil:
			r.keysMu.Lock()
			// 読んでいる間に捨てられたら積まない (#3732)。読んだ行は捨てる前の
			// ものかもしれない。この呼び出しの答えにだけ使う。
			if r.keysGen == gen {
				r.keys[actorID] = publicKeyEntry{pem: pk.KeyPEM, fetchedAt: r.clock()}
			}
			r.keysMu.Unlock()
			return pk.KeyPEM, nil
		case !repository.IsNotFound(err):
			// **「鍵が無い」に潰さない** (#3121)。潰すと呼び出し側が署名検証の
			// 失敗として ack し、DB 障害のあいだ届いた activity が失われる。
			return "", fmt.Errorf("%w: public key for actor %q: %v", ErrLookupUnavailable, actorID, err)
		}
	}
	return "", fmt.Errorf("public key for actor %q not cached", actorID)
}

// ResolveActor returns the local model.User row for a remote actor URI,
// fetching and creating it if necessary. 既存ユーザーであっても LastFetchedAt
// が actorTTL を超えていたら fetch しなおして name / inbox / sharedInbox /
// publicKey を更新する。fetch 失敗時はベストエフォートで既存値を返す。
func (r *Resolver) ResolveActor(uri string) (*model.User, error) {
	return r.resolveActor(uri, false, false, nil)
}

// ResolveActorAllowCrossHost is ResolveActor for user-initiated lookups
// (/api/ap/show) where upstream relaxes the request-url ↔ id binding to
// CrossOrigin softfail (ap/show.ts:153、#1828)。entry URI が admin の手入力で
// attacker 制御でないため cross-host redirect を許容する。finalURL ↔ id binding は
// 引き続き適用される。
func (r *Resolver) ResolveActorAllowCrossHost(uri string) (*model.User, error) {
	return r.resolveActor(uri, true, false, nil)
}

// resolveActor is ResolveActor carrying the cross-host-allowed flag (#1828)。
// resolveActorWithID resolves an actor, reusing a pre-assigned local ID when
// the row does not exist yet (#2332).
//
// ephemeral として採番した ID を materialize 後もそのまま使うために要る。
// ここを新規採番に任せると、ミュートで著者を materialize したときに ID が
// 変わり、Redis に残っている既存ノートが古い ID を指したままになる。結果と
// して **ミュートしたのにタイムラインから消えない** 状態が TTL 切れまで続く。
func (r *Resolver) resolveActorWithID(uri, preassignedID string) (*model.User, error) {
	key := crossHostKey(uri, false)
	chain := (*resolveChain)(nil).ensureTree()
	v, err := r.joinResolve(&r.resolveActorGroup, chain, actorWaitKey(key), key, func() (any, error) {
		return r.resolveActorOnceWithID(uri, false, preassignedID, false, false, false, chain)
	})
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, ErrInvalidActor
	}
	return v.(*model.User), nil
}

func (r *Resolver) resolveActor(uri string, allowCrossHost, skipFeatured bool, chain *resolveChain) (*model.User, error) {
	// 同一 URI への並行呼び出しは singleflight で 1 つに collapse する
	// (#300 3-7)。cache hit 経路は微秒なので serialize の影響は無視でき、
	// cold な fetch + Create で発生する HTTP fan-out + UNIQUE 衝突を抑える
	// 効果が大きい。
	key := actorGroupKey(uri, allowCrossHost, skipFeatured)
	chain = chain.ensureTree()
	v, err := r.joinResolve(&r.resolveActorGroup, chain, actorWaitKey(key), key, func() (any, error) {
		return r.resolveActorOnceWithID(uri, allowCrossHost, "", false, false, skipFeatured, chain)
	})
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, ErrInvalidActor
	}
	return v.(*model.User), nil
}

// resolveActorOnce is the body of resolveActor, invoked once per URI by
// resolveGroup.
func (r *Resolver) resolveActorOnceWithID(uri string, allowCrossHost bool, preassignedID string, ephemeral, viaRelay, skipFeatured bool, chain *resolveChain) (*model.User, error) {
	// fragment 付き URL は HTTP(S) で fragment が送られず正しく解決できないため
	// 拒否する (本家 Resolver の b94fd5b1 guard、#1828)。keyId fragment は
	// ResolveActorByKeyID -> ResolveKeyURL で除去済みなのでここには来ない。
	if strings.Contains(uri, "#") {
		return nil, ErrResolveFragment
	}
	// **FindByURI より前に弾く。** ローカル利用者は `user.uri` が NULL なので
	// ここで引けるのは「過去に作られた shadow 行」だけで、返すと refresh まで
	// 走る。upstream の `resolvePerson` は自ホスト URI を DB から引いて
	// ローカル利用者を返すが、mk-go では**返さない** — `resolveNoteAuthor` が
	// この戻り値をそのまま note の著者にするので、`allowCrossHost` 経路で
	// 「ローカル利用者名義の偽ノート」を作れるようになってしまう。
	// ローカル利用者を URI から引きたい呼び出し側は ExtractLocalUserID +
	// userRepo.FindByID を使うこと。
	if r.isSelfHostURI(uri) {
		return nil, ErrLocalActor
	}
	existing, ferr := r.userRepo.FindByURI(uri)
	switch {
	case ferr == nil:
		if r.shouldRefreshActor(existing) {
			// 失敗しても既存値を返す (best-effort)。
			_ = r.refreshActor(existing, uri, skipFeatured, chain)
		} else {
			r.keysMu.RLock()
			_, cached := r.keys[existing.ID]
			r.keysMu.RUnlock()
			if !cached {
				// TTL 内であっても publicKey キャッシュが空 (再起動直後など)
				// なら取り直す。
				r.refreshPublicKey(existing.ID, uri)
			}
		}
		return existing, nil
	case !repository.IsNotFound(ferr):
		// **障害を「まだ知らない actor」に倒さない** (#3121)。倒すとリモートへ
		// fetch しに行くが、通常経路ではその直後の `Create` が同じ障害で落ちる
		// ので、呼び出し側からは「解決できなかった」としか見えない。
		// **署名検証はその結果を ack する**ので、DB 障害のあいだに届いた
		// activity がまるごと失われる。種別を残しておけば inbox が retry に
		// 倒せるし、落ちている DB を相手に無駄な remote fetch もしない。
		//
		// **ephemeral (リレー) 経路もここで止まる。** あちらは `Create` に到達
		// しないので (下の `if ephemeral` で戻る)、倒せば Redis 側で完結できて
		// しまうが、**倒さない**。倒すと「DB が落ちているあいだだけミュート済み
		// / materialize 済みの著者を引けず、別 ID の ephemeral 行として
		// タイムラインに戻る」— `resolveNoteAuthor` が DB を先に引く理由
		// そのもの (#2332) が崩れる。**DB 障害中はリレー由来の取り込みも
		// 止まる**が、その間 timeline は DB を読めないので失うものは小さい。
		return nil, fmt.Errorf("%w: actor %q: %v", ErrLookupUnavailable, truncateRunes(uri, userURIMaxRunes), ferr)
	}

	actor, err := r.fetchActor(uri, allowCrossHost)
	if err != nil {
		return nil, err
	}

	host, err := hostFromURI(actor.ID)
	if err != nil {
		return nil, ErrInvalidActor
	}
	// **fetch 後にも見る。** 通常は finalURL ↔ id の binding
	// (`assertResponseHostMatches`) が先に落とす (実測: `allowCrossHost` でも
	// そちらが `ErrObjectHostMismatch` を返す) が、あれは **fetcher が最終 URL を
	// 返せるときにしか効かない**。id が自ホストを指す document を「リモート
	// actor」として取り込む経路を、binding が無い条件でも閉じておく。
	if r.isSelfHost(host) {
		slog.Warn("federation: refusing to create a remote row for a local actor URI",
			"uri", truncateRunes(actor.ID, userURIMaxRunes))
		return nil, ErrLocalActor
	}
	// **必須の値が列に入らないなら actor ごと拒否する** (#2723)。`uri` / `host` は
	// 身元そのもので、切ると別人になるし捨てるわけにもいかない (lookup の鍵)。
	// ここで弾かないと `Create` が 22001 / 22021 で落ち、**actor が 1 行も
	// 作られない**まま refresh が同じ失敗を繰り返す。
	//
	// `username` はここで見ない。`validRemoteUsername` が fetchActor の時点で
	// byte 長 128 で弾いており (列は 128 コードポイントなので常に厳しい側)、
	// 重ねると届かない検査が増えるだけ。
	if !fitsColumn(actor.ID, userURIMaxRunes) || !fitsColumn(host, userHostMaxRunes) {
		slog.Warn("federation: rejecting actor whose identity does not fit its columns",
			"actor", truncateRunes(actor.ID, userURIMaxRunes), "host", host)
		return nil, ErrInvalidActor
	}
	// **取りに行った URI と文書の id が違えば、id で引き直す。** 冒頭の lookup は
	// 要求した URI で引いているので、`/@alice` のような別名や、id が既知の別の
	// actor を指す文書では「まだ無い」と判定される。そのまま作ると `uri` が同じ
	// 行が 2 つになり (`user.uri` は一意でない)、uri で引く経路と username / host
	// で引く経路が別の行を指すようになる。本家も createPerson の前に
	// `findOneBy({ uri: person.id })` で既存を返す。既存の行は別名の文書の中身
	// (鍵を含む) で書き換えない。
	if actor.ID != uri {
		known, kerr := r.userRepo.FindByURI(actor.ID)
		switch {
		case kerr == nil && known != nil:
			return known, nil
		case kerr != nil && !repository.IsNotFound(kerr):
			return nil, fmt.Errorf("%w: actor %q: %v", ErrLookupUnavailable, truncateRunes(actor.ID, userURIMaxRunes), kerr)
		}
	}

	now := r.clock()
	user := &model.User{
		ID: pickID(preassignedID, r.idGen, now),
		// リレー経由で初めて観測した行に印を付ける (#2340)。孤児掃除の対象を
		// リレー由来に限定するために使う。
		Username:      actor.PreferredUsername,
		UsernameLower: strings.ToLower(actor.PreferredUsername),
		Host:          &host,
		URI:           &actor.ID,
		// inbox は配送先。収まらないなら捨てる (行は作る) — 配送はできなく
		// なるが、表示や mention の解決は生きる (#2723)。
		Inbox: inboxPtr(remoteURIValue(actor.ID, "user.inbox", actor.Inbox)),
		IsBot: activitypub.IsBotActorType(actor.Type.String()),
		// AP actorの manuallyApprovesFollowers を IsLocked (承認制) として
		// 取り込む。これが false だとリモートの承認制ユーザに対するフォロー
		// が非 locked として処理されて即 Following が成立し、ボタン挙動と
		// AP仕様が崩れる。
		IsLocked:      actor.ManuallyApproves.Bool(),
		LastFetchedAt: &now,
		// **発信元が凍結を明示しているなら、作る時点で凍結しておく** (#2951)。
		// Mastodon 系は凍結しても `Delete` を送らず actor に `suspended` を
		// 立てるだけなので、読まないと「相手は切ったのにこちらでは生きている」
		// 状態になる。
		//
		// **`Delete` 経路と同じ結末にはならない。** あちらは `isDeleted` を
		// 立ててノート・ドライブ・following 行を消すが、こちらは `isSuspended`
		// を立てて読み取り時に隠すだけ。逆に `applySuspendedAuthorExclusion`
		// は `isSuspended` しか見ないので、**その人宛のローカル利用者の返信や
		// リノートまで timeline から消える** (`Delete` 経路では消えない)。
		// 詳細は docs/divergence/federation.md §3-3a。
		IsSuspended: actor.Suspended.Bool(),
		// アカウントの作成日時 (#3465)。actor の `published` が無い・読めない
		// ときは NULL のままにする (Misskey 系はフォローを受けたときに
		// AccountCreatedAtFiller が `/api/users/show` から埋める)。
		AccountCreatedAt: parseAccountPublished(actor.Published.String(), now),
	}
	if user.IsSuspended {
		// **由来を持てないなら凍結しない。** 記録できないまま凍結すると、
		// モデレーターが解除しても次の refresh で無言で戻る (#2951 の制約)。
		if r.suspensionOriginRepo == nil {
			user.IsSuspended = false
		} else {
			slog.Info("federation: creating remote actor as suspended per toot:suspended",
				"uri", actor.ID, "userId", user.ID)
		}
	}
	if name := remoteDisplayName(actor.Name.String()); name != "" {
		user.Name = &name
	}
	// remote actor の `_misskey_canChat` を chatScope に翻訳する (#692)。
	// CherryPick / 連合先は granular な scope (followers/following/mutual)
	// を AP に expose しないため、boolean (everyone/none) しか取れない。
	// chatScope が "none" / "everyone" のどちらかになることを保証することで、
	// chat service の canChat() が remote 相手でも正しく判定できる。
	//
	// 新規 fetch 時 (ここ) は flag 欠落 = everyone 扱い。`_misskey_canChat`
	// を export しない実装 (Mastodon / 古い Misskey / 一般的な AP 実装) は
	// そもそも chat 連合できないので、誤って "none" に倒すと local 側で
	// 送信前 reject されて UX が悪化するだけ。
	// refresh 経路 (refreshActor) は flag 欠落 = 既存値保持 (連合先 server が
	// フィールドを一時的に消した場合に scope を上書きしない安全策) で挙動が
	// 異なる点に注意。
	if actor.MisskeyCanChat != nil && !*actor.MisskeyCanChat {
		user.ChatScope = "none"
	} else {
		user.ChatScope = "everyone"
	}
	// **任意の URI は「収まらないなら値ごと捨てる」。** 切った URI は別物で、
	// 取りに行っても無駄なうえ壊れた参照を保存することになる。avatarUrl /
	// bannerUrl を守っている `remoteMediaURL` と同じ扱いに揃える (#2723)。
	if shared := remoteURIValue(actor.ID, "user.sharedInbox", remoteSharedInbox(actor)); shared != "" {
		user.SharedInbox = &shared
	}
	// 受信したノートの可視性の判定 (deriveVisibility) が followersUri と完全一致で
	// 比べるので保存する (本家 ApPersonService.createPerson の followersUri、#3330)。
	if followers := remoteURIValue(actor.ID, "user.followersUri", actor.Followers.String()); followers != "" {
		user.FollowersURI = &followers
	}
	if featured := remoteURIValue(actor.ID, "user.featured", actor.Featured.String()); featured != "" {
		user.Featured = &featured
	}
	if movedTo := remoteURIValue(actor.ID, "user.movedToUri", actor.MovedTo.String()); movedTo != "" {
		user.MovedToURI = &movedTo
		user.MovedAt = &now
	}
	if aka := remoteURIList(actor.ID, "user.alsoKnownAs", actor.AlsoKnownAs); aka != "" {
		user.AlsoKnownAs = &aka
	}
	// actor.icon / actor.image はそれぞれアバター / バナー画像。空 URL や
	// icon自体が欠落している actor (Service 系など) もあるので nil チェック。
	if avatarURL := remoteMediaURL(actor.ID, "user.avatarUrl", actor.Icon.URLOrEmpty(), maxRemoteAvatarURLLen); avatarURL != "" {
		user.AvatarURL = &avatarURL
	}
	if bannerURL := remoteMediaURL(actor.ID, "user.bannerUrl", actor.Image.URLOrEmpty(), maxRemoteBannerURLLen); bannerURL != "" {
		user.BannerURL = &bannerURL
	}
	// AP Person Tag配列からカスタム絵文字を抽出してDBにupsert
	user.Emojis = r.upsertEmojis(extractEmojiTags(actor.Tag), host)
	// upstream ApPersonService と同じく person.tag の Hashtag entry を user.tags に
	// 取り込む (hashtags/users の containment query 用)。summary text からは抽出しない。
	user.Tags = model.StringArray(hashtag.ExtractUserTags(extractHashtagTagNames(actor.Tag)...))
	if ephemeral {
		// リレー経由でしか観測しない著者は DB に入れない (#2332)。呼び出し元
		// (ingestNoteWithCreated) が PutNote でノートごと Redis に置く。
		//
		// 公開鍵は保存しない。リレー配送では Announce に署名するのがリレー
		// actor で、ノート本体は origin から HTTPS で取得するため著者の鍵を
		// 使う場面が無い。後で必要になれば (直接配送を受けた等) 通常経路の
		// ResolveActor が引き直して DB 行ごと作る。
		//
		// hashtag 集計 / user_profile / chart も DB 書き込みなので行わない。
		return user, nil
	}
	// フォロー / フォロワー一覧の公開範囲は、upstream createPerson と同じく
	// actor の following / followers collection を読んで決める (失敗は非公開)。
	// user 行を作る前に取る。後にすると、取得の間 profile の無い user 行が残り、
	// その間は一覧の公開範囲が未設定 (= public 扱い) になる。
	followingVis, followersVis := r.followVisibilitiesOnCreate(actor.ID, actor.FollowingRaw, actor.FollowersRaw)
	if err := r.userRepo.Create(user); err != nil {
		// **同じ actor を別の経路が先に作っていたら、その行を使う** (#3299)。
		// singleflight の鍵は経路ごとに分かれる (allowCrossHost / skipFeatured)
		// ので、inbox の署名検証と ap/show などが同時に同じ actor を取り込むと、
		// 両方がここまで来て片方が一意制約違反になる。そのまま返すと inbox の
		// activity が捨てられる。upstream createPerson も duplicate key なら
		// uri で引き直して使う。後続の処理 (profile など) は先に作った側が行う。
		if repository.IsUniqueViolation(err) && user.URI != nil {
			if existing, ferr := r.userRepo.FindByURI(*user.URI); ferr == nil && existing != nil {
				// **鍵はこちらでも入れる。** 先に作った側は profile を作ってから鍵を
				// 保存するので、その間に返すと inbox の署名検証が「鍵が無い」で
				// 失敗し、activity がやはり捨てられる。upstream は user /
				// user_profile / user_publickey を 1 つの transaction で作るので
				// この隙間が無い。どちらも冪等で、同じ actor 文書から作る。
				r.cachePublicKey(existing.ID, actor.PublicKey.ID, actor.PublicKey.PublicKeyPEM.String())
				r.cacheAssertionMethods(existing.ID, actor.ID, actor.AssertionMethod)
				// 先に作った側が featured を飛ばす経路 (featured の取り込み中の
				// 著者の解決) だと誰も取り込まないので、upstream と同じくこちらで行う。
				if !skipFeatured {
					r.updateFeatured(existing, chain)
				}
				return existing, nil
			}
		}
		return nil, err
	}
	// **凍結して作ったなら由来も刻む** (#2973)。刻まないと、この行は次の
	// refresh で「由来不明 = 解除方向では local 扱い」になり、**発信元が
	// 解除しても永久に凍結のまま**になる (two-way が効かない層ができる)。
	//
	// FK があるので Create の後でしか書けない。失敗したら宣言どおり凍結を
	// 取り下げる — 記録の無い凍結は解除できるかが運任せになるため。
	if user.IsSuspended && r.suspensionOriginRepo != nil {
		if oerr := r.suspensionOriginRepo.Set(user.ID, model.SuspensionOriginRemote); oerr != nil {
			slog.Warn("federation: failed to record suspension origin on create; unsuspending",
				"userId", user.ID, "err", oerr)
			if uerr := r.userRepo.UpdateUser(user.ID, map[string]any{"isSuspended": false}); uerr != nil {
				slog.Warn("federation: failed to roll back suspension", "userId", user.ID, "err", uerr)
			}
			user.IsSuspended = false
		}
	}
	// リレー経由で初めて観測した行を記録する (#2340)。孤児掃除の対象をリレー
	// 由来に限定するために使う。**新規作成時のみ**。既に DB に在る行 (上の
	// FindByURI で返る) には付けないので、リレー購読前から居る行や
	// プロフィール閲覧で解決された行を巻き込まない。
	if viaRelay && r.relayObserved != nil {
		if oerr := r.relayObserved.MarkObserved(user.ID); oerr != nil {
			slog.Warn("federation: failed to record relay-observed user", "userId", user.ID, "err", oerr)
		}
	}
	// hashtag 集計 (attachedUsersCount) を新規 remote user の tags で更新する
	// (old=nil)。remote なので isLocal=false。non-blocking hook (#1362)。
	if r.hashtagHook != nil && len(user.Tags) > 0 {
		r.hashtagHook.UpdateUsertags(user.ID, false, nil, []string(user.Tags))
	}
	// remote actor の bio (= AP `summary` または `_misskey_summary`) を
	// `user_profile.description` に取り込む (#1022)。profile 行を作成しないと
	// 既存の Description 取得経路が常に NULL を返してしまい、frontend で
	// 自己紹介欄が空のまま表示される regression につながる。
	//
	// あわせて location / birthday / fields も取り込む (#2661)。送信側
	// (renderer) は vcard:Address / vcard:bday / PropertyValue を出していたが、
	// 受信側が description しか読んでおらず、リモートユーザーのプロフィール
	// 追加項目が常に空になっていた。
	hostCopy := host
	extras := extractRemoteProfileExtras(actor)
	profile := &model.UserProfile{
		UserID:              user.ID,
		Description:         extractRemoteDescription(actor),
		Location:            extras.location,
		Birthday:            extras.birthday,
		Fields:              extras.fields,
		UserHost:            &hostCopy,
		FollowingVisibility: followingVis,
		FollowersVisibility: followersVis,
	}
	if err := r.userRepo.CreateProfile(profile); err != nil {
		// profile 作成失敗時は user 取り込みは成立させる (= 次回 refreshActor
		// で back-fill する経路を維持)。production race / fixture 衝突など
		// transient な失敗時に actor resolve 自体まで道連れにしない。
		slog.Warn("create remote user profile failed", "userId", user.ID, "err", err)
	}
	r.cachePublicKey(user.ID, actor.PublicKey.ID, actor.PublicKey.PublicKeyPEM.String())
	r.cacheAssertionMethods(user.ID, actor.ID, actor.AssertionMethod)
	r.notifyInstance(host)
	if r.chartHook != nil {
		r.chartHook.OnRemoteUserCreated(user)
	}
	// 観測を始めた時点で既にピン留めされていた投稿を取り込む (#2552)。inbound の
	// Add はピン留めされた瞬間にしか飛んで来ないので、これが無いと観測より前の
	// ピン留めは永久に拾えない。
	if !skipFeatured && !ephemeral {
		r.updateFeatured(user, chain)
	}
	return user, nil
}

// Column limits for the remote media URLs we copy onto the user row.
//
// `user.avatarUrl` は varchar(1024)、`user.bannerUrl` は varchar(512)。
// 超過値を渡すと `userRepo.Create` が SQLSTATE 22001 で落ち、**その actor が
// 1 行も作られない** (#2662)。URL は truncate すると壊れるだけなので落とす。
// upstream は Drive に取り込んで file id を持つので、長い URL でも user は
// 作られる。
const (
	maxRemoteAvatarURLLen = 1024
	maxRemoteBannerURLLen = 512
)

// remoteMediaURL returns the URL when it fits the target column, or "" when it
// does not (the actor is still imported, just without that image).
func remoteMediaURL(actorURI, column, raw string, max int) string {
	if raw == "" {
		return ""
	}
	// **NUL も落とす。** PostgreSQL の text は NUL を受け付けず (SQLSTATE 22021)、
	// 長さ超過と同じく **INSERT / UPDATE ごと**落ちる。create 経路では actor が
	// 1 行も作られず、refresh 経路では atomic UPDATE ごと失敗して
	// `lastFetchedAt` が進まないため、inbound activity 1 件につき outbound
	// fetch が 1 回走り続ける (#2662)。
	//
	// 除去ではなく破棄する。NUL を抜いた URL は別物で、取りに行っても無駄。
	if strings.ContainsRune(raw, 0) {
		slog.Warn("federation: dropping media url containing NUL",
			"actor", truncateRunes(actorURI, userURIMaxRunes), "column", column)
		return ""
	}
	if len([]rune(raw)) > max {
		slog.Warn("federation: dropping oversized media url",
			"actor", truncateRunes(actorURI, userURIMaxRunes), "column", column,
			"len", len([]rune(raw)), "max", max)
		return ""
	}
	// 画面に出すときにプロキシで包めない形の URL は、取り込まない。
	clean, ok := remoteurl.Media(raw)
	if !ok {
		slog.Warn("federation: dropping malformed media url",
			"actor", truncateRunes(actorURI, userURIMaxRunes), "column", column)
		return ""
	}
	// 符号化で長くなることがある。列に入らなければ、長すぎる値と同じく捨てる。
	if len([]rune(clean)) > max {
		slog.Warn("federation: dropping oversized media url",
			"actor", truncateRunes(actorURI, userURIMaxRunes), "column", column,
			"len", len([]rune(clean)), "max", max)
		return ""
	}
	return clean
}

// remoteSharedInbox picks the actor's shared inbox, preferring the top-level
// field like upstream's `x.sharedInbox ?? x.endpoints?.sharedInbox`.
//
// mk-go は endpoints しか見ていなかったので、top-level のみ publish する実装
// (#1560 で mk-go 自身が両方出しているとおり、実在の慣習) では shared inbox を
// 取りこぼしていた。取りこぼすと individual inbox へ落ちるだけなので配送は
// 成立するが、束ね配送が効かない (#2662)。
//
// host 検証は fetchActor で済ませてあるので、ここに来る値は actor と同一
// host か空。
func remoteSharedInbox(actor *activitypub.Person) string {
	if actor.SharedInbox != "" {
		return actor.SharedInbox.String()
	}
	return actor.Endpoints.SharedInbox.String()
}

// maxRemoteNameLen bounds the imported display name (`user.name`).
//
// upstream validateActor は `name` が truthy な string のとき
// `truncate(person.name, 128)` を通す (非 string なら throw、空文字なら
// undefined 化)。mk-go は素通ししていたので、128 文字を超える `name` を持つ
// actor は `user.name` (varchar(128)) の insert が SQLSTATE 22001 で落ち、
// **その actor がまったく作られなかった** (#2662)。NUL も 22021 で同じく落とす。
//
// upstream の `truncate` は `stringz.substring` = 書記素クラスタ単位だが、
// PostgreSQL の varchar はコードポイントで数えるので rune 単位で切る。
const maxRemoteNameLen = 128

// maxRemoteUsernameLen bounds `preferredUsername` (`user.username` /
// `usernameLower`)。upstream validateActor の `length <= 128` に対応する。
// 列長がたまたま同じだけで maxRemoteNameLen とは別の制約なので定数を分ける。
const maxRemoteUsernameLen = 128

// remoteDisplayName normalises a remote actor's display name for `user.name`.
func remoteDisplayName(name string) string {
	return colfit.Text(name, maxRemoteNameLen)
}

// usernamePattern mirrors upstream validateActor's preferredUsername check
// (`/^\w([\w-.]*\w)?$/`). 1 文字の場合は先頭の `\w` だけで成立する。
var usernamePattern = regexp.MustCompile(`^\w([\w-.]*\w)?$`)

// validRemoteUsername reports whether preferredUsername can be stored and is
// shaped like upstream requires.
//
// `\w` は Go / JS とも ASCII `[0-9A-Za-z_]` なので、この pattern を通る値は
// 必ず ASCII。したがって長さを byte で数えても upstream の UTF-16 `.length`
// と一致する。
//
// `user.username` / `usernameLower` は varchar(128) NOT NULL。長すぎる値や NUL
// 入りの値は `userRepo.Create` 自体を落とし、**その actor がまったく作られない**
// (#2662)。upstream も同じ条件で `invalid Actor: wrong username` を投げるので、
// DB エラーではなく検証で弾く。
func validRemoteUsername(name string) bool {
	if name == "" || len(name) > maxRemoteUsernameLen {
		return false
	}
	return usernamePattern.MatchString(name)
}

// Limits applied when importing a remote actor's profile extras.
//
// **upstream は location を truncate していない**が、mk-go の
// user_profile.location は varchar(128) (migration/000001_initial.up.sql)。
// 超過値をそのまま渡すと insert / update ごと落ちて、同じ書き込みに乗っている
// description まで巻き添えになる。description が 2048 文字で切っているのと
// 同じ形で切る。
//
// fields の件数も upstream の analyzeAttachments には上限が無い。ローカルの
// i/update は maxItems: 16 なので、リモートも同じ上限に揃える。上限が無いと
// 任意件数を送り込める。
const (
	maxRemoteLocationLen = 128
	maxRemoteFields      = 16
)

// sanitizeRemoteText strips NUL bytes from a remote-supplied string.
//
// **NUL は書き込みごと落とす。** JSON の NUL エスケープは正当な入力で、Go の
// decoder は実 NUL バイトを作る。PostgreSQL の text 系列はこれを受け付けず
// (実測 SQLSTATE 22021 `invalid byte sequence for encoding "UTF8": 0x00`)、
// jsonb も拒否する (22P05)。
//
// **SQLSTATE は protocol mode で変わる。** 本番の `internal/db` は pgx の
// extended protocol なので 22021 だが、`internal/testutil` は
// `PreferSimpleProtocol: true` なので同じ入力が 08P01 (`invalid message
// format`) になる。運用ログを grep するときは 22021 の方。
//
// 同じ書き込みに乗っている description まで巻き添えになり、しかも create 経路
// では **user_profile 行が 1 行も作られない** (以後の refresh も同じ create を
// 繰り返して失敗し続ける)。長さの truncate と同じ理由で、ここで落とす。
//
// 適用先は「列へ生で行く可能性のある文字列」すべて。summary 経由の
// description は mfm.FromHTML が NUL を落とすが、**_misskey_summary は
// FromHTML を通らない** ので description も対象に含める。
func sanitizeRemoteText(s string) string {
	return colfit.ToStorable(s)
}

// apTypeOf returns an AP object's `type`, tolerating the array form.
//
// upstream の getApType は `type` が配列なら先頭要素を見る
// (`activitypub/type.ts` の getApType / isPropertyValue)。string 決め打ちに
// すると `"type": ["PropertyValue"]` を送る実装を取りこぼす。
func apTypeOf(m map[string]any) string {
	switch t := m["type"].(type) {
	case string:
		return t
	case []any:
		if len(t) == 0 {
			return ""
		}
		first, _ := t[0].(string)
		return first
	}
	return ""
}

// birthdayPattern matches the leading YYYY-MM-DD of a vcard:bday value.
// upstream ApPersonService は `person['vcard:bday']?.match(/^\d{4}-\d{2}-\d{2}/)`
// で先頭だけを取る (= 時刻付きでも日付部分を使う)。
var birthdayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// remoteProfileExtras holds the profile columns imported from a remote actor
// besides the description.
type remoteProfileExtras struct {
	location *string
	birthday *string
	fields   datatypes.JSON
}

// extractRemoteProfileExtras maps the actor's vcard / attachment properties to
// user_profile columns, mirroring upstream ApPersonService:
//
// **upstream との乖離**: 空文字 / 空白のみの vcard:Address は NULL にする
// (upstream はそのまま保存する)。ローカルの i/update と同じ正規化。
//
//	fields:   analyzeAttachments(person.attachment ?? [])
//	birthday: person['vcard:bday']?.match(/^\d{4}-\d{2}-\d{2}/)?.[0] ?? null
//	location: person['vcard:Address'] ?? null
func extractRemoteProfileExtras(actor *activitypub.Person) remoteProfileExtras {
	var out remoteProfileExtras
	if loc := strings.TrimSpace(sanitizeRemoteText(actor.VcardAddress.String())); loc != "" {
		if runes := []rune(loc); len(runes) > maxRemoteLocationLen {
			loc = string(runes[:maxRemoteLocationLen])
		}
		out.location = &loc
	}
	if bday := birthdayPattern.FindString(actor.VcardBday.String()); bday != "" {
		out.birthday = &bday
	}
	out.fields = extractRemoteFields(actor.Attachment)
	return out
}

// extractRemoteFields converts the actor's `attachment` array into the
// [{name, value}] shape stored in user_profile.fields.
//
// upstream analyzeAttachments は isPropertyValue (getApType が "PropertyValue"
// かつ name が string かつ value を持ち string) で絞り、value を fromHtml で
// MFM に変換する。value は Mastodon 等が HTML (`<a href=...>`) で送るため、
// 素通しすると frontend でタグがリテラル表示される (description と同じ問題)。
//
// **upstream との乖離**: trim して name / value のどちらかが空になる entry は
// 落とす。upstream の analyzeAttachments は trim も空排除もしないが、ローカルの
// i/update (core/user) が同じ正規化をしているので揃える。
//
// 返り値は常に non-nil。取り込むものが無ければ "[]" を返して、jsonb 列を
// null にしない (golden の fields は配列必須)。
func extractRemoteFields(attachments []any) datatypes.JSON {
	empty := datatypes.JSON([]byte("[]"))
	if len(attachments) == 0 {
		return empty
	}
	type field struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	out := make([]field, 0, min(len(attachments), maxRemoteFields))
	for _, raw := range attachments {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if apTypeOf(m) != "PropertyValue" {
			continue
		}
		name, nameOK := m["name"].(string)
		value, valueOK := m["value"].(string)
		if !nameOK || !valueOK {
			continue
		}
		name = strings.TrimSpace(sanitizeRemoteText(name))
		// FromHTML は末尾で TrimSpace 済み。NUL も x/net/html のトークナイザが
		// 落とすが、経路の前提に依存しないよう明示的に落とす。
		value = mfm.FromHTML(sanitizeRemoteText(value))
		if name == "" || value == "" {
			continue
		}
		out = append(out, field{Name: name, Value: value})
		if len(out) >= maxRemoteFields {
			break
		}
	}
	if len(out) == 0 {
		return empty
	}
	data, err := json.Marshal(out)
	if err != nil {
		return empty
	}
	return datatypes.JSON(data)
}

// extractRemoteDescription returns the actor's bio mapped to
// UserProfile.Description. upstream `ApPersonService` の logic と同じく
// `_misskey_summary` (MFM そのまま) を優先し、なければ AP `summary`
// (HTML、Mastodon 系は `<p>...</p>` で wrap して送ってくる) を `mfm.FromHTML`
// で MFM 変換してから保存する。生 HTML を保存すると frontend MFM render が
// `<p>` を escape してリテラル表示する drop-in regression を起こすため
// (#1140 で発覚)。varchar(2048) 制約を rune 単位で respect し、空は nil で表す。
//
// 注: truncation 順序は upstream と意図的に差をつけている。upstream は
// HTML 段階で truncate (mid-tag 切断の risk) してから htmlToMfm、本実装は
// FromHTML で MFM に縮約してから truncate する。後者の方が content 効率
// が良く、mid-tag 切断による壊れ HTML を parser に渡す経路が無い。
func extractRemoteDescription(actor *activitypub.Person) *string {
	var raw string
	if actor.MisskeySummary != nil && *actor.MisskeySummary != "" {
		raw = actor.MisskeySummary.String()
	} else if actor.Summary != "" {
		// AP summary は Mastodon / Pleroma 等が HTML で送ってくる仕様
		// (`<p>...</p>` ラップが典型)。MFM render を期待する frontend に
		// そのまま渡すと escape されてタグがリテラル表示される。
		raw = mfm.FromHTML(actor.Summary.String())
	}
	raw = sanitizeRemoteText(raw)
	if raw == "" {
		return nil
	}
	const maxLen = 2048
	if runes := []rune(raw); len(runes) > maxLen {
		raw = string(runes[:maxLen])
	}
	return &raw
}
