// Package blocking provides UserBlockingService for managing user blocks.
package blocking

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// Errors returned by Service.
var (
	// ErrSelfBlock is returned when a user attempts to block themselves.
	ErrSelfBlock = errors.New("cannot block yourself")
	// ErrBlockeeNotFound is returned when the target user does not exist.
	ErrBlockeeNotFound = errors.New("blockee not found")
	// ErrAlreadyBlocking is returned when the blocker already blocks the blockee.
	ErrAlreadyBlocking = errors.New("already blocking")
	// ErrNotBlocking is returned when there is no blocking relationship to delete.
	ErrNotBlocking = errors.New("not blocking")
)

// Service manages user blocking relationships.
type Service struct {
	userRepo      repository.UserRepository
	blockingRepo  repository.BlockingRepository
	followingRepo repository.FollowingRepository
	instanceRepo  repository.InstanceRepository // optional, for #596 incremental counters
	idGen         id.Generator
	// federationHook は local user が remote user を (un)block した際に
	// Block / Undo(Block) を相手 inbox へ配信する (#1560)。nil なら配信しない。
	federationHook FederationHook
	// quoteRevoker はブロックした相手の引用に出していた承認を取り消す
	// (FEP-044f、#3234 段階 4)。nil なら何もしない。
	quoteRevoker QuoteRevoker
	// followRequestCanceller は block 時に保留中の follow request を
	// 双方向で取り消す。実装は core/following.Service。nil なら何もしない。
	followRequestCanceller FollowRequestCanceller
	// userMaterializer はリレーでしか観測していない相手を DB へ昇格させる
	// (#2332)。muting.muteeId / blocking.blockeeId が user への外部キー。
	userMaterializer UserMaterializer
	// relationReload は block 変更を streaming connection へ通知する (#2400)。
	relationReload RelationReloadPublisher
}

// FederationHook delivers Block / Undo(Block) AP activities to a remote
// blockee on local block / unblock (#1560)。実装は core/federation。
type FederationHook interface {
	OnBlocked(blockerID, blockeeID string)
	OnUnblocked(blockerID, blockeeID string)
}

// QuoteRevoker withdraws the quote approvals the blocker granted to the
// blockee (#3234)。実装は core/federation。best-effort で、戻り値を持たない。
type QuoteRevoker interface {
	RevokeQuotesOnBlock(blockerID, blockeeID string)
}

// SetQuoteRevoker wires the quote approval revocation run on block (#3234)。
func (s *Service) SetQuoteRevoker(r QuoteRevoker) {
	s.quoteRevoker = r
}

// FollowRequestCanceller cancels any pending follow request between two users.
// 実装は core/following.Service。循環依存を避けるため interface で受け取る。
type FollowRequestCanceller interface {
	// CancelFollowRequestsBetween cancels pending follow requests in both
	// directions between a and b. 該当する request が無い場合は no-op。
	CancelFollowRequestsBetween(a, b string) error
}

// SetFollowRequestCanceller wires the pending-follow-request cleanup used by
// Block. upstream UserBlockingService.block は cancelRequest を双方向で呼び、
// 保留中の申請を削除する (相手が remote なら Undo(Follow) / Reject を配送)。
// 未配線だと block しても申請が残り、block 中に承認されるとフォロー関係が
// 成立してしまう。
func (s *Service) SetFollowRequestCanceller(h FollowRequestCanceller) {
	s.followRequestCanceller = h
}

// HasFollowRequestCanceller reports whether the canceller was wired.
// 起動時検査 (criticalWiring) に使う。
func (s *Service) HasFollowRequestCanceller() bool {
	return s.followRequestCanceller != nil
}

// SetFederationHook wires the AP delivery hook used by Block / Unblock (#1560)。
func (s *Service) SetFederationHook(h FederationHook) {
	s.federationHook = h
}

// NewService constructs a UserBlockingService.
// followingRepo は省略可だが、指定されているとブロック時に既存のフォロー関係を
// 自動解除する。
func NewService(
	userRepo repository.UserRepository,
	blockingRepo repository.BlockingRepository,
	followingRepo repository.FollowingRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		userRepo:      userRepo,
		blockingRepo:  blockingRepo,
		followingRepo: followingRepo,
		idGen:         idGen,
	}
}

// SetInstanceRepo wires an InstanceRepository so the auto-unfollow side effect
// of Block also adjusts remote instance followers/following counts (#596)。
// 未配線でも本機能には影響しない (起動時 RecomputeFollowCounts が安全網)。
func (s *Service) SetInstanceRepo(r repository.InstanceRepository) {
	s.instanceRepo = r
}

