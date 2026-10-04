// Package move implements i/move (account migration) business logic:
// destination URI resolution via the federation Resolver, alsoKnownAs
// verification, local user row updates (movedToUri/movedAt/alsoKnownAs),
// and Move activity delivery to follower inboxes.
package move

import (
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/repository"
)

// Errors returned by Service.Move.
var (
	// ErrNoSuchUser is returned when the destination URI cannot be resolved.
	ErrNoSuchUser = errors.New("no such user")
	// ErrAlreadyMoved is returned when the source user already has movedToUri set.
	ErrAlreadyMoved = errors.New("already moved")
	// ErrDestinationForbids is returned when the destination does not declare
	// the source URI in its alsoKnownAs, or the destination itself has already
	// moved elsewhere.
	ErrDestinationForbids = errors.New("destination account forbids")
	// ErrURINull is returned when the source or destination user lacks a URI.
	ErrURINull = errors.New("user URI is null")
	// ErrRemoteSourceForbidden is returned when the caller is not a local user.
	// AP 配送のため秘密鍵が必要で、リモートユーザーを move 元にはできない。
	ErrRemoteSourceForbidden = errors.New("source must be a local user")
)

// Resolver fetches a remote actor by URI and returns the persisted local
// representation. Implemented by federation.Resolver; kept as a local
// interface so Service doesn't import federation (avoids circular deps during
// wiring).
type Resolver interface {
	ResolveActor(uri string) (*model.User, error)
	// ExtractLocalUserID returns the local user id when uri points at this
	// instance, or "" otherwise.
	//
	// **自ホストの actor URI は `ResolveActor` が拒否する。** 自分自身を AP で
	// 取りに行くと「リモート扱いの」shadow user 行ができるため。同一
	// インスタンス内の移行は upstream も DB 参照 (`fetchPerson`) で解決して
	// いるので、こちらも DB を引く。
	ExtractLocalUserID(uri string) string
}

// Deliverer delivers a pre-rendered activity body to the follower inboxes of
// signerUserID. Implemented by federation.DeliverService.
type Deliverer interface {
	DeliverToFollowers(signerUserID string, body []byte) error
}

// FollowEnqueuer schedules follow jobs. Implemented by queue.Client.
//
// アカウント移行のフォロワー引き継ぎは 1 回で大量の follow を生むため、
// 必ず queue 経由にする。#2403 で relationship queue に分離済みなので、
// ここが AP 配送 (deliver queue) の worker を奪うことはない。
type FollowEnqueuer interface {
	EnqueueFollowBulk(payloads []queue.FollowPayload) error
	// EnqueueUnfollowBulkDelayed schedules unfollow jobs after a delay.
	// 移行元が残したフォローを後片付けするのに使う (#2420)。
	EnqueueUnfollowBulkDelayed(payloads []queue.UnfollowPayload, delay time.Duration) error
}

// unfollowAfterMove is how long the old account keeps its outgoing follows
// before they are removed.
//
// upstream `moveFromLocal` の `1000 * 60 * 60 * 24`。すぐ解除しないのは、移行
// 直後に相手側へ Undo(Follow) が殺到するのを避けるためと、移行を取り消した
// 場合に関係を復元する余地を残すため。
const unfollowAfterMove = 24 * time.Hour

// ProxyAccountIDResolver returns the local proxy account's user id.
// The second return value is false when no proxy account is configured.
type ProxyAccountIDResolver func() (string, bool)

// Service owns the account-move workflow.
type Service struct {
	userRepo      repository.UserRepository
	followingRepo repository.FollowingRepository
	urls          *activitypub.URLBuilder
	renderer      *activitypub.Renderer
	resolver      Resolver
	deliverer     Deliverer

	// followQueue / proxyAccount は post-move 処理 (#2418) 用。既存の
	// NewService 呼び出しを壊さないよう setter で後付けする。未設定なら
	// フォロワー引き継ぎは skip する (移行そのものは成立させる)。
	followQueue  FollowEnqueuer
	proxyAccount ProxyAccountIDResolver

	// carry-over (#2419) 用。未設定の系統は skip する。
	blockingRepo repository.BlockingRepository
	mutingRepo   repository.MutingRepository
	userListRepo repository.UserListRepository
	idGen        id.Generator
	blockQueue   BlockEnqueuer
	roleAssigner RoleAssigner
	antennaMover AntennaMover
}

