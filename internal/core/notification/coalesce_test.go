package notification

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func abuseInput(notifiee, reportID string) CreateInput {
	return CreateInput{
		NotifieeID: notifiee,
		NotifierID: "reporter",
		Type:       TypeAbuseReport,
		Extra:      map[string]any{"reportId": reportID},
	}
}

func abuseEntries(t *testing.T, svc *Service, userID string) []string {
	t.Helper()
	es, err := svc.entriesOfType(context.Background(), userID, TypeAbuseReport)
	require.NoError(t, err)
	var ids []string
	for _, e := range es {
		ids = append(ids, e.n.Extra["reportId"].(string))
	}
	return ids
}

func streamLen(t *testing.T, svc *Service, userID string) int64 {
	t.Helper()
	n, err := testRedis.Client.XLen(context.Background(), svc.streamKey(userID)).Result()
	require.NoError(t, err)
	return n
}

func coalesceKeys(svc *Service, userID string) (lock, window string) {
	suffix := string(TypeAbuseReport) + ":" + userID
	return svc.keyPrefix + "notificationCoalesceLock:" + suffix, svc.keyPrefix + "notificationCoalesceWindow:" + suffix
}

// TestCreateCoalesced_FoldsWithinWindow: 窓の中では作らない (ポップアップが鳴り続けない)。
func TestCreateCoalesced_FoldsWithinWindow(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	opt := Coalesce{Window: time.Hour}

	n, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), opt)
	require.NoError(t, err)
	require.NotNil(t, n)
	for i := 0; i < 5; i++ {
		_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "rX"), opt)
		assert.ErrorIs(t, err, ErrCoalesced)
	}
	assert.Equal(t, []string{"r1"}, abuseEntries(t, svc, "mod"))
}

// TestCreateCoalesced_IgnoresReadMarker: 既読位置を判定に使わない。frontend は通知欄を
// 開いている間、届くたびに既読にするので、既読を根拠にすると連打が素通りする。
func TestCreateCoalesced_IgnoresReadMarker(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	opt := Coalesce{Window: time.Hour}

	_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), opt)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		require.NoError(t, svc.MarkAllAsRead(ctx, "mod", false))
		_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "rX"), opt)
		assert.ErrorIs(t, err, ErrCoalesced)
	}
	assert.EqualValues(t, 1, streamLen(t, svc, "mod"))
}

// TestCreateCoalesced_ReplacesAfterWindow: 窓が明けたら既存を消して作り直す。
// 一覧に残るのは常に 1 件なので、他の通知が押し出されない。
func TestCreateCoalesced_ReplacesAfterWindow(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{NotifieeID: "mod", NotifierID: "x", Type: TypeFollow})
	require.NoError(t, err)
	for _, id := range []string{"r1", "r2", "r3"} {
		n, err := svc.CreateCoalesced(ctx, abuseInput("mod", id), Coalesce{})
		require.NoError(t, err)
		require.NotNil(t, n)
	}
	assert.Equal(t, []string{"r3"}, abuseEntries(t, svc, "mod"))
	assert.EqualValues(t, 2, streamLen(t, svc, "mod"), "other notifications stay")

	_, window := coalesceKeys(svc, "mod")
	_, err = svc.CreateCoalesced(ctx, abuseInput("mod", "r4"), Coalesce{Window: time.Hour})
	require.NoError(t, err)
	ttl := testRedis.Client.TTL(ctx, window).Val()
	assert.Greater(t, ttl, 59*time.Minute, "a creation opens the window")
	require.NoError(t, testRedis.Client.Del(ctx, window).Err())
	_, err = svc.CreateCoalesced(ctx, abuseInput("mod", "r5"), Coalesce{Window: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, []string{"r5"}, abuseEntries(t, svc, "mod"))
}

// TestCreateCoalesced_QuietKeepsExisting: Quiet は窓の外でも既存を残す。
func TestCreateCoalesced_QuietKeepsExisting(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), Coalesce{})
	require.NoError(t, err)
	_, err = svc.CreateCoalesced(ctx, abuseInput("mod", "r2"), Coalesce{Quiet: true})
	assert.ErrorIs(t, err, ErrCoalesced)
	assert.Equal(t, []string{"r1"}, abuseEntries(t, svc, "mod"))
}

// TestCreateCoalesced_CreatesWhenNothingAlive: 既存が無ければ Quiet でも窓の中でも作る。
// 見送ると、見送った通報が見える場所がどこにも無くなる (押し出された / 参照先が消えた)。
func TestCreateCoalesced_CreatesWhenNothingAlive(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	opt := Coalesce{Window: time.Hour, Quiet: true, Alive: func(n *Notification) bool { return n.Extra["reportId"] != "gone" }}

	n, err := svc.CreateCoalesced(ctx, abuseInput("mod", "gone"), opt)
	require.NoError(t, err, "no existing entry: Quiet must not suppress")
	require.NotNil(t, n)

	n, err = svc.CreateCoalesced(ctx, abuseInput("mod", "r2"), opt)
	require.NoError(t, err, "a dead entry must not suppress, even within the window")
	require.NotNil(t, n)
	assert.Equal(t, []string{"r2"}, abuseEntries(t, svc, "mod"), "dead entries are cleaned up")

	_, err = svc.CreateCoalesced(ctx, abuseInput("mod", "r3"), opt)
	assert.ErrorIs(t, err, ErrCoalesced, "the live r2 suppresses r3")
}

