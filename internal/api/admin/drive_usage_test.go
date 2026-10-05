package admin_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/driveusage"
	"github.com/elythia-network/elythia/internal/repository"
)

// stubDriveUsage records the forceRecalc it was asked for.
type stubDriveUsage struct {
	gotForce bool
	calls    int
	res      *driveusage.Result
	err      error
}

func (s *stubDriveUsage) Breakdown(forceRecalc bool) (*driveusage.Result, error) {
	s.gotForce = forceRecalc
	s.calls++
	return s.res, s.err
}

type usageBucketJSON struct {
	Count     int64 `json:"count"`
	Size      int64 `json:"size"`
	LinkCount int64 `json:"linkCount"`
}

type usageResponseJSON struct {
	CalculatedAt    string `json:"calculatedAt"`
	ElapsedMs       int64  `json:"elapsedMs"`
	Cached          bool   `json:"cached"`
	CacheTTLSeconds int    `json:"cacheTtlSeconds"`
	TopLimit        int    `json:"topLimit"`
	Source          string `json:"source"`
	Total           usageBucketJSON
	Local           usageBucketJSON
	Remote          usageBucketJSON
	ByKind          []struct {
		Kind   string `json:"kind"`
		Origin string `json:"origin"`
		usageBucketJSON
	} `json:"byKind"`
	ByHost []struct {
		Host string `json:"host"`
		usageBucketJSON
	} `json:"byHost"`
	ByUser []struct {
		UserID   string `json:"userId"`
		Username string `json:"username"`
		usageBucketJSON
	} `json:"byUser"`
}

