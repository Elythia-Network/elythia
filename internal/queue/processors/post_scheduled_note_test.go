package processors

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubDraftRepo / stubUserRepo / stubPublisher are minimal in-memory doubles
// for PostScheduledNoteProcessor tests.

type stubDraftRepo struct {
	drafts map[string]*model.NoteDraft
	// deleted records the (id, userID) pairs that Delete saw.
	deleted []string
	findErr error
	delErr  error
}

func (s *stubDraftRepo) FindByID(id string) (*model.NoteDraft, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	d, ok := s.drafts[id]
	if !ok {
		// **sentinel を返す。** 生の error だと呼び出し側が not-found と障害を
		// 区別できず、#3116 の分岐を試せない。
		return nil, repository.ErrNotFound
	}
	return d, nil
}

func (s *stubDraftRepo) Delete(id, _ string) (int64, error) {
	if s.delErr != nil {
		return 0, s.delErr
	}
	if _, ok := s.drafts[id]; ok {
		delete(s.drafts, id)
		s.deleted = append(s.deleted, id)
		return 1, nil
	}
	return 0, nil
}

type stubUserRepo struct {
	users map[string]*model.User
	err   error
}

func (s *stubUserRepo) FindByID(id string) (*model.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	u, ok := s.users[id]
	if !ok {
		// **sentinel を返す** (#3121)。production の `userRepository.FindByID` は
		// miss で `gorm.ErrRecordNotFound` を返すので、生の error にすると
		// 「retry しても変わらない not-found」と DB 障害を取り違えたまま緑になる。
		return nil, repository.ErrNotFound
	}
	return u, nil
}

type stubPublisher struct {
	calls []note.CreateInput
	err   error
}

func (s *stubPublisher) Create(in note.CreateInput) (*model.Note, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.calls = append(s.calls, in)
	return &model.Note{ID: "n_published", UserID: in.User.ID}, nil
}

func newProcessor(drafts map[string]*model.NoteDraft, users map[string]*model.User) (*PostScheduledNoteProcessor, *stubDraftRepo, *stubPublisher) {
	p, dr, _, pub := newProcessorFull(drafts, users)
	return p, dr, pub
}

// newProcessorFull is newProcessor plus the user repo, for tests that inject a
// lookup failure there (#3121)。
func newProcessorFull(drafts map[string]*model.NoteDraft, users map[string]*model.User) (*PostScheduledNoteProcessor, *stubDraftRepo, *stubUserRepo, *stubPublisher) {
	dr := &stubDraftRepo{drafts: drafts}
	ur := &stubUserRepo{users: users}
	pub := &stubPublisher{}
	return NewPostScheduledNoteProcessor(dr, ur, pub), dr, ur, pub
}

func taskFor(draftID string) driver.Task {
	return queue.NewPostScheduledNoteTask(queue.PostScheduledNotePayload{NoteDraftID: draftID})
}

func TestPostScheduledNote_HappyPath(t *testing.T) {
	now := int64(1234)
	scheduledAt := timeFromMs(now)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID:                  "d1",
			UserID:              "u1",
			Visibility:          "public",
			IsActuallyScheduled: true,
			ScheduledAt:         &scheduledAt,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1", Username: "alice"}}
	p, draftRepo, pub := newProcessor(drafts, users)
	err := p.Handle(context.Background(), taskFor("d1"))
	require.NoError(t, err)
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].User.ID)
	// draft は publish 後に削除される
	assert.NotContains(t, draftRepo.drafts, "d1")
	assert.Contains(t, draftRepo.deleted, "d1")
}

// Draft が削除済み (= user 手動 cancel など) でも error 化せず silent success。
func TestPostScheduledNote_DraftMissing(t *testing.T) {
	p, _, pub := newProcessor(map[string]*model.NoteDraft{}, map[string]*model.User{})
	err := p.Handle(context.Background(), taskFor("ghost"))
	require.NoError(t, err)
	assert.Empty(t, pub.calls, "draft が無いなら publish しない")
}