// RelationReloadPublisher notifies streaming connections that a viewer's
// relation snapshot changed (#2400)。実装は stream 側の adapter。
type RelationReloadPublisher interface {
	// PublishMuteBlockReload は mute/block snapshot だけを取り直させる。
	PublishMuteBlockReload(userID string)
	// PublishFollowingReload は following snapshot だけを取り直させる。
	PublishFollowingReload(userID string)
}

// SetRelationReloadPublisher wires the streaming reload publisher. 未配線なら
// 通知しない (= 従来どおり再接続まで stale)。
func (s *Service) SetRelationReloadPublisher(p RelationReloadPublisher) {
	s.relationReload = p
}

// Block creates a blocking relationship from blocker to blockee.
// 既存のフォロー関係 (双方向) があれば自動的に解除する。
func (s *Service) Block(blockerID, blockeeID string) (*model.Blocking, error) {
	if blockerID == blockeeID {
		return nil, ErrSelfBlock
	}
	_, uerr := s.userRepo.FindByID(blockeeID)
	// **materialize の前に分ける** (#2799)。not-found でない error は
	// materialize しても直らないのに、EnsureUser / EnsureNote が ephemeral
	// store の引きを 1 回余分に走らせる。ヒットすればそのまま WebFinger +
	// actor fetch まで進むので、DB 断中は 1 リクエストごとに outbound HTTP が
	// 出うる。
	if uerr != nil && !repository.IsNotFound(uerr) {
		return nil, uerr
	}
	if s.materializeUserIfMissing(blockeeID, uerr) {
		_, uerr = s.userRepo.FindByID(blockeeID)
	}
	if uerr != nil {
		// **DB 障害を not-found に丸めない** (#2799)。materialize の再試行後も
		// 種別は残るので、ここで分ける。
		if !repository.IsNotFound(uerr) {
			return nil, uerr
		}
		return nil, ErrBlockeeNotFound
	}

	if exists, err := s.blockingRepo.Exists(blockerID, blockeeID); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrAlreadyBlocking
	}

	b := &model.Blocking{
		ID:        s.idGen.Generate(time.Now()),
		BlockerID: blockerID,
		BlockeeID: blockeeID,
	}
	if err := s.blockingRepo.Create(b); err != nil {
		return nil, err
	}

	// 既存のフォロー関係を双方向で解除する。fold-in counterも調整する。
	var blockerUnfollowed, blockeeUnfollowed bool
	if s.followingRepo != nil {
		blockerUnfollowed = s.removeFollowing(blockerID, blockeeID)
		blockeeUnfollowed = s.removeFollowing(blockeeID, blockerID)
	}

	// 保留中の follow request を双方向で取り消す。upstream
	// UserBlockingService.block の cancelRequest 相当。
	// **失敗しても block 自体は成立させる** (best-effort)。残すと、block 中に
	// 承認されたときにフォロー関係が成立してしまう。
	if s.followRequestCanceller != nil {
		if err := s.followRequestCanceller.CancelFollowRequestsBetween(blockerID, blockeeID); err != nil {
			slog.Warn("block: cancel pending follow requests failed",
				"blocker", blockerID, "blockee", blockeeID, "err", err)
		}
	}

	// ブロックした相手の引用に出していた承認を取り消す (#3234)。
	if s.quoteRevoker != nil {
		s.quoteRevoker.RevokeQuotesOnBlock(blockerID, blockeeID)
	}

	// remote blockee へ Block activity を配信する (#1560)。
	if s.federationHook != nil {
		s.federationHook.OnBlocked(blockerID, blockeeID)
	}

	s.publishBlockReload(blockerID, blockeeID, blockerUnfollowed, blockeeUnfollowed)
	return b, nil
}

// publishBlockReload notifies both sides of a block/unblock (#2400).
//
// **向きに注意。** MuteBlockSnapshot が持つのは `BlockingMe` (= 自分を block して
// いる人) なので、block/unblock で mute-block snapshot が変わるのは **blockee 側**。
// ここを blocker 側にすると「block したのに相手の画面に流れ続ける」形で残る。
//
// あわせて Block は既存 follow を双方向で解除しうる。**実際に解除された側だけ**
// following の通知を出す。無条件に出すと、follow 関係の無い相手を block した
// ときにも reload が 2 回飛び、接続中の全 connection で無駄な DB 往復が起きる。
//
// Unblock は follow を復元しないので本関数を使わず、mute-block だけを直接
// 通知している。
func (s *Service) publishBlockReload(blockerID, blockeeID string, blockerUnfollowed, blockeeUnfollowed bool) {
	if s.relationReload == nil {
		return
	}
	// blockee: BlockingMe が増える。
	s.relationReload.PublishMuteBlockReload(blockeeID)
	if blockerUnfollowed {
		s.relationReload.PublishFollowingReload(blockerID)
	}
	if blockeeUnfollowed {
		s.relationReload.PublishFollowingReload(blockeeID)
	}
}

