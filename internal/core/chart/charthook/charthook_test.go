package charthook

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/chart"
	corecharts "github.com/elythia-network/elythia/internal/core/chart/charts"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// fakeRepo is a tiny in-memory chart.Repository sufficient to drive
// the wrappers from the hooks tests. It mirrors the helper used in
// internal/api/charts/handler_test.go.
type fakeRepo struct {
	mu     sync.Mutex
	nextID int64
	hour   map[string][]*chart.Row
	day    map[string][]*chart.Row
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		hour: make(map[string][]*chart.Row),
		day:  make(map[string][]*chart.Row),
	}
}

func (r *fakeRepo) tableFor(span chart.Span) map[string][]*chart.Row {
	if span == chart.SpanDay {
		return r.day
	}
	return r.hour
}

func (r *fakeRepo) FindCurrent(_ context.Context, span chart.Span, group string, ts int64) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.tableFor(span)[group] {
		if row.Date == ts {
			return row, nil
		}
	}
	return nil, chart.ErrRowNotFound
}

func (r *fakeRepo) FindLatest(_ context.Context, span chart.Span, group string) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.tableFor(span)[group]
	if len(rows) == 0 {
		return nil, chart.ErrRowNotFound
	}
	latest := rows[0]
	for _, row := range rows[1:] {
		if row.Date > latest.Date {
			latest = row
		}
	}
	return latest, nil
}

func (r *fakeRepo) FindBefore(_ context.Context, span chart.Span, group string, ts int64) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *chart.Row
	for _, row := range r.tableFor(span)[group] {
		if row.Date < ts {
			if best == nil || row.Date > best.Date {
				best = row
			}
		}
	}
	if best == nil {
		return nil, chart.ErrRowNotFound
	}
	return best, nil
}