// **DB 障害は ack しない** (#3116)。ack すると job が成功扱いになって retry
// されず、**予約投稿が黙って消える**。直上の DraftMissing (not-found = ack が
// 正しい) と対にして読むこと — 片方だけだと、両方を同じ側へ倒す実装で緑になる。
func TestPostScheduledNote_DraftLookupFailurePropagates(t *testing.T) {
	p, draftRepo, pub := newProcessor(map[string]*model.NoteDraft{}, map[string]*model.User{})
	boom := errors.New("connection refused")
	draftRepo.findErr = boom

	err := p.Handle(context.Background(), taskFor("d1"))
	require.ErrorIs(t, err, boom, "DB 障害を ack している")
	assert.Empty(t, pub.calls)
}

// **lock は publish の直前でだけ取る** (#3121)。
//
// 手前で取ると、lookup が失敗して retry に回ったとき TTL (5 分) のあいだ
// `ok=false` で silent skip になり、**障害が「already being published」という
// 事実と逆の Info 1 行で成功扱いになる**。初回 backoff (約 60 秒) は TTL より
// 短いので、TTL では救えない。
func TestPostScheduledNote_LockTakenOnlyAtPublish(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	draftsOf := func() map[string]*model.NoteDraft {
		return map[string]*model.NoteDraft{
			"d1": {ID: "d1", UserID: "u1", ScheduledAt: &at, IsActuallyScheduled: true},
		}
	}

	t.Run("draft の lookup が引けないときは取らない", func(t *testing.T) {
		p, draftRepo, pub := newProcessor(draftsOf(), map[string]*model.User{"u1": {ID: "u1"}})
		lock := &stubLock{acquired: true}
		p.SetLock(lock)
		draftRepo.findErr = errors.New("connection refused")

		require.Error(t, p.Handle(context.Background(), taskFor("d1")))
		assert.Equal(t, 0, lock.calls, "lock を握ったまま retry に回している")
		assert.Empty(t, pub.calls)
	})

	t.Run("利用者の lookup が引けないときも取らない", func(t *testing.T) {
		p, _, userRepo, pub := newProcessorFull(draftsOf(), map[string]*model.User{"u1": {ID: "u1"}})
		lock := &stubLock{acquired: true}
		p.SetLock(lock)
		userRepo.err = errors.New("connection refused")

		require.Error(t, p.Handle(context.Background(), taskFor("d1")))
		assert.Equal(t, 0, lock.calls, "lock を握ったまま retry に回している")
		assert.Empty(t, pub.calls)
	})

	t.Run("publish するときだけ取る", func(t *testing.T) {
		p, _, pub := newProcessor(draftsOf(), map[string]*model.User{"u1": {ID: "u1"}})
		lock := &stubLock{acquired: true}
		p.SetLock(lock)

		require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
		assert.Equal(t, 1, lock.calls, "publish の手前で取っていない")
		require.Len(t, pub.calls, 1)
	})

	// **not-found は retry に倒さない** (#3121)。`note_draft.userId` は
	// ON DELETE CASCADE なので普通は起きないが、起きたなら空振りを繰り返すだけ。
	t.Run("利用者が消えていたら ack", func(t *testing.T) {
		p, _, pub := newProcessor(draftsOf(), map[string]*model.User{})
		lock := &stubLock{acquired: true}
		p.SetLock(lock)

		require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
		assert.Equal(t, 0, lock.calls)
		assert.Empty(t, pub.calls)
	})
}

// isActuallyScheduled=false な draft (= 後で unschedule された) は publish せず
// silent skip (draft も残す)。
func TestPostScheduledNote_Unscheduled(t *testing.T) {
	drafts := map[string]*model.NoteDraft{
		"d1": {ID: "d1", UserID: "u1", IsActuallyScheduled: false},
	}
	p, draftRepo, pub := newProcessor(drafts, map[string]*model.User{"u1": {ID: "u1"}})
	err := p.Handle(context.Background(), taskFor("d1"))
	require.NoError(t, err)
	assert.Empty(t, pub.calls)
	assert.Contains(t, draftRepo.drafts, "d1", "unscheduled draft は残す")
}