// SetFollowQueue wires the queue used to schedule follower migration jobs.
func (s *Service) SetFollowQueue(q FollowEnqueuer) { s.followQueue = q }

// SetProxyAccountResolver wires the lookup used to exclude the proxy account
// from follower migration.
func (s *Service) SetProxyAccountResolver(f ProxyAccountIDResolver) { s.proxyAccount = f }

// NewService constructs a Service. resolver / deliverer may be nil in tests;
// Move returns ErrNoSuchUser early if resolver is missing, and skips delivery
// if deliverer is missing.
func NewService(
	userRepo repository.UserRepository,
	followingRepo repository.FollowingRepository,
	urls *activitypub.URLBuilder,
	renderer *activitypub.Renderer,
	resolver Resolver,
	deliverer Deliverer,
) *Service {
	return &Service{
		userRepo:      userRepo,
		followingRepo: followingRepo,
		urls:          urls,
		renderer:      renderer,
		resolver:      resolver,
		deliverer:     deliverer,
	}
}

// Move migrates src to the user identified by dstURI.
//
// Preconditions (mirror upstream AccountMoveService.moveFromLocal):
//   - src is a local user with a URI
//   - src.movedToUri is empty
//   - resolver.ResolveActor(dstURI) returns a user whose alsoKnownAs contains
//     src's canonical URI and whose movedToUri is empty
//
// Side effects on success:
//   - user row updated: movedToUri, movedAt, alsoKnownAs (ensures dstURI is
//     in the csv list so remote instances treat the migration as bidirectional)
//   - Move activity rendered and enqueued to every remote follower inbox
func (s *Service) Move(src *model.User, dstURI string) error {
	if src == nil {
		return ErrNoSuchUser
	}
	if !src.IsLocal() {
		return ErrRemoteSourceForbidden
	}
	if src.MovedToURI != nil && *src.MovedToURI != "" {
		return ErrAlreadyMoved
	}
	dstURI = strings.TrimSpace(dstURI)
	if dstURI == "" {
		return ErrNoSuchUser
	}
	srcURI := s.urls.UserURI(src.ID)

	if s.resolver == nil {
		return ErrNoSuchUser
	}
	dst, viaLocalURI, err := s.resolveDestination(dstURI)
	if err != nil || dst == nil {
		return ErrNoSuchUser
	}
	// 解決後の dst URI は resolver が正規化した形 (actor.id) を使う。
	// これが空なら遷移先が AP 的に不正。
	//
	// ローカルの移行先は uri 列を持たないので、受け取った dstURI ではなく
	// canonical な actor URI を組み立てる。ExtractLocalUserID は `{url}/` 配下の
	// 最後の段を読む (本家 fetchPerson と同じ) ので、`{url}/notes/<id>` のような
	// 非 canonical な URI でも解決でき、それをそのまま movedToUri に保存して
	// 配ると、相手のサーバーは移行先を引けない (#3330)。
	dstCanonical := ""
	switch {
	case dst.URI != nil && *dst.URI != "":
		dstCanonical = *dst.URI
	case viaLocalURI:
		dstCanonical = s.urls.UserURI(dst.ID)
	default:
		dstCanonical = dstURI
	}
	if !alsoKnownAsIncludes(dst.AlsoKnownAs, srcURI) {
		return ErrDestinationForbids
	}
	if dst.MovedToURI != nil && *dst.MovedToURI != "" {
		return ErrDestinationForbids
	}

	now := time.Now()
	newAlsoKnownAs := appendIfMissing(src.AlsoKnownAs, dstCanonical)
	fields := map[string]any{
		"movedToUri":  dstCanonical,
		"movedAt":     now,
		"alsoKnownAs": newAlsoKnownAs,
	}
	if err := s.userRepo.UpdateUser(src.ID, fields); err != nil {
		return err
	}
	// 呼び出し側が返り値で最新状態を参照できるよう、local struct も in-place 更新する。
	src.MovedToURI = &dstCanonical
	src.MovedAt = &now
	src.AlsoKnownAs = strPtr(newAlsoKnownAs)

	// フォロワーの引き継ぎ。DB は既にコミット済みなので best-effort
	// (エラーを返して再試行されると次回 ErrAlreadyMoved で詰む)。
	// deliverer 未配線でも引き継ぎは行いたいので、下の early return より前で呼ぶ。
	s.PostMoveProcess(src, dst)

	if s.deliverer == nil {
		return nil
	}
	move := s.renderer.RenderMove(src, dstCanonical)
	body, err := json.Marshal(move)
	if err != nil {
		return err
	}
	// DB は既にコミット済みなので、配送失敗で handler がエラーを返して再試行
	// されると次回 ErrAlreadyMoved で詰む。follower 通知は best-effort にし、
	// 失敗は log に落として握り潰す (note_delivery_hook と同じパターン)。
	if err := s.deliverer.DeliverToFollowers(src.ID, body); err != nil {
		slog.Warn("move: deliver to followers failed (DB already committed)",
			"srcID", src.ID, "dstURI", dstCanonical, "err", err)
	}
	return nil
}

