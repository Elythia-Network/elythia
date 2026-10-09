package following

import (
	"errors"
	"log/slog"
	"time"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// FollowApprovalMaxSeconds is the upper bound of followApprovalLocalSeconds /
// followApprovalRemoteSeconds accepted by i/update (30 days, the same as
// upstream FOLLOW_APPROVAL_MAX_SECONDS).
const FollowApprovalMaxSeconds = 30 * 24 * 60 * 60

// RequiresFollowApproval reports whether a follow from follower toward the
// owner of profile falls under the account-age setting (#3466): the setting
// for the follower's origin (local / remote) is positive and the follower's
// account is younger than it. It mirrors upstream
// shouldRequireFollowApproval except for the account age of remote users.
//
// The age comes from core/user.AccountCreatedAt: for remote users the stored
// account creation time (#3465) when known, the time this server first saw
// them otherwise; for local users the sign-up time. When the age cannot be
// derived it returns false.
//
// 本家はリモートの人も ID の日時 (このサーバーが初めて知った日時) で数えるので、
// 初めてやりとりする相手は何年も使われているアカウントでも全員が新しい扱いに
// なる。作成日時が分かればそちらを使う (docs/divergence/api.md)。日時が出せない
// のは ID の形式を読めないときだけで、Elythia が作った ID ではないので、止めずに
// 通す (止める側に倒すと、その人からのフォローが期間に関係なく永久に止まる)。
func RequiresFollowApproval(profile *model.UserProfile, follower *model.User, gen id.Generator, now time.Time) bool {
	if profile == nil || follower == nil {
		return false
	}
	seconds := profile.FollowApprovalLocalSeconds
	if !follower.IsLocal() {
		seconds = profile.FollowApprovalRemoteSeconds
	}
	if seconds == nil || *seconds <= 0 {
		return false
	}
	createdAt, ok := coreuser.AccountCreatedAt(follower, gen)
	if !ok {
		return false
	}
	return now.Sub(createdAt) < time.Duration(*seconds)*time.Second
}

// followApproval is the outcome of the account-age check in Follow.
type followApproval struct {
	// request は承認待ちにするか。silentRequest のときも true。
	request bool
	// silentRequest は承認待ちにするが receiveFollowRequest の通知を作らないか。
	silentRequest bool
	// silentFollow はフォローを成立させるが follow の通知を作らず、
	// silent_follow に記録するか。
	silentFollow bool
}

// checkFollowApproval applies the followee's account-age setting (#3466) to a
// follow from follower. Only local followees have the setting.
func (s *Service) checkFollowApproval(follower, followee *model.User) (followApproval, error) {
	if !followee.IsLocal() {
		return followApproval{}, nil
	}
	profile, err := s.userRepo.FindProfileByUserID(followee.ID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。profile が無いだけなら
		// 設定も無いので止めない。
		if !repository.IsNotFound(err) {
			return followApproval{}, err
		}
		return followApproval{}, nil
	}
	if !RequiresFollowApproval(profile, follower, s.idGen, time.Now()) {
		return followApproval{}, nil
	}
	switch model.NormalizeFollowApprovalAction(profile.FollowApprovalAction) {
	case model.FollowApprovalActionSilentRequest:
		return followApproval{request: true, silentRequest: true}, nil
	case model.FollowApprovalActionSilentFollow:
		return followApproval{silentFollow: true}, nil
	default:
		return followApproval{request: true}, nil
	}
}

// SetSilentFollowRepo wires the store of follows that succeeded silently
// (#3466). Without it the follows still succeed without a notification but
// are not listed in following/silent/list.
func (s *Service) SetSilentFollowRepo(r repository.SilentFollowRepository) {
	s.silentFollowRepo = r
}

// HasSilentFollowRepo reports whether the silent follow store was wired.
//
// 未配線だと「静かに成立させる」で通ったフォローが一覧に残らず、受け手が後から
// 確かめられない。起動時検査に使う。
func (s *Service) HasSilentFollowRepo() bool { return s != nil && s.silentFollowRepo != nil }

// recordSilentFollow stores a silent follow. Failures are logged only: the
// follow itself has already been created.
func (s *Service) recordSilentFollow(followerID, followeeID string) {
	if s.silentFollowRepo == nil {
		return
	}
	row := &model.SilentFollow{ID: s.idGen.Generate(time.Now()), FollowerID: followerID, FolloweeID: followeeID}
	if err := s.silentFollowRepo.Record(row); err != nil {
		slog.Warn("following: failed to record silent follow", "follower", followerID, "followee", followeeID, "error", err)
	}
}

// ListSilentFollows returns the follows toward userID that succeeded without
// a notification (#3466), newest first.
func (s *Service) ListSilentFollows(userID string, limit int, sinceID, untilID string) ([]*model.SilentFollow, error) {
	if s.silentFollowRepo == nil {
		return []*model.SilentFollow{}, nil
	}
	return s.silentFollowRepo.ListByFollowee(userID, limit, sinceID, untilID)
}

// acceptAllPageSize is the page size used to read every pending request in
// AcceptAllRequests.
const acceptAllPageSize = 100

// AcceptAllRequests accepts the pending follow requests toward followeeID,
// as upstream acceptAllFollowRequests does when a user unlocks the account
// (i/update with isLocked going from true to false).
//
// Requests from followers that still fall under the account-age setting stay
// pending, unless the followee follows them and has autoAcceptFollowed on.
// Requests that disappear meanwhile (cancelled, or a follower deleted) are
// skipped. The account-age check ignores followApprovalAction: unlocking never
// turns a pending request into a silent follow.
func (s *Service) AcceptAllRequests(followeeID string) error {
	var requests []*model.FollowRequest
	untilID := ""
	for {
		page, err := s.followRequestRepo.ListReceived(followeeID, acceptAllPageSize, "", untilID)
		if err != nil {
			return err
		}
		requests = append(requests, page...)
		if len(page) < acceptAllPageSize {
			break
		}
		untilID = page[len(page)-1].ID
	}
	if len(requests) == 0 {
		return nil
	}

	profile, err := s.userRepo.FindProfileByUserID(followeeID)
	if err != nil {
		if !repository.IsNotFound(err) {
			return err
		}
		profile = nil
	}
	now := time.Now()
	for _, req := range requests {
		follower, err := s.userRepo.FindByID(req.FollowerID)
		if err != nil {
			// 処理中に消えた人のリクエストは飛ばす (本家の follower == null)。
			if repository.IsNotFound(err) {
				continue
			}
			return err
		}
		if RequiresFollowApproval(profile, follower, s.idGen, now) {
			// 期間の条件に当たる人は残す。ただし自分が既にフォローしていて
			// autoAcceptFollowed が有効なら、フォローの時点でも即成立する
			// 相手なので承認する (本家と同じ)。
			if !profile.AutoAcceptFollowed {
				continue
			}
			followed, err := s.followingRepo.Exists(followeeID, follower.ID)
			if err != nil {
				return err
			}
			if !followed {
				continue
			}
		}
		if err := s.AcceptRequest(followeeID, req.FollowerID); err != nil {
			// 処理中に取り消されたリクエストは飛ばす (本家は IdentifiableError
			// 8884c2dd... だけを握る)。ブロックで承認できないものも残りを
			// 止めずに飛ばす — 本家はブロックのときに申請を消すので、ここに
			// 来るのは取り消しに失敗して残った申請だけ (AcceptRequest の多層防御)。
			if errors.Is(err, ErrRequestNotFound) || errors.Is(err, ErrBlocked) || errors.Is(err, ErrBlocking) {
				continue
			}
			return err
		}
	}
	return nil
}
