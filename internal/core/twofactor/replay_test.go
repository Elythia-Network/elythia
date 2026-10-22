package twofactor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pquerna/otp/totp"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMiniRedis(t *testing.T) (*miniredis.Miniredis, goredis.UniversalClient) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestRedisReplayGuard_FirstUseAccepted(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)

	ok, err := g.MarkUsed(context.Background(), "u1", "123456")
	require.NoError(t, err)
	assert.True(t, ok, "first acceptance must succeed")
}

func TestRedisReplayGuard_SecondUseRejected(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	ctx := context.Background()

	ok, err := g.MarkUsed(ctx, "u1", "123456")
	require.NoError(t, err)
	require.True(t, ok)

	ok2, err := g.MarkUsed(ctx, "u1", "123456")
	require.NoError(t, err)
	assert.False(t, ok2, "second acceptance of same (user, code) must be refused")
}

func TestRedisReplayGuard_DifferentUsersIndependent(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	ctx := context.Background()

	ok, _ := g.MarkUsed(ctx, "alice", "123456")
	require.True(t, ok)
	// 別ユーザは同じ 6 桁を独立して使えること
	ok2, err := g.MarkUsed(ctx, "bob", "123456")
	require.NoError(t, err)
	assert.True(t, ok2)
}

func TestRedisReplayGuard_DifferentCodesIndependent(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	ctx := context.Background()

	ok, _ := g.MarkUsed(ctx, "u1", "111111")
	require.True(t, ok)
	ok2, err := g.MarkUsed(ctx, "u1", "222222")
	require.NoError(t, err)
	assert.True(t, ok2)
}

func TestRedisReplayGuard_TTLExpiresEntry(t *testing.T) {
	mr, client := newMiniRedis(t)
	g := &RedisReplayGuard{Client: client, TTL: 1 * time.Second}
	ctx := context.Background()

	ok, _ := g.MarkUsed(ctx, "u1", "123456")
	require.True(t, ok)
	// miniredis の FastForward で TTL を強制経過
	mr.FastForward(2 * time.Second)
	ok2, err := g.MarkUsed(ctx, "u1", "123456")
	require.NoError(t, err)
	assert.True(t, ok2, "entry must expire after TTL so a brand-new code with the same digits is accepted")
}

func TestRedisReplayGuard_CustomKeyPrefix(t *testing.T) {
	mr, client := newMiniRedis(t)
	g := &RedisReplayGuard{Client: client, KeyPrefix: "custom:prefix"}
	ctx := context.Background()

	_, err := g.MarkUsed(ctx, "u1", "777777")
	require.NoError(t, err)
	keys := mr.Keys()
	require.Len(t, keys, 1)
	assert.Equal(t, "custom:prefix:u1:777777", keys[0])
}

func TestNewRedisReplayGuard_NilClient(t *testing.T) {
	g := NewRedisReplayGuard(nil)
	assert.Nil(t, g, "nil client must return nil guard so caller can opt out cleanly")
}

func TestRedisReplayGuard_NilReceiver_FailsOpen(t *testing.T) {
	// nil guard 経由でも panic せず "accept" を返す (= 後方互換 fallback)
	var g *RedisReplayGuard
	ok, err := g.MarkUsed(context.Background(), "u1", "123456")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestValidateWithReplay_NilGuard_BehavesLikeValidate(t *testing.T) {
	secret, _, err := GenerateSecret("Misskey", "user")
	require.NoError(t, err)
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	// nil guard でも valid code は通る
	assert.True(t, ValidateWithReplay(context.Background(), nil, "u1", code, secret, false))
	// invalid code は通らない
	assert.False(t, ValidateWithReplay(context.Background(), nil, "u1", "000000", secret, false))
}

func TestValidateWithReplay_RejectsReplayWithRedisGuard(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)

	secret, _, err := GenerateSecret("Misskey", "user")
	require.NoError(t, err)
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	ctx := context.Background()

	assert.True(t, ValidateWithReplay(ctx, g, "u1", code, secret, false), "first use must succeed")
	assert.False(t, ValidateWithReplay(ctx, g, "u1", code, secret, false), "second use of same code must be refused as replay")
	assert.False(t, ValidateWithReplay(ctx, g, "u1", code, secret, false), "subsequent retries are also refused")
}

func TestValidateWithReplay_InvalidCode_DoesNotConsumeGuardSlot(t *testing.T) {
	mr, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)

	secret, _, err := GenerateSecret("Misskey", "user")
	require.NoError(t, err)
	ctx := context.Background()

	// 構造的に invalid なコードを送っても Redis には何も書かない
	assert.False(t, ValidateWithReplay(ctx, g, "u1", "000000", secret, false))
	assert.Empty(t, mr.Keys(), "failed validation must not consume the replay slot")
}