// resolveDestination resolves the move destination, looking local URIs up in
// the database instead of fetching them over ActivityPub.
//
// `ResolveActor` は自ホストの actor URI を拒否する (shadow user 行を作らせない
// ため)。同一インスタンス内の移行はそれとは別の話なので、ローカル URI は
// `user` 行から直接引く (upstream の `fetchPerson` と同じ扱い)。viaLocalURI
// reports whether dstURI was read as a local URI.
func (s *Service) resolveDestination(dstURI string) (dst *model.User, viaLocalURI bool, err error) {
	if localID := s.resolver.ExtractLocalUserID(dstURI); localID != "" {
		dst, err = s.userRepo.FindByID(localID)
		return dst, true, err
	}
	dst, err = s.resolver.ResolveActor(dstURI)
	return dst, false, err
}

// alsoKnownAsIncludes returns true if the csv alsoKnownAs field contains uri.
// 本家の alsoKnownAs は配列だが Go 側は text csv として格納している点に注意。
// Move() の呼び出し側が uri != "" を保証しているため、ここでは csv 側だけ
// 空チェックする。
func alsoKnownAsIncludes(csvPtr *string, uri string) bool {
	if csvPtr == nil || *csvPtr == "" {
		return false
	}
	for _, part := range strings.Split(*csvPtr, ",") {
		if strings.TrimSpace(part) == uri {
			return true
		}
	}
	return false
}

// appendIfMissing returns the csv with uri appended if not already present.
// 既存要素は順序を保ったまま残す (remote の expectation を壊さないため)。
// 呼び出し元 Move() が uri != "" を保証しているので空文字のハンドリングはしない。
func appendIfMissing(csvPtr *string, uri string) string {
	if csvPtr == nil || *csvPtr == "" {
		return uri
	}
	for _, part := range strings.Split(*csvPtr, ",") {
		if strings.TrimSpace(part) == uri {
			return *csvPtr
		}
	}
	return *csvPtr + "," + uri
}

func strPtr(s string) *string { return &s }

// PostMoveProcess carries everything that should follow the user over to their
// new account: blocks, mutes, roles, list memberships, antennas, and finally
// the local followers.
//
// upstream AccountMoveService.postMoveProcess と同じ単位。move-out (Move) と
// move-in (federation の remote move 検知) の両方から呼ばれるので exported。
//
// **best-effort。** 呼び出し時点で DB の movedToUri は既にコミット済みで、
// ここでエラーを返して呼び出し側が再試行すると ErrAlreadyMoved で詰む。
// 失敗は log に落として握り潰す (Move の配送失敗と同じ扱い)。
func (s *Service) PostMoveProcess(src, dst *model.User) {
	if src == nil || dst == nil {
		return
	}
	s.carryOver(src, dst)
	s.migrateFollowers(src, dst)
	s.scheduleOutgoingUnfollow(src)
}

