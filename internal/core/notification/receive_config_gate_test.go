package notification

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// countingChecker denies the listed types per user and counts the lookups.
type countingChecker struct {
	deny  map[string][]Type
	calls int
}

func (c *countingChecker) PassesReceiveConfig(notifieeID string, in CreateInput) bool {
	c.calls++
	for _, t := range c.deny[notifieeID] {
		if t == in.Type {
			return false
		}
	}
	return true
}

// 実績・投票の終了・アプリ・予約投稿の通知は Service を直接呼ぶ。受け取りの設定で
// 「受け取らない」にした種類は、ここでも作らない (#4058)。
func TestCreate_ReceiveConfigSuppressesDirectCalls(t *testing.T) {
	svc := newTestSvc(t)
	svc.SetReceiveConfigChecker(&countingChecker{deny: map[string][]Type{"u1": {TypeAchievementEarned}}})
	ctx := context.Background()

	n, err := svc.Create(ctx, CreateInput{NotifieeID: "u1", Type: TypeAchievementEarned})
	require.NoError(t, err, "抑制はエラーではない")
	require.Nil(t, n, "受け取らない種類の通知を作った")

	n, err = svc.Create(ctx, CreateInput{NotifieeID: "u1", Type: TypeApp})
	require.NoError(t, err)
	require.NotNil(t, n, "受け取る種類まで抑制した")

	list, err := svc.List(ctx, "u1", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, TypeApp, list[0].Type)
}

// まとめる経路 (通報など) も、ロックを取る前に受け取りの設定で抑制する。
func TestCreateCoalesced_ReceiveConfigSuppresses(t *testing.T) {
	svc := newTestSvc(t)
	checker := &countingChecker{deny: map[string][]Type{"mod": {TypeAbuseReport}}}
	svc.SetReceiveConfigChecker(checker)
	ctx := context.Background()
	// 他者がロックを握っていても、ロックより前で抑制すれば ErrCoalesced にならない
	lock, _ := coalesceKeys(svc, "mod")
	require.NoError(t, testRedis.Client.Set(ctx, lock, "someone-else", 0).Err())

	n, err := svc.CreateCoalesced(ctx, abuseInput("mod", "r1"), Coalesce{})
	require.NoError(t, err, "受け取らない人はロックより前で判定する")
	require.Nil(t, n)
	assert.Equal(t, int64(0), streamLen(t, svc, "mod"))
	assert.Equal(t, 1, checker.calls, "まとめる経路で受信の設定を二重に判定した")
}

// 通すときも判定は 1 回だけ (createWithPush で読み直さない)。
func TestCreateCoalesced_ReceiveConfigCheckedOnce(t *testing.T) {
	svc := newTestSvc(t)
	checker := &countingChecker{}
	svc.SetReceiveConfigChecker(checker)

	n, err := svc.CreateCoalesced(context.Background(), abuseInput("mod", "r1"), Coalesce{})
	require.NoError(t, err)
	require.NotNil(t, n)
	assert.Equal(t, 1, checker.calls)
}

// Hook を通る通知は Hook が先に判定するので、Service は判定役をもう一度呼ばない
// (プロフィールを 2 回読まない)。
func TestHook_ReceiveConfigIsNotCheckedTwice(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	checker := &countingChecker{}
	svc.SetReceiveConfigChecker(checker)

	h.OnLogin("alice")

	list, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, list, 1, "Hook の通知が作られない")
	assert.Equal(t, 0, checker.calls, "Hook が判定した通知を、Service がもう一度判定した")
}

// 組み立てと同じく Hook を判定役にすると、プロフィールの notificationRecieveConfig が
// Service を直接呼ぶ通知にも効く。
func TestService_HookAsReceiveConfigChecker(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	repo.Profiles["alice"] = &model.UserProfile{UserID: "alice", NotificationRecieveConfig: datatypes.JSON(`{"achievementEarned":{"type":"never"}}`)}
	svc.SetReceiveConfigChecker(h)

	n, err := svc.Create(context.Background(), CreateInput{NotifieeID: "alice", Type: TypeAchievementEarned})
	require.NoError(t, err)
	assert.Nil(t, n)
}

// 送り主の居ない通知 (実績・アプリ・投票の終了など) は、関係で決まる設定では抑制しない。
// 本家も関係の判定は notifierId があるときだけ行う。
func TestService_ReceiveConfigRelationIgnoredWithoutNotifier(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	repo.Profiles["alice"] = &model.UserProfile{UserID: "alice", NotificationRecieveConfig: datatypes.JSON(`{"achievementEarned":{"type":"following"},"app":{"type":"follower"}}`)}
	// 誰もフォローしていない状態にして、関係の判定に進めば抑制される形にする
	h.SetNoteNotifyRepos(testutil.NewMockFollowingRepository(), nil)
	svc.SetReceiveConfigChecker(h)

	for _, typ := range []Type{TypeAchievementEarned, TypeApp} {
		n, err := svc.Create(context.Background(), CreateInput{NotifieeID: "alice", Type: typ})
		require.NoError(t, err)
		assert.NotNil(t, n, "%s は送り主が居ないのに、関係の設定で抑制された", typ)
	}
}