func (r *fakeRepo) FindRange(_ context.Context, span chart.Span, group string, gt, lt int64) ([]*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*chart.Row
	for _, row := range r.tableFor(span)[group] {
		if row.Date >= gt && row.Date <= lt {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

func (r *fakeRepo) Insert(_ context.Context, span chart.Span, group string, ts int64, cols map[string]any) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	row := &chart.Row{ID: r.nextID, Date: ts, Cols: make(map[string]any, len(cols))}
	if group != "" {
		row.Group = sql.NullString{Valid: true, String: group}
	}
	maps.Copy(row.Cols, cols)
	r.tableFor(span)[group] = append(r.tableFor(span)[group], row)
	return row, nil
}

func (r *fakeRepo) ApplyDeltas(_ context.Context, span chart.Span, id int64, deltas map[string]int64, uniqueAppends map[string][]string, setInts map[string]int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.findByID(span, id)
	if row == nil {
		return fmt.Errorf("fakeRepo: row %d not found", id)
	}
	for k, v := range deltas {
		row.Cols[k] = toInt64(row.Cols[k]) + v
	}
	for k, items := range uniqueAppends {
		key := k + ":unique"
		cur, _ := row.Cols[key].([]string)
		row.Cols[key] = append(cur, items...)
	}
	for k, v := range setInts {
		row.Cols[k] = v
	}
	return nil
}

func (r *fakeRepo) SetColumns(_ context.Context, span chart.Span, id int64, cols map[string]int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.findByID(span, id)
	if row == nil {
		return fmt.Errorf("fakeRepo: row %d not found", id)
	}
	for k, v := range cols {
		row.Cols[k] = v
	}
	return nil
}

func (r *fakeRepo) ResetUniqueTempColumns(_ context.Context, span chart.Span, gt, lt int64, columns []string) error {
	return nil
}

func (r *fakeRepo) findByID(span chart.Span, id int64) *chart.Row {
	for _, rows := range r.tableFor(span) {
		for _, row := range rows {
			if row.ID == id {
				return row
			}
		}
	}
	return nil
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}

// fakeClock returns a fixed UTC instant.
type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

func newEngine(t *testing.T, schema chart.Schema, clk chart.Clock) (*chart.Chart, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	c, err := chart.New(chart.Config{
		Schema: schema,
		Repo:   repo,
		Lock:   chart.NewMemoryLocker(),
		Clock:  clk,
	})
	require.NoError(t, err)
	return c, repo
}

// buildHooks constructs a fully populated Hooks bundle plus per-chart
// repos so tests can assert on the persisted state.
type harness struct {
	hooks *Hooks
	repos struct {
		notes      *fakeRepo
		users      *fakeRepo
		drive      *fakeRepo
		federation *fakeRepo
		instance   *fakeRepo
		apReq      *fakeRepo
		active     *fakeRepo
		puNotes    *fakeRepo
		puDrive    *fakeRepo
		puFollow   *fakeRepo
		puPv       *fakeRepo
		puReact    *fakeRepo
	}
	charts chartBundle
	clock  fakeClock
}

type chartBundle struct {
	notes, users, drive, federation, instance, apReq, active, puNotes, puDrive, puFollow, puPv, puReact *chart.Chart
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := fakeClock{now: time.Date(2026, 4, 9, 12, 30, 0, 0, time.UTC)}
	h := &harness{clock: clk}
	h.charts.notes, h.repos.notes = newEngine(t, corecharts.SchemaNotes(), clk)
	h.charts.users, h.repos.users = newEngine(t, corecharts.SchemaUsers(), clk)
	h.charts.drive, h.repos.drive = newEngine(t, corecharts.SchemaDrive(), clk)
	h.charts.federation, h.repos.federation = newEngine(t, corecharts.SchemaFederation(), clk)
	h.charts.instance, h.repos.instance = newEngine(t, corecharts.SchemaInstance(), clk)
	h.charts.apReq, h.repos.apReq = newEngine(t, corecharts.SchemaApRequest(), clk)
	h.charts.active, h.repos.active = newEngine(t, corecharts.SchemaActiveUsers(), clk)
	h.charts.puNotes, h.repos.puNotes = newEngine(t, corecharts.SchemaPerUserNotes(), clk)
	h.charts.puDrive, h.repos.puDrive = newEngine(t, corecharts.SchemaPerUserDrive(), clk)
	h.charts.puFollow, h.repos.puFollow = newEngine(t, corecharts.SchemaPerUserFollowing(), clk)
	h.charts.puPv, h.repos.puPv = newEngine(t, corecharts.SchemaPerUserPv(), clk)
	h.charts.puReact, h.repos.puReact = newEngine(t, corecharts.SchemaPerUserReaction(), clk)

	idgen, _ := id.NewGenerator("aidx")
	h.hooks = New(Config{
		Notes:            h.charts.notes,
		Users:            h.charts.users,
		Drive:            h.charts.drive,
		Federation:       h.charts.federation,
		Instance:         h.charts.instance,
		ApRequest:        h.charts.apReq,
		ActiveUsers:      h.charts.active,
		PerUserNotes:     h.charts.puNotes,
		PerUserDrive:     h.charts.puDrive,
		PerUserFollowing: h.charts.puFollow,
		PerUserPv:        h.charts.puPv,
		PerUserReaction:  h.charts.puReact,
		IDGen:            idgen,
		Clock:            clk,
	})
	return h
}

func (h *harness) saveAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, c := range []*chart.Chart{
		h.charts.notes, h.charts.users, h.charts.drive, h.charts.federation,
		h.charts.instance, h.charts.apReq, h.charts.active, h.charts.puNotes,
		h.charts.puDrive, h.charts.puFollow, h.charts.puPv, h.charts.puReact,
	} {
		require.NoError(t, c.Save(ctx))
	}
}

// strPtr is a tiny pointer helper for *string fields.
func strPtr(s string) *string { return &s }

// --- New / nil safety -------------------------------------------------------

func TestHooks_NilSafety(t *testing.T) {
	var h *Hooks
	// nil receiver should be a no-op for every method.
	h.OnNoteCreated(&model.Note{ID: "n"})
	h.OnNoteDeleted(&model.Note{ID: "n"})
	h.OnFollow(&model.User{ID: "a"}, &model.User{ID: "b"})
	h.OnUnfollow(&model.User{ID: "a"}, &model.User{ID: "b"})
	h.OnReactionCreated(&model.User{ID: "a"}, &model.Note{ID: "n"})
	h.OnFileUploaded(&model.DriveFile{ID: "f"})
	h.OnFileDeleted(&model.DriveFile{ID: "f"})
	h.OnRemoteUserCreated(&model.User{ID: "u"})
	h.OnInboxReceived("e.x")
	h.OnDelivered("e.x", true)
	h.OnUserShow("o", "v", "")
}

func TestNew_PartialConfig(t *testing.T) {
	// 部分構成で nil engine を渡しても panic せず、対応するフィールドだけが
	// nil のまま残る。
	notes, _ := newEngine(t, corecharts.SchemaNotes(), fakeClock{now: time.Now()})
	h := New(Config{Notes: notes})
	assert.NotNil(t, h.Notes)
	assert.Nil(t, h.Users)
	assert.Nil(t, h.PerUserPv)
	// nil chart 経由のイベントも no-op で動く (Users が nil)
	h.OnRemoteUserCreated(&model.User{ID: "x", Host: strPtr("e.x")})
}

// --- Note hook --------------------------------------------------------------

func TestHooks_OnNoteCreated_Local(t *testing.T) {
	h := newHarness(t)
	note := &model.Note{ID: "n1", UserID: "alice"}
	h.hooks.OnNoteCreated(note)
	h.saveAll(t)

	// notes ungrouped row
	require.Len(t, h.repos.notes.hour[""], 1)
	row := h.repos.notes.hour[""][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["local.total"]))

	// per-user notes row keyed on userId
	require.Len(t, h.repos.puNotes.hour["alice"], 1)
	assert.Equal(t, int64(1), toInt64(h.repos.puNotes.hour["alice"][0].Cols["total"]))

	// activeUsers write set
	require.Len(t, h.repos.active.hour[""], 1)
	writes, _ := h.repos.active.hour[""][0].Cols["write:unique"].([]string)
	assert.Equal(t, []string{"alice"}, writes)

	// instance chart untouched (note is local)
	assert.Empty(t, h.repos.instance.hour)
}