// scheduleOutgoingUnfollow removes the old account's own follows after a delay.
//
// upstream `moveFromLocal` の "Unfollow after 24 hours" に対応する (#2420)。
//
// # なぜ遅延させるか
//
// `adjustFollowingCounts` は移行時に**フォロー先の followersCount を先に 1 減らす**
// 一方、フォロー行は残す。行が残ったままだと行数とカウンタが恒久的にずれるので、
// 最終的には行も消す必要がある。
//
// ただし即座に消すと、移行直後に相手側へ Undo(Follow) が殺到する。24 時間置くのは
// それを均すためと、移行を取り消した場合に関係を復元する余地を残すため。
//
// # カウントの二重減算は起きない
//
// 実行時、`following.Service.unfollow` の movedToUri ガード (#2418) がカウント調整を
// 飛ばす。`adjustFollowingCounts` が既に引いているので、ここで引くと二重になる。
// **このガードが外れると本処理がカウントを壊す**ので、両者はセットで扱うこと。
//
// best-effort。移行そのものは確定済みなのでエラーは log に落とす。
func (s *Service) scheduleOutgoingUnfollow(src *model.User) {
	if s.followQueue == nil || s.followingRepo == nil || src == nil {
		return
	}
	followeeIDs, err := s.followingRepo.ListFolloweeIDs(src.ID)
	if err != nil {
		slog.Warn("move: list followees for delayed unfollow failed",
			"srcID", src.ID, "err", err)
		return
	}
	if len(followeeIDs) == 0 {
		return
	}
	payloads := make([]queue.UnfollowPayload, 0, len(followeeIDs))
	for _, followeeID := range followeeIDs {
		payloads = append(payloads, queue.UnfollowPayload{
			FollowerID: src.ID,
			FolloweeID: followeeID,
		})
	}
	if err := s.followQueue.EnqueueUnfollowBulkDelayed(payloads, unfollowAfterMove); err != nil {
		slog.Warn("move: enqueue delayed unfollow failed",
			"srcID", src.ID, "follows", len(payloads), "err", err)
	}
}

// migrateFollowers schedules follow jobs so local followers of src end up
// following dst. 旧アカウントは unfollow しない (adjustFollowingCounts 参照)。
func (s *Service) migrateFollowers(src, dst *model.User) {
	if src == nil || dst == nil || s.followingRepo == nil {
		return
	}
	followerIDs, err := s.followingRepo.ListLocalFollowerIDs(src.ID)
	if err != nil {
		slog.Warn("move: list local followers failed",
			"srcID", src.ID, "dstID", dst.ID, "err", err)
		return
	}
	// proxy account はリストの購読用に機械的にフォローしているだけなので、
	// 移行先へ follow させない (upstream も followerId: Not(proxy.id) で除外)。
	if s.proxyAccount != nil {
		if proxyID, ok := s.proxyAccount(); ok {
			followerIDs = slices.DeleteFunc(followerIDs, func(id string) bool {
				return id == proxyID
			})
		}
	}
	if len(followerIDs) == 0 {
		return
	}

	// カウント調整を先に行う。follow job は非同期なので、先に enqueue すると
	// worker が新しい follow を作った後で古いカウントを引くことになり、
	// 二重に減る余地がある。
	s.adjustFollowingCounts(followerIDs, src)

	if s.followQueue == nil {
		slog.Warn("move: follow queue not wired; followers were not migrated",
			"srcID", src.ID, "dstID", dst.ID, "followers", len(followerIDs))
		return
	}
	payloads := make([]queue.FollowPayload, 0, len(followerIDs))
	for _, followerID := range followerIDs {
		payloads = append(payloads, queue.FollowPayload{
			FollowerID: followerID,
			FolloweeID: dst.ID,
		})
	}
	if err := s.followQueue.EnqueueFollowBulk(payloads); err != nil {
		slog.Warn("move: enqueue follower migration failed",
			"srcID", src.ID, "dstID", dst.ID, "followers", len(payloads), "err", err)
	}
}

// adjustFollowingCounts zeroes the old account's counters and decrements the
// counters of everyone who was in a follow relationship with it.
//
// **旧アカウントを unfollow しない**のが要点。upstream のコメントどおり
// "Decrease following count instead of unfollowing" で、フォロー行は残したまま
// カウントだけ落とす。旧アカウントが移行後もまだ機能しうるため関係を切らない、
// という設計。素直に unfollow に置き換えると挙動が変わる。
//
// 結果として DB は「フォロー行はあるがカウントに出ない」状態になる。これは
// upstream と同じ状態で、意図的なもの。
func (s *Service) adjustFollowingCounts(localFollowerIDs []string, src *model.User) {
	if len(localFollowerIDs) == 0 || s.userRepo == nil {
		return
	}
	// 旧アカウント自身のカウントを 0 にする。
	if err := s.userRepo.UpdateUser(src.ID, map[string]any{
		"followersCount": 0,
		"followingCount": 0,
	}); err != nil {
		slog.Warn("move: reset old account counters failed", "srcID", src.ID, "err", err)
	}
	// ローカルフォロワーの followingCount を 1 ずつ減らす。
	for _, followerID := range localFollowerIDs {
		if err := s.userRepo.IncrementFollowingCount(followerID, -1); err != nil {
			slog.Warn("move: decrement follower's followingCount failed",
				"followerID", followerID, "err", err)
		}
	}
	// 旧アカウントがフォローしていた相手の followersCount を 1 ずつ減らす。
	followeeIDs, err := s.followingRepo.ListFolloweeIDs(src.ID)
	if err != nil {
		slog.Warn("move: list followees of old account failed", "srcID", src.ID, "err", err)
		return
	}
	for _, followeeID := range followeeIDs {
		if err := s.userRepo.IncrementFollowersCount(followeeID, -1); err != nil {
			slog.Warn("move: decrement followee's followersCount failed",
				"followeeID", followeeID, "err", err)
		}
	}
}

