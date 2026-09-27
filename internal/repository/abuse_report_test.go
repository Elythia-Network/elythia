package repository

import (
	"context"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupAbuseReport(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "abuse_user_report" WHERE id = ?`, id)
}

func cleanupModLog(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "moderation_log" WHERE id = ?`, id)
}

// --- AbuseReportRepository ---

func TestAbuseReportRepository_CRUD(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)

	createTestUser(t, "ar_target")
	createTestUser(t, "ar_reporter")

	r := &model.AbuseUserReport{
		ID:           "ar_1",
		TargetUserID: "ar_target",
		ReporterID:   "ar_reporter",
		Comment:      "spam",
	}
	require.NoError(t, repo.Create(r))
	defer cleanupAbuseReport(t, r.ID)

	found, err := repo.FindByID(r.ID)
	require.NoError(t, err)
	assert.Equal(t, "spam", found.Comment)

	// List
	reports, err := repo.List(nil, "", "", "", "", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, reports)

	// List with resolved filter
	f := false
	reports, err = repo.List(&f, "", "", "", "", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, reports)

	// UpdateFields
	require.NoError(t, repo.UpdateFields(r.ID, map[string]any{"resolved": true}))
	found, _ = repo.FindByID(r.ID)
	assert.True(t, found.Resolved)
}

func TestAbuseReportRepository_List_Pagination(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	createTestUser(t, "ar_p_t")
	createTestUser(t, "ar_p_r")

	// 以下の呼び出しはどれも sinceID を単独指定しないので paginationOrder は
	// DESC を返す。よって "ar_p2" → "ar_p1" の順になる。
	r1 := &model.AbuseUserReport{ID: "ar_p1", TargetUserID: "ar_p_t", ReporterID: "ar_p_r"}
	r2 := &model.AbuseUserReport{ID: "ar_p2", TargetUserID: "ar_p_t", ReporterID: "ar_p_r"}
	require.NoError(t, repo.Create(r1))
	require.NoError(t, repo.Create(r2))
	defer cleanupAbuseReport(t, r1.ID)
	defer cleanupAbuseReport(t, r2.ID)

	// cursor 無しなので paginationOrder は DESC を返す。limit=1 で新しい方
	// (ar_p2) が来る。単なる len 1 ではなく id まで assert することで order の
	// regression を guard する (= 旧 mock 修正時の sort 忘れを catch する系)。
	reports, err := repo.List(nil, "", "", "", "", 1)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "ar_p2", reports[0].ID, "limit=1 with id DESC should return the newer report")

	// untilID cursor: id < ar_p2 を要求すると ar_p1 のみ来る (#1114 core)
	reports, err = repo.List(nil, "", "", "", "ar_p2", 10)
	require.NoError(t, err)
	var sawP1, sawP2 bool
	for _, r := range reports {
		if r.ID == "ar_p1" {
			sawP1 = true
		}
		if r.ID == "ar_p2" {
			sawP2 = true
		}
	}
	assert.True(t, sawP1, "untilID=ar_p2 should include ar_p1")
	assert.False(t, sawP2, "untilID=ar_p2 should exclude ar_p2 itself")

	// limit cap
	reports, err = repo.List(nil, "", "", "", "", 999)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(reports), 100)
}

func TestAbuseReportRepository_FindByID_NotFound(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	_, err := repo.FindByID("ghost")
	assert.Error(t, err)
}

func TestAbuseReportRepository_List_ResolvedTrue(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	tr := true
	reports, err := repo.List(&tr, "", "", "", "", 10)
	require.NoError(t, err)
	_ = reports
}

func TestAbuseReportRepository_List_DefaultLimit(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	reports, err := repo.List(nil, "", "", "", "", 0) // limit=0 → default 10
	require.NoError(t, err)
	_ = reports
}