func TestCreateCoalesced_OtherTypesAndRecipientsDoNotCount(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	opt := Coalesce{Window: time.Hour, Quiet: true}

	_, err := svc.Create(ctx, CreateInput{NotifieeID: "mod", NotifierID: "x", Type: TypeFollow})
	require.NoError(t, err)
	_, err = svc.CreateCoalesced(ctx, abuseInput("other", "r1"), opt)
	require.NoError(t, err)

	n, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), opt)
	require.NoError(t, err)
	require.NotNil(t, n)
}

// TestCreateCoalesced_ConcurrentCallsCreateOne: 並列に投げても 1 件しか作らない。
func TestCreateCoalesced_ConcurrentCallsCreateOne(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r"), Coalesce{Window: time.Hour})
			if err != nil {
				assert.ErrorIs(t, err, ErrCoalesced)
			}
		}()
	}
	close(start)
	wg.Wait()
	assert.EqualValues(t, 1, streamLen(t, svc, "mod"))
}

func TestCreateCoalesced_HeldLockSkipsAndIsNotReleased(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	lock, _ := coalesceKeys(svc, "mod")
	require.NoError(t, testRedis.Client.Set(ctx, lock, "someone-else", 0).Err())

	_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), Coalesce{})
	assert.ErrorIs(t, err, ErrCoalesced)
	assert.EqualValues(t, 0, streamLen(t, svc, "mod"))
	assert.Equal(t, "someone-else", testRedis.Client.Get(ctx, lock).Val(), "never release a lock we do not own")
}

// TestCreateCoalesced_DoesNotReleaseLockTakenOver: TTL が切れて別の呼び出しが取り直した
// ロックを消さない。消すと 3 つ目の呼び出しが並走して重複して作る。
func TestCreateCoalesced_DoesNotReleaseLockTakenOver(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	lock, _ := coalesceKeys(svc, "mod")

	_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), Coalesce{})
	require.NoError(t, err)
	// Alive はロックを握っている間に呼ばれる。そこで持ち主を入れ替える。
	takeOver := func(*Notification) bool {
		require.NoError(t, testRedis.Client.Set(ctx, lock, "someone-else", 0).Err())
		return false
	}
	_, err = svc.CreateCoalesced(ctx, abuseInput("mod", "r2"), Coalesce{Alive: takeOver})
	require.NoError(t, err)
	assert.Equal(t, "someone-else", testRedis.Client.Get(ctx, lock).Val())
}

func TestCreateCoalesced_ReleasesOwnLock(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), Coalesce{})
	require.NoError(t, err)
	lock, _ := coalesceKeys(svc, "mod")
	assert.EqualValues(t, 0, testRedis.Client.Exists(ctx, lock).Val())
}

// TestCreateCoalesced_NonCreatingCallsTakeNoLock: 作らない呼び出しがロックを取ると、
// 同時に来た別の通報がロック待ちで見送られ、誰にも届かない。ロックを他者が
// 握った状態で呼び、ロックに触れる前に判定していれば ErrCoalesced にならない。
func TestCreateCoalesced_NonCreatingCallsTakeNoLock(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	lock, _ := coalesceKeys(svc, "mod")
	require.NoError(t, testRedis.Client.Set(ctx, lock, "someone-else", 0).Err())

	_, err := svc.CreateCoalesced(ctx, CreateInput{NotifieeID: "mod", NotifierID: "mod", Type: TypeAbuseReport}, Coalesce{})
	assert.ErrorIs(t, err, ErrSelfNotification)

	svc.SetPolicyResolver(&stubPolicyResolver{optOut: map[string][]string{"mod": {string(TypeAbuseReport)}}})
	n, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), Coalesce{})
	assert.NoError(t, err, "an opted-out recipient is decided before the lock")
	assert.Nil(t, n)
}

func TestCreateCoalesced_Errors(t *testing.T) {
	svc := newTestSvc(t)
	_, err := svc.CreateCoalesced(context.Background(), CreateInput{Type: TypeAbuseReport}, Coalesce{})
	assert.Error(t, err)

	broken := NewService(closedClient(t), idGen, "")
	_, err = broken.CreateCoalesced(context.Background(), abuseInput("mod", "r1"), Coalesce{})
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrCoalesced)
}

// flippingPolicy passes the first n queries and opts out of every type afterwards.
type flippingPolicy struct{ passes int }

func (f *flippingPolicy) OptOutNotificationTypes(string) []string {
	if f.passes > 0 {
		f.passes--
		return nil
	}
	return []string{string(TypeAbuseReport)}
}

// TestCreateCoalesced_FailedCreateKeepsExisting: 作成できなかったとき既存を消さない。
// 先に消すと、早期判定の後に opt-out へ変わった / XADD が失敗したときに何も残らない。
func TestCreateCoalesced_FailedCreateKeepsExisting(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), Coalesce{})
	require.NoError(t, err)

	// ロック前の判定だけ通し、createWithPush の中の判定で opt-out にする。
	svc.SetPolicyResolver(&flippingPolicy{passes: 1})
	n, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r2"), Coalesce{})
	require.NoError(t, err)
	require.Nil(t, n)
	assert.Equal(t, []string{"r1"}, abuseEntries(t, svc, "mod"))
	_, window := coalesceKeys(svc, "mod")
	assert.EqualValues(t, 0, testRedis.Client.Exists(ctx, window).Val(), "no window without a creation")
}
