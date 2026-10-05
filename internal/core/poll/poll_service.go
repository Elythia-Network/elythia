// Package poll provides PollService for casting votes on note polls.
package poll

import (
	"context"
	"errors"
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Errors returned by Service.
var (
	// ErrNoteNotFound is returned when the target note does not exist.
	ErrNoteNotFound = errors.New("note not found")
	// ErrNoteNotVisible is returned when the user cannot see the target note.
	ErrNoteNotVisible = errors.New("note not visible to user")
	// ErrNoPoll is returned when the target note has no poll attached.
	ErrNoPoll = errors.New("note has no poll")
	// ErrInvalidChoice is returned when the choice index is out of range.
	ErrInvalidChoice = errors.New("invalid choice")
	// ErrPollExpired is returned when the poll has already expired.
	ErrPollExpired = errors.New("poll has expired")
	// ErrAlreadyVoted is returned when the user has already voted.
	ErrAlreadyVoted = errors.New("already voted")
	// ErrYouHaveBeenBlocked is returned when the note author has blocked the
	// voter, mirroring upstream notes/polls/vote.ts youHaveBeenBlocked.
	ErrYouHaveBeenBlocked = errors.New("blocked by note author")
)

// BlockingChecker reports whether one user has blocked another. 循環依存を避ける
// ため interface で受け取る (実装は core/blocking)。reaction.Service と同じ契約。
type BlockingChecker interface {
	IsBlocked(blockerID, blockeeID string) (bool, error)
}

// NotificationHook is invoked after a vote is recorded so the notification
// subsystem can deliver a "pollVote" notification to the note author.
type NotificationHook interface {
	OnPollVote(notifieeID, notifierID, noteID string, choice int)
}

// NoteEventPublisher emits a `noteUpdated` sub-event (e.g. pollVoted) on the
// note's pubsub topic so subscribers (frontend WebSocket clients viewing the
// note) get real-time updates without reloading. 循環依存回避のため interface
// で受け取り、実装は internal/stream にある (#690 follow-up)。
type NoteEventPublisher interface {
	PublishToNoteStream(noteID string, eventType string, body any)
}

// FederationDeliveryHook delivers poll-related AP activities. Two paths:
//   - OnVote: local user voted on a REMOTE poll → AP Note (vote) → author inbox
//   - OnLocalPollUpdated: local poll's vote count changed (someone voted) →
//     AP Update(Question) → all remote followers, so they refresh counts
//     without re-fetching (#690)。
//
// 循環依存回避のため interface で受け取る (実装は core/federation)。
type FederationDeliveryHook interface {
	OnVote(voter *model.User, target *model.Note, choice int, choiceName string)
	OnLocalPollUpdated(target *model.Note)
}

// Service manages poll voting.
type Service struct {
	noteRepo         repository.NoteRepository
	pollRepo         repository.PollRepository
	pollVoteRepo     repository.PollVoteRepository
	followingRepo    repository.FollowingRepository
	idGen            id.Generator
	notificationHook NotificationHook
	eventPub         NoteEventPublisher
	federationHook   FederationDeliveryHook
	blockingChecker  BlockingChecker
	nowFn            func() time.Time
	// materializer はリレー由来で DB に無い投票対象を昇格させる (#2332)。
	// poll_vote.noteId が note への外部キー。
	materializer NoteMaterializer
}

// NewService constructs a PollService.
// followingRepo は followers 可視性ノートへの投票チェックに使われる (省略可)。
func NewService(
	noteRepo repository.NoteRepository,
	pollRepo repository.PollRepository,
	pollVoteRepo repository.PollVoteRepository,
	followingRepo repository.FollowingRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		noteRepo:      noteRepo,
		pollRepo:      pollRepo,
		pollVoteRepo:  pollVoteRepo,
		followingRepo: followingRepo,
		idGen:         idGen,
		nowFn:         time.Now,
	}
}

// SetNotificationHook attaches a NotificationHook used after a successful vote.
func (s *Service) SetNotificationHook(h NotificationHook) {
	s.notificationHook = h
}

// SetEventPublisher attaches a NoteEventPublisher used to broadcast pollVoted
// events to subscribers of the target note. nil 配線時は publish skip
// (フロントは reload で count を取り直すフォールバックに頼る、#690)。
func (s *Service) SetEventPublisher(p NoteEventPublisher) {
	s.eventPub = p
}

// SetBlockingChecker attaches a BlockingChecker used by Vote to reject votes
// from users the note author has blocked (upstream youHaveBeenBlocked)。nil
// 配線時は block 判定を skip (local-only テスト環境向け)。
func (s *Service) SetBlockingChecker(c BlockingChecker) {
	s.blockingChecker = c
}

// SetFederationHook attaches a FederationDeliveryHook used after a vote is
// recorded on a remote poll, so the choice is propagated to the remote
// author's inbox via AP. nil 配線時は配信 skip — local-only deployments
// で federation infra を持たないテスト環境でも poll が動く。
func (s *Service) SetFederationHook(h FederationDeliveryHook) {
	s.federationHook = h
}

