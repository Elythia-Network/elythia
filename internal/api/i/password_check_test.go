package i

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/passwordguard"
	"github.com/shiroha-a/mk/internal/core/twofactor"
	"github.com/shiroha-a/mk/internal/model"
)

// fakeGuard records reservations. limited makes Begin refuse; err makes the
// store look broken.
type fakeGuard struct {
	mu       sync.Mutex
	limited  bool
	err      error
	begun    []string
	released int
}

type fakeAttempt struct{ g *fakeGuard }

func (a fakeAttempt) Release(context.Context) {
	a.g.mu.Lock()
	defer a.g.mu.Unlock()
	a.g.released++
}

func (g *fakeGuard) Begin(_ context.Context, userID, _ string) (passwordguard.Attempt, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.begun = append(g.begun, userID)
	if g.limited {
		return nil, &passwordguard.LimitedError{RetryAfter: 90*time.Second + time.Millisecond}
	}
	if g.err != nil {
		return nil, g.err
	}
	return fakeAttempt{g: g}, nil
}

// failures is the number of reservations left counted as failures.
func (g *fakeGuard) failures() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.begun) - g.released
}

// passwordEndpoint is one handler that verifies the current password.
type passwordEndpoint struct {
	name string
	call func(h *Handler, password string, user *model.User) int
}

// passwordEndpoints lists every i/* handler that compares the current
// password. 網羅は entitycompat の TestPasswordChecksAreFailureLimited が
// AST から見る。ここは振る舞いを見る。
var passwordEndpoints = []passwordEndpoint{
	{"i/change-password", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.ChangePassword, `{"currentPassword":"`+pw+`","newPassword":"newpass1"}`, u).Code
	}},
	{"i/delete-account", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.DeleteAccount, `{"password":"`+pw+`"}`, u).Code
	}},
	{"i/regenerate-token", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.RegenerateToken, `{"password":"`+pw+`"}`, u).Code
	}},
	{"i/update-email", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.UpdateEmail, `{"password":"`+pw+`","email":null}`, u).Code
	}},
	{"i/move", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.Move, `{"moveToAccount":"@x@remote.example","password":"`+pw+`"}`, u).Code
	}},
	{"i/2fa/register", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.TwoFARegister, `{"password":"`+pw+`"}`, u).Code
	}},
	{"i/2fa/unregister", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.TwoFAUnregister, `{"password":"`+pw+`"}`, u).Code
	}},
	{"i/2fa/register-key", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.TwoFARegisterKey, `{"password":"`+pw+`"}`, u).Code
	}},
	{"i/2fa/key-done", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.TwoFAKeyDone, `{"password":"`+pw+`","credential":{"id":"x"}}`, u).Code
	}},
	{"i/2fa/remove-key", func(h *Handler, pw string, u *model.User) int {
		return postExtra(h.TwoFARemoveKey, `{"password":"`+pw+`","credentialId":"c1"}`, u).Code
	}},
}

func newGuardedHandler(t *testing.T, g passwordguard.Guard) (*Handler, *model.User) {
	t.Helper()
	h, repo := newExtraHandler(t)
	// requireWebAuthn は依存が nil だと照合の手前で 503 を返すので、Redis 無しの
	// サービスを入れておく (照合より先には Redis を使わない)。
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", nil)
	require.NoError(t, err)
	h.SetWebAuthn(svc, newInMemSKRepo())
	h.SetPasswordFailureGuard(g)
	return h, setupUserWithPassword(repo, "u1", "correct")
}

// 照合に失敗した回数だけをアカウント単位で数え、尽きたら照合せずに 429
// (upstream の RATE_LIMIT_EXCEEDED) を返す。正しいパスワードでも 429 になる
// ことで「照合していない」ことを見る。
func TestPasswordEndpoints_RefuseWhenFailureBudgetIsExhausted(t *testing.T) {
	for _, ep := range passwordEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			g := &fakeGuard{limited: true}
			h, u := newGuardedHandler(t, g)
			assert.Equal(t, http.StatusTooManyRequests, ep.call(h, "correct", u),
				"%s が失敗の上限を見ていない", ep.name)
			assert.Equal(t, []string{"u1"}, g.begun)
		})
	}
}