func TestHooks_OnNoteCreated_Remote(t *testing.T) {
	h := newHarness(t)
	note := &model.Note{ID: "n2", UserID: "bob", UserHost: strPtr("example.com")}
	h.hooks.OnNoteCreated(note)
	h.saveAll(t)

	// instance chart for the host
	require.Len(t, h.repos.instance.hour["example.com"], 1)
	assert.Equal(t, int64(1), toInt64(h.repos.instance.hour["example.com"][0].Cols["notes.total"]))

	// activeUsers untouched (remote author)
	require.Empty(t, h.repos.active.hour)
}

func TestHooks_OnNoteDeleted(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnNoteDeleted(&model.Note{ID: "n", UserID: "alice"})
	h.saveAll(t)
	row := h.repos.notes.hour[""][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["local.total"]))
}

func TestHooks_OnNoteDeleted_Remote(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnNoteDeleted(&model.Note{ID: "n", UserID: "bob", UserHost: strPtr("e.x")})
	h.saveAll(t)
	// instance row is decremented
	assert.Equal(t, int64(-1), toInt64(h.repos.instance.hour["e.x"][0].Cols["notes.total"]))
}

func TestHooks_OnNoteCreated_NilNote(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnNoteCreated(nil)
	h.hooks.OnNoteDeleted(nil)
	// 何も起きない (panic 無しを確認)
	h.saveAll(t)
	assert.Empty(t, h.repos.notes.hour)
}

