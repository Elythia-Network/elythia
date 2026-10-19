package server

import (
	"github.com/elythia-network/elythia/internal/core/event"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/stream"
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
