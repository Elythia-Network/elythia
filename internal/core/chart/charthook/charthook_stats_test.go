package charthook

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

type counterAdd struct {
	host   string
	column string
	delta  int
}

type fakeInstanceCounter struct {
	mu   sync.Mutex
	adds []counterAdd
}

func (c *fakeInstanceCounter) Add(host, column string, delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adds = append(c.adds, counterAdd{host: host, column: column, delta: delta})
}

func (c *fakeInstanceCounter) snapshot() []counterAdd {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]counterAdd(nil), c.adds...)
}

// TestHooks_InstanceStats_NoteAndUserGates pins upstream's nesting for the
// remote note / user events (#3330): the instance counters move only under
// enableStatsForFederatedInstances, and the instance chart additionally needs
// enableChartsForFederatedInstances.
func TestHooks_InstanceStats_NoteAndUserGates(t *testing.T) {
	cases := []struct {
		name          string
		stats, charts bool
		wantCounter   bool
		wantChart     bool
	}{
		{name: "both on", stats: true, charts: true, wantCounter: true, wantChart: true},
		{name: "stats off", stats: false, charts: true, wantCounter: false, wantChart: false},
		{name: "charts off", stats: true, charts: false, wantCounter: true, wantChart: false},
		{name: "both off", stats: false, charts: false, wantCounter: false, wantChart: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			counter := &fakeInstanceCounter{}
			h.hooks.InstanceCounter = counter
			h.hooks.StatsForFederatedInst = tc.stats
			h.hooks.ChartsForFederatedInst = tc.charts

			h.hooks.OnNoteCreated(&model.Note{ID: "n1", UserID: "bob", UserHost: strPtr("notes.example")})
			h.hooks.OnNoteCreated(&model.Note{ID: "n2", UserID: "bob", UserHost: strPtr("notes.example")})
			h.hooks.OnNoteDeleted(&model.Note{ID: "n1", UserID: "bob", UserHost: strPtr("notes.example")})
			h.hooks.OnRemoteUserCreated(&model.User{ID: "carol", Host: strPtr("users.example")})
			h.saveAll(t)

			if tc.wantCounter {
				assert.Equal(t, []counterAdd{
					{host: "notes.example", column: "notesCount", delta: 1},
					{host: "notes.example", column: "notesCount", delta: 1},
					{host: "notes.example", column: "notesCount", delta: -1},
					{host: "users.example", column: "usersCount", delta: 1},
				}, counter.snapshot())
			} else {
				assert.Empty(t, counter.snapshot())
			}
			if tc.wantChart {
				require.Len(t, h.repos.instance.hour["notes.example"], 1)
				assert.Equal(t, int64(1), toInt64(h.repos.instance.hour["notes.example"][0].Cols["notes.total"]))
				require.Len(t, h.repos.instance.hour["users.example"], 1)
				assert.Equal(t, int64(1), toInt64(h.repos.instance.hour["users.example"][0].Cols["users.total"]))
			} else {
				assert.Empty(t, h.repos.instance.hour)
			}
			// instance 以外の chart は stats フラグと無関係に動く。
			assert.Equal(t, int64(1), toInt64(h.repos.notes.hour[""][0].Cols["remote.total"]))
			assert.Equal(t, int64(1), toInt64(h.repos.users.hour[""][0].Cols["remote.total"]))
		})
	}
}

// TestHooks_InstanceStats_LocalEventsDoNotCount pins that local notes and
// users never touch an instance counter.
func TestHooks_InstanceStats_LocalEventsDoNotCount(t *testing.T) {
	h := newHarness(t)
	counter := &fakeInstanceCounter{}
	h.hooks.InstanceCounter = counter

	h.hooks.OnNoteCreated(&model.Note{ID: "n", UserID: "alice"})
	h.hooks.OnNoteDeleted(&model.Note{ID: "n", UserID: "alice", UserHost: strPtr("")})
	h.hooks.OnRemoteUserCreated(&model.User{ID: "alice"})
	h.hooks.OnRemoteUserCreated(&model.User{ID: "dave", Host: strPtr("")})

	assert.Empty(t, counter.snapshot())
}

