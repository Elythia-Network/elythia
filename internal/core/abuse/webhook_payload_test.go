package abuse_test

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/abuse"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
)

func payloadJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// 本家 SystemWebhookService の AbuseReportPayload と同じキーを持つ。createdAt
// だけは mk-go が前から送っていた追加の項目 (#3260)。
func TestWebhookPayload_HasUpstreamKeys(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	body := payloadJSON(t, abuse.WebhookPayload(cnReport(t, idGen), nil, nil, nil, abuse.UserLookups{}, idGen))

	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{
		"assignee", "assigneeId", "comment", "createdAt", "forwarded", "id",
		"moderationNote", "reporter", "reporterHost", "reporterId", "resolved",
		"resolvedAs", "targetUser", "targetUserHost", "targetUserId",
	}, keys)
	assert.Equal(t, "2026-01-02T03:04:05.000Z", body["createdAt"], "通報の id から作成時刻を出す")
}

// 行の値をそのまま載せる。host は 2 つとも通報の列から出す。
func TestWebhookPayload_CopiesReportColumns(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	reporterHost, targetHost := "remote.example", "other.example"
	assigneeID := "mod1"
	resolvedAs := "accept"
	report := &model.AbuseUserReport{
		ID: idGen.Generate(cnT0), TargetUserID: "bob", ReporterID: "alice",
		AssigneeID: &assigneeID, Resolved: true, Forwarded: true,
		Comment: "spam", ModerationNote: "checked", ResolvedAs: &resolvedAs,
		ReporterHost: &reporterHost, TargetUserHost: &targetHost,
	}
	body := payloadJSON(t, abuse.WebhookPayload(report, nil, nil, nil, abuse.UserLookups{}, idGen))
	assert.Equal(t, "bob", body["targetUserId"])
	assert.Equal(t, "other.example", body["targetUserHost"])
	assert.Equal(t, "alice", body["reporterId"])
	assert.Equal(t, "remote.example", body["reporterHost"])
	assert.Equal(t, "mod1", body["assigneeId"])
	assert.Equal(t, true, body["resolved"])
	assert.Equal(t, true, body["forwarded"])
	assert.Equal(t, "spam", body["comment"])
	assert.Equal(t, "checked", body["moderationNote"])
	assert.Equal(t, "accept", body["resolvedAs"])

	// ローカルの利用者の host は null (列が NULL)。
	local := payloadJSON(t, abuse.WebhookPayload(&model.AbuseUserReport{ID: report.ID}, nil, nil, nil, abuse.UserLookups{}, idGen))
	assert.Contains(t, local, "targetUserHost")
	assert.Nil(t, local["targetUserHost"])
	assert.Contains(t, local, "reporterHost")
	assert.Nil(t, local["reporterHost"])
	assert.Nil(t, local["resolvedAs"])
	assert.Nil(t, local["assigneeId"])
}

// 利用者は UserLite で載せる。本家は packMany(..., { schema: 'UserLite' })。
func TestWebhookPayload_UsersAreUserLite(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	host := "remote.example"
	reporter := &model.User{ID: "alice", Username: "alice", Host: &host, FollowersCount: 3}
	target := &model.User{ID: "bob", Username: "bob"}
	assignee := &model.User{ID: "mod1", Username: "mod"}
	assigneeID := "mod1"
	report := cnReport(t, idGen)
	report.AssigneeID = &assigneeID

	body := payloadJSON(t, abuse.WebhookPayload(report, reporter, target, assignee, abuse.UserLookups{}, idGen))
	for key, u := range map[string]*model.User{"reporter": reporter, "targetUser": target, "assignee": assignee} {
		assert.Equal(t, payloadJSON(t, entity.PackUserLite(u)), body[key], key)
	}
	assert.NotContains(t, body["reporter"], "followersCount", "UserDetailed の項目を載せない")
}

// 本家は assigneeId が無ければ assignee を null にする。
func TestWebhookPayload_NoAssigneeIDMeansNullAssignee(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	body := payloadJSON(t, abuse.WebhookPayload(cnReport(t, idGen), nil, nil, &model.User{ID: "mod1"}, abuse.UserLookups{}, idGen))
	assert.Contains(t, body, "assignee")
	assert.Nil(t, body["assignee"])
}

// idGen が無い、または id から時刻を取れないときは createdAt を空文字にする。
func TestWebhookPayload_CreatedAtWithoutIDGen(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	assert.Equal(t, "", abuse.WebhookPayload(cnReport(t, idGen), nil, nil, nil, abuse.UserLookups{}, nil)["createdAt"])
	assert.Equal(t, "", abuse.WebhookPayload(&model.AbuseUserReport{ID: "not-an-aidx"}, nil, nil, nil, abuse.UserLookups{}, idGen)["createdAt"])
}

type wpInstances struct{ rows []*model.Instance }

func (s wpInstances) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	var out []*model.Instance
	for _, r := range s.rows {
		for _, h := range hosts {
			if r.Host == h {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

type wpEmojis struct{ rows []*model.Emoji }

func (s wpEmojis) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	var out []*model.Emoji
	for _, e := range s.rows {
		if (host == nil) != (e.Host == nil) || (host != nil && *host != *e.Host) {
			continue
		}
		for _, n := range names {
			if e.Name == n {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// リモートの利用者には本家の UserLite と同じく instance を付け、絵文字の URL を
// 解決する。Flag の通報者は必ずリモートなので、ここが抜けると Flag 経由の通報だけ
// 本文の形が変わる (#3260)。
func TestWebhookPayload_ResolvesInstanceAndEmojis(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	host := "remote.example"
	instName := "Remote"
	lookups := abuse.UserLookups{
		Instances: wpInstances{rows: []*model.Instance{{Host: host, Name: &instName}}},
		Emojis: wpEmojis{rows: []*model.Emoji{
			{Name: "blob", Host: &host, PublicURL: "https://remote.example/blob.png"},
			{Name: "blob", PublicURL: "https://local.example/blob.png"},
		}},
	}
	reporter := &model.User{ID: "alice", Username: "alice", Host: &host, Emojis: []string{"blob"}}
	target := &model.User{ID: "bob", Username: "bob", Emojis: []string{"blob"}}

	body := payloadJSON(t, abuse.WebhookPayload(cnReport(t, idGen), reporter, target, nil, lookups, idGen))
	rep := body["reporter"].(map[string]any)
	inst, ok := rep["instance"].(map[string]any)
	require.True(t, ok, "リモートの利用者に instance を付ける: %v", rep["instance"])
	assert.Equal(t, "Remote", inst["name"])
	assert.Equal(t, map[string]any{"blob": "https://remote.example/blob.png"}, rep["emojis"])

	// ローカルの利用者の emojis は本家と同じく解決せず空にする (#3270)。同名の
	// ローカルの絵文字があっても入れない。
	tgt := body["targetUser"].(map[string]any)
	assert.Nil(t, tgt["instance"], "ローカルの利用者に instance は付けない")
	assert.Equal(t, map[string]any{}, tgt["emojis"])
}