// #2106 L61: publish 失敗時も upstream 同様 error を返さず job を完了扱いにする
// (retry しない)。draft は残る。
func TestPostScheduledNote_PublishErrorNoRetry(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, draftRepo, pub := newProcessor(drafts, users)
	pub.err = errors.New("publish boom")
	err := p.Handle(context.Background(), taskFor("d1"))
	require.NoError(t, err, "publish 失敗でも retry せず job 完了 (upstream 互換)")
	assert.Contains(t, draftRepo.drafts, "d1", "publish 失敗時は draft 残す")
}

// payload decode 失敗は error 返却 (= driver に retry / dead letter 判断させる)。
func TestPostScheduledNote_PayloadDecodeError(t *testing.T) {
	p, _, _ := newProcessor(map[string]*model.NoteDraft{}, map[string]*model.User{})
	bad := driver.RawTask{TypeName: queue.TaskTypePostScheduledNote, Body: []byte("{not json")}
	err := p.Handle(context.Background(), bad)
	require.Error(t, err)
	// **retry させない** (#3121)。attempts を積んだので、付けないと壊れた
	// payload が backoff 込みで 30 時間ほど再試行され続ける。
	require.ErrorIs(t, err, driver.ErrSkipRetry)
}

// timeFromMs is a tiny helper to convert ms epoch to time.Time.
func timeFromMs(ms int64) time.Time {
	return time.UnixMilli(ms)
}

// --- #1045 Phase 2-A: idempotency lock ---

type stubLock struct {
	acquired bool
	err      error
	calls    int
}

func (s *stubLock) TryAcquire(_ context.Context, _ string) (bool, error) {
	s.calls++
	return s.acquired, s.err
}

func TestPostScheduledNote_LockAcquired_PublishesOnce(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, _, pub := newProcessor(drafts, users)
	lock := &stubLock{acquired: true}
	p.SetLock(lock)

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	assert.Equal(t, 1, lock.calls, "lock は 1 度だけ取得試行")
	require.Len(t, pub.calls, 1, "占有成功なら publish")
}

func TestPostScheduledNote_LockBusy_SkipsPublish(t *testing.T) {
	// **ScheduledAt を持たせる。** 持たせないと lock より手前の gate で ack して
	// しまい、lock の検証が空虚になる (#3121 で lock を publish 直前へ下げた)。
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {ID: "d1", UserID: "u1", IsActuallyScheduled: true, ScheduledAt: &scheduledAt},
	}
	p, _, pub := newProcessor(drafts, map[string]*model.User{"u1": {ID: "u1"}})
	p.SetLock(&stubLock{acquired: false})

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	assert.Empty(t, pub.calls, "lock 占有失敗時は publish しない")
}

func TestPostScheduledNote_LockError_Retries(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {ID: "d1", UserID: "u1", IsActuallyScheduled: true, ScheduledAt: &scheduledAt},
	}
	p, _, _ := newProcessor(drafts, map[string]*model.User{"u1": {ID: "u1"}})
	p.SetLock(&stubLock{err: errors.New("redis down")})

	err := p.Handle(context.Background(), taskFor("d1"))
	require.Error(t, err, "lock backend error は retry 用に伝播")
}

// --- #1045 Phase 2-D: poll restoration ---

// PollExpiredAfter (= 相対 ms) が指定された draft は publish 時に
// `time.Now() + d ms` で expiresAt を計算する (= upstream
// PostScheduledNoteProcessorService と同 logic)。
func TestPostScheduledNote_WithPollExpiredAfter(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	expiredAfter := int64(60 * 60 * 1000) // 1 hour in ms
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
			HasPoll:          true,
			PollChoices:      []string{"a", "b"},
			PollMultiple:     true,
			PollExpiredAfter: &expiredAfter,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, _, pub := newProcessor(drafts, users)

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	require.Len(t, pub.calls, 1)
	require.NotNil(t, pub.calls[0].Poll, "poll が CreateInput に伝播")
	assert.Equal(t, []string{"a", "b"}, pub.calls[0].Poll.Choices)
	assert.True(t, pub.calls[0].Poll.Multiple)
	require.NotNil(t, pub.calls[0].Poll.ExpiresAt)
	// publish 時刻 + 1h が ExpiresAt になっている (= 数秒のズレを許容)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *pub.calls[0].Poll.ExpiresAt, 5*time.Second)
}