func decodeUsage(t *testing.T, body []byte) usageResponseJSON {
	t.Helper()
	var out usageResponseJSON
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// sampleUsage builds a snapshot with one cell per origin plus rankings.
//
// **時刻は UTC 以外で作る。** UTC で作ると `.UTC()` を落としても同じ文字列に
// なるので、書式の検証が空振りする (サーバーの TZ が UTC でなければ、末尾 `Z`
// を付けたままローカル時刻を出す実バグになる)。
func sampleUsage() *driveusage.Result {
	jst := time.FixedZone("JST", 9*60*60)
	return &driveusage.Result{
		CalculatedAt: time.Date(2026, 9, 19, 12, 4, 5, 600*int(time.Millisecond), jst),
		Elapsed:      1234 * time.Millisecond,
		TopN:         30,
		TTL:          5 * time.Minute,
		Breakdown: &repository.DriveUsageBreakdown{
			ByKind: []repository.DriveUsageKindRow{
				{
					Kind: repository.DriveUsageKindAttachment, Origin: repository.DriveUsageOriginLocal,
					DriveUsageBucket: repository.DriveUsageBucket{Count: 3, Size: 300},
				},
				{
					Kind: repository.DriveUsageKindEmoji, Origin: repository.DriveUsageOriginLocal,
					DriveUsageBucket: repository.DriveUsageBucket{Count: 1, Size: 100},
				},
				{
					Kind: repository.DriveUsageKindAttachment, Origin: repository.DriveUsageOriginRemote,
					DriveUsageBucket: repository.DriveUsageBucket{Count: 5, Size: 0, LinkCount: 5},
				},
			},
			ByHost: []repository.DriveUsageHostRow{{
				Host:             "a.example",
				DriveUsageBucket: repository.DriveUsageBucket{Count: 5, LinkCount: 5},
			}},
			ByUser: []repository.DriveUsageUserRow{{
				UserID: "u1", Username: "alice",
				DriveUsageBucket: repository.DriveUsageBucket{Count: 4, Size: 400},
			}},
		},
	}
}

func TestDriveUsage_ReturnsBreakdown(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveUsage{res: sampleUsage()}
	h.SetDriveUsageProvider(stub)

	rec := doPost(h.DriveUsage, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	got := decodeUsage(t, rec.Body.Bytes())
	assert.Equal(t, "2026-09-19T03:04:05.600Z", got.CalculatedAt)
	assert.Equal(t, int64(1234), got.ElapsedMs)
	assert.False(t, got.Cached)
	assert.Equal(t, 300, got.CacheTTLSeconds)
	assert.Equal(t, 30, got.TopLimit)
	// **「DB が把握している量」であることが応答に出ていること。** 実ストレージの
	// 使用量と取り違えられると、請求や逼迫の判断を誤る。
	assert.Equal(t, "database", got.Source)

	assert.Equal(t, usageBucketJSON{Count: 4, Size: 400}, got.Local)
	assert.Equal(t, usageBucketJSON{Count: 5, Size: 0, LinkCount: 5}, got.Remote)
	assert.Equal(t, usageBucketJSON{Count: 9, Size: 400, LinkCount: 5}, got.Total)

	require.Len(t, got.ByKind, 3)
	assert.Equal(t, "attachment", got.ByKind[0].Kind)
	assert.Equal(t, "local", got.ByKind[0].Origin)
	assert.Equal(t, int64(300), got.ByKind[0].Size)
	assert.Equal(t, int64(5), got.ByKind[2].LinkCount)

	require.Len(t, got.ByHost, 1)
	assert.Equal(t, "a.example", got.ByHost[0].Host)
	assert.Equal(t, int64(5), got.ByHost[0].LinkCount)

	require.Len(t, got.ByUser, 1)
	assert.Equal(t, "u1", got.ByUser[0].UserID)
	assert.Equal(t, "alice", got.ByUser[0].Username)
	assert.Equal(t, int64(400), got.ByUser[0].Size)
}

// body 無しでも既定で応答する (管理画面が引数なしで叩く)。
func TestDriveUsage_NoBodyIsNotForced(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveUsage{res: sampleUsage()}
	h.SetDriveUsageProvider(stub)

	rec := doPost(h.DriveUsage, ``, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, stub.calls)
	assert.False(t, stub.gotForce)
}

// 「更新」ボタンの forceRecalc が集計側まで届くこと。落ちると押しても古い
// 数字が返り続ける。
func TestDriveUsage_PassesForceRecalc(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveUsage{res: sampleUsage()}
	h.SetDriveUsageProvider(stub)

	rec := doPost(h.DriveUsage, `{"forceRecalc":true}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, stub.gotForce)
}

// キャッシュの使い回しは応答に出す。出ないと管理者が「今の値」と誤読する。
func TestDriveUsage_ReportsCached(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	res := sampleUsage()
	res.Cached = true
	h.SetDriveUsageProvider(&stubDriveUsage{res: res})

	rec := doPost(h.DriveUsage, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, decodeUsage(t, rec.Body.Bytes()).Cached)
}

// 行が 1 つも無くても配列は null ではなく空配列で返す (フロントが length を見る)。
func TestDriveUsage_EmptyRankingsAreArrays(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDriveUsageProvider(&stubDriveUsage{res: &driveusage.Result{
		Breakdown: &repository.DriveUsageBreakdown{},
		TopN:      30,
		TTL:       5 * time.Minute,
	}})

	rec := doPost(h.DriveUsage, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"byKind":[]`)
	assert.Contains(t, body, `"byHost":[]`)
	assert.Contains(t, body, `"byUser":[]`)
}

// captureSlog swaps the default logger for the duration of the test and returns
// what was written.
//
// **500 の本文は汎用なので、ログがこの経路の唯一の診断材料になる。** 出ていない
// ことに気付けるよう、失敗経路のログは必ずここで見る。
func captureSlog(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// 集計の失敗は 500。0 バイトを返すと「使っていない」という誤った事実になる。
func TestDriveUsage_ErrorIsInternal(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetDriveUsageProvider(&stubDriveUsage{err: assertError{}})
	logged := captureSlog(t)

	rec := doPost(h.DriveUsage, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"size"`)
	assert.Contains(t, logged.String(), "admin/drive/usage",
		"集計の失敗がログに残っていない (応答は汎用 500 なので、ここが唯一の手掛かり)")
	assert.Contains(t, logged.String(), "stub failure", "失敗の理由がログに残っていない")
}

// provider が内訳を返さなかったときも 500。`packDriveUsage` がそのまま触ると panic
// するし、空の内訳を描けば「容量ゼロ」という誤った事実になる。
func TestDriveUsage_MissingBreakdownIsInternal(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *driveusage.Result
	}{
		{"nil result", nil},
		{"nil breakdown", &driveusage.Result{TopN: 30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			h.SetDriveUsageProvider(&stubDriveUsage{res: tc.res})
			logged := captureSlog(t)

			rec := doPost(h.DriveUsage, `{}`, adminUser)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.NotContains(t, rec.Body.String(), `"total"`)
			assert.Contains(t, logged.String(), "admin/drive/usage",
				"契約違反がログに残っていない")
		})
	}
}

// 未配線でも同じ。空の集計を返して「容量ゼロ」に見せない。
func TestDriveUsage_UnwiredIsInternal(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	rec := doPost(h.DriveUsage, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"total"`)
}
