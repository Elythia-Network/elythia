package server

import (
	corebubbleversus "github.com/elythia-network/elythia/internal/core/bubbleversus"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/event"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/stream/channels"
)

// streamParts holds the streaming parts setupRoutes builds through
// buildStream (#3692): the channel registry, the connection manager and the
// publishers setupRoutes hands out after the stream section starts. The
// pubsub bus is built there too but only handed to the manager, so it has no
// field.
//
// Fields are named after the local variables setupRoutes has always used and
// keep the concrete type the constructor returns. Only parts whose
// constructor just builds a value are listed: no Redis access (the pubsub
// connects on its first Subscribe, which setupRoutes still calls), no log, no
// goroutine, timer, shutdown hook or global write, and dependencies limited
// to the stream pubsub and the ID generator.
//
// Streaming parts that stay in setupRoutes (docs/architecture.md 4.2): the
// publishers that receivers before the stream section need (note events,
// drive, reversi and main stream for core services, the federation processor
// and handlers, built at the top of setupRoutes since #3676; the user list
// publisher for usersHandler), the server and queue stats publishers (they start
// a ticker and register a shutdown hook), and the publishers built inside the
// arguments of another constructor (the admin stream publisher of the abuse
// report notifier, the bubble game publisher of its service).
//
// buildStream only constructs. Every setter on these parts is still called at
// its old position in setupRoutes.
type streamParts struct {
	streamRegistry        *stream.Registry
	streamManager         *stream.Manager
	streamRevokePublisher *stream.StreamRevokePublisher
	notePublisher         *stream.NotePublisher
	notificationPublisher *stream.NotificationPublisher
	broadcastPublisher    *stream.BroadcastPublisher
	chatPublisher         *stream.ChatPublisher
}

// buildStream builds the streaming parts on streamPubSub, in the order
// setupRoutes used to build them. It reads no Server field, so it is a plain
// function: streamPubSub is the value setupRoutes creates (and stores in
// s.streamPubSub) at its top, passed in as a variable like every earlier group
// (docs/architecture.md 4.2).
func buildStream(streamPubSub *event.PubSubService, idGen id.Generator) streamParts {
	// 下の行は setupRoutes にあった行をそのまま移した。ゲートが構築の式を引数まで
	// 含めて照合しうるので、引数も setupRoutes と同じ名前で受ける。
	//
	// Redis pubsub bus (核となる publish/subscribe チャンネル)。作っても購読は
	// 始めない。購読は setupRoutes の stream の節の Subscribe… で始める。
	streamBus := stream.NewEventPubSubBus(streamPubSub)
	// Channel registry。registry は factory を map に入れるだけで、引くのは接続を
	// 受けたとき (dispatcher の Lookup)。
	streamRegistry := stream.NewRegistry()
	// Connection manager。registry と bus の参照を持つだけで、接続も購読も始めない。
	streamManager := stream.NewManager(streamRegistry, streamBus)
	// 失効を扱う handler へ配る revoke publisher。自プロセスの Manager も渡す —
	// publish が失敗しても、ここに居る接続は閉じる。
	streamRevokePublisher := stream.NewStreamRevokePublisher(streamPubSub, streamManager)
	notePublisher := stream.NewNotePublisher(streamPubSub, idGen)
	notificationPublisher := stream.NewNotificationPublisher(streamPubSub)
	// broadcast stream publisher (emoji / global announcement 等を全 connection へ、#2046/#2056)。
	broadcastPublisher := stream.NewBroadcastPublisher(streamPubSub)
	chatPublisher := stream.NewChatPublisher(streamPubSub)

	return streamParts{
		streamRegistry:        streamRegistry,
		streamManager:         streamManager,
		streamRevokePublisher: streamRevokePublisher,
		notePublisher:         notePublisher,
		notificationPublisher: notificationPublisher,
		broadcastPublisher:    broadcastPublisher,
		chatPublisher:         chatPublisher,
	}
}

