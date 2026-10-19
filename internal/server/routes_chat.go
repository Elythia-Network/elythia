package server

import (
	apichat "github.com/elythia-network/elythia/internal/api/chat"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// registerChatRoutes registers the chat routes on the /api group.
func registerChatRoutes(api apiRoutes, chatHandler *apichat.Handler, roleService *corerole.Service) {
	// #1796: 全 chat endpoint に chatAvailability role-policy gate (upstream
	// checkChatAvailability(me.id, 'read'|'write')) を付与する。mode は upstream
	// の各 endpoint 呼び出しに一致させる (read = available|readonly、write =
	// available のみ)。RequireScope の scope (read/write:chat) とは一致しない
	// ものがある点に注意 (例: rooms/members は write:chat scope だが read availability)。
	chatRead := func() echo.MiddlewareFunc { return middleware.RequireChatAvailability(roleService, "read") }
	chatWrite := func() echo.MiddlewareFunc { return middleware.RequireChatAvailability(roleService, "write") }

	api.POST("/chat/rooms/create", chatHandler.RoomsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/show", chatHandler.RoomsShow, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/update", chatHandler.RoomsUpdate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/delete", chatHandler.RoomsDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/owned", chatHandler.RoomsOwned, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/joined", chatHandler.RoomsJoined, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/leave", chatHandler.RoomsLeave, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/mute", chatHandler.RoomsMute, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/unmute", chatHandler.RoomsUnmute, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/transfer-ownership", chatHandler.RoomsTransferOwnership, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/join", chatHandler.RoomsJoin, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/joining", chatHandler.RoomsJoining, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/members", chatHandler.RoomsMembers, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatRead())
	api.POST("/chat/rooms/invitations/create", chatHandler.InvitationsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/delete", chatHandler.InvitationsDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/accept", chatHandler.InvitationsAccept, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/reject", chatHandler.InvitationsReject, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/ignore", chatHandler.InvitationsIgnore, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/inbox", chatHandler.InvitationsInbox, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/invitations/outbox", chatHandler.InvitationsOutbox, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/members/ban", chatHandler.MembersBan, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/members/update-membership", chatHandler.MembersUpdateMembership, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages", chatHandler.Messages, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	// create は create-to-user / create-to-room と同じ処理を呼ぶので、移行済みの
	// アカウントを拒む RequireNotMoved も揃える。どれか1本でも欠くと、そこから迂回できる (#3437)。
	api.POST("/chat/messages/create", chatHandler.MessagesCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/create-to-user", chatHandler.MessagesCreateToUser, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/create-to-room", chatHandler.MessagesCreateToRoom, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/show", chatHandler.MessagesShow, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/update", chatHandler.MessagesUpdate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/delete", chatHandler.MessagesDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/read", chatHandler.MessagesRead, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatRead())
	api.POST("/chat/messages/search", chatHandler.MessagesSearch, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/user-timeline", chatHandler.UserTimeline, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/room-timeline", chatHandler.RoomTimeline, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/react", chatHandler.ReactionsCreate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/unreact", chatHandler.ReactionsDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/reactions/create", chatHandler.ReactionsCreate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/reactions/delete", chatHandler.ReactionsDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/history", chatHandler.History, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/read-all", chatHandler.ReadAll, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatRead())
	api.POST("/chat/unread-count", chatHandler.UnreadCount, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
}