// Unblock removes a blocking relationship.
func (s *Service) Unblock(blockerID, blockeeID string) error {
	if blockerID == blockeeID {
		return ErrSelfBlock
	}
	b, err := s.blockingRepo.FindByPair(blockerID, blockeeID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNotBlocking
	}
	if err := s.blockingRepo.Delete(b); err != nil {
		return err
	}
	// remote blockee へ Undo(Block) を配信する (#1560)。
	if s.federationHook != nil {
		s.federationHook.OnUnblocked(blockerID, blockeeID)
	}
	// unblock は follow を復元しないので mute-block 側だけ通知する。
	if s.relationReload != nil {
		s.relationReload.PublishMuteBlockReload(blockeeID)
	}
	return nil
}

// IsBlocked reports whether blocker has blocked blockee.
func (s *Service) IsBlocked(blockerID, blockeeID string) (bool, error) {
	return s.blockingRepo.Exists(blockerID, blockeeID)
}

// List returns the user's blockings with cursor (sinceID/untilID) or offset
// pagination. Cursor 指定時は offset 無視 (upstream makePaginationQuery と一致)。
func (s *Service) List(blockerID, sinceID, untilID string, limit, offset int) ([]*model.Blocking, error) {
	if limit <= 0 {
		limit = 10
	}
	return s.blockingRepo.ListByBlocker(blockerID, sinceID, untilID, limit, offset)
}

// removeFollowing deletes a follow edge if it exists and adjusts counters.
// エラーは握り潰し (ベストエフォート)。
//
// IMPORTANT: instance counter 調整ロジックは following.Service の
// adjustInstanceCountsForFollowing と **mirror で維持** すること。循環依存
// 回避のため共通 helper にせず inline しているので、片方変更時には他方も
// 揃える (#596 / PR #626 review)。
// 戻り値は「実際に follow row を消したか」。streaming の following snapshot は
// 解除が起きたときだけ取り直せばよく、無条件に通知すると block のたびに無駄な
// reload と DB 往復が 2 回増える (#2400)。
func (s *Service) removeFollowing(followerID, followeeID string) bool {
	f, err := s.followingRepo.FindByPair(followerID, followeeID)
	if err != nil {
		return false
	}
	if err := s.followingRepo.Delete(f); err != nil {
		return false
	}
	_ = s.userRepo.IncrementFollowingCount(followerID, -1)
	_ = s.userRepo.IncrementFollowersCount(followeeID, -1)
	// remote instance の集計列も -1 する (#596)。block→自動 unfollow 経路で
	// follow row が消えるので following.Service.Unfollow と同じ調整が必要。
	// 失敗は warn-log で観測 signal を残す (following.Service と同様)。
	if s.instanceRepo != nil {
		if f.FollowerHost != nil {
			if err := s.instanceRepo.IncrementFollowersCount(*f.FollowerHost, -1); err != nil {
				slog.Warn("instance counter: followersCount adjust failed (block path)",
					"host", *f.FollowerHost, "delta", -1, "err", err)
			}
		}
		if f.FolloweeHost != nil {
			if err := s.instanceRepo.IncrementFollowingCount(*f.FolloweeHost, -1); err != nil {
				slog.Warn("instance counter: followingCount adjust failed (block path)",
					"host", *f.FolloweeHost, "delta", -1, "err", err)
			}
		}
	}
	return true
}

// UserMaterializer promotes a relay-only author out of the ephemeral store
// into a real database row (#2332)。実装は core/ephemeral.Materializer。
//
// ミュート / ブロックは user への外部キーだけを要求し、ノート行は要らない。
// リレーでしか観測していない相手をミュートしようとしたときに、対象が DB に
// 居ないと登録できないため。
type UserMaterializer interface {
	EnsureUser(ctx context.Context, userID string) (*model.User, error)
}

// SetUserMaterializer attaches the ephemeral-author materializer. Optional.
func (s *Service) SetUserMaterializer(m UserMaterializer) {
	s.userMaterializer = m
}

// materializeUserIfMissing promotes an ephemeral author only when the database
// lookup already failed.
func (s *Service) materializeUserIfMissing(userID string, lookupErr error) bool {
	if lookupErr == nil || s.userMaterializer == nil {
		return false
	}
	_, err := s.userMaterializer.EnsureUser(context.Background(), userID)
	return err == nil
}