func TestValidateWithReplay_AnotherUserSameCode_AllowedIndependently(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)

	secret, _, err := GenerateSecret("Misskey", "user")
	require.NoError(t, err)
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	ctx := context.Background()

	assert.True(t, ValidateWithReplay(ctx, g, "alice", code, secret, false))
	// 同じ secret を共有することは通常ないが、guard の隔離性は user 単位
	assert.True(t, ValidateWithReplay(ctx, g, "bob", code, secret, false))
}

// failingGuard returns an error from MarkUsed, simulating Redis outage.
type failingGuard struct{}

func (failingGuard) MarkUsed(_ context.Context, _, _ string) (bool, error) {
	return false, errors.New("redis down")
}

func TestValidateWithReplay_GuardError_FailsOpen(t *testing.T) {
	// Redis 障害時に 2FA を完全閉塞させない fail-open ポリシーを guard する。
	secret, _, err := GenerateSecret("Misskey", "user")
	require.NoError(t, err)
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	assert.True(t, ValidateWithReplay(context.Background(), failingGuard{}, "u1", code, secret, false))
}

// --- test bypass (upstream UserAuthService.validateOtp 互換) ---

// testMode かつ MISSKEY_TEST_CHECK_DUPLICATED_TOTP != "1" のとき、TOTP 検証と
// replay 保護をまとめて素通しにする。本家 e2e は 2FA 登録に使ったコードを直後の
// signin でも使い回すので、これが無いと replay 保護に阻まれて成立しない。
func TestValidateWithReplay_TestModeBypass(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	secret, _, err := GenerateSecret("Misskey", "user")
	require.NoError(t, err)
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	ctx := context.Background()

	t.Setenv("MISSKEY_TEST_CHECK_DUPLICATED_TOTP", "")

	assert.True(t, ValidateWithReplay(ctx, g, "u1", code, secret, true))
	assert.True(t, ValidateWithReplay(ctx, g, "u1", code, secret, true), "bypass 中は同じコードを何度でも受け付ける")
	assert.True(t, ValidateWithReplay(ctx, g, "u1", "000000", secret, true), "bypass 中は不正コードも通る (upstream と同じ)")
}

// 同 env に "1" が立っているときは bypass せず、replay 保護が効く。
func TestValidateWithReplay_TestModeRespectsCheckDuplicatedEnv(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	secret, _, err := GenerateSecret("Misskey", "user")
	require.NoError(t, err)
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	ctx := context.Background()

	t.Setenv("MISSKEY_TEST_CHECK_DUPLICATED_TOTP", "1")

	assert.True(t, ValidateWithReplay(ctx, g, "u1", code, secret, true))
	assert.False(t, ValidateWithReplay(ctx, g, "u1", code, secret, true), "env=1 なら replay を拒否する")
	assert.False(t, ValidateWithReplay(ctx, g, "u1", "000000", secret, true), "env=1 なら不正コードを拒否する")
}

// 本番 (testMode=false) では env の値に関係なく bypass しない。
func TestValidateWithReplay_ProductionIgnoresEnv(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	secret, _, err := GenerateSecret("Misskey", "user")
	require.NoError(t, err)
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	ctx := context.Background()

	t.Setenv("MISSKEY_TEST_CHECK_DUPLICATED_TOTP", "")

	assert.True(t, ValidateWithReplay(ctx, g, "u1", code, secret, false))
	assert.False(t, ValidateWithReplay(ctx, g, "u1", code, secret, false), "本番は常に replay 保護が効く")
	assert.False(t, ValidateWithReplay(ctx, g, "u1", "000000", secret, false), "本番は不正コードを拒否する")
}

// Release は記録を消して同じコードを再び受け付けられるようにする (#2852)。
//
// **必要になる理由。** 2FA を検証したあとに走る password 検証で落ちると、記録が
// 残ったまま操作は失敗する。利用者が同じ (まだ有効な) コードで打ち直すと replay
// として弾かれ、原因から遠い INVALID_TOKEN になる。
func TestRedisReplayGuard_ReleaseAllowsReuse(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	ctx := context.Background()

	ok, err := g.MarkUsed(ctx, "u1", "123456")
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, g.Release(ctx, "u1", "123456"))

	ok2, err := g.MarkUsed(ctx, "u1", "123456")
	require.NoError(t, err)
	assert.True(t, ok2, "Release 後も replay 扱いのままになっている")
}

// Release は他のコード / 他の利用者の記録を消さない。
func TestRedisReplayGuard_ReleaseIsScoped(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	ctx := context.Background()

	mustMark(ctx, t, g, "u1", "111111")
	mustMark(ctx, t, g, "u2", "111111")
	mustMark(ctx, t, g, "u1", "222222")

	require.NoError(t, g.Release(ctx, "u1", "111111"))

	for _, tt := range []struct {
		user, code string
		reusable   bool
	}{
		{user: "u1", code: "111111", reusable: true},
		{user: "u2", code: "111111", reusable: false},
		{user: "u1", code: "222222", reusable: false},
	} {
		ok, err := g.MarkUsed(ctx, tt.user, tt.code)
		require.NoError(t, err)
		assert.Equal(t, tt.reusable, ok, "user=%s code=%s", tt.user, tt.code)
	}
}

