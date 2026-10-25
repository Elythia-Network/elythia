package entity

// Packer holds the lookups the entity packers consult beyond the row they
// pack: role-derived flags, avatar decorations, meta-backed switches and the
// media-proxy URL rewriter.
//
// 以前は依存ごとにパッケージの変数を持ち、router が Set* で 1 度だけ書いて
// いた。newServer が組み立てのたびにプロセス全体の状態を書き換えるのをやめる
// ため、依存を 1 つの値にまとめる (#3788、#3666 の段階 (5))。(5-5) では公開
// 関数を Packer のメソッドにし、パッケージ関数は既定の Packer へ委ねる。
// 呼び出し側は系統ごとに Packer を受け取る形へ移し、(5-9) でパッケージ関数は
// 無くなった (#3790〜#3798)。(5-10) で既定の Packer と Set* も本番から消し、
// このパッケージのテスト用の補助 (default_packer_test.go) にだけ残した (#3800)。
//
// nil の *Packer と、依存が欠けた Packer は、それぞれの依存が未配線のときと
// 同じに動く。**mediaURL が無いとメディアの URL を生のまま出す** (閲覧者の IP が
// リモートへ漏れる側) ので、mediaURL は NewPacker の必須の引数で受ける (#3834)。
// 組み立てた Packer が mediaURL を持つことは、server の
// TestBuiltHandlersHaveNoNilDependencies が見る。
type Packer struct {
	// avatarDecoration embeds `url` on each avatarDecorations entry. nil omits
	// url (the pre-#521 behaviour).
	avatarDecoration AvatarDecorationLookup
	// emojiDecoration resolves emoji-backed avatarDecorations entries (#2975).
	// nil drops those entries (fail-closed): unlike catalog entries, which are
	// kept without `url` when unwired, there is no way to tell whether an emoji
	// is still non-sensitive.
	emojiDecoration EmojiDecorationLookup
	// canChat derives canChat from role policies. nil yields the upstream
	// default `chatAvailability = "available"` (canChat=true).
	canChat CanChatLookup
	// userRoles populates `roles` / `badgeRoles`. nil yields empty arrays.
	userRoles UserRolesLookup
	// showRemoteBadges reads meta.showRoleBadgesOfRemoteUsers (upstream
	// `meta.showRoleBadgesOfRemoteUsers || user.host == null`、#1653). nil keeps
	// the conservative local-only default.
	showRemoteBadges func() bool
	// instanceIconURL reads meta.iconUrl for upstream's local-system-account
	// special case: a local user whose username contains '.' (relay.actor /
	// instance.actor / proxy.actor) uses the instance icon as its avatarUrl
	// (upstream getIdenticonUrl, UserEntityService.ts:386-391、#1781). nil / 空なら
	// identicon route に fallback する。
	instanceIconURL func() string
	// silenced derives isSilenced from role policies. nil leaves it false.
	silenced SilencedLookup
	// mediaURL rewrites media URLs through the media proxy. nil leaves every URL
	// raw.
	//
	// 無いと、リモートのメディア (添付ファイル・アバター等) の URL を media proxy を
	// 通さずに返し、閲覧者のブラウザがリモートから直接読むので閲覧者の IP がリモートへ
	// 漏れる。以前は PackerDeps の欄で受けて起動時の検査 (criticalWiring の
	// entityPacker.mediaURL の行) で見ていたが、#3834 で NewPacker の必須の引数にした。
	mediaURL *MediaURLContext
}

// PackerDeps names the dependencies of a Packer other than the media-proxy URL
// rewriter, which NewPacker takes as its own argument. Unset fields behave as
// the corresponding lookup being unwired.
type PackerDeps struct {
	AvatarDecoration AvatarDecorationLookup
	EmojiDecoration  EmojiDecorationLookup
	CanChat          CanChatLookup
	UserRoles        UserRolesLookup
	ShowRemoteBadges func() bool
	InstanceIconURL  func() string
	Silenced         SilencedLookup
}

// NewPacker returns a Packer that rewrites media URLs through mediaURL and
// consults deps. A nil mediaURL leaves every media URL raw; only tests that do
// not look at media URLs pass nil.
func NewPacker(mediaURL *MediaURLContext, deps PackerDeps) *Packer {
	return &Packer{
		avatarDecoration: deps.AvatarDecoration,
		emojiDecoration:  deps.EmojiDecoration,
		canChat:          deps.CanChat,
		userRoles:        deps.UserRoles,
		showRemoteBadges: deps.ShowRemoteBadges,
		instanceIconURL:  deps.InstanceIconURL,
		silenced:         deps.Silenced,
		mediaURL:         mediaURL,
	}
}

// HasMediaURL reports whether pkr rewrites media URLs through the media
// proxy. false means every remote media URL goes out raw and the viewer's
// browser fetches it from the remote server directly.
//
// 見るのは mediaURL だけ。canChat や silenced などが欠けても true を返す。欠けた
// ときに閲覧者の IP が漏れる側に倒れるのは mediaURL だけなので、Packer を構築子で
// 受ける部品 (webpush の 2 つと userpack) は、受け取った Packer がこれを持つかを
// 起動時の検査 (criticalWiring) で見る。Packer 自身の mediaURL は #3834 から
// NewPacker の必須の引数で、起動時の検査の行は無い。
func (pkr *Packer) HasMediaURL() bool {
	return pkr != nil && pkr.mediaURL != nil
}

// get returns the dependencies of pkr, or none for a nil Packer.
func (pkr *Packer) get() Packer {
	if pkr == nil {
		return Packer{}
	}
	return *pkr
}