// TestHooks_RequestCharts_NeedChartsFlag pins that the instance request
// charts follow enableChartsForFederatedInstances only (upstream
// InboxProcessorService / DeliverProcessorService), while apRequest and
// federation charts keep firing.
func TestHooks_RequestCharts_NeedChartsFlag(t *testing.T) {
	h := newHarness(t)
	h.hooks.ChartsForFederatedInst = false
	h.hooks.OnInboxReceived("e.x")
	h.hooks.OnDelivered("e.x", true)
	h.hooks.OnDelivered("e.x", false)
	h.saveAll(t)

	assert.Empty(t, h.repos.instance.hour)
	assert.Equal(t, int64(1), toInt64(h.repos.apReq.hour[""][0].Cols["inboxReceived"]))
	assert.Equal(t, int64(1), toInt64(h.repos.apReq.hour[""][0].Cols["deliverSucceeded"]))
	assert.Equal(t, int64(1), toInt64(h.repos.apReq.hour[""][0].Cols["deliverFailed"]))
	assert.NotEmpty(t, h.repos.federation.hour)

	// stats だけ切っても request chart は動く (本家は stats の分岐の外)。
	h2 := newHarness(t)
	h2.hooks.StatsForFederatedInst = false
	h2.hooks.OnInboxReceived("e.x")
	h2.hooks.OnDelivered("e.x", true)
	h2.saveAll(t)
	require.Len(t, h2.repos.instance.hour["e.x"], 1)
	assert.Equal(t, int64(1), toInt64(h2.repos.instance.hour["e.x"][0].Cols["requests.received"]))
	assert.Equal(t, int64(1), toInt64(h2.repos.instance.hour["e.x"][0].Cols["requests.succeeded"]))
}

// TestHooks_DriveInstanceChart_IgnoresStatsFlag pins that the drive instance
// chart only follows enableChartsForFederatedInstances, as upstream
// DriveService does.
func TestHooks_DriveInstanceChart_IgnoresStatsFlag(t *testing.T) {
	h := newHarness(t)
	h.hooks.StatsForFederatedInst = false
	h.hooks.OnFileUploaded(&model.DriveFile{ID: "f", UserID: strPtr("bob"), UserHost: strPtr("e.x"), Size: 1000})
	h.saveAll(t)
	require.Len(t, h.repos.instance.hour["e.x"], 1)
	assert.Equal(t, int64(1), toInt64(h.repos.instance.hour["e.x"][0].Cols["drive.totalFiles"]))

	h2 := newHarness(t)
	h2.hooks.ChartsForFederatedInst = false
	h2.hooks.OnFileUploaded(&model.DriveFile{ID: "f", UserID: strPtr("bob"), UserHost: strPtr("e.x"), Size: 1000})
	h2.saveAll(t)
	assert.Empty(t, h2.repos.instance.hour)
}

// TestHooks_OnFollowersMovedAway pins the chart tail of upstream
// AccountMoveService.adjustFollowingCounts: one per-user following decrement
// per local follower, and a single instance followers decrement when the old
// account is remote.
func TestHooks_OnFollowersMovedAway(t *testing.T) {
	t.Run("remote old account", func(t *testing.T) {
		h := newHarness(t)
		old := &model.User{ID: "old", Host: strPtr("moved.example")}
		h.hooks.OnFollowersMovedAway(old, []string{"alice", "bob", ""})
		h.saveAll(t)

		for _, id := range []string{"alice", "bob"} {
			require.Len(t, h.repos.puFollow.hour[id], 1, id)
			assert.Equal(t, int64(-1), toInt64(h.repos.puFollow.hour[id][0].Cols["local.followings.total"]), id)
		}
		require.Len(t, h.repos.puFollow.hour["old"], 1)
		assert.Equal(t, int64(-2), toInt64(h.repos.puFollow.hour["old"][0].Cols["remote.followers.total"]))
		assert.Empty(t, h.repos.puFollow.hour[""], "empty follower ids are skipped")
		// 本家は人数に関わらず updateFollowers(host, false) を 1 回だけ呼ぶ。
		require.Len(t, h.repos.instance.hour["moved.example"], 1)
		assert.Equal(t, int64(-1), toInt64(h.repos.instance.hour["moved.example"][0].Cols["followers.total"]))
	})
	t.Run("local old account", func(t *testing.T) {
		h := newHarness(t)
		h.hooks.OnFollowersMovedAway(&model.User{ID: "old"}, []string{"alice"})
		h.hooks.OnFollowersMovedAway(&model.User{ID: "old2", Host: strPtr("")}, []string{"alice"})
		h.saveAll(t)
		assert.Len(t, h.repos.puFollow.hour["alice"], 1)
		assert.Empty(t, h.repos.instance.hour)
	})
	t.Run("stats off", func(t *testing.T) {
		h := newHarness(t)
		h.hooks.StatsForFederatedInst = false
		h.hooks.OnFollowersMovedAway(&model.User{ID: "old", Host: strPtr("moved.example")}, []string{"alice"})
		h.saveAll(t)
		assert.Empty(t, h.repos.instance.hour)
		assert.Len(t, h.repos.puFollow.hour["alice"], 1, "per-user chart does not depend on the stats flag")
	})
	t.Run("charts off", func(t *testing.T) {
		h := newHarness(t)
		h.hooks.ChartsForFederatedInst = false
		h.hooks.OnFollowersMovedAway(&model.User{ID: "old", Host: strPtr("moved.example")}, []string{"alice"})
		h.saveAll(t)
		assert.Empty(t, h.repos.instance.hour)
	})
	t.Run("remote charts off", func(t *testing.T) {
		h := newHarness(t)
		h.hooks.ChartsForRemoteUser = false
		h.hooks.OnFollowersMovedAway(&model.User{ID: "old", Host: strPtr("moved.example")}, []string{"alice"})
		h.saveAll(t)
		assert.Empty(t, h.repos.puFollow.hour)
		assert.Len(t, h.repos.instance.hour["moved.example"], 1)
	})
	t.Run("no-ops", func(t *testing.T) {
		h := newHarness(t)
		h.hooks.OnFollowersMovedAway(nil, []string{"alice"})
		h.hooks.OnFollowersMovedAway(&model.User{ID: "old", Host: strPtr("moved.example")}, nil)
		var nilHooks *Hooks
		nilHooks.OnFollowersMovedAway(&model.User{ID: "old"}, []string{"alice"})
		h.saveAll(t)
		assert.Empty(t, h.repos.puFollow.hour)
		assert.Empty(t, h.repos.instance.hour)
	})
}