// BlockEnqueuer schedules block jobs. Implemented by queue.Client.
type BlockEnqueuer interface {
	EnqueueBlockBulk(payloads []queue.BlockPayload) error
}

// RoleAssigner exposes the subset of role.Service needed to carry role
// assignments over to the destination account.
type RoleAssigner interface {
	GetUserAssigns(userID string) ([]*model.RoleAssignment, error)
	FindRole(roleID string) (*model.Role, error)
	Assign(userID, roleID string, expiresAt *time.Time) error
	IsAlreadyAssigned(err error) bool
}

// AntennaMover appends the destination account to antennas that list the
// source. Implemented by antenna.Service.
type AntennaMover interface {
	OnMoveAccount(src, dst *model.User)
}

// SetCarryOverRepos wires the repositories used by the post-move carry-over
// (#2419). 未設定の系統は skip する (移行そのものは成立させる)。
func (s *Service) SetCarryOverRepos(
	blockingRepo repository.BlockingRepository,
	mutingRepo repository.MutingRepository,
	userListRepo repository.UserListRepository,
	idGen id.Generator,
) {
	s.blockingRepo = blockingRepo
	s.mutingRepo = mutingRepo
	s.userListRepo = userListRepo
	s.idGen = idGen
}

// SetBlockQueue wires the queue used to schedule block carry-over jobs.
func (s *Service) SetBlockQueue(q BlockEnqueuer) { s.blockQueue = q }

// SetRoleAssigner wires the role service used to carry role assignments over.
func (s *Service) SetRoleAssigner(r RoleAssigner) { s.roleAssigner = r }

// SetAntennaMover wires the antenna service used to carry antennas over.
func (s *Service) SetAntennaMover(a AntennaMover) { s.antennaMover = a }

// carryOver copies the relationships that should follow the user to their new
// account: blocks, mutes, roles, list memberships and antennas.
//
// upstream AccountMoveService.postMoveProcess の前半に対応する。**すべて
// 「旧側を消さずに新側を足す」方向**で、片方向であることを崩さない。旧アカウント
// が移行後もまだ機能しうるため、旧側の関係は残す。
//
// best-effort。個々の失敗は log に落として次へ進む (upstream も Promise.all を
// try/catch で丸ごと握り潰している)。
func (s *Service) carryOver(src, dst *model.User) {
	s.copyBlocking(src, dst)
	s.copyMutings(src, dst)
	s.copyRoles(src, dst)
	s.updateLists(src, dst)
	if s.antennaMover != nil {
		s.antennaMover.OnMoveAccount(src, dst)
	}
}

// copyBlocking makes everyone who blocks src block dst as well.
//
// 移行先は移行前にローカルユーザーをフォローしていた可能性があり、ブロックを
// 引き継がないと「ブロックしたはずの相手のフォロワー」が復活する。旧アカウント
// の unblock はしない (upstream の "no need to unblock the old account because
// it may be still functional")。
func (s *Service) copyBlocking(src, dst *model.User) {
	if s.blockingRepo == nil || s.blockQueue == nil {
		return
	}
	srcBlockers, err := s.blockingRepo.ListBlockerIDs(src.ID)
	if err != nil {
		slog.Warn("move: list blockers of old account failed", "srcID", src.ID, "err", err)
		return
	}
	if len(srcBlockers) == 0 {
		return
	}
	dstBlockers, err := s.blockingRepo.ListBlockerIDs(dst.ID)
	if err != nil {
		slog.Warn("move: list blockers of new account failed", "dstID", dst.ID, "err", err)
		return
	}
	already := make(map[string]struct{}, len(dstBlockers))
	for _, id := range dstBlockers {
		already[id] = struct{}{}
	}
	payloads := make([]queue.BlockPayload, 0, len(srcBlockers))
	for _, blockerID := range srcBlockers {
		if _, dup := already[blockerID]; dup {
			continue
		}
		payloads = append(payloads, queue.BlockPayload{BlockerID: blockerID, BlockeeID: dst.ID})
	}
	if len(payloads) == 0 {
		return
	}
	if err := s.blockQueue.EnqueueBlockBulk(payloads); err != nil {
		slog.Warn("move: enqueue block carry-over failed",
			"srcID", src.ID, "dstID", dst.ID, "blocks", len(payloads), "err", err)
	}
}

