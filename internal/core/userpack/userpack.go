// Package userpack packs a user the way upstream UserEntityService.pack does
// for server-originated events (main stream follow / unfollow / followed and
// the matching user webhooks), where no API handler is in the call path.
package userpack

import (
	"context"
	"log/slog"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
)

// ProfileLookup loads the profile row of a user. repository.UserRepository
// satisfies it.
type ProfileLookup interface {
	FindProfileByUserID(userID string) (*model.UserProfile, error)
}

// RelationApplier writes the viewer->target relation block (isFollowing,
// isBlocking, memo, ...) onto a packed user and reports whether the viewer
// follows the target. userrelation.Repos satisfies it.
type RelationApplier interface {
	Apply(detailed *entity.UserDetailed, viewerID string, target *model.User, profile *model.UserProfile) bool
}

// ModeratorChecker reports whether a user holds moderator privileges.
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// DetailExtras fills the UserDetailed parts that need the users/show
// dependencies: pinnedNoteIds / pinnedNotes / pinnedPageId / pinnedPage (gated
// by viewer) and movedTo / alsoKnownAs. The users API handler satisfies it.
type DetailExtras interface {
	FillDetailedExtras(ctx context.Context, viewer, u *model.User, profile *model.UserProfile, d *entity.UserDetailed)
}

// DetailTarget is one user of a list response whose detail extras are filled
// in a batch.
type DetailTarget struct {
	User     *model.User
	Profile  *model.UserProfile
	Detailed *entity.UserDetailed
}

// DetailExtrasMany fills the DetailExtras parts for every user of a list
// response with batched queries, the way upstream UserEntityService.packMany
// does. The users API handler satisfies it.
//
// 本家 packMany はピン留めを閲覧者がいるときだけ IN でまとめて引く (匿名なら
// pinnedNoteIds / pinnedNotes は空)。移行先とピン留めのページは利用者ごとに引く。
type DetailExtrasMany interface {
	FillDetailedExtrasMany(ctx context.Context, viewer *model.User, targets []DetailTarget)
}

// Lookups resolves the parts of the packed user that come from other tables.
// A nil lookup leaves that part out (test fixtures / partial wiring), except
// Profiles: without it DetailedNotMe refuses to pack. Production wires all.
type Lookups struct {
	Instances  entity.InstanceLookup
	Emojis     entity.EmojiLookup
	Profiles   ProfileLookup
	Relations  RelationApplier
	Moderators ModeratorChecker
	Extras     DetailExtras
}

// Packer packs users as UserLite / UserDetailedNotMe.
type Packer struct {
	lookups Lookups
	idGen   id.Generator
}

// New constructs a Packer. idGen derives createdAt from the user ID.
func New(l Lookups, idGen id.Generator) *Packer {
	return &Packer{lookups: l, idGen: idGen}
}

// SetDetailExtras wires the pinned / move-target filler after construction.
//
// 埋める側 (users の handler) は Packer を渡す先よりも後に組み立てられるので、
// 起動時の配線の途中で差し込めるようにしている。
func (p *Packer) SetDetailExtras(x DetailExtras) {
	p.lookups.Extras = x
}

// HasDetailExtras reports whether the pinned / move-target filler was wired.
//
// 未配線だと pinnedNotes / pinnedNoteIds / pinnedPage が空、movedTo /
// alsoKnownAs が null のまま返る。起動時検査に使う。
func (p *Packer) HasDetailExtras() bool { return p.lookups.Extras != nil }

// Lite packs u as upstream's default UserLite: remote users get instance and
// the display-name emojis are resolved.
//
// PackUserLite だけでは instance も絵文字の URL も付かず、相手がリモートだと
// 本家と差が出る。
func (p *Packer) Lite(u *model.User) entity.UserLite {
	lite := entity.PackUserLite(u)
	p.resolveLite(u, &lite)
	return lite
}

func (p *Packer) resolveLite(u *model.User, lite *entity.UserLite) {
	entity.NewInstanceResolver(p.lookups.Instances, u).FillUserLite(lite)
	entity.NewEmojiResolver(p.lookups.Emojis, []*model.Note{{User: u}}).PopulateUserEmojis(u, lite)
}

// DetailedNotMe packs target as UserDetailedNotMe seen by viewer. It returns
// false when the profile cannot be loaded.
//
// 本家は profile を findOneByOrFail で読み、無ければ例外になって送らない。
// profile 無しで組むと followersVisibility が既定の public に倒れ、伏せるべき
// カウントが出るので、読めないときは送らない側に倒す。
func (p *Packer) DetailedNotMe(ctx context.Context, target, viewer *model.User) (entity.UserDetailed, bool) {
	// 配線が外れたとき (Profiles が nil) も、公開範囲を確かめられないので閉じる側に倒す。
	if p.lookups.Profiles == nil {
		slog.Warn("userpack: profile lookup is not wired; packed user dropped", "userId", target.ID)
		return entity.UserDetailed{}, false
	}
	profile, err := p.lookups.Profiles.FindProfileByUserID(target.ID)
	if err != nil || profile == nil {
		slog.Warn("userpack: load profile failed", "userId", target.ID, "err", err)
		return entity.UserDetailed{}, false
	}
	d := entity.PackUserDetailed(target, profile, p.idGen)
	p.resolveLite(target, &d.UserLite)
	iAmModerator := p.lookups.Moderators != nil && p.lookups.Moderators.IsModerator(viewer.ID)
	// 本家は閲覧者がモデレーターなら moderationNote と 2FA の 3 項目を足す
	// (users/show と同じ扱い)。
	if iAmModerator {
		note := ""
		if profile.ModerationNote != nil {
			note = *profile.ModerationNote
		}
		d.ModerationNote = &note
	}
	entity.ApplyModeratorSecurityFields(&d, iAmModerator, profile)
	// ピン留めのノート・ページと移行先は users/show と同じ規則で埋める
	// (閲覧者から見えないピン留めは本文を出さない、#3310)。
	if p.lookups.Extras != nil {
		p.lookups.Extras.FillDetailedExtras(ctx, viewer, target, profile, &d)
	}
	viewerIsFollowing := false
	if p.lookups.Relations != nil {
		viewerIsFollowing = p.lookups.Relations.Apply(&d, viewer.ID, target, profile)
	}
	// フォロワー限定のカウントは、閲覧者がフォロワーのときだけ見せる。follow の
	// 直後は関係の行があるので見え、unfollow の直後は見えない (本家と同じ)。
	entity.GateCountVisibility(&d, false, iAmModerator, viewerIsFollowing)
	return d, true
}