// #3443: time.Duration の範囲 (約 9.22e12 ms) を超える PollExpiredAfter でも
// 期限が過去へ巻き戻らず、publish 時刻 + expiredAfter になる。
func TestPostScheduledNote_PollExpiredAfterBeyondDuration(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	expiredAfter := int64(9_300_000_000_000) // 約 295 年。Duration に掛けると溢れる
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
			HasPoll:          true,
			PollChoices:      []string{"a", "b"},
			PollExpiredAfter: &expiredAfter,
		},
	}
	p, _, pub := newProcessor(drafts, map[string]*model.User{"u1": {ID: "u1"}})

	before := time.Now().UnixMilli()
	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	after := time.Now().UnixMilli()
	require.Len(t, pub.calls, 1)
	require.NotNil(t, pub.calls[0].Poll)
	require.NotNil(t, pub.calls[0].Poll.ExpiresAt)
	got := pub.calls[0].Poll.ExpiresAt.UnixMilli()
	assert.GreaterOrEqual(t, got, before+expiredAfter)
	assert.LessOrEqual(t, got, after+expiredAfter)
}

// #3443: 期限が上限 (9999-12-30T23:59:59.999Z) を超える PollExpiredAfter は、publish 失敗
// として scheduledNotePostFailed を通知し、publish しない。
func TestPostScheduledNote_PollExpiredAfterOutOfRange(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	expiredAfter := int64(math.MaxInt64)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
			HasPoll:          true,
			PollChoices:      []string{"a", "b"},
			PollExpiredAfter: &expiredAfter,
		},
	}
	p, dr, pub := newProcessor(drafts, map[string]*model.User{"u1": {ID: "u1"}})
	notif := &stubNotifier{}
	p.SetNotifier(notif)

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")), "publish 失敗は retry しない")
	assert.Empty(t, pub.calls, "範囲外の期限で publish しない")
	require.Len(t, notif.calls, 1)
	assert.Equal(t, notification.TypeScheduledNotePostFailed, notif.calls[0].Type)
	assert.Empty(t, dr.deleted, "失敗した draft は消さない")
}

// #3443: 上限 (9999-12-30T23:59:59.999Z) を超える PollExpiresAt を持つ draft も
// publish せず、scheduledNotePostFailed を通知する。
func TestPostScheduledNote_PollExpiresAtOutOfRange(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	expiresAt := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
			HasPoll:       true,
			PollChoices:   []string{"a", "b"},
			PollExpiresAt: &expiresAt,
		},
	}
	p, dr, pub := newProcessor(drafts, map[string]*model.User{"u1": {ID: "u1"}})
	notif := &stubNotifier{}
	p.SetNotifier(notif)

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	assert.Empty(t, pub.calls, "範囲外の期限で publish しない")
	require.Len(t, notif.calls, 1)
	assert.Equal(t, notification.TypeScheduledNotePostFailed, notif.calls[0].Type)
	assert.Empty(t, dr.deleted, "失敗した draft は消さない")
}

// PollExpiredAfter なし、PollExpiresAt のみ指定された draft は絶対時刻が
// そのまま CreateInput.Poll.ExpiresAt に渡る。
func TestPostScheduledNote_WithPollExpiresAtFallback(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	expiresAt := time.Now().Add(48 * time.Hour)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
			HasPoll:       true,
			PollChoices:   []string{"yes", "no"},
			PollExpiresAt: &expiresAt,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, _, pub := newProcessor(drafts, users)

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	require.Len(t, pub.calls, 1)
	require.NotNil(t, pub.calls[0].Poll)
	require.NotNil(t, pub.calls[0].Poll.ExpiresAt)
	assert.WithinDuration(t, expiresAt, *pub.calls[0].Poll.ExpiresAt, time.Second)
}

// hasPoll=false の draft は CreateInput.Poll=nil で publish。
func TestPostScheduledNote_NoPoll(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
			HasPoll: false,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, _, pub := newProcessor(drafts, users)

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	require.Len(t, pub.calls, 1)
	assert.Nil(t, pub.calls[0].Poll, "hasPoll=false の draft は Poll 設定なし")
}

