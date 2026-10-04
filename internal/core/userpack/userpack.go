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

// RelationManyApplier is RelationApplier for every user of a list response:
// details[i] receives the relation block from viewerID to targets[i], with one
// query per relation instead of one per user. userrelation.Repos satisfies it.
//
// 本家 packMany は getRelations で閲覧者の関係をまとめて引く。利用者ごとに
// Apply すると 1 人あたり 10 回の問い合わせになる (#3330)。
type RelationManyApplier interface {
	ApplyMany(viewerID string, details []*entity.UserDetailed, targets []*model.User, profiles []*model.UserProfile) []bool
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

// DetailExtrasMany fills the DetailExtras parts, plus UserLite.instance and
// UserLite.emojis, for every user of a list response with batched queries, the
// way upstream UserEntityService.packMany does. The users API handler
// satisfies it.
//
// 本家 packMany はピン留めを閲覧者がいるときだけ IN でまとめて引く (匿名なら
// pinnedNoteIds / pinnedNotes は空)。移行先とピン留めのページは利用者ごとに引く。
// instance と絵文字もここで埋める (一覧の handler が個別に引くと N+1 になり、
// 埋め忘れるとリモートの利用者だけ本家と差が出るため、#3330)。
type DetailExtrasMany interface {
	FillDetailedExtrasMany(ctx context.Context, viewer *model.User, targets []DetailTarget)
}

// ListPacker packs the users of a list response the way upstream packMany
// does: DetailedMany for UserDetailed lists and FillLites for UserLite lists.
// *Packer satisfies it.
type ListPacker interface {
	DetailedMany(ctx context.Context, viewer *model.User, users []*model.User, profiles map[string]*model.UserProfile) []entity.UserDetailed
	LiteFiller
}

// LiteFiller resolves instance and emojis of lites[i] from users[i] in one
// batch. *Packer satisfies it.
type LiteFiller interface {
	FillLites(users []*model.User, lites []*entity.UserLite)
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
	ExtrasMany DetailExtrasMany
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

// SetDetailExtrasMany wires the batch filler used by DetailedMany after
// construction (see SetDetailExtras for why it is late-bound).
func (p *Packer) SetDetailExtrasMany(x DetailExtrasMany) {
	p.lookups.ExtrasMany = x
}

// HasDetailExtrasMany reports whether the batch filler was wired.
//
// 未配線だと DetailedMany で組む一覧の pinnedNotes などが空、movedTo /
// alsoKnownAs が null のまま返る。起動時検査に使う。
func (p *Packer) HasDetailExtrasMany() bool { return p.lookups.ExtrasMany != nil }

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
	p.FillLites([]*model.User{u}, []*entity.UserLite{lite})
}

// FillLites resolves instance and emojis of lites[i] from users[i] with
// batched lookups, the way upstream packMany(users, me) packs UserLite lists.
func (p *Packer) FillLites(users []*model.User, lites []*entity.UserLite) {
	entity.FillUserLites(p.lookups.Instances, p.lookups.Emojis, users, lites)
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

// applyRelationsMany writes the viewer->target relation blocks of a list
// response, batched when the applier supports it.
func (p *Packer) applyRelationsMany(viewerID string, details []*entity.UserDetailed, users []*model.User, profiles []*model.UserProfile) []bool {
	if p.lookups.Relations == nil {
		return make([]bool, len(users))
	}
	if many, ok := p.lookups.Relations.(RelationManyApplier); ok {
		return many.ApplyMany(viewerID, details, users, profiles)
	}
	out := make([]bool, len(users))
	for i, u := range users {
		out[i] = p.lookups.Relations.Apply(details[i], viewerID, u, profiles[i])
	}
	return out
}

// DetailedMany packs users as upstream packMany(users, viewer, {schema:
// 'UserDetailed'}) does, in the order of users. viewer may be nil (upstream
// packMany(users, null)). profiles supplies the user_profile rows; a user
// without one is packed with the packer defaults and its counts are shown
// only to the user themself and to moderators. The relation blocks are read
// with one query per relation when Relations implements RelationManyApplier.
//
// The viewer's own entry is returned as UserDetailed; callers whose list can
// contain the viewer promote it with meself.Pack (upstream returns MeDetailed
// when isMe).
//
// 本家 packMany と同じく、閲覧者がモデレーターなら moderationNote と 2FA の
// 3 項目を足し、閲覧者から見た関係とカウントのゲートを通し、ピン留め・ページ・
// 移行先・instance・絵文字をまとめて埋める。匿名の閲覧者 (viewer=nil) には
// ピン留めを出さない (packMany は me が無いと pinNotes を引かない)。
func (p *Packer) DetailedMany(ctx context.Context, viewer *model.User, users []*model.User, profiles map[string]*model.UserProfile) []entity.UserDetailed {
	out := make([]entity.UserDetailed, len(users))
	if len(users) == 0 {
		return out
	}
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	iAmModerator := viewer != nil && p.lookups.Moderators != nil && p.lookups.Moderators.IsModerator(viewer.ID)
	details := make([]*entity.UserDetailed, len(users))
	profs := make([]*model.UserProfile, len(users))
	for i, u := range users {
		profile := profiles[u.ID]
		out[i] = entity.PackUserDetailed(u, profile, p.idGen)
		d := &out[i]
		if iAmModerator {
			note := ""
			if profile != nil && profile.ModerationNote != nil {
				note = *profile.ModerationNote
			}
			d.ModerationNote = &note
		}
		entity.ApplyModeratorSecurityFields(d, iAmModerator, profile)
		details[i], profs[i] = d, profile
	}
	following := p.applyRelationsMany(viewerID, details, users, profs)
	for i, u := range users {
		// profile が無い利用者は GateCountVisibility が本人とモデレーター以外に
		// 伏せる (PackUserDetailed が印を付ける)。
		entity.GateCountVisibility(details[i], viewerID == u.ID, iAmModerator, following[i])
	}
	if p.lookups.ExtrasMany != nil {
		targets := make([]DetailTarget, len(users))
		for i, u := range users {
			targets[i] = DetailTarget{User: u, Profile: profiles[u.ID], Detailed: &out[i]}
		}
		p.lookups.ExtrasMany.FillDetailedExtrasMany(ctx, viewer, targets)
		return out
	}
	// 一覧向けの埋め手が無い構成 (テストの部分配線) でも instance と絵文字は埋める。
	lites := make([]*entity.UserLite, len(users))
	for i := range users {
		lites[i] = &out[i].UserLite
	}
	p.FillLites(users, lites)
	return out
}