// --- Follow hook ------------------------------------------------------------

func TestHooks_OnFollow_LocalLocal(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnFollow(&model.User{ID: "alice"}, &model.User{ID: "bob"})
	h.saveAll(t)
	// per-user follows for both sides
	assert.Len(t, h.repos.puFollow.hour["alice"], 1)
	assert.Len(t, h.repos.puFollow.hour["bob"], 1)
	// instance untouched (both local)
	assert.Empty(t, h.repos.instance.hour)
}

func TestHooks_OnFollow_RemoteFollowsLocal(t *testing.T) {
	h := newHarness(t)
	rem := &model.User{ID: "carol", Host: strPtr("e.x")}
	loc := &model.User{ID: "dave"}
	h.hooks.OnFollow(rem, loc)
	h.saveAll(t)
	// 本家 UserFollowingService.insertFollowingDoc: remote → local は
	// instanceChart.updateFollowing(follower.host) (#3330)。
	row := h.repos.instance.hour["e.x"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["following.total"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["followers.total"]))
}

func TestHooks_OnUnfollow_LocalFollowsRemote(t *testing.T) {
	h := newHarness(t)
	loc := &model.User{ID: "alice"}
	rem := &model.User{ID: "ed", Host: strPtr("other.test")}
	h.hooks.OnUnfollow(loc, rem)
	h.saveAll(t)
	// 本家 decrementFollowing: local → remote は
	// instanceChart.updateFollowers(followee.host, false) (#3330)。
	row := h.repos.instance.hour["other.test"][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["followers.total"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["following.total"]))
}

// TestHooks_OnFollow_InstanceChartNeedsStatsFlag pins that the follow-related
// instance chart is skipped when meta.enableStatsForFederatedInstances is off,
// even if enableChartsForFederatedInstances is on (upstream nests the chart
// update inside the stats flag).
func TestHooks_OnFollow_InstanceChartNeedsStatsFlag(t *testing.T) {
	cases := []struct {
		name          string
		stats, charts bool
		want          bool
	}{
		{name: "both on", stats: true, charts: true, want: true},
		{name: "stats off", stats: false, charts: true, want: false},
		{name: "charts off", stats: true, charts: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			require.True(t, h.hooks.StatsForFederatedInst, "New defaults to the upstream default (true)")
			h.hooks.StatsForFederatedInst = tc.stats
			h.hooks.ChartsForFederatedInst = tc.charts
			h.hooks.OnFollow(&model.User{ID: "carol", Host: strPtr("e.x")}, &model.User{ID: "dave"})
			h.hooks.OnFollow(&model.User{ID: "dave"}, &model.User{ID: "erin", Host: strPtr("f.x")})
			h.saveAll(t)
			if tc.want {
				assert.Len(t, h.repos.instance.hour["e.x"], 1)
				assert.Len(t, h.repos.instance.hour["f.x"], 1)
			} else {
				assert.Empty(t, h.repos.instance.hour)
			}
			// per-user following chart は stats フラグと無関係に動く。
			assert.NotEmpty(t, h.repos.puFollow.hour["dave"])
		})
	}
}

func TestHooks_OnFollow_NilUsers(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnFollow(nil, &model.User{ID: "x"})
	h.hooks.OnFollow(&model.User{ID: "x"}, nil)
	h.saveAll(t)
	assert.Empty(t, h.repos.puFollow.hour)
}

// --- Reaction hook ----------------------------------------------------------

func TestHooks_OnReactionCreated(t *testing.T) {
	h := newHarness(t)
	reactor := &model.User{ID: "alice"}
	note := &model.Note{ID: "n", UserID: "owner"}
	h.hooks.OnReactionCreated(reactor, note)
	h.saveAll(t)
	row := h.repos.puReact.hour["owner"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["local.count"]))
}