// --- #1045 Phase 2-B: notification ---

// stubNotifier captures Create calls for assertion. err != nil で
// notification 発火経路が publish 結果を損なわないことを確認する。
type stubNotifier struct {
	calls []notification.CreateInput
	err   error
}

func (s *stubNotifier) Create(_ context.Context, in notification.CreateInput) (*notification.Notification, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.calls = append(s.calls, in)
	return &notification.Notification{ID: "stub-notif", Type: in.Type}, nil
}

// publish 成功時は scheduledNotePosted 通知が 1 度発火し、payload に
// publish した note.ID が乗る。
func TestPostScheduledNote_NotifiesOnPosted(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, _, pub := newProcessor(drafts, users)
	notif := &stubNotifier{}
	p.SetNotifier(notif)

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	require.Len(t, pub.calls, 1, "publish は 1 度")
	require.Len(t, notif.calls, 1, "posted 通知が 1 度発火")
	assert.Equal(t, "u1", notif.calls[0].NotifieeID)
	assert.Equal(t, notification.TypeScheduledNotePosted, notif.calls[0].Type)
	assert.Equal(t, "n_published", notif.calls[0].NoteID,
		"publish した note の ID が通知に乗る")
}

// publish 失敗時は scheduledNotePostFailed 通知が発火し、payload の Extra
// に noteDraftId が乗る。publish error 自体は handler 戻り値で伝播する。
func TestPostScheduledNote_NotifiesOnFailed(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, _, pub := newProcessor(drafts, users)
	pub.err = errors.New("publish boom")
	notif := &stubNotifier{}
	p.SetNotifier(notif)

	err := p.Handle(context.Background(), taskFor("d1"))
	require.NoError(t, err, "#2106 L61: publish 失敗でも error を返さない (retry しない)")
	require.Len(t, notif.calls, 1, "postFailed 通知が 1 度発火")
	assert.Equal(t, "u1", notif.calls[0].NotifieeID)
	assert.Equal(t, notification.TypeScheduledNotePostFailed, notif.calls[0].Type)
	require.NotNil(t, notif.calls[0].Extra)
	assert.Equal(t, "d1", notif.calls[0].Extra["noteDraftId"])
}

// notifier 自体が error を返しても publish 経路 (= handler 戻り値) には
// 影響しない (= best-effort)。
func TestPostScheduledNote_NotifierErrorDoesNotBreakPublish(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, _, pub := newProcessor(drafts, users)
	p.SetNotifier(&stubNotifier{err: errors.New("notif boom")})

	require.NoError(t, p.Handle(context.Background(), taskFor("d1")),
		"notification 失敗は handler error にしない")
	assert.Len(t, pub.calls, 1, "publish 自体は走る")
}

// logNotificationErr は graceful shutdown / deadline 系 error を Debug に
// 格下げし、通常 error は Warn のまま残す。panic しないことを smoke 確認
// (= slog の出力先は別管理なので、ここでは分岐のカバレッジ確保が目的)。
func TestLogNotificationErr_ShutdownErrorsAreDebug(t *testing.T) {
	logNotificationErr("posted", "d1", context.Canceled)
	logNotificationErr("postFailed", "d1", context.DeadlineExceeded)
}

func TestLogNotificationErr_GenericErrorsAreWarn(t *testing.T) {
	logNotificationErr("posted", "d1", errors.New("boom"))
}

// notifier 未配線 (= Phase 2-A 互換挙動) でも publish 経路は通常動作する。
func TestPostScheduledNote_NoNotifierConfigured(t *testing.T) {
	scheduledAt := timeFromMs(1234)
	drafts := map[string]*model.NoteDraft{
		"d1": {
			ID: "d1", UserID: "u1", Visibility: "public",
			IsActuallyScheduled: true, ScheduledAt: &scheduledAt,
		},
	}
	users := map[string]*model.User{"u1": {ID: "u1"}}
	p, _, pub := newProcessor(drafts, users)
	// SetNotifier 呼ばない → nil
	require.NoError(t, p.Handle(context.Background(), taskFor("d1")))
	assert.Len(t, pub.calls, 1)
}