// nil guard の Release は落ちない (production 以外では guard を配線しない)。
func TestRedisReplayGuard_ReleaseNilSafe(t *testing.T) {
	var g *RedisReplayGuard
	assert.NoError(t, g.Release(context.Background(), "u1", "123456"))
	assert.NoError(t, (&RedisReplayGuard{}).Release(context.Background(), "u1", "123456"))
}

// releaseReplay は ReplayReleaser を実装しない guard では何もしない。
func TestReleaseReplay_IgnoresGuardsWithoutRelease(t *testing.T) {
	// failingGuard は Release を持たないので type assertion に失敗する。
	releaseReplay(context.Background(), failingGuard{}, "u1", "123456")
	releaseReplay(context.Background(), nil, "u1", "123456")
}

// mustMark records the code and fails the test if it was already present.
func mustMark(ctx context.Context, t *testing.T, g *RedisReplayGuard, user, code string) {
	t.Helper()
	ok, err := g.MarkUsed(ctx, user, code)
	require.NoError(t, err)
	require.True(t, ok, "user=%s code=%s", user, code)
}

// ReserveOnce は同じ key を 2 度取らせない (#2852)。
func TestReserveOnce_SecondAttemptRejected(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisReplayGuard(client)
	ctx := context.Background()

	assert.True(t, ReserveOnce(ctx, g, "u1", "bc:code"), "1 本目が取れない")
	assert.False(t, ReserveOnce(ctx, g, "u1", "bc:code"), "2 本目が通っている")
	assert.True(t, ReserveOnce(ctx, g, "u1", "bc:other"), "別の key まで弾いている")
	assert.True(t, ReserveOnce(ctx, g, "u2", "bc:code"), "別の user まで弾いている")
}

// guard が無い構成では素通しする (fail-open)。
//
// **Redis 障害で 2FA を閉塞させない。** operator が自分の環境から締め出される
// 事故を避けるため、MarkUsed と同じ判断に揃えている。
func TestReserveOnce_FailsOpen(t *testing.T) {
	ctx := context.Background()
	assert.True(t, ReserveOnce(ctx, nil, "u1", "bc:code"), "guard が nil なら素通し")
	assert.True(t, ReserveOnce(ctx, failingGuard{}, "u1", "bc:code"), "guard 障害でも素通し")
}

// Release の失敗は握り潰す (記録が残るだけで安全側)。
func TestReleaseReplay_SwallowsFailure(t *testing.T) {
	releaseReplay(context.Background(), failingReleaser{}, "u1", "code")
}

// failingReleaser always fails both operations.
type failingReleaser struct{ failingGuard }

func (failingReleaser) Release(_ context.Context, _, _ string) error {
	return errors.New("boom")
}

// releaseGuard counts Release calls and honours ctx, mirroring the real
// RedisReplayGuard (whose Del fails with `context canceled`).
type releaseGuard struct {
	used     map[string]bool
	releases int
}

func (g *releaseGuard) MarkUsed(ctx context.Context, userID, code string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	k := userID + ":" + code
	if g.used[k] {
		return false, nil
	}
	g.used[k] = true
	return true, nil
}

func (g *releaseGuard) Release(ctx context.Context, userID, code string) error {
	g.releases++
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(g.used, userID+":"+code)
	return nil
}

// **ReleaseReservation はリクエストの ctx から切り離す** (#2852)。
//
// password 検証が `OutcomeUnavailable` になる理由の 1 つが ctx キャンセルで、
// そのときリクエストの ctx をそのまま使うと `Del` が `context canceled` で
// 落ちて予約が残る。「503 でも焼けない」という狙いが半分しか効かなくなる。
//
// **この検証はここに置くこと** (#2862)。呼び出し側のパッケージに置くと、
// 関数が住んでいるパッケージのカバレッジには計上されず、移動したときに
// 検証ごと迷子になる。
func TestReleaseReservation_DetachesFromRequestContext(t *testing.T) {
	guard := &releaseGuard{used: map[string]bool{"u1:bc:backup1": true}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ReleaseReservation(ctx, guard, "u1", "bc:backup1")

	assert.Equal(t, 1, guard.releases)
	assert.Empty(t, guard.used, "リクエストの ctx で release しているため予約が残っている")
}

// バックアップコードの予約は TOTP の keyspace と衝突しない (#2852 / #2862)。
//
// **signin と i/* が同じ関数を使う。** prefix が食い違うと両者が別々の
// keyspace を予約し、跨いだ同時実行を 1 本に絞れない。
func TestBackupCodeGuardKey_NamespacesAwayFromTOTP(t *testing.T) {
	assert.Equal(t, "bc:123456", BackupCodeGuardKey("123456"))
	assert.NotEqual(t, "123456", BackupCodeGuardKey("123456"),
		"TOTP と同じ keyspace を使うと互いを弾き合う")
}
