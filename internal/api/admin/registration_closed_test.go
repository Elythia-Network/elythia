package admin_test

import (
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 「受け付けない」は他の受け付け方より優先する (#3186)。
func TestUpdateMeta_RegistrationClosed(t *testing.T) {
	tests := []struct {
		name    string
		current *model.Meta
		body    string
		// 400 を期待するときだけ設定する。
		wantRejected bool

		wantClosed              bool
		wantDisableRegistration bool
		wantApprovalRequired    bool
	}{
		{
			// admin/meta が返す生の disableRegistration も閉じた状態に揃えるため。
			name:                    "誰でも登録できる状態から閉じると招待制の値も立つ",
			current:                 &model.Meta{ID: "x"},
			body:                    `{"registrationClosed":true}`,
			wantClosed:              true,
			wantDisableRegistration: true,
		},
		{
			name:                    "閉じる更新で登録の開放を明示しても閉じる側が勝つ",
			current:                 &model.Meta{ID: "x"},
			body:                    `{"registrationClosed":true,"disableRegistration":false}`,
			wantClosed:              true,
			wantDisableRegistration: true,
		},
		{
			// 照会を開けておくのと、解除したときに戻れるように。
			name:                    "承認制は閉じても外さない",
			current:                 &model.Meta{ID: "x", ApprovalRequiredForSignup: true},
			body:                    `{"registrationClosed":true}`,
			wantClosed:              true,
			wantDisableRegistration: true,
			wantApprovalRequired:    true,
		},
		{
			// normalizeSignupConditions は承認制を入れる更新で登録を開けるが、
			// 閉じている間はそれを閉じ直す。
			name:                    "閉じている間に承認制を入れても登録は開かない",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true},
			body:                    `{"approvalRequiredForSignup":true}`,
			wantClosed:              true,
			wantDisableRegistration: true,
			wantApprovalRequired:    true,
		},
		{
			name:                    "閉じている間に登録を開けようとしても開かない",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true},
			body:                    `{"disableRegistration":false}`,
			wantClosed:              true,
			wantDisableRegistration: true,
		},
		{
			name:                    "閉じている間の無関係な更新でも閉じたまま",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true},
			body:                    `{"name":"x"}`,
			wantClosed:              true,
			wantDisableRegistration: true,
		},
		{
			// 開く側へは倒さない。
			name:                    "解除するだけなら招待制で再開する",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true},
			body:                    `{"registrationClosed":false}`,
			wantDisableRegistration: true,
		},
		{
			// 閉じる更新で立てた disableRegistration が残ると、承認制の入口が
			// approvalOpen で塞がったままになる (#2565)。
			name:                    "承認制が残っていれば解除で登録を開け直す",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true, ApprovalRequiredForSignup: true},
			body:                    `{"registrationClosed":false}`,
			wantDisableRegistration: false,
			wantApprovalRequired:    true,
		},
		{
			// 尊重すると承認制と招待制が重なり、入口が 1 つも無くなる (#2565)。
			name:                    "承認制が残るなら解除で招待制を明示しても開け直す",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true, ApprovalRequiredForSignup: true},
			body:                    `{"registrationClosed":false,"disableRegistration":true}`,
			wantDisableRegistration: false,
			wantApprovalRequired:    true,
		},
		{
			name:                    "解除で承認制を外して招待制を明示すれば尊重する",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true, ApprovalRequiredForSignup: true},
			body:                    `{"registrationClosed":false,"approvalRequiredForSignup":false,"disableRegistration":true}`,
			wantDisableRegistration: true,
		},
		{
			name:                    "解除で誰でも登録できる状態を明示すれば尊重する",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true},
			body:                    `{"registrationClosed":false,"disableRegistration":false}`,
			wantDisableRegistration: false,
		},
		{
			name:                    "解除と同時に承認制を入れれば承認制で開く",
			current:                 &model.Meta{ID: "x", RegistrationClosed: true, DisableRegistration: true},
			body:                    `{"registrationClosed":false,"approvalRequiredForSignup":true}`,
			wantDisableRegistration: false,
			wantApprovalRequired:    true,
		},
		{
			// 元から閉じていないサーバーへの解除は、他の値を触らない。
			name:                    "閉じていない状態で解除しても招待制を開けない",
			current:                 &model.Meta{ID: "x", DisableRegistration: true, ApprovalRequiredForSignup: false},
			body:                    `{"registrationClosed":false}`,
			wantDisableRegistration: true,
		},
		{
			// string を通すと正規化をすり抜けて列だけ書き換わる (#2803 と同じ型)。
			name:         "bool でなければ弾く",
			current:      &model.Meta{ID: "x"},
			body:         `{"registrationClosed":"true"}`,
			wantRejected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			metaRepo.Meta = tt.current
			before := *tt.current

			rec := doPost(h.UpdateMeta, tt.body, adminUser)
			if tt.wantRejected {
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Equal(t, before.RegistrationClosed, metaRepo.Meta.RegistrationClosed)
				assert.Equal(t, before.DisableRegistration, metaRepo.Meta.DisableRegistration)
				return
			}
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.Equal(t, tt.wantClosed, metaRepo.Meta.RegistrationClosed, "registrationClosed")
			assert.Equal(t, tt.wantDisableRegistration, metaRepo.Meta.DisableRegistration, "disableRegistration")
			assert.Equal(t, tt.wantApprovalRequired, metaRepo.Meta.ApprovalRequiredForSignup, "approvalRequiredForSignup")
		})
	}
}

func TestAdminMeta_ExposesRegistrationClosed(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x", RegistrationClosed: true}
	rec := doPost(h.AdminMeta, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"registrationClosed":true`)
}

// 管理者が作るアカウントは「受け付けない」の影響を受けない (#3186)。運営者の逃げ道
// として残す。最初の管理者の作成も含む。
func TestAccountsCreate_UnaffectedByRegistrationClosed(t *testing.T) {
	t.Run("初回セットアップ", func(t *testing.T) {
		h, _, metaRepo, _ := newTestHandler(t)
		metaRepo.Meta.RegistrationClosed = true
		metaRepo.Meta.DisableRegistration = true
		rec := doPost(h.AccountsCreate, `{"username":"admin","password":"pass123"}`, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	t.Run("root による作成", func(t *testing.T) {
		h, _, metaRepo, _ := newTestHandler(t)
		rootID := "root1"
		metaRepo.Meta.RootUserID = &rootID
		metaRepo.Meta.RegistrationClosed = true
		metaRepo.Meta.DisableRegistration = true
		rec := doPost(h.AccountsCreate, `{"username":"user2","password":"pass"}`, &model.User{ID: "root1", Username: "root"})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}
