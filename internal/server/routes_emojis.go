package server

import (
	"github.com/elythia-network/elythia/internal/api/avatardecorations"
	apiemojiapplications "github.com/elythia-network/elythia/internal/api/emojiapplications"
	apiemojis "github.com/elythia-network/elythia/internal/api/emojis"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerEmojisRoutes registers the custom emoji, emoji application and
// avatar decoration routes on the /api group.
func registerEmojisRoutes(api apiRoutes, emojisHandler *apiemojis.Handler, emojiApplicationHandler *apiemojiapplications.Handler, avatarDecorationsHandler *avatardecorations.Handler, roleService *corerole.Service) {
	api.POST("/emojis", emojisHandler.Emojis)
	api.GET("/emojis", emojisHandler.Emojis)
	api.POST("/emoji", emojisHandler.Emoji)
	api.GET("/emoji", emojisHandler.Emoji)

	// **申請できる人をロールで絞る (#2934)。** canManageCustomEmojis を持つ人は
	// 申請ではなく直接登録できるので、この policy は「登録はできないが頼める人」。
	//
	// **第三者アプリのトークンは入れない (#3037)。** この 3 つは mk-go 独自
	// なので upstream の `kind` にあたる宣言が無く、`RequireScope` を配線
	// できない。upstream は「`kind` が無く、かつ資格情報を要する endpoint」に
	// 対し app token を一律で拒否する (`ApiCallService.ts:412-413`) ので、
	// 同じ側に倒す。本体の他の kind 無し endpoint は `RequireSecure` で
	// もっと強く塞がれており、抜けていたのはここだけ。
	api.POST("/emoji-application/create", emojiApplicationHandler.Create,
		middleware.RequireAuth(),
		middleware.RejectAppToken(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanRequestCustomEmojis))
	// **一覧と取り下げは policy で塞がない。** 後から policy を外された人が
	// 自分の申請を確認することも取り下げることもできなくなる。
	api.POST("/emoji-application/list-mine", emojiApplicationHandler.ListMine, middleware.RequireAuth(), middleware.RejectAppToken())
	api.POST("/emoji-application/cancel", emojiApplicationHandler.Cancel, middleware.RequireAuth(), middleware.RejectAppToken())

	// get-avatar-decorations — アバターデコレーション全件取得
	api.POST("/get-avatar-decorations", avatarDecorationsHandler.Get)

	// export-custom-emojis — 全 local custom emoji を zip (画像 + meta.json) に
	// export する非同期ジョブを enqueue する (#1217)。upstream 同様 endpoint は
	// 即 204 を返し、生成完了後に exportCompleted 通知 + drive file で受け取る。
	api.POST("/export-custom-emojis", emojisHandler.ExportCustomEmojis,
		middleware.RequireAuth(), middleware.RequireSecure())
}