// streamChannelDeps holds what the built-in channel factories are built from
// (#3692). setupRoutes fills it from the same local names its setters use, so
// a registered factory is bound to the service or publisher the setters
// configured (the reversi, bubble game and chat services, the stats
// publishers, the cached antenna repository).
//
// Field names differ from the local variable names, so a gate that matches a
// "name: name" line elsewhere does not also match this literal.
type streamChannelDeps struct {
	antennas     repository.AntennaRepository
	userLists    repository.UserListRepository
	clips        repository.ClipRepository
	roles        *corerole.Service
	serverStats  *stream.ServerStatsPublisher
	queueStats   *stream.QueueStatsPublisher // nil when the server has no queue inspector
	reversi      *corereversi.Service
	bubbleVersus *corebubbleversus.Service
	chatRoom     *channels.ChatRoomFactory
	chat         *corechat.Service
}

// streamChannel pairs a channel name, as clients send it in
// connect.body.channel, with its factory and whether only an authenticated
// connection may subscribe.
type streamChannel struct {
	name         string
	factory      stream.ChannelFactory
	credentialed bool
}

// builtinStreamChannels returns the built-in WebSocket channels (#3692). It is
// the only place that says which factory serves which channel name and which
// channels require credentials. Each name appears once: the registry keeps
// factories in a map keyed by name, so a second entry would silently replace
// the first (and Register would not clear a credentialed flag set earlier).
//
// queueStats is the one channel whose factory depends on the server: with a
// queue inspector it reads the history of the queue stats publisher, without
// one (tests and tools) it answers with empty data.
func builtinStreamChannels(d streamChannelDeps) []streamChannel {
	const (
		anyone       = false
		credentialed = true
	)
	// queueInspector が無い起動 (テスト等) では、publisher を作らず fallback の
	// factory で空配列だけ返す。
	var queueStats stream.ChannelFactory = channels.NewQueueStats
	if d.queueStats != nil {
		queueStats = channels.NewQueueStatsFactory(d.queueStats)
	}
	return []streamChannel{
		{"homeTimeline", channels.NewHomeTimeline, credentialed},
		{"localTimeline", channels.NewLocalTimeline, anyone},
		{"globalTimeline", channels.NewGlobalTimeline, anyone},
		{"hybridTimeline", channels.NewHybridTimeline, credentialed},
		{"notifications", channels.NewNotifications, credentialed},
		{"main", channels.NewMain, credentialed},
		{"drive", channels.NewDrive, credentialed},
		{"hashtag", channels.NewHashtag, anyone},
		{"antenna", channels.NewAntennaFactory(d.antennas).New, credentialed},
		{"channel", channels.NewChannelTimeline, anyone},
		// userList は membership lookup を渡して per-member withReplies gate を有効化 (#2020)。
		{"userList", channels.NewUserListFactory(d.userLists).New, anyone},
		// clip は本家と同じく未ログインでも公開クリップを購読できる (#3809)。
		{"clip", channels.NewClipFactory(d.clips).New, anyone},
		{"roleTimeline", channels.NewRoleTimelineFactory(d.roles).New, anyone},
		{"admin", channels.NewAdminFactory(d.roles).New, credentialed},
		// publisher が ring buffer を持つので、StatsLogProvider として渡して requestLog
		// 応答に historical 値を返す (#571 item 2)。
		{"serverStats", channels.NewServerStatsFactory(d.serverStats), anyone},
		{"queueStats", queueStats, anyone},
		{"reversiGame", channels.NewReversiGameFactory(d.reversi).New, anyone},
		{"reversi", channels.NewReversi, credentialed},
		{"bubbleVersus", channels.NewBubbleVersus, credentialed},
		{"bubbleVersusMatch", channels.NewBubbleVersusMatchFactory(d.bubbleVersus).New, credentialed},
		// 起動時の検査 (streamChannels.chatRoom.service) が見るのと同じ factory を登録する。
		{"chatRoom", d.chatRoom.New, credentialed},
		{"chatUser", channels.NewChatUserFactory(d.chat).New, credentialed},
	}
}

// registerStreamChannels registers cs on r, in order. Registration only fills
// the registry's maps; the dispatcher looks a channel up when a client
// connects to it. setupRoutes calls it once, before it mounts /streaming.
func registerStreamChannels(r *stream.Registry, cs []streamChannel) {
	for _, c := range cs {
		if c.credentialed {
			r.RegisterCredentialed(c.name, c.factory)
		} else {
			r.Register(c.name, c.factory)
		}
	}
}
