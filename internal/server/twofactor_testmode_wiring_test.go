package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// **設定のtestModeが、TOTPを検証する2つのhandler(signinとi)へ渡ること(#3780)。**
//
// 本家のe2eは、2FAの登録に使ったコードをsigninでも使い回すので、testModeの
// ときの素通し(本家UserAuthService.validateOtp)に頼る。以前はtwofactorの
// パッケージの変数をrouterが書き換えていた。handlerへ渡す形にしたので、1つ
// 渡し忘れるとその経路だけe2eが落ちる。testModeの有無で2回組み立てて、正しく
// ないコードが通るかどうかを両方の経路で見る。
func TestNewServer_WiresTestModeToTheTOTPCheck(t *testing.T) {
	for _, tt := range []struct {
		name     string
		testMode bool
	}{
		{name: "test mode bypasses the TOTP check", testMode: true},
		{name: "production validates the TOTP code", testMode: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MISSKEY_TEST_CHECK_DUPLICATED_TOTP", "")
			srv := newServerWithConfig(t, func(cfg *config.Config) { cfg.TestMode = tt.testMode })
			db := serverIntegrationDB
			cleanupPluginAccountUsers(t, db, "tm_user")
			createLocalUser(t, db, "tm-user", "tm_user", "tmusertokenxx780", "tm-pass", false)
			secret := "JBSWY3DPEHPK3PXP"
			require.NoError(t, db.Model(&model.UserProfile{}).Where(`"userId" = ?`, "tm-user").
				Updates(map[string]any{"twoFactorEnabled": true, "twoFactorSecret": secret}).Error)

			rec := postJSON(t, srv, "/api/signin-flow", "",
				map[string]any{"username": "tm_user", "password": "tm-pass", "token": "000000"})
			signedIn := rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"i"`)
			assert.Equal(t, tt.testMode, signedIn, "signin-flow: %d %s", rec.Code, rec.Body.String())

			rec = postJSON(t, srv, "/api/i/change-password", "tmusertokenxx780",
				map[string]any{"currentPassword": "tm-pass", "newPassword": "tm-pass-2", "token": "000000"})
			changed := rec.Code == http.StatusNoContent
			assert.Equal(t, tt.testMode, changed, "i/change-password: %d %s", rec.Code, rec.Body.String())
		})
	}
}
