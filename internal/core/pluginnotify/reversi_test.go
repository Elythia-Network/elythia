package pluginnotify

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// リバーシの招待と対局の進みを、プラグインが管理するアカウントへ積む (#3942)。

func TestReversiInvited_DeliversToManagedInvitee(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(managed("acc-a", "bot-a"))})
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return at }

	d.ReversiInvited(context.Background(), "acc-a", "alice", "game1")
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "bot-a", enq.calls[0].plugin)
	assert.Equal(t, Event{
		ID: "reversiInvited:game1", Type: TypeReversiInvited, AccountID: "acc-a", UserID: "alice", GameID: "game1", CreatedAt: at,
	}, enq.calls[0].ev)
}

func TestReversiInvited_Skips(t *testing.T) {
	pairs := map[string]bool{"mute:acc-a>muted": true, "block:acc-a>blocked": true}
	us := users(managed("acc-a", "bot-a"), managed("acc-c", "bot-c"))
	cases := map[string][3]string{
		"普通の利用者への招待":         {"normal", "alice", "g"},
		"宣言していないプラグインのアカウント": {"acc-c", "alice", "g"},
		"招待された人が見つからない":      {"ghost", "alice", "g"},
		"自分からの招待":            {"acc-a", "acc-a", "g"},
		"対局の ID が無い":         {"acc-a", "alice", ""},
		"ミュートした相手から":         {"acc-a", "muted", "g"},
		"ブロックした相手から":         {"acc-a", "blocked", "g"},
		"凍結中の相手から":           {"acc-a", "frozen", "g"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			enq := &fakeEnqueuer{}
			uu := us
			uu.users["frozen"] = &model.User{ID: "frozen", IsSuspended: true}
			d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: uu, Mutes: fakeRelations{pairs: pairs}, Blocks: fakeBlocks{pairs: pairs}})
			d.ReversiInvited(context.Background(), c[0], c[1], c[2])
			assert.Empty(t, enq.calls)
		})
	}
}

func TestReversiGameEvent_DeliversToEachManagedPlayer(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a", "bot-b"}, Deps{Enqueuer: enq, Users: users(managed("acc-a", "bot-a"), managed("acc-b", "bot-b"))})
	at := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return at }

	d.ReversiGameEvent(context.Background(), &model.ReversiGame{ID: "game1", User1ID: "acc-a", User2ID: "acc-b"}, "log")
	require.Len(t, enq.calls, 2)
	byPlugin := map[string]Event{}
	for _, c := range enq.calls {
		byPlugin[c.plugin] = c.ev
	}
	// UserID は相手。自分の手で起きたイベントでも相手を入れる
	assert.Equal(t, "acc-a", byPlugin["bot-a"].AccountID)
	assert.Equal(t, "acc-b", byPlugin["bot-a"].UserID)
	assert.Equal(t, "acc-b", byPlugin["bot-b"].AccountID)
	assert.Equal(t, "acc-a", byPlugin["bot-b"].UserID)
	for _, ev := range byPlugin {
		assert.Equal(t, TypeReversiGame, ev.Type)
		assert.Equal(t, "game1", ev.GameID)
		assert.Equal(t, "log", ev.GameEvent)
		assert.True(t, at.Equal(ev.CreatedAt))
		assert.Contains(t, ev.ID, "game1")
	}
}

// 同じ対局の同じ種類のイベント (1 手ごとの log) は、別の ID で積む。同じ ID だと
// 冪等に書いたプラグインが 2 手目以降を捨てる。
func TestReversiGameEvent_GivesEachEventItsOwnID(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(managed("acc-a", "bot-a"))})
	tick := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	d.now = func() time.Time { tick = tick.Add(time.Millisecond); return tick }
	game := &model.ReversiGame{ID: "game1", User1ID: "acc-a", User2ID: "alice"}
	d.ReversiGameEvent(context.Background(), game, "log")
	d.ReversiGameEvent(context.Background(), game, "log")
	require.Len(t, enq.calls, 2)
	assert.NotEqual(t, enq.calls[0].ev.ID, enq.calls[1].ev.ID)
}

// 始めた対局の進みなので、相手をミュート・ブロックしていても届ける。
func TestReversiGameEvent_IgnoresMutesAndBlocks(t *testing.T) {
	pairs := map[string]bool{"mute:acc-a>alice": true, "block:acc-a>alice": true}
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(managed("acc-a", "bot-a")), Mutes: fakeRelations{pairs: pairs}, Blocks: fakeBlocks{pairs: pairs}})
	d.ReversiGameEvent(context.Background(), &model.ReversiGame{ID: "g", User1ID: "alice", User2ID: "acc-a"}, "started")
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "acc-a", enq.calls[0].ev.AccountID)
}

func TestReversiGameEvent_SkipsGamesWithoutManagedPlayers(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(managed("acc-c", "bot-c"), managed("acc-a", "bot-a"))})
	d.ReversiGameEvent(context.Background(), &model.ReversiGame{ID: "g", User1ID: "alice", User2ID: "bob"}, "log")
	d.ReversiGameEvent(context.Background(), &model.ReversiGame{ID: "g", User1ID: "alice", User2ID: "acc-c"}, "log")
	d.ReversiGameEvent(context.Background(), &model.ReversiGame{ID: "g", User1ID: "alice", User2ID: "ghost"}, "log")
	d.ReversiGameEvent(context.Background(), nil, "log")
	d.ReversiGameEvent(context.Background(), &model.ReversiGame{ID: "g", User1ID: "acc-a", User2ID: "alice"}, "")
	assert.Empty(t, enq.calls)
}

// 同じプラグインの 2 つのアカウントが対局しても、2 件の ID は別。同じだと、ID で冪等に
// 書いたプラグインが片方の通知を捨てる。
func TestReversiGameEvent_SamePluginAccountsGetDistinctIDs(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(managed("acc-a", "bot-a"), managed("acc-a2", "bot-a"))})
	d.now = func() time.Time { return time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC) }
	d.ReversiGameEvent(context.Background(), &model.ReversiGame{ID: "g", User1ID: "acc-a", User2ID: "acc-a2"}, "log")
	require.Len(t, enq.calls, 2)
	assert.NotEqual(t, enq.calls[0].ev.ID, enq.calls[1].ev.ID)
}

// 自分自身との対局 (作る経路はどれも弾いている) では積まない。
func TestReversiGameEvent_SkipsAGameAgainstItself(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(managed("acc-a", "bot-a"))})
	d.ReversiGameEvent(context.Background(), &model.ReversiGame{ID: "g", User1ID: "acc-a", User2ID: "acc-a"}, "log")
	assert.Empty(t, enq.calls)
}