// copyMutings re-creates active mutes of src against dst, preserving expiresAt.
//
// dst を**無期限**ミュート済みの muter はスキップする (upstream と同じ条件)。
// 期限付きミュートしか無い muter には、src と同じ期限で dst のミュートを足す。
func (s *Service) copyMutings(src, dst *model.User) {
	if s.mutingRepo == nil || s.idGen == nil {
		return
	}
	oldMutings, err := s.mutingRepo.ListByMutee(src.ID)
	if err != nil {
		slog.Warn("move: list mutings of old account failed", "srcID", src.ID, "err", err)
		return
	}
	if len(oldMutings) == 0 {
		return
	}
	existing, err := s.mutingRepo.ListByMutee(dst.ID)
	if err != nil {
		slog.Warn("move: list mutings of new account failed", "dstID", dst.ID, "err", err)
		return
	}
	indefinite := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		if m.ExpiresAt == nil {
			indefinite[m.MuterID] = struct{}{}
		}
	}
	now := time.Now()
	for _, m := range oldMutings {
		if _, dup := indefinite[m.MuterID]; dup {
			continue
		}
		if err := s.mutingRepo.Create(&model.Muting{
			ID:        s.idGen.Generate(now),
			MuterID:   m.MuterID,
			MuteeID:   dst.ID,
			ExpiresAt: m.ExpiresAt,
		}); err != nil {
			slog.Warn("move: create muting for new account failed",
				"muterID", m.MuterID, "dstID", dst.ID, "err", err)
		}
	}
}

// copyRoles assigns dst the roles of src that opted into surviving a move.
//
// `preserveAssignmentOnMoveAccount` が false のロールは引き継がない。
// 既に割り当て済みなら skip する。
func (s *Service) copyRoles(src, dst *model.User) {
	if s.roleAssigner == nil {
		return
	}
	assigns, err := s.roleAssigner.GetUserAssigns(src.ID)
	if err != nil {
		slog.Warn("move: list role assignments failed", "srcID", src.ID, "err", err)
		return
	}
	for _, a := range assigns {
		role, err := s.roleAssigner.FindRole(a.RoleID)
		if err != nil || role == nil {
			// ロールが消えている場合など。upstream も見つからなければ skip。
			continue
		}
		if !role.PreserveAssignmentOnMoveAccount {
			continue
		}
		if err := s.roleAssigner.Assign(dst.ID, a.RoleID, a.ExpiresAt); err != nil {
			if s.roleAssigner.IsAlreadyAssigned(err) {
				continue
			}
			slog.Warn("move: assign role to new account failed",
				"roleID", a.RoleID, "dstID", dst.ID, "err", err)
		}
	}
}

// updateLists adds dst to every user list that contains src.
//
// **旧アカウントはリストから外さない**。人数上限もチェックしない (upstream の
// doc コメントに明記されている挙動)。
func (s *Service) updateLists(src, dst *model.User) {
	if s.userListRepo == nil || s.idGen == nil {
		return
	}
	oldMemberships, err := s.userListRepo.ListMembershipsByUser(src.ID)
	if err != nil {
		slog.Warn("move: list memberships of old account failed", "srcID", src.ID, "err", err)
		return
	}
	if len(oldMemberships) == 0 {
		return
	}
	existing, err := s.userListRepo.ListMembershipsByUser(dst.ID)
	if err != nil {
		slog.Warn("move: list memberships of new account failed", "dstID", dst.ID, "err", err)
		return
	}
	already := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		already[m.UserListID] = struct{}{}
	}
	now := time.Now()
	for _, m := range oldMemberships {
		if _, dup := already[m.UserListID]; dup {
			continue
		}
		if err := s.userListRepo.AddMember(&model.UserListMembership{
			ID:             s.idGen.Generate(now),
			UserID:         dst.ID,
			UserListID:     m.UserListID,
			UserListUserID: m.UserListUserID,
		}); err != nil {
			slog.Warn("move: add new account to list failed",
				"listID", m.UserListID, "dstID", dst.ID, "err", err)
		}
	}
}

