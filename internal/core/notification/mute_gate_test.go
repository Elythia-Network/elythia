package notification

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pairMuteChecker reports the listed muter -> mutee pairs as muted and counts
// the lookups.
type pairMuteChecker struct {
	muted map[[2]string]bool
	err   error
	calls int
}

func (c *pairMuteChecker) IsMuted(muterID, muteeID string) (bool, error) {
	c.calls++
	if c.err != nil {
		return false, c.err
	}
	return c.muted[[2]string{muterID, muteeID}], nil
}

func muting(muter, mutee string) *pairMuteChecker {
	return &pairMuteChecker{muted: map[[2]string]bool{{muter, mutee}: true}}
}

// 絵文字の申請を受けた通知は Service を直接呼ぶ。審査する人が申請者をミュートしていれば
// 作らない (#4065)。ミュートしていない相手からの申請は届く。
func TestCreate_MuteSuppressesDirectCalls(t *testing.T) {
	svc := newTestSvc(t)
	svc.SetMuteChecker(muting("reviewer", "muted"))
	ctx := context.Background()

	n, err := svc.Create(ctx, CreateInput{NotifieeID: "reviewer", NotifierID: "muted", Type: TypeEmojiApplicationReceived})
	require.NoError(t, err, "抑制はエラーではない")
	require.Nil(t, n, "ミュートしている相手からの申請の通知を作った")

	n, err = svc.Create(ctx, CreateInput{NotifieeID: "reviewer", NotifierID: "other", Type: TypeEmojiApplicationReceived})
	require.NoError(t, err)
	require.NotNil(t, n, "ミュートしていない相手からの申請まで抑制した")
}

// 通報はミュートしていても届ける (#4065 で決めた方針)。判定役も呼ばない。
func TestCreateCoalesced_AbuseReportIgnoresMute(t *testing.T) {
	svc := newTestSvc(t)
	checker := muting("mod", "reporter")
	svc.SetMuteChecker(checker)

	n, err := svc.CreateCoalesced(context.Background(), abuseInput("mod", "r1"), Coalesce{})
	require.NoError(t, err)
	require.NotNil(t, n, "ミュートしている通報者からの通報の通知が届かない")
	assert.Equal(t, 0, checker.calls, "通報でミュートを判定した")
}

// まとめる経路でも、通報以外の種類はロックを取る前にミュートで抑制し、判定は 1 回だけ。
func TestCreateCoalesced_MuteSuppressesOtherTypes(t *testing.T) {
	svc := newTestSvc(t)
	checker := muting("reviewer", "muted")
	svc.SetMuteChecker(checker)
	ctx := context.Background()

	n, err := svc.CreateCoalesced(ctx, CreateInput{NotifieeID: "reviewer", NotifierID: "muted", Type: TypeEmojiApplicationReceived}, Coalesce{})
	require.NoError(t, err)
	require.Nil(t, n, "まとめる経路でミュートしている相手の通知を作った")

	n, err = svc.CreateCoalesced(ctx, CreateInput{NotifieeID: "reviewer", NotifierID: "other", Type: TypeEmojiApplicationReceived}, Coalesce{})
	require.NoError(t, err)
	require.NotNil(t, n)
	assert.Equal(t, 2, checker.calls, "まとめる経路でミュートを二重に判定した")
}

// まとめる経路は、ミュートの判定もロックを取る前に行う。他者がロックを握っていても、
// ミュートで抑制する通知は ErrCoalesced にならない。
func TestCreateCoalesced_MuteCheckedBeforeLock(t *testing.T) {
	svc := newTestSvc(t)
	svc.SetMuteChecker(muting("reviewer", "muted"))
	ctx := context.Background()
	// coalesceKeys は通報の鍵を返すので、申請の種類の鍵をここで作る
	lock := svc.keyPrefix + "notificationCoalesceLock:" + string(TypeEmojiApplicationReceived) + ":reviewer"
	require.NoError(t, testRedis.Client.Set(ctx, lock, "someone-else", 0).Err())

	n, err := svc.CreateCoalesced(ctx, CreateInput{NotifieeID: "reviewer", NotifierID: "muted", Type: TypeEmojiApplicationReceived}, Coalesce{})
	require.NoError(t, err, "ミュートの判定がロックより後にある")
	require.Nil(t, n)
}

// 送り主の居ない通知 (実績など) はミュートと関係が無いので、判定役を呼ばずに作る。
// 判定に失敗したときは、Hook と同じく許可側に倒す。
func TestService_MuteGateFailsOpen(t *testing.T) {
	svc := newTestSvc(t)
	checker := &pairMuteChecker{err: errors.New("db down")}
	svc.SetMuteChecker(checker)
	ctx := context.Background()

	n, err := svc.Create(ctx, CreateInput{NotifieeID: "u1", Type: TypeAchievementEarned})
	require.NoError(t, err)
	require.NotNil(t, n)
	assert.Equal(t, 0, checker.calls, "送り主の居ない通知でミュートを判定した")

	n, err = svc.Create(ctx, CreateInput{NotifieeID: "u1", NotifierID: "x", Type: TypeEmojiApplicationReceived})
	require.NoError(t, err)
	assert.NotNil(t, n, "判定の失敗で通知を落とした")
}

// Hook を通る通知は Hook がミュートを判定するので、Service は判定役をもう一度呼ばない。
func TestHook_MuteIsNotCheckedTwice(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")
	hookChecker := &pairMuteChecker{}
	svcChecker := &pairMuteChecker{}
	h.SetMuteChecker(hookChecker)
	svc.SetMuteChecker(svcChecker)

	h.OnFollowed("bob", "alice")

	list, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, list, 1, "Hook の通知が作られない")
	assert.Equal(t, 1, hookChecker.calls, "Hook がミュートを判定していない (前提)")
	assert.Equal(t, 0, svcChecker.calls, "Hook が判定した通知を、Service がもう一度判定した")
}