func TestHooks_OnReactionCreated_NilArgs(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnReactionCreated(nil, &model.Note{ID: "n", UserID: "o"})
	h.hooks.OnReactionCreated(&model.User{ID: "x"}, nil)
	h.saveAll(t)
	assert.Empty(t, h.repos.puReact.hour)
}

// --- Drive hook -------------------------------------------------------------

func TestHooks_OnFileUploaded(t *testing.T) {
	h := newHarness(t)
	file := &model.DriveFile{
		ID:     "f1",
		UserID: strPtr("alice"),
		Size:   5000,
	}
	h.hooks.OnFileUploaded(file)
	h.saveAll(t)
	// drive ungrouped
	assert.Equal(t, int64(1), toInt64(h.repos.drive.hour[""][0].Cols["local.incCount"]))
	// per-user drive
	assert.Equal(t, int64(5), toInt64(h.repos.puDrive.hour["alice"][0].Cols["incSize"]))
	// instance untouched (local file)
	assert.Empty(t, h.repos.instance.hour)
}

func TestHooks_OnFileDeleted_Remote(t *testing.T) {
	h := newHarness(t)
	file := &model.DriveFile{
		ID:       "f2",
		UserID:   strPtr("bob"),
		UserHost: strPtr("e.x"),
		Size:     2500,
	}
	h.hooks.OnFileDeleted(file)
	h.saveAll(t)
	assert.Equal(t, int64(1), toInt64(h.repos.drive.hour[""][0].Cols["remote.decCount"]))
	// instance updated for the remote host
	assert.Equal(t, int64(-1), toInt64(h.repos.instance.hour["e.x"][0].Cols["drive.totalFiles"]))
}

func TestHooks_OnFileUploaded_NoOwner(t *testing.T) {
	h := newHarness(t)
	// no owner: per-user drive must be skipped silently
	h.hooks.OnFileUploaded(&model.DriveFile{ID: "f", Size: 1000})
	h.saveAll(t)
	assert.Empty(t, h.repos.puDrive.hour)
}

func TestHooks_OnFileUploaded_NilFile(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnFileUploaded(nil)
	h.hooks.OnFileDeleted(nil)
	h.saveAll(t)
	assert.Empty(t, h.repos.drive.hour)
}

// --- Federation hooks -------------------------------------------------------

func TestHooks_OnRemoteUserCreated(t *testing.T) {
	h := newHarness(t)
	user := &model.User{ID: "u", Host: strPtr("e.x")}
	h.hooks.OnRemoteUserCreated(user)
	h.saveAll(t)
	assert.Equal(t, int64(1), toInt64(h.repos.users.hour[""][0].Cols["remote.total"]))
	assert.Equal(t, int64(1), toInt64(h.repos.instance.hour["e.x"][0].Cols["users.total"]))
}

func TestHooks_OnRemoteUserCreated_NilOrLocal(t *testing.T) {
	h := newHarness(t)
	// nil and local-only users are no-ops for the instance chart
	h.hooks.OnRemoteUserCreated(nil)
	h.hooks.OnRemoteUserCreated(&model.User{ID: "local"})
	h.saveAll(t)
	// users chart still fires for the local user (local.total +1)
	require.Len(t, h.repos.users.hour[""], 1)
	assert.Equal(t, int64(1), toInt64(h.repos.users.hour[""][0].Cols["local.total"]))
	// instance untouched
	assert.Empty(t, h.repos.instance.hour)
}

func TestHooks_OnInboxReceived(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnInboxReceived("e.x")
	h.saveAll(t)
	assert.Equal(t, int64(1), toInt64(h.repos.apReq.hour[""][0].Cols["inboxReceived"]))
	assert.Equal(t, int64(1), toInt64(h.repos.federation.hour[""][0].Cols["inboxInstances"]))
	assert.Equal(t, int64(1), toInt64(h.repos.instance.hour["e.x"][0].Cols["requests.received"]))
}