// #1114 regression guard: reporterOrigin / targetUserOrigin filter が
// 実 DB query (= "Host IS NULL" / "IS NOT NULL") として動く。
func TestAbuseReportRepository_List_OriginFilter(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	createTestUser(t, "ar_o_localtgt")
	createTestUser(t, "ar_o_localrep")
	createTestUser(t, "ar_o_remotetgt")
	createTestUser(t, "ar_o_remoterep")

	remote := "remote.example.com"
	rLocal := &model.AbuseUserReport{
		ID:           "ar_o_local",
		TargetUserID: "ar_o_localtgt",
		ReporterID:   "ar_o_localrep",
		// reporterHost / targetUserHost を未設定 (= local)
	}
	rRemote := &model.AbuseUserReport{
		ID:             "ar_o_remote",
		TargetUserID:   "ar_o_remotetgt",
		ReporterID:     "ar_o_remoterep",
		ReporterHost:   &remote,
		TargetUserHost: &remote,
	}
	require.NoError(t, repo.Create(rLocal))
	require.NoError(t, repo.Create(rRemote))
	defer cleanupAbuseReport(t, rLocal.ID)
	defer cleanupAbuseReport(t, rRemote.ID)

	// reporterOrigin=local → reporterHost IS NULL のみ通る
	reports, err := repo.List(nil, "local", "", "", "", 100)
	require.NoError(t, err)
	var local, remoteOK bool
	for _, r := range reports {
		if r.ID == "ar_o_local" {
			local = true
		}
		if r.ID == "ar_o_remote" {
			remoteOK = true
		}
	}
	assert.True(t, local, "reporterOrigin=local must include local report")
	assert.False(t, remoteOK, "reporterOrigin=local must exclude remote report")

	// reporterOrigin=remote → reporterHost IS NOT NULL のみ通る
	reports, err = repo.List(nil, "remote", "", "", "", 100)
	require.NoError(t, err)
	local, remoteOK = false, false
	for _, r := range reports {
		if r.ID == "ar_o_local" {
			local = true
		}
		if r.ID == "ar_o_remote" {
			remoteOK = true
		}
	}
	assert.False(t, local, "reporterOrigin=remote must exclude local report")
	assert.True(t, remoteOK, "reporterOrigin=remote must include remote report")

	// targetUserOrigin=remote 同様
	reports, err = repo.List(nil, "", "remote", "", "", 100)
	require.NoError(t, err)
	local, remoteOK = false, false
	for _, r := range reports {
		if r.ID == "ar_o_local" {
			local = true
		}
		if r.ID == "ar_o_remote" {
			remoteOK = true
		}
	}
	assert.False(t, local)
	assert.True(t, remoteOK)
}

func TestModerationLogRepository_List_DefaultLimit(t *testing.T) {
	repo := NewModerationLogRepository(testDB)
	logs, err := repo.List(model.ModerationLogFilter{}) // limit=0 → default 10
	require.NoError(t, err)
	_ = logs
}

func TestModerationLogRepository_List_LimitCap(t *testing.T) {
	repo := NewModerationLogRepository(testDB)
	logs, err := repo.List(model.ModerationLogFilter{Limit: 999}) // > 100 → capped
	require.NoError(t, err)
	assert.LessOrEqual(t, len(logs), 100)
}

func TestAbuseReportRepository_List_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewAbuseReportRepository(testDB.WithContext(ctx))
	_, err := repo.List(nil, "", "", "", "", 10)
	assert.Error(t, err)
}

// --- ModerationLogRepository ---

func TestModerationLogRepository_CRUD(t *testing.T) {
	repo := NewModerationLogRepository(testDB)

	createTestUser(t, "ml_user")

	log := &model.ModerationLog{
		ID:     "ml_1",
		UserID: "ml_user",
		Type:   "suspend",
		Info:   []byte(`{"targetUserId":"someone"}`),
	}
	require.NoError(t, repo.Create(log))
	defer cleanupModLog(t, log.ID)

	logs, err := repo.List(model.ModerationLogFilter{Limit: 10})
	require.NoError(t, err)
	assert.NotEmpty(t, logs)
}

