package repository

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
)

type recipientFK struct {
	Name       string `gorm:"column:conname"`
	DeleteRule string `gorm:"column:confdeltype"`
	RefTable   string `gorm:"column:reftable"`
}

// recipientFKs lists the foreign keys of abuse_report_notification_recipient
// in the current schema. pg_constraint は全 schema の行を返すので、
// pg_namespace で今の schema に絞る (#2777)。
func recipientFKs(t *testing.T) []recipientFK {
	t.Helper()
	var rows []recipientFK
	require.NoError(t, testDB.Raw(`
		SELECT c.conname, c.confdeltype::text AS confdeltype, f.relname AS reftable
		FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid
		JOIN pg_class f ON f.oid = c.confrelid
		JOIN pg_namespace n ON n.oid = r.relnamespace
		WHERE n.nspname = current_schema()
		  AND r.relname = 'abuse_report_notification_recipient'
		  AND c.contype = 'f'`).Scan(&rows).Error)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// #3264: 外部キーは本家と同じ 3 本で、どれも ON DELETE CASCADE。利用者や
// System Webhook を消すと、それを宛先にした通知先も消える (SET NULL だと宛先の
// 無い行が残っていた)。
//
// **migration をテストの中で流し直す。** テスト用の schema は実行をまたいで
// 残るので、適用済みの状態を見るだけだと、migration の中身を変えても (台帳が
// 流し直しても、制約が既にあるので何も変わらず) 緑のまま通る。down で 000028
// の状態に戻してから up を流し、その結果を見る。
func TestMigration000106_RecipientFKsCascadeLikeUpstream(t *testing.T) {
	up := migrationSQL(t, "000106_abuse_report_notification_recipient_fk_cascade.up.sql")
	down := migrationSQL(t, "000106_abuse_report_notification_recipient_fk_cascade.down.sql")
	want := []recipientFK{
		{"FK_abuse_report_notification_recipient_systemWebhookId", "c", "system_webhook"},
		{"FK_abuse_report_notification_recipient_userId1", "c", "user"},
		{"FK_abuse_report_notification_recipient_userId2", "c", "user_profile"},
	}

	// mk-go で作った DB: 000028 の SET NULL から付け替える。
	require.NoError(t, testDB.Exec(down).Error)
	// 途中で失敗しても schema を up の状態に戻す。down の ADD CONSTRAINT は
	// 冪等ではないので、戻さないと次の実行の down が "already exists" で落ちる。
	t.Cleanup(func() {
		if err := testDB.Exec(up).Error; err != nil {
			t.Errorf("000106 の up で schema を戻せなかった (次の実行の down が失敗する): %v", err)
		}
	})
	assert.Equal(t, []recipientFK{
		{"abuse_report_notification_recipient_systemWebhookId_fkey", "n", "system_webhook"},
		{"abuse_report_notification_recipient_userId_fkey", "n", "user"},
	}, recipientFKs(t), "down は 000028 の状態に戻す")

	// 000028 の外部キーは通すが、新しい外部キーを満たさない行。残したまま
	// 張ると検証で失敗し、migration が dirty のまま止まる。
	noProfile := insertTestUser(t, "arnfk_np", "arnfknp")
	defer cleanupUser(t, noProfile.ID)
	withProfile := insertTestUser(t, "arnfk_wp", "arnfkwp")
	defer cleanupUser(t, withProfile.ID)
	require.NoError(t, testDB.Create(&model.UserProfile{UserID: withProfile.ID}).Error)
	defer testDB.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, withProfile.ID)
	hook0 := &model.SystemWebhook{ID: "arnfk_wh0", Name: "hook0", URL: "https://hook0.example"}
	require.NoError(t, testDB.Create(hook0).Error)
	defer testDB.Exec(`DELETE FROM "system_webhook" WHERE id = ?`, hook0.ID)
	npID, wpID, wh0ID := noProfile.ID, withProfile.ID, hook0.ID
	legacy := []*model.AbuseReportNotificationRecipient{
		{ID: "arnfk_l_np", Name: "np", Method: "email", IsActive: true, UserID: &npID},
		{ID: "arnfk_l_hook", Name: "hook", Method: "webhook", IsActive: true, UserID: &wpID},
		{ID: "arnfk_l_ok", Name: "ok", Method: "email", IsActive: true, UserID: &wpID},
		{ID: "arnfk_l_mailhook", Name: "mailhook", Method: "email", IsActive: true, UserID: &wpID, SystemWebhookID: &wh0ID},
		{ID: "arnfk_l_hookok", Name: "hookok", Method: "webhook", IsActive: true, SystemWebhookID: &wh0ID},
	}
	for _, r := range legacy {
		require.NoError(t, testDB.Create(r).Error)
		defer cleanupRecipient(t, r.ID)
	}

	require.NoError(t, testDB.Exec(up).Error)
	assert.Equal(t, want, recipientFKs(t))

	userIDOf := func(id string) *string {
		var r model.AbuseReportNotificationRecipient
		require.NoError(t, testDB.First(&r, "id = ?", id).Error)
		return r.UserID
	}
	assert.Nil(t, userIDOf("arnfk_l_np"), "プロフィールの無い利用者を指す userId は空にする")
	assert.Nil(t, userIDOf("arnfk_l_hook"), "webhook 方式の行の userId は空にする")
	require.NotNil(t, userIDOf("arnfk_l_ok"), "正しい行の userId は残す")
	assert.Equal(t, wpID, *userIDOf("arnfk_l_ok"))
	hookOf := func(id string) *string {
		var r model.AbuseReportNotificationRecipient
		require.NoError(t, testDB.First(&r, "id = ?", id).Error)
		return r.SystemWebhookID
	}
	assert.Nil(t, hookOf("arnfk_l_mailhook"), "email 方式の行の systemWebhookId は空にする")
	require.NotNil(t, hookOf("arnfk_l_hookok"), "webhook 方式の行の systemWebhookId は残す")

	// TS 版から引き継いだ DB: 本家の 3 本が既にある。流しても失敗せず、
	// 重複も作らない。
	require.NoError(t, testDB.Exec(up).Error)
	assert.Equal(t, want, recipientFKs(t))

	user := insertTestUser(t, "arnfk_u", "arnfkuser")
	defer cleanupUser(t, user.ID)
	require.NoError(t, testDB.Create(&model.UserProfile{UserID: user.ID}).Error)
	defer testDB.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, user.ID)

	hook := &model.SystemWebhook{ID: "arnfk_wh", Name: "hook", URL: "https://hook.example"}
	require.NoError(t, testDB.Create(hook).Error)
	defer testDB.Exec(`DELETE FROM "system_webhook" WHERE id = ?`, hook.ID)

	userID, hookID := user.ID, hook.ID
	byEmail := &model.AbuseReportNotificationRecipient{ID: "arnfk_email", Name: "mail", Method: "email", IsActive: true, UserID: &userID}
	byHook := &model.AbuseReportNotificationRecipient{ID: "arnfk_hook", Name: "hook", Method: "webhook", IsActive: true, SystemWebhookID: &hookID}
	require.NoError(t, testDB.Create(byEmail).Error)
	defer cleanupRecipient(t, byEmail.ID)
	require.NoError(t, testDB.Create(byHook).Error)
	defer cleanupRecipient(t, byHook.ID)

	exists := func(id string) bool {
		var n int64
		require.NoError(t, testDB.Model(&model.AbuseReportNotificationRecipient{}).Where("id = ?", id).Count(&n).Error)
		return n == 1
	}

	require.NoError(t, testDB.Exec(`DELETE FROM "system_webhook" WHERE id = ?`, hook.ID).Error)
	assert.False(t, exists(byHook.ID), "System Webhook を消すと、それを宛先にした通知先も消える")
	assert.True(t, exists(byEmail.ID))

	require.NoError(t, testDB.Exec(`DELETE FROM "user" WHERE id = ?`, user.ID).Error)
	assert.False(t, exists(byEmail.ID), "利用者を消すと、それを宛先にした通知先も消える")
}
