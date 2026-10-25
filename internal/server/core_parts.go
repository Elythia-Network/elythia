package server

import (
	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	coreclip "github.com/elythia-network/elythia/internal/core/clip"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	coremodlog "github.com/elythia-network/elythia/internal/core/moderationlog"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	corepoll "github.com/elythia-network/elythia/internal/core/poll"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	coresignup "github.com/elythia-network/elythia/internal/core/signup"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
)

// coreParts holds the parts buildCore builds whose dependencies are required
// constructor arguments (#3825). 本番では読まない。
//
// core の欄を全部は並べない。構築子の必須の引数にした部品だけを持つ。
// TestBuiltHandlersHaveNoNilDependencies が組み立てた後の部品のフィールドを見て、
// nil を渡した配線を落とすために持つ (routeParts と同じ)。欄の名前は setupRoutes の
// 別名 (blockingService など) にする。core のフィールド名 (blocking・signup など) は
// apiHandlers・routeParts の欄とぶつかり、handlerNilAllowed の key が区別できない。
// setupRoutes は全部の setter を呼んだ後 (recordCriticalWiring の直前) で代入する。
type coreParts struct {
	// packer の要らない部品 (#3825 の 2 段目)。
	blockingService     *coreblocking.Service
	reactionService     *corereaction.Service
	signupService       *coresignup.Service
	clipService         *coreclip.Service
	antennaService      *coreantenna.Service
	pollService         *corepoll.Service
	modLogService       *coremodlog.Service
	notificationService *corenotification.Service

	// packer の要る部品 (#3825 の 3 段目)。
	noteCreateService    *corenote.CreateService
	followingService     *corefollowing.Service
	driveService         *coredrive.Service
	userService          *coreuser.Service
	webhookNoteHook      *corewebhook.NoteCreateHook
	webhookReactionHook  *corewebhook.ReactionCreateHook
	webhookFollowingHook *corewebhook.FollowingHook
}