func TestModerationLogRepository_CreateMany(t *testing.T) {
	repo := NewModerationLogRepository(testDB)
	createTestUser(t, "ml_batch_user")

	rows := []*model.ModerationLog{
		{ID: "ml_b1", UserID: "ml_batch_user", Type: "deleteCustomEmoji", Info: []byte(`{"emojiId":"e1"}`)},
		{ID: "ml_b2", UserID: "ml_batch_user", Type: "deleteCustomEmoji", Info: []byte(`{"emojiId":"e2"}`)},
		{ID: "ml_b3", UserID: "ml_batch_user", Type: "deleteCustomEmoji", Info: []byte(`{"emojiId":"e3"}`)},
	}
	require.NoError(t, repo.CreateMany(rows))
	defer cleanupModLog(t, "ml_b1")
	defer cleanupModLog(t, "ml_b2")
	defer cleanupModLog(t, "ml_b3")

	logs, err := repo.List(model.ModerationLogFilter{Limit: 100})
	require.NoError(t, err)
	// 3 件以上ある (他のテストの行も拾う可能性あるが少なくとも 3 件は入っている)
	var found int
	for _, l := range logs {
		if l.UserID == "ml_batch_user" {
			found++
		}
	}
	assert.Equal(t, 3, found)
}

func TestModerationLogRepository_CreateMany_EmptyIsNoop(t *testing.T) {
	repo := NewModerationLogRepository(testDB)
	assert.NoError(t, repo.CreateMany(nil))
	assert.NoError(t, repo.CreateMany([]*model.ModerationLog{}))
}

func TestModerationLogRepository_List_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewModerationLogRepository(testDB.WithContext(ctx))
	_, err := repo.List(model.ModerationLogFilter{Limit: 10})
	assert.Error(t, err)
}

func TestModerationLogRepository_List_Pagination(t *testing.T) {
	repo := NewModerationLogRepository(testDB)
	createTestUser(t, "ml_pag")
	log1 := &model.ModerationLog{ID: "ml_p1", UserID: "ml_pag", Type: "a", Info: []byte("{}")}
	log2 := &model.ModerationLog{ID: "ml_p2", UserID: "ml_pag", Type: "b", Info: []byte("{}")}
	require.NoError(t, repo.Create(log1))
	require.NoError(t, repo.Create(log2))
	defer cleanupModLog(t, log1.ID)
	defer cleanupModLog(t, log2.ID)

	logs, err := repo.List(model.ModerationLogFilter{Limit: 1})
	require.NoError(t, err)
	assert.Len(t, logs, 1)

	// #1539: untilId cursor で ml_p2 より前 (= ml_p1) を返す。
	logs, err = repo.List(model.ModerationLogFilter{Limit: 10, UntilID: "ml_p2"})
	require.NoError(t, err)
	require.NotEmpty(t, logs)
	for _, l := range logs {
		assert.Less(t, l.ID, "ml_p2")
	}
}

// #1539: type / userId / search の絞り込み。
func TestModerationLogRepository_List_Filters(t *testing.T) {
	repo := NewModerationLogRepository(testDB)
	createTestUser(t, "ml_flt")
	createTestUser(t, "ml_flt2")
	a := &model.ModerationLog{ID: "ml_f1", UserID: "ml_flt", Type: "suspend", Info: []byte(`{"targetUserId":"victimA"}`)}
	b := &model.ModerationLog{ID: "ml_f2", UserID: "ml_flt2", Type: "deleteNote", Info: []byte(`{"noteId":"n9"}`)}
	require.NoError(t, repo.Create(a))
	require.NoError(t, repo.Create(b))
	defer cleanupModLog(t, a.ID)
	defer cleanupModLog(t, b.ID)

	byType, err := repo.List(model.ModerationLogFilter{Limit: 100, Type: "suspend"})
	require.NoError(t, err)
	for _, l := range byType {
		assert.Equal(t, "suspend", l.Type)
	}
	byUser, err := repo.List(model.ModerationLogFilter{Limit: 100, UserID: "ml_flt2"})
	require.NoError(t, err)
	for _, l := range byUser {
		assert.Equal(t, "ml_flt2", l.UserID)
	}
	bySearch, err := repo.List(model.ModerationLogFilter{Limit: 100, Search: "victimA"})
	require.NoError(t, err)
	require.NotEmpty(t, bySearch)
	for _, l := range bySearch {
		assert.Contains(t, string(l.Info), "victimA")
	}
}

