package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupInstance(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "instance" WHERE id = ?`, id)
}

func newTestInstance(id, host string) *model.Instance {
	return &model.Instance{
		ID:               id,
		Host:             host,
		FirstRetrievedAt: time.Now(),
		SuspensionState:  model.SuspensionStateNone,
	}
}

func TestInstanceRepository_CreateAndFindByHost(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	inst := newTestInstance("i_ir_1", "alpha.example")
	require.NoError(t, repo.Create(inst))
	defer cleanupInstance(t, inst.ID)

	got, err := repo.FindByHost("alpha.example")
	require.NoError(t, err)
	assert.Equal(t, "alpha.example", got.Host)
}

func TestInstanceRepository_FindByHost_NotFound(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	_, err := repo.FindByHost("missing.example")
	assert.Error(t, err)
}

func TestInstanceRepository_FindManyByHosts(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	inst1 := newTestInstance("i_fmbh_1", "fmbh1.example")
	inst2 := newTestInstance("i_fmbh_2", "fmbh2.example")
	inst3 := newTestInstance("i_fmbh_3", "fmbh3.example")
	require.NoError(t, repo.Create(inst1))
	defer cleanupInstance(t, inst1.ID)
	require.NoError(t, repo.Create(inst2))
	defer cleanupInstance(t, inst2.ID)
	require.NoError(t, repo.Create(inst3))
	defer cleanupInstance(t, inst3.ID)

	// batch fetch 2 existing + 1 missing host
	got, err := repo.FindManyByHosts([]string{"fmbh1.example", "fmbh3.example", "missing.example"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	hosts := []string{got[0].Host, got[1].Host}
	assert.ElementsMatch(t, []string{"fmbh1.example", "fmbh3.example"}, hosts)
}

func TestInstanceRepository_FindManyByHosts_Empty(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	got, err := repo.FindManyByHosts(nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = repo.FindManyByHosts([]string{})
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestInstanceRepository_UpdateFields(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	inst := newTestInstance("i_ir_2", "beta.example")
	require.NoError(t, repo.Create(inst))
	defer cleanupInstance(t, inst.ID)

	name := "Beta"
	desc := "Beta instance"
	require.NoError(t, repo.UpdateFields("beta.example", map[string]any{
		"name":        &name,
		"description": &desc,
	}))

	got, err := repo.FindByHost("beta.example")
	require.NoError(t, err)
	require.NotNil(t, got.Name)
	assert.Equal(t, "Beta", *got.Name)
	require.NotNil(t, got.Description)
	assert.Equal(t, "Beta instance", *got.Description)
}

func TestInstanceRepository_UpdateFields_NoOp(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	require.NoError(t, repo.UpdateFields("any.example", nil))
}

func TestInstanceRepository_IncrementCount(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	inst := newTestInstance("i_ir_3", "gamma.example")
	require.NoError(t, repo.Create(inst))
	defer cleanupInstance(t, inst.ID)

	require.NoError(t, repo.IncrementCount("gamma.example", "usersCount", 3))
	require.NoError(t, repo.IncrementCount("gamma.example", "usersCount", -1))

	got, err := repo.FindByHost("gamma.example")
	require.NoError(t, err)
	assert.Equal(t, 2, got.UsersCount)
}

// TestInstanceRepository_IncrementCount_FloorsAtZero pins that a decrement
// never takes a counter below 0 (#3330): rows created before mk-go maintained
// notesCount still hold 0, and deleting a note ingested before the upgrade
// must not make it negative.
func TestInstanceRepository_IncrementCount_FloorsAtZero(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	inst := newTestInstance("i_ir_floor", "floor.example")
	inst.UsersCount = 1
	require.NoError(t, repo.Create(inst))
	defer cleanupInstance(t, inst.ID)

	require.NoError(t, repo.IncrementCount("floor.example", "notesCount", -1))
	require.NoError(t, repo.IncrementCount("floor.example", "usersCount", -3))
	got, err := repo.FindByHost("floor.example")
	require.NoError(t, err)
	assert.Equal(t, 0, got.NotesCount)
	assert.Equal(t, 0, got.UsersCount)

	// 0 で止めた後も加算は普通に効く。
	require.NoError(t, repo.IncrementCount("floor.example", "notesCount", 2))
	require.NoError(t, repo.IncrementCount("floor.example", "notesCount", -1))
	got, err = repo.FindByHost("floor.example")
	require.NoError(t, err)
	assert.Equal(t, 1, got.NotesCount)
}

// #1777: host filter は LIKE metacharacter を escape する。"a_b" は literal
// underscore 一致になり、"axb.example" を wildcard で誤マッチしない。
func TestInstanceRepository_List_HostFilterEscapesLikeMetachars(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	literal := newTestInstance("i_ir_esc1", "a_b.example")
	wildcardish := newTestInstance("i_ir_esc2", "axb.example")
	for _, inst := range []*model.Instance{literal, wildcardish} {
		require.NoError(t, repo.Create(inst))
		defer cleanupInstance(t, inst.ID)
	}

	rows, err := repo.List(model.InstanceListFilter{Host: "a_b"})
	require.NoError(t, err)
	hosts := map[string]bool{}
	for _, r := range rows {
		hosts[r.Host] = true
	}
	assert.True(t, hosts["a_b.example"], "literal a_b.example should match")
	assert.False(t, hosts["axb.example"], "underscore must be escaped, not treated as a wildcard")
}

func TestInstanceRepository_List_Filters(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	a := newTestInstance("i_ir_4a", "list-a.example")
	a.UsersCount = 5
	a.NotesCount = 100
	a.FollowingCount = 1
	b := newTestInstance("i_ir_4b", "list-b.example")
	b.UsersCount = 1
	b.NotesCount = 10
	b.FollowersCount = 1
	b.IsNotResponding = true
	c := newTestInstance("i_ir_4c", "list-c.example")
	c.SuspensionState = model.SuspensionStateManuallySuspended

	// "list-"に含まれないhostでfollowersCount>0のinstanceを混ぜておく。
	// federating=trueのOR条件が括弧で包まれていないと
	// (host ILIKE ... AND followingCount>0) OR followersCount>0
	// になり、Host filterを無視してdを拾ってしまうため、その回帰テスト。
	d := newTestInstance("i_ir_4d", "other-d.example")
	d.FollowersCount = 2

	for _, inst := range []*model.Instance{a, b, c, d} {
		require.NoError(t, repo.Create(inst))
		defer cleanupInstance(t, inst.ID)
	}

	rows, err := repo.List(model.InstanceListFilter{Host: "list-"})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rows), 3)

	suspendedTrue := true
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", Suspended: &suspendedTrue})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "list-c.example", rows[0].Host)

	suspendedFalse := false
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", Suspended: &suspendedFalse})
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	notRespTrue := true
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", NotResponding: &notRespTrue})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "list-b.example", rows[0].Host)

	federating := true
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", Federating: &federating})
	require.NoError(t, err)
	assert.Len(t, rows, 2) // a と b

	notFederating := false
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", Federating: &notFederating})
	require.NoError(t, err)
	assert.Len(t, rows, 1) // c (followingCount=0 AND followersCount=0)
	assert.Equal(t, "list-c.example", rows[0].Host)

	subscribing := true
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", Subscribing: &subscribing})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "list-b.example", rows[0].Host)

	notSubscribing := false
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", Subscribing: &notSubscribing})
	require.NoError(t, err)
	assert.Len(t, rows, 2) // a と c

	publishing := true
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", Publishing: &publishing})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "list-a.example", rows[0].Host)

	notPublishing := false
	rows, err = repo.List(model.InstanceListFilter{Host: "list-", Publishing: &notPublishing})
	require.NoError(t, err)
	assert.Len(t, rows, 2) // b と c
}

// TestInstanceRepository_List_BlockedSilenced exercises the exact host
// IN/NOT IN matching used by the blocked / silenced filters, including the
// empty-list edge cases (true+empty -> 0 rows, false+empty -> all rows).
func TestInstanceRepository_List_BlockedSilenced(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	blocked := newTestInstance("i_ir_bs_a", "bs-blocked.example")
	silenced := newTestInstance("i_ir_bs_b", "bs-silenced.example")
	normal := newTestInstance("i_ir_bs_c", "bs-normal.example")
	for _, inst := range []*model.Instance{blocked, silenced, normal} {
		require.NoError(t, repo.Create(inst))
		defer cleanupInstance(t, inst.ID)
	}

	blockedHosts := []string{"bs-blocked.example"}
	silencedHosts := []string{"bs-silenced.example"}
	tt := true
	ff := false

	rows, err := repo.List(model.InstanceListFilter{Host: "bs-", Blocked: &tt, BlockedHosts: blockedHosts})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "bs-blocked.example", rows[0].Host)

	rows, err = repo.List(model.InstanceListFilter{Host: "bs-", Blocked: &ff, BlockedHosts: blockedHosts})
	require.NoError(t, err)
	assert.Len(t, rows, 2) // silenced と normal

	// blocked:true かつ blockedHosts が空 -> 0 件 (本家 1=0 相当)。
	rows, err = repo.List(model.InstanceListFilter{Host: "bs-", Blocked: &tt})
	require.NoError(t, err)
	assert.Empty(t, rows)

	// blocked:false かつ blockedHosts が空 -> 条件なしで全件。
	rows, err = repo.List(model.InstanceListFilter{Host: "bs-", Blocked: &ff})
	require.NoError(t, err)
	assert.Len(t, rows, 3)

	rows, err = repo.List(model.InstanceListFilter{Host: "bs-", Silenced: &tt, SilencedHosts: silencedHosts})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "bs-silenced.example", rows[0].Host)

	rows, err = repo.List(model.InstanceListFilter{Host: "bs-", Silenced: &ff, SilencedHosts: silencedHosts})
	require.NoError(t, err)
	assert.Len(t, rows, 2) // blocked と normal

	rows, err = repo.List(model.InstanceListFilter{Host: "bs-", Silenced: &tt})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestInstanceRepository_List_Sort(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	a := newTestInstance("i_ir_5a", "sort-a.example")
	a.UsersCount = 1
	a.NotesCount = 1
	b := newTestInstance("i_ir_5b", "sort-b.example")
	b.UsersCount = 5
	b.NotesCount = 5
	for _, inst := range []*model.Instance{a, b} {
		require.NoError(t, repo.Create(inst))
		defer cleanupInstance(t, inst.ID)
	}

	for _, sortBy := range []string{
		"+host", "-host", "+notes", "-notes", "+users", "-users",
		"+following", "-following", "+followers", "-followers",
		"+pubSub", "-pubSub", "+firstRetrievedAt", "-firstRetrievedAt",
		"+latestRequestReceivedAt", "-latestRequestReceivedAt", "",
	} {
		rows, err := repo.List(model.InstanceListFilter{Host: "sort-", SortBy: sortBy, Limit: 10})
		require.NoError(t, err)
		assert.Len(t, rows, 2, "sort=%s", sortBy)
	}
}

// TestInstanceRepository_List_SortDirection は sort key の向きが本家 TS と
// 一致すること (= "+" が DESC、"-" が ASC) を実順序で検証する regression
// guard。以前は向きが逆 (frontend の "降順" ラベルと食い違い) だった。
func TestInstanceRepository_List_SortDirection(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	now := time.Now()
	low := newTestInstance("i_ir_6a", "sortdir-low.example")
	low.NotesCount = 1
	low.FollowersCount = 1
	low.FollowingCount = 1
	lowReq := now.Add(-2 * time.Hour)
	low.LatestRequestReceivedAt = &lowReq
	high := newTestInstance("i_ir_6b", "sortdir-high.example")
	high.NotesCount = 100
	high.FollowersCount = 50
	high.FollowingCount = 80
	highReq := now.Add(-1 * time.Hour)
	high.LatestRequestReceivedAt = &highReq
	for _, inst := range []*model.Instance{low, high} {
		require.NoError(t, repo.Create(inst))
		defer cleanupInstance(t, inst.ID)
	}

	// "+" は DESC: 値の大きい high が先頭。
	for _, sortBy := range []string{"+notes", "+followers", "+following", "+pubSub", "+latestRequestReceivedAt"} {
		rows, err := repo.List(model.InstanceListFilter{Host: "sortdir-", SortBy: sortBy, Limit: 10})
		require.NoError(t, err)
		require.Len(t, rows, 2, "sort=%s", sortBy)
		assert.Equal(t, "sortdir-high.example", rows[0].Host, "sort=%s should put larger value first", sortBy)
	}
	// "-" は ASC: 値の小さい low が先頭。
	for _, sortBy := range []string{"-notes", "-followers", "-following", "-pubSub", "-latestRequestReceivedAt"} {
		rows, err := repo.List(model.InstanceListFilter{Host: "sortdir-", SortBy: sortBy, Limit: 10})
		require.NoError(t, err)
		require.Len(t, rows, 2, "sort=%s", sortBy)
		assert.Equal(t, "sortdir-low.example", rows[0].Host, "sort=%s should put smaller value first", sortBy)
	}
}

// TestInstanceRepository_List_LatestRequestNullsOrder verifies the NULLS LAST /
// NULLS FIRST handling for latestRequestReceivedAt: +(DESC) keeps NULLs last,
// -(ASC) puts NULLs first.
func TestInstanceRepository_List_LatestRequestNullsOrder(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	withTime := newTestInstance("i_ir_7a", "nulls-has.example")
	ts := time.Now().Add(-1 * time.Hour)
	withTime.LatestRequestReceivedAt = &ts
	noTime := newTestInstance("i_ir_7b", "nulls-none.example") // latestRequestReceivedAt = NULL
	for _, inst := range []*model.Instance{withTime, noTime} {
		require.NoError(t, repo.Create(inst))
		defer cleanupInstance(t, inst.ID)
	}

	rows, err := repo.List(model.InstanceListFilter{Host: "nulls-", SortBy: "+latestRequestReceivedAt", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "nulls-none.example", rows[1].Host, "NULL sorts last on +(DESC)")

	rows, err = repo.List(model.InstanceListFilter{Host: "nulls-", SortBy: "-latestRequestReceivedAt", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "nulls-none.example", rows[0].Host, "NULL sorts first on -(ASC)")
}

func TestInstanceRepository_List_LimitClamp(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	rows, err := repo.List(model.InstanceListFilter{Limit: 9999})
	require.NoError(t, err)
	assert.NotNil(t, rows)

	rows, err = repo.List(model.InstanceListFilter{Limit: -10})
	require.NoError(t, err)
	assert.NotNil(t, rows)
}

func TestInstanceRepository_List_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewInstanceRepository(db)
	_, err := repo.List(model.InstanceListFilter{})
	assert.Error(t, err)
}

// IncrementFollowersCount / IncrementFollowingCount の incremental 動作検証
// (#596)。delta=0 / host="" は no-op、未登録 host も error にならない。
func TestInstanceRepository_IncrementCounters(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	inst := newTestInstance("i_ic_1", "counter.example")
	inst.FollowersCount = 10
	inst.FollowingCount = 5
	require.NoError(t, repo.Create(inst))
	defer cleanupInstance(t, inst.ID)

	// +1
	require.NoError(t, repo.IncrementFollowersCount("counter.example", 1))
	got, _ := repo.FindByHost("counter.example")
	assert.Equal(t, 11, got.FollowersCount)

	// -2
	require.NoError(t, repo.IncrementFollowersCount("counter.example", -2))
	got, _ = repo.FindByHost("counter.example")
	assert.Equal(t, 9, got.FollowersCount)

	// followingCount も独立に動く
	require.NoError(t, repo.IncrementFollowingCount("counter.example", 3))
	got, _ = repo.FindByHost("counter.example")
	assert.Equal(t, 8, got.FollowingCount)

	// host="" は no-op
	require.NoError(t, repo.IncrementFollowersCount("", 100))
	got, _ = repo.FindByHost("counter.example")
	assert.Equal(t, 9, got.FollowersCount, "host が空文字なら touch しない")

	// delta=0 も no-op (UPDATE 自体走らない)
	require.NoError(t, repo.IncrementFollowersCount("counter.example", 0))
	got, _ = repo.FindByHost("counter.example")
	assert.Equal(t, 9, got.FollowersCount)

	// 未登録 host も error にしない (best-effort)
	require.NoError(t, repo.IncrementFollowersCount("unknown.example", 1))
	require.NoError(t, repo.IncrementFollowingCount("unknown.example", -1))
}

func TestInstanceRepository_IncrementCounters_DBError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewInstanceRepository(db)
	assert.Error(t, repo.IncrementFollowersCount("any.example", 1))
	assert.Error(t, repo.IncrementFollowingCount("any.example", 1))
}

// ListPeerHosts は suspended を除いた host を昇順で返すこと。
// GET /api/v1/instance/peers (Mastodon 互換) の裏側 (#2245)。
func TestInstanceRepository_ListPeerHosts(t *testing.T) {
	repo := NewInstanceRepository(testDB)

	live1 := newTestInstance("i_peers_1", "zeta.peers.example")
	live2 := newTestInstance("i_peers_2", "alpha.peers.example")
	suspended := newTestInstance("i_peers_3", "gone.peers.example")
	suspended.SuspensionState = model.SuspensionStateManuallySuspended
	autoSuspended := newTestInstance("i_peers_4", "dead.peers.example")
	autoSuspended.SuspensionState = model.SuspensionStateAutoSuspendedForNotResponding
	// blocked / 応答不能でも upstream は除外しないので peers に残ること
	notResponding := newTestInstance("i_peers_5", "down.peers.example")
	notResponding.IsNotResponding = true

	for _, inst := range []*model.Instance{live1, live2, suspended, autoSuspended, notResponding} {
		require.NoError(t, repo.Create(inst))
		defer cleanupInstance(t, inst.ID)
	}

	hosts, err := repo.ListPeerHosts()
	require.NoError(t, err)

	got := map[string]bool{}
	for _, h := range hosts {
		got[h] = true
	}
	assert.True(t, got["alpha.peers.example"])
	assert.True(t, got["zeta.peers.example"])
	assert.True(t, got["down.peers.example"], "isNotResponding は除外しない")
	assert.False(t, got["gone.peers.example"], "手動 suspend は除外する")
	assert.False(t, got["dead.peers.example"], "自動 suspend も除外する")

	// 並びは host ASC。テスト用 host だけ抜き出して単調増加を確認する。
	var ours []string
	for _, h := range hosts {
		if len(h) > len(".peers.example") && h[len(h)-len(".peers.example"):] == ".peers.example" {
			ours = append(ours, h)
		}
	}
	assert.Equal(t, []string{"alpha.peers.example", "down.peers.example", "zeta.peers.example"}, ours)
}

// TestInstanceRepository_RecomputeFollowCounts pins the upstream column
// semantics (#3330): followingCount counts remote(host) → local follows and
// followersCount counts local → remote(host) follows. Remote → remote rows,
// rows involving a moved account and stale values on instances without
// follows must not survive.
func TestInstanceRepository_RecomputeFollowCounts(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	insts := []*model.Instance{
		newTestInstance("i_rfc_1", "rfc1.example"),
		newTestInstance("i_rfc_2", "rfc2.example"),
		newTestInstance("i_rfc_3", "rfc3.example"),
	}
	insts[2].FollowersCount = 9
	insts[2].FollowingCount = 9
	for _, inst := range insts {
		require.NoError(t, repo.Create(inst))
		t.Cleanup(func() { cleanupInstance(t, inst.ID) })
	}

	hosts := map[string]*string{}
	seedHostUser := func(id string, host *string, movedTo *string) {
		hosts[id] = host
		require.NoError(t, testDB.Exec(
			`INSERT INTO "user" (id, "updatedAt", username, "usernameLower", token, host, "movedToUri") VALUES (?, NOW(), ?, ?, ?, ?, ?)`,
			id, "u_"+id, "u_"+id, "tok_"+id, host, movedTo,
		).Error)
		t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
	}
	h1, h2 := "rfc1.example", "rfc2.example"
	moved, empty := "https://rfc2.example/users/new", ""
	seedHostUser("rfc_l1", nil, nil)
	seedHostUser("rfc_l2", nil, nil)
	seedHostUser("rfc_lm", nil, &moved) // 移行済みのローカル
	seedHostUser("rfc_r1a", &h1, nil)
	seedHostUser("rfc_r1b", &h1, &empty) // 空文字は移行済みとみなさない
	seedHostUser("rfc_r2", &h2, nil)
	seedHostUser("rfc_r2m", &h2, &moved) // 移行済みのリモート

	follows := [][2]string{
		{"rfc_r1a", "rfc_l1"}, {"rfc_r1b", "rfc_l1"}, {"rfc_r1a", "rfc_l2"}, // rfc1 → local: 3
		{"rfc_l1", "rfc_r1a"},                      // local → rfc1: 1
		{"rfc_l1", "rfc_r2"}, {"rfc_l2", "rfc_r2"}, // local → rfc2: 2
		{"rfc_r1b", "rfc_r2"}, // remote → remote: 数えない
		{"rfc_l1", "rfc_r2m"}, // 移行済みのリモートへ: 数えない
		{"rfc_r2m", "rfc_l2"}, // 移行済みのリモートから: 数えない
		{"rfc_lm", "rfc_r1a"}, // 移行済みのローカルから: 数えない
		{"rfc_r1a", "rfc_lm"}, // 移行済みのローカルへ: 数えない
	}
	for i, f := range follows {
		id := "rfc_f" + string(rune('a'+i))
		// 非正規化列は本番の作成経路 (following.Service) と同じく利用者の host を写す。
		require.NoError(t, testDB.Exec(
			`INSERT INTO "following" (id, "followerId", "followeeId", "followerHost", "followeeHost") VALUES (?, ?, ?, ?, ?)`,
			id, f[0], f[1], hosts[f[0]], hosts[f[1]],
		).Error)
		t.Cleanup(func() { testDB.Exec(`DELETE FROM "following" WHERE id = ?`, id) })
	}

	require.NoError(t, repo.RecomputeFollowCounts())

	want := map[string][2]int{ // host → {followingCount, followersCount}
		"rfc1.example": {3, 1},
		"rfc2.example": {0, 2},
		"rfc3.example": {0, 0},
	}
	for host, w := range want {
		got, err := repo.FindByHost(host)
		require.NoError(t, err)
		assert.Equal(t, w[0], got.FollowingCount, "%s followingCount", host)
		assert.Equal(t, w[1], got.FollowersCount, "%s followersCount", host)
	}
}

// TestInstanceRepository_RecomputeFollowCounts_SkipsUnchangedRows pins that
// the startup recompute only writes instance rows whose counts change, so it
// does not lock every instance row (#3330). A row's xmin changes only when the
// row is rewritten.
func TestInstanceRepository_RecomputeFollowCounts_SkipsUnchangedRows(t *testing.T) {
	repo := NewInstanceRepository(testDB)
	ok := newTestInstance("i_rfcu_1", "rfcu1.example") // follow なし、0 のまま
	stale := newTestInstance("i_rfcu_2", "rfcu2.example")
	stale.FollowersCount = 4
	for _, inst := range []*model.Instance{ok, stale} {
		require.NoError(t, repo.Create(inst))
		t.Cleanup(func() { cleanupInstance(t, inst.ID) })
	}
	xmin := func(id string) string {
		var rows []string
		require.NoError(t, testDB.Raw(`SELECT xmin::text FROM "instance" WHERE id = ?`, id).Scan(&rows).Error)
		require.Len(t, rows, 1)
		return rows[0]
	}
	okBefore, staleBefore := xmin(ok.ID), xmin(stale.ID)

	require.NoError(t, repo.RecomputeFollowCounts())

	assert.Equal(t, okBefore, xmin(ok.ID), "a row whose counts are already right must not be rewritten")
	assert.NotEqual(t, staleBefore, xmin(stale.ID), "a stale row must be rewritten")
	got, err := repo.FindByHost("rfcu2.example")
	require.NoError(t, err)
	assert.Equal(t, 0, got.FollowersCount)
}

func TestInstanceRepository_RecomputeFollowCounts_DBError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewInstanceRepository(testDB.WithContext(ctx))
	assert.Error(t, repo.RecomputeFollowCounts())
}