// moveImportGrace is how long after a confirmed move the destination account
// may upload oversized import files. upstream import-* は `movedAt` から 2 時間。
const moveImportGrace = 2 * time.Hour

// HasRecentConfirmedMoveIn reports whether dst is the destination of a move that
// completed within the grace period, i.e. whether the bulk-import size limit
// should be relaxed for them.
//
// upstream `AccountMoveService.validateAlsoKnownAs(me, check, instant=true)` の
// import-* 用の呼び出しに対応する (#2415)。dst の `alsoKnownAs` を辿り、各移行元を
// 引いて次の両方を満たすものが 1 件でもあれば true。
//
//  1. 移行元の movedToUri が dst の正規 URI を指している (相互確認)
//  2. 移行元の movedAt が grace 以内
//
// **1 は security boundary。** 省くと、任意の actor を alsoKnownAs に並べるだけで
// 緩和された上限を得られる。alsoKnownAs は自己申告なので、移行元側が dst を
// 指し返していることまで確かめないと意味がない。
//
// 2 は #2412 で `movedAt` を遷移時だけ打刻するようにしたのが前提。再取得のたびに
// 打ち直していた頃はこの窓が永久に閉じない。
//
// upstream は dst が remote の場合だけ actor を再取得するが、ここで扱う dst は
// 常に呼び出し元のローカルユーザーなので DB 参照だけで完結する (ネットワークに
// 出ない = import のたびに外部 fetch を誘発しない)。
func (s *Service) HasRecentConfirmedMoveIn(dst *model.User) bool {
	if dst == nil || s.userRepo == nil || s.urls == nil {
		return false
	}
	if dst.AlsoKnownAs == nil || *dst.AlsoKnownAs == "" {
		return false
	}
	dstURI := dst.URI
	dstCanonical := ""
	if dstURI != nil {
		dstCanonical = *dstURI
	}
	if dstCanonical == "" {
		dstCanonical = s.urls.UserURI(dst.ID)
	}
	now := time.Now()
	for _, raw := range strings.Split(*dst.AlsoKnownAs, ",") {
		srcURI := strings.TrimSpace(raw)
		if srcURI == "" || srcURI == dstCanonical {
			continue
		}
		src := s.lookupByURI(srcURI)
		if src == nil {
			// このサーバーが知らない移行元はフォロー関係も無いので無視する
			// (upstream も fetchPerson が null なら continue)。
			continue
		}
		if src.MovedToURI == nil || *src.MovedToURI != dstCanonical {
			continue
		}
		if src.MovedAt == nil || now.Sub(*src.MovedAt) > moveImportGrace {
			continue
		}
		return true
	}
	return false
}

// lookupByURI resolves a user already known to this instance from its actor
// URI, handling local users (whose `uri` column is NULL) by id.
func (s *Service) lookupByURI(uri string) *model.User {
	if u, err := s.userRepo.FindByURI(uri); err == nil && u != nil {
		return u
	}
	if localID := s.urls.LocalUserIDFromURI(uri); localID != "" {
		if u, err := s.userRepo.FindByID(localID); err == nil && u != nil {
			return u
		}
	}
	return nil
}

// HasCarryOverRepos reports whether the post-move carry-over repositories were
// wired.
//
// 未配線だと移行先アカウントにブロック / ミュート / リストが引き継がれない。
// 移行そのものは成立するので、利用者からは「引き継がれなかった」ことしか
// 見えない。起動時検査に使う (#2683)。
func (s *Service) HasCarryOverRepos() bool {
	// **blockQueue も含める。** ブロックの引き継ぎは `blockingRepo == nil ||
	// blockQueue == nil` の**両方**で止まるので、queue だけ消えても検査が
	// 通ってしまうと effect に書いたブロック引き継ぎが黙って止まる。
	return s.blockingRepo != nil && s.mutingRepo != nil && s.userListRepo != nil &&
		s.idGen != nil && s.blockQueue != nil
}
