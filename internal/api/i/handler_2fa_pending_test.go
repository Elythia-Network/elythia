package i

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/pquerna/otp/totp"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPendingGuard wires a miniredis-backed temp secret guard onto h.
func newPendingGuard(t *testing.T, h *Handler) *miniredis.Miniredis {
	t.Helper()
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	h.SetTwoFAPendingGuard(twofactor.NewRedisPendingGuard(client))
	require.True(t, h.HasTwoFAPendingGuard())
	return mr
}

// registerTwoFA runs i/2fa/register and returns the issued secret.
func registerTwoFA(t *testing.T, h *Handler, user *model.User) string {
	t.Helper()
	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp["secret"].(string)
}

// wrongTOTP returns a code that is not accepted for secret around now.
func wrongTOTP(t *testing.T, secret string) string {
	t.Helper()
	valid := map[string]bool{}
	for _, d := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		code, err := totp.GenerateCode(secret, time.Now().Add(d))
		require.NoError(t, err)
		valid[code] = true
	}
	for i := 0; ; i++ {
		code := fmt.Sprintf("%06d", i)
		if !valid[code] {
			return code
		}
	}
}

func doneWith(h *Handler, user *model.User, code string) int {
	return postExtra(h.TwoFADone, `{"token":"`+code+`"}`, user).Code
}

// i/2fa/done discards the temp secret after PendingMaxAttempts wrong codes.
func TestTwoFADone_DiscardsTempSecretAfterTooManyFailures(t *testing.T) {
	h, repo := newExtraHandler(t)
	newPendingGuard(t, h)
	user := setupUserWithPassword(repo, "u1", "pass")
	secret := registerTwoFA(t, h, user)
	wrong := wrongTOTP(t, secret)

	for i := 0; i < twofactor.PendingMaxAttempts; i++ {
		require.Equal(t, http.StatusForbidden, doneWith(h, user, wrong), "attempt %d", i+1)
	}
	require.NotNil(t, repo.Profiles["u1"].TwoFactorTempSecret, "still within the budget")

	assert.Equal(t, http.StatusBadRequest, doneWith(h, user, wrong))
	assert.Nil(t, repo.Profiles["u1"].TwoFactorTempSecret, "the temp secret is discarded")

	valid, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, doneWith(h, user, valid), "a discarded secret cannot be confirmed")
	assert.False(t, repo.Profiles["u1"].TwoFactorEnabled)
}

// A temp secret older than PendingTTL, or one not issued through the guard
// (left over from before it existed), cannot be confirmed.
func TestTwoFADone_RejectsExpiredTempSecret(t *testing.T) {
	cases := map[string]func(t *testing.T, mr *miniredis.Miniredis, h *Handler, user *model.User) string{
		"expired": func(t *testing.T, mr *miniredis.Miniredis, h *Handler, user *model.User) string {
			secret := registerTwoFA(t, h, user)
			mr.FastForward(twofactor.PendingTTL)
			return secret
		},
		"issued without the guard": func(t *testing.T, mr *miniredis.Miniredis, h *Handler, user *model.User) string {
			secret := registerTwoFA(t, h, user)
			mr.FlushAll()
			return secret
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h, repo := newExtraHandler(t)
			mr := newPendingGuard(t, h)
			user := setupUserWithPassword(repo, "u1", "pass")
			secret := setup(t, mr, h, user)

			valid, err := totp.GenerateCode(secret, time.Now())
			require.NoError(t, err)
			assert.Equal(t, http.StatusBadRequest, doneWith(h, user, valid))
			assert.Nil(t, repo.Profiles["u1"].TwoFactorTempSecret)
			assert.False(t, repo.Profiles["u1"].TwoFactorEnabled)
		})
	}
}

// A confirmed temp secret drops its record.
func TestTwoFADone_FinishesPendingRecord(t *testing.T) {
	h, repo := newExtraHandler(t)
	mr := newPendingGuard(t, h)
	user := setupUserWithPassword(repo, "u1", "pass")
	secret := registerTwoFA(t, h, user)
	require.True(t, mr.Exists("mk:2fa:pending:u1"))

	valid, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, doneWith(h, user, valid))
	assert.True(t, repo.Profiles["u1"].TwoFactorEnabled)
	assert.False(t, mr.Exists("mk:2fa:pending:u1"))
}

// When the guard's store fails, register does not hand out a secret and done
// does not check the code.
func TestTwoFA_PendingStoreFailure(t *testing.T) {
	h, repo := newExtraHandler(t)
	mr := newPendingGuard(t, h)
	user := setupUserWithPassword(repo, "u1", "pass")
	secret := registerTwoFA(t, h, user)
	mr.Close()

	valid, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, doneWith(h, user, valid))
	assert.False(t, repo.Profiles["u1"].TwoFactorEnabled)

	repo.Profiles["u1"].TwoFactorTempSecret = nil
	rec := postExtra(h.TwoFARegister, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Nil(t, repo.Profiles["u1"].TwoFactorTempSecret, "no temp secret without its record")
}