func TestHooks_OnInboxReceived_EmptyHost(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnInboxReceived("")
	h.saveAll(t)
	// apRequest still fires (host-less counter)
	assert.Equal(t, int64(1), toInt64(h.repos.apReq.hour[""][0].Cols["inboxReceived"]))
	// federation/instance skipped (no host)
	assert.Empty(t, h.repos.federation.hour)
	assert.Empty(t, h.repos.instance.hour)
}

func TestHooks_OnDelivered_Success(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnDelivered("e.x", true)
	h.saveAll(t)
	assert.Equal(t, int64(1), toInt64(h.repos.apReq.hour[""][0].Cols["deliverSucceeded"]))
	assert.Equal(t, int64(1), toInt64(h.repos.federation.hour[""][0].Cols["deliveredInstances"]))
	assert.Equal(t, int64(1), toInt64(h.repos.instance.hour["e.x"][0].Cols["requests.succeeded"]))
}

func TestHooks_OnDelivered_Failure(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnDelivered("bad.x", false)
	h.saveAll(t)
	assert.Equal(t, int64(1), toInt64(h.repos.apReq.hour[""][0].Cols["deliverFailed"]))
	assert.Equal(t, int64(1), toInt64(h.repos.federation.hour[""][0].Cols["stalled"]))
	assert.Equal(t, int64(1), toInt64(h.repos.instance.hour["bad.x"][0].Cols["requests.failed"]))
}

func TestHooks_OnDelivered_EmptyHost(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnDelivered("", true)
	h.saveAll(t)
	// apRequest still fires; federation / instance skipped
	assert.Equal(t, int64(1), toInt64(h.repos.apReq.hour[""][0].Cols["deliverSucceeded"]))
	assert.Empty(t, h.repos.federation.hour)
	assert.Empty(t, h.repos.instance.hour)
}

// --- User-show hook ---------------------------------------------------------

func TestHooks_OnUserShow_Authenticated(t *testing.T) {
	h := newHarness(t)
	// 認証済みユーザーが他人のプロフィールを開いたケース
	idGen, _ := id.NewGenerator("aidx")
	viewerID := idGen.Generate(time.Now().Add(-3 * 24 * time.Hour))
	h.hooks.IDGen = idGen
	h.hooks.OnUserShow("owner", viewerID, "")
	h.saveAll(t)
	// per-user pv (commitByUser)
	row := h.repos.puPv.hour["owner"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["pv.user"]))
	// activeUsers Read 集合に viewer が入る
	reads, _ := h.repos.active.hour[""][0].Cols["read:unique"].([]string)
	assert.Contains(t, reads, viewerID)
}

func TestHooks_OnUserShow_Anonymous(t *testing.T) {
	h := newHarness(t)
	h.hooks.OnUserShow("owner", "", "session-key")
	h.saveAll(t)
	row := h.repos.puPv.hour["owner"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["pv.visitor"]))
	// activeUsers untouched (no viewer id)
	assert.Empty(t, h.repos.active.hour)
}

func TestHooks_OnUserShow_NoOwner(t *testing.T) {
	h := newHarness(t)
	// owner が空の場合は pv chart は触らないが activeUsers は走る
	h.hooks.OnUserShow("", "alice", "")
	h.saveAll(t)
	// puPv 何もない
	assert.Empty(t, h.repos.puPv.hour)
}

func TestHooks_OnUserShow_BadID(t *testing.T) {
	h := newHarness(t)
	// パース不能な viewer id を渡しても落ちないことを確認
	h.hooks.IDGen, _ = id.NewGenerator("aidx")
	h.hooks.OnUserShow("owner", "!!", "")
	h.saveAll(t)
	// puPv の pv.user は加算されている
	assert.Equal(t, int64(1), toInt64(h.repos.puPv.hour["owner"][0].Cols["pv.user"]))
	// activeUsers Read は失敗してパスされる (空のまま)
	assert.Empty(t, h.repos.active.hour)
}