// Vote records a vote for the given user/note/choice.
func (s *Service) Vote(user *model.User, noteID string, choice int) error {
	if user == nil {
		return errors.New("user is required")
	}

	target, err := s.noteRepo.FindByIDWithUser(noteID)
	// **materialize の前に分ける** (#2799)。not-found でない error は
	// materialize しても直らないのに、EnsureUser / EnsureNote が ephemeral
	// store の引きを 1 回余分に走らせる。ヒットすればそのまま WebFinger +
	// actor fetch まで進むので、DB 断中は 1 リクエストごとに outbound HTTP が
	// 出うる。
	if err != nil && !repository.IsNotFound(err) {
		return err
	}
	if s.materializeIfMissing(noteID, err) {
		target, err = s.noteRepo.FindByIDWithUser(noteID)
	}
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。materialize の再試行後も
		// 種別は残るので、ここで分ける。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNoteNotFound
	}
	if !note.CanSeeNote(user, target, s.followingRepo) {
		return ErrNoteNotVisible
	}
	if !target.HasPoll {
		return ErrNoPoll
	}

	// 投票者が note 著者にブロックされている場合は弾く。upstream vote.ts は
	// hasPoll guard の直後・poll lookup の前に checkBlocked(note.userId, me.id)
	// を行う (#1538)。自分の poll への投票は対象外。
	if s.blockingChecker != nil && target.UserID != user.ID {
		if blocked, err := s.blockingChecker.IsBlocked(target.UserID, user.ID); err != nil {
			return err
		} else if blocked {
			return ErrYouHaveBeenBlocked
		}
	}

	p, err := s.pollRepo.FindByNoteID(target.ID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNoPoll
	}

	// #2106 L32: upstream notes/polls/vote.ts は alreadyExpired を invalidChoice より先に
	// 判定する。期限切れ poll への範囲外 choice は ALREADY_EXPIRED を返す。
	if p.ExpiresAt != nil && !s.nowFn().Before(*p.ExpiresAt) {
		return ErrPollExpired
	}
	if choice < 0 || choice >= len(p.Choices) {
		return ErrInvalidChoice
	}

	// 既に同じ choice に投票していたら重複エラー
	if _, err := s.pollVoteRepo.FindByUserAndChoice(user.ID, target.ID, choice); err == nil {
		return ErrAlreadyVoted
	}
	// single モードで既に何かに投票していたらエラー
	if !p.Multiple {
		count, err := s.pollVoteRepo.CountByUserAndNote(user.ID, target.ID)
		if err != nil {
			return err
		}
		if count > 0 {
			return ErrAlreadyVoted
		}
	}

	v := &model.PollVote{
		ID:     s.idGen.Generate(s.nowFn()),
		UserID: user.ID,
		NoteID: target.ID,
		Choice: choice,
	}
	if err := s.pollVoteRepo.Create(v); err != nil {
		return err
	}
	_ = s.pollRepo.IncrementVote(target.ID, choice, 1)

	// pollVoted を note の stream topic に publish して subscribe 中の
	// frontend (note 詳細画面 / timeline) が reload なしで count を更新
	// できるようにする (#690)。Misskey TS の PollService.vote と同じ payload。
	if s.eventPub != nil {
		s.eventPub.PublishToNoteStream(target.ID, "pollVoted", map[string]any{
			"choice": choice,
			"userId": user.ID,
		})
	}

	// target が remote ならば AP Note (with name + inReplyTo) を author の
	// inbox に配信して向こう側でも count に反映させる (#690)。local poll や
	// hook 未配線時は no-op。
	if s.federationHook != nil {
		if target.UserHost != nil && *target.UserHost != "" {
			s.federationHook.OnVote(user, target, choice, p.Choices[choice])
		} else {
			// target は local poll: 投票で更新された count を AP
			// Update(Question) として follower に push する。これがないと
			// 連合先 instance が古い count のままになる (Misskey TS の
			// PollService.deliverQuestionUpdate と同等)。
			s.federationHook.OnLocalPollUpdated(target)
		}
	}

	// Misskey TS には per-vote の通知が無く (frontend の notification type にも
	// 存在しない) ため、mk-go でも notificationHook.OnPollVote は呼び出さない。
	// 過去の配線を残すと frontend が unknown type の notification を空 (= 「無」)
	// で render してしまう (#690 user 報告)。pollEnded (期限切れ) は upstream
	// にあるが本 service の責務外で別 path から発火する想定。
	_ = s.notificationHook

	return nil
}

// NoteMaterializer promotes a relay-delivered note out of the ephemeral store
// into a real database row (#2332)。実装は core/ephemeral.Materializer。
type NoteMaterializer interface {
	EnsureNote(ctx context.Context, noteID string) (*model.Note, error)
}

// SetNoteMaterializer attaches the ephemeral-note materializer. Optional.
func (s *Service) SetNoteMaterializer(m NoteMaterializer) {
	s.materializer = m
}

// materializeIfMissing promotes an ephemeral note only when the database
// lookup already failed.
//
// 通常のノートでは Redis を一切引かないので、ホットパスに追加コストが無い。
func (s *Service) materializeIfMissing(noteID string, lookupErr error) bool {
	if lookupErr == nil || s.materializer == nil {
		return false
	}
	_, err := s.materializer.EnsureNote(context.Background(), noteID)
	return err == nil
}

// HasBlockingChecker reports whether the blocking checker was wired.
//
// 未配線だと**自分をブロックしている**相手のアンケートに投票できる。gate は
// `IsBlocked(target.UserID, user.ID)` (blocker が note 著者)。起動時検査に
// 使う (#2708)。
func (s *Service) HasBlockingChecker() bool { return s.blockingChecker != nil }