// TestHooks_SetMetaSource pins that a wired meta source overrides the
// startup snapshot on every event, so admin toggles apply without a restart,
// and that a failed or empty fetch falls back to the snapshot fields.
func TestHooks_SetMetaSource(t *testing.T) {
	t.Run("live meta overrides snapshot", func(t *testing.T) {
		h := newHarness(t)
		counter := &fakeInstanceCounter{}
		h.hooks.InstanceCounter = counter
		meta := &model.Meta{
			EnableChartsForRemoteUser:         true,
			EnableChartsForFederatedInstances: true,
			EnableStatsForFederatedInstances:  false,
		}
		h.hooks.SetMetaSource(func() (*model.Meta, error) { return meta, nil })

		h.hooks.OnNoteCreated(&model.Note{ID: "n1", UserID: "bob", UserHost: strPtr("e.x")})
		assert.Empty(t, counter.snapshot(), "stats off in meta must win over the snapshot (true)")

		meta.EnableStatsForFederatedInstances = true
		h.hooks.OnNoteCreated(&model.Note{ID: "n2", UserID: "bob", UserHost: strPtr("e.x")})
		assert.Len(t, counter.snapshot(), 1, "toggling meta takes effect on the next event")

		meta.EnableChartsForFederatedInstances = false
		h.hooks.OnInboxReceived("req.example")
		meta.EnableChartsForRemoteUser = false
		h.hooks.OnNoteCreated(&model.Note{ID: "n3", UserID: "bob", UserHost: strPtr("e.x")})
		h.saveAll(t)
		assert.Empty(t, h.repos.instance.hour["req.example"])
		require.Len(t, h.repos.puNotes.hour["bob"], 1)
		assert.Equal(t, int64(2), toInt64(h.repos.puNotes.hour["bob"][0].Cols["total"]),
			"per-user notes chart stops once enableChartsForRemoteUser is off")
	})
	t.Run("fetch error falls back", func(t *testing.T) {
		h := newHarness(t)
		counter := &fakeInstanceCounter{}
		h.hooks.InstanceCounter = counter
		h.hooks.StatsForFederatedInst = false
		h.hooks.SetMetaSource(func() (*model.Meta, error) { return nil, errors.New("db down") })
		h.hooks.OnNoteCreated(&model.Note{ID: "n", UserID: "bob", UserHost: strPtr("e.x")})
		assert.Empty(t, counter.snapshot())

		h.hooks.StatsForFederatedInst = true
		h.hooks.SetMetaSource(func() (*model.Meta, error) { return nil, nil })
		h.hooks.OnNoteCreated(&model.Note{ID: "n", UserID: "bob", UserHost: strPtr("e.x")})
		assert.Len(t, counter.snapshot(), 1)
	})
}