// 間違えたパスワードは失敗として残り、正しいパスワードは数えない。
func TestPasswordEndpoints_CountOnlyIncorrectPasswords(t *testing.T) {
	for _, ep := range passwordEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			g := &fakeGuard{}
			h, u := newGuardedHandler(t, g)
			assert.Equal(t, http.StatusBadRequest, ep.call(h, "wrong", u))
			assert.Equal(t, 1, g.failures(), "%s の照合失敗が数えられていない", ep.name)

			g2 := &fakeGuard{}
			h2, u2 := newGuardedHandler(t, g2)
			ep.call(h2, "correct", u2)
			require.Len(t, g2.begun, 1, "%s が guard を通っていない", ep.name)
			assert.Equal(t, 0, g2.failures(), "%s が正しいパスワードを失敗として数えた", ep.name)
		})
	}
}

func TestBeginPasswordCheck_RetryAfterAndBody(t *testing.T) {
	g := &fakeGuard{limited: true}
	h, repo := newExtraHandler(t)
	h.SetPasswordFailureGuard(g)
	u := setupUserWithPassword(repo, "u1", "correct")

	rec := postExtra(h.RegenerateToken, `{"password":"correct"}`, u)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "91", rec.Header().Get("Retry-After"))
	var body struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "RATE_LIMIT_EXCEEDED", body.Error.Code)
	assert.Equal(t, apierr.UUIDRateLimitExceeded, body.Error.ID)
	// 照合していないので token は変わっていない。
	assert.Equal(t, "tok12345678901234", *repo.Users["u1"].Token)
}

// store の障害で利用者のパスワード操作を止めない (limiter と同じ fail-open)。
func TestBeginPasswordCheck_StoreFailureFailsOpen(t *testing.T) {
	g := &fakeGuard{err: errors.New("dial tcp: connection refused")}
	h, repo := newExtraHandler(t)
	h.SetPasswordFailureGuard(g)
	u := setupUserWithPassword(repo, "u1", "correct")

	assert.Equal(t, http.StatusBadRequest, postExtra(h.RegenerateToken, `{"password":"wrong"}`, u).Code)
	assert.Equal(t, http.StatusNoContent, postExtra(h.RegenerateToken, `{"password":"correct"}`, u).Code)
}

// 検証枠が取れなかった (照合していない) change-password は失敗として数えない。
func TestChangePassword_VerifierBusyDoesNotCountAsFailure(t *testing.T) {
	g := &fakeGuard{}
	h, repo := newExtraHandler(t)
	h.SetPasswordFailureGuard(g)
	user := setupUserWithArgon2Password(repo, "u1", "oldpass")

	rec := postExtraCanceled(h.ChangePassword, `{"currentPassword":"oldpass","newPassword":"newpass"}`, user)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Len(t, g.begun, 1)
	assert.Equal(t, 0, g.failures())
}

// 切断されたリクエストでも予約は取り消しに引きずられず、失敗として数える。
// request の ctx で予約すると Exec が context canceled で失敗し、fail-open で
// 数えられない照合が走っていた。
func TestBeginPasswordCheck_CanceledRequestStillReserves(t *testing.T) {
	g := &ctxRecordingGuard{}
	h, repo := newExtraHandler(t)
	h.SetPasswordFailureGuard(g)
	u := setupUserWithPassword(repo, "u1", "correct")

	postExtraCanceled(h.RegenerateToken, `{"password":"wrong"}`, u)
	require.Len(t, g.errs, 1)
	assert.NoError(t, g.errs[0], "Begin must not see the request cancellation")
}

type ctxRecordingGuard struct{ errs []error }

func (g *ctxRecordingGuard) Begin(ctx context.Context, _, _ string) (passwordguard.Attempt, error) {
	g.errs = append(g.errs, ctx.Err())
	return passwordguard.NoopAttempt(), nil
}