// TestAbuseReportRepository_CountUnresolved checks that only unresolved reports
// are counted (#3200). 他のテストの行が残っていても壊れないよう差分で見る。
func TestAbuseReportRepository_CountUnresolved(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	createTestUser(t, "ar_cu_t")
	createTestUser(t, "ar_cu_r")

	before, err := repo.CountUnresolved()
	require.NoError(t, err)

	open := &model.AbuseUserReport{ID: "ar_cu1", TargetUserID: "ar_cu_t", ReporterID: "ar_cu_r"}
	done := &model.AbuseUserReport{ID: "ar_cu2", TargetUserID: "ar_cu_t", ReporterID: "ar_cu_r", Resolved: true}
	require.NoError(t, repo.Create(open))
	defer cleanupAbuseReport(t, open.ID)
	require.NoError(t, repo.Create(done))
	defer cleanupAbuseReport(t, done.ID)

	after, err := repo.CountUnresolved()
	require.NoError(t, err)
	assert.Equal(t, before+1, after)
}

// TestAbuseReportRepository_HasOlderUnresolvedByPair pins every condition of
// the duplicate check (#3200): same pair, unresolved, strictly older id.
func TestAbuseReportRepository_HasOlderUnresolvedByPair(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	createTestUser(t, "ar_hp_t")
	createTestUser(t, "ar_hp_t2")
	createTestUser(t, "ar_hp_r")
	createTestUser(t, "ar_hp_r2")

	older := &model.AbuseUserReport{ID: "ar_hp2", TargetUserID: "ar_hp_t", ReporterID: "ar_hp_r"}
	require.NoError(t, repo.Create(older))
	defer cleanupAbuseReport(t, older.ID)

	cases := []struct {
		name             string
		reporter, target string
		beforeID         string
		want             bool
	}{
		{"same pair older unresolved", "ar_hp_r", "ar_hp_t", "ar_hp3", true},
		{"the report itself is not older", "ar_hp_r", "ar_hp_t", "ar_hp2", false},
		{"newer report does not count", "ar_hp_r", "ar_hp_t", "ar_hp1", false},
		{"other reporter", "ar_hp_r2", "ar_hp_t", "ar_hp3", false},
		{"other target", "ar_hp_r", "ar_hp_t2", "ar_hp3", false},
		{"NUL never matches", "ar_hp_r\x00", "ar_hp_t", "ar_hp3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.HasOlderUnresolvedByPair(tc.reporter, tc.target, tc.beforeID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	require.NoError(t, repo.UpdateFields(older.ID, map[string]any{"resolved": true}))
	got, err := repo.HasOlderUnresolvedByPair("ar_hp_r", "ar_hp_t", "ar_hp3")
	require.NoError(t, err)
	assert.False(t, got, "resolved reports must not suppress new notifications")
}

// TestAbuseReportRepository_HasOlderUnresolvedFromHost pins every condition of
// the per-host check (#3200).
func TestAbuseReportRepository_HasOlderUnresolvedFromHost(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	createTestUser(t, "ar_hh_t")
	createTestUser(t, "ar_hh_r")

	host := "flood.example"
	older := &model.AbuseUserReport{ID: "ar_hh2", TargetUserID: "ar_hh_t", ReporterID: "ar_hh_r", ReporterHost: &host}
	require.NoError(t, repo.Create(older))
	defer cleanupAbuseReport(t, older.ID)

	cases := []struct {
		name     string
		host     string
		beforeID string
		want     bool
	}{
		{"same host older unresolved", host, "ar_hh3", true},
		{"the report itself is not older", host, "ar_hh2", false},
		{"newer report does not count", host, "ar_hh1", false},
		{"other host", "other.example", "ar_hh3", false},
		{"NUL never matches", host + "\x00", "ar_hh3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.HasOlderUnresolvedFromHost(tc.host, tc.beforeID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	require.NoError(t, repo.UpdateFields(older.ID, map[string]any{"resolved": true}))
	got, err := repo.HasOlderUnresolvedFromHost(host, "ar_hh3")
	require.NoError(t, err)
	assert.False(t, got, "resolved reports must not suppress new notifications")
}
