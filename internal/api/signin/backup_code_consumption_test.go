package signin_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/api/signin"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signinCountingGuard is the in-memory ReplayGuard used by these tests.
//
// ctx を尊重するのは本物 (RedisReplayGuard) に合わせるため。**ここでは
// キャンセル済み ctx を通すテストは持っていない** — 解放が ctx から
// 切り離されていることは `internal/core/twofactor` の
// TestReleaseReservation_DetachesFromRequestContext が見ている。
//
// releases は Release の**呼び出し回数**を数える (ctx エラーでも加算する)。
// i/* の countingReplayGuard と同じ意味にしてある。
type signinCountingGuard struct {
	used     map[string]bool
	releases int
}

func (g *signinCountingGuard) MarkUsed(ctx context.Context, userID, code string) (bool, error) {
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

func (g *signinCountingGuard) Release(ctx context.Context, userID, code string) error {
	g.releases++
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(g.used, userID+":"+code)
	return nil
}

func newBackupCodeHandler(t *testing.T, codes ...string) (*signin.Handler, *testutil.MockUserRepository, *signinCountingGuard) {
	t.Helper()
	h, repo := newTestHandler(t)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", codes)
	guard := &signinCountingGuard{used: map[string]bool{}}
	h.SetTOTPReplayGuard(guard)
	return h, repo, guard
}

// **スナップショットを書き戻さない** (#2862)。配列を丸ごと書き戻すと、別々の
// コードを使う同時実行が互いの消費を打ち消し合う ([c1 c2 c3] から A が c1、
// B が c2 を消すと後勝ちで片方が戻る)。消費は array_remove でなければならない。
func TestBackupCode_ConsumedWithoutSnapshotWriteback(t *testing.T) {
	h, repo, _ := newBackupCodeHandler(t, "abcd1234", "efgh5678")
	repo.UpdateProfileFn = func(_ string, fields map[string]any) error {
		if _, ok := fields["twoFactorBackupSecret"]; ok {
			t.Errorf("twoFactorBackupSecret をスナップショットで書き戻している: %v", fields)
		}
		return nil
	}
	removed := []string{}
	repo.RemoveBackupCodeFn = func(_, code string) error {
		removed = append(removed, code)
		return nil
	}

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"abcd1234"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"abcd1234"}, removed, "使ったコードだけを array_remove で消す")
}

// **同じコードは 1 本しか通らない** (#2862)。予約が無いと、同じスナップショットを
// 読んだ複数のリクエストが全部通る。
//
// **消費を no-op にしてスナップショットを固定すること。** 素直に 2 回叩く形だと、
// 1 本目の `RemoveBackupCode` でコードが消えるので、**予約が無くても 2 本目は
// 落ちる** — 予約を丸ごと外しても緑のままになる (#2852 で同じ罠を踏んで
// `i/twofa_consumption_test.go` に書き残してあったのに、こちらで繰り返した)。
// 消費を止めれば「同じ配列を読んだ 2 本」が再現でき、予約だけが両者を分ける。
func TestBackupCode_ReservedOnceAcrossRequests(t *testing.T) {
	h, repo, _ := newBackupCodeHandler(t, "abcd1234", "efgh5678")
	repo.RemoveBackupCodeFn = func(string, string) error { return nil }

	first := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"abcd1234"}`)
	require.Equal(t, http.StatusOK, first.Code)

	second := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"abcd1234"}`)
	assert.Equal(t, http.StatusForbidden, second.Code, "同じコードで 2 本通っている")

	// 別のコードは影響を受けない。
	other := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"efgh5678"}`)
	assert.Equal(t, http.StatusOK, other.Code)
}

// signin と i/* は同じ keyspace を予約する (#2862)。片方で使ったコードは
// もう片方でも通らない。prefix が食い違うと跨いだ同時実行を絞れない。
func TestBackupCode_SharesGuardKeyspaceWithSelfServiceEndpoints(t *testing.T) {
	h, _, guard := newBackupCodeHandler(t, "abcd1234")

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"abcd1234"}`)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.True(t, guard.used["u1:bc:abcd1234"],
		"twofactor.BackupCodeGuardKey の prefix で予約していない: %v", guard.used)
}

// **消費できなかったら通さない** (#2862)。通すと、DB への書き込みが落ちている
// あいだ同じコードで何度でもセッションを取れる。upstream も同じ 403 を返す。
//
// そのうえで予約は解放する。403 を返す以上は打ち直しが起きるので、残すと
// TTL のあいだ正当な利用者を締め出す。signin は未認証経路なので影響が大きい。
func TestBackupCode_ConsumeFailureIsRejectedAndReleases(t *testing.T) {
	h, repo, guard := newBackupCodeHandler(t, "abcd1234")
	repo.RemoveBackupCodeFn = func(string, string) error { return errors.New("db down") }

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"abcd1234"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "消費に失敗したのにログインさせている")

	assert.Equal(t, 1, guard.releases, "消費に失敗したのに予約を解放していない")
	assert.False(t, guard.used["u1:bc:abcd1234"], "予約が残っている")
}
