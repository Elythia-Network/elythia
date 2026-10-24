package server

import (
	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/api/oauth"
	apiresetpassword "github.com/elythia-network/elythia/internal/api/resetpassword"
	apisignin "github.com/elythia-network/elythia/internal/api/signin"
	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	coreabuse "github.com/elythia-network/elythia/internal/core/abuse"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
)

// routeParts holds the components setupRoutes builds in its own body (not in
// buildHandlers) whose dependencies are required constructor arguments
// (#3823). 本番では読まない。
//
// 構築子の必須の引数にした依存は、型では渡し忘れを防げても nil は渡せる。
// TestBuiltHandlersHaveNoNilDependencies が組み立てた後の部品のフィールドを見て、
// nil を渡した配線を落とすために持つ。setupRoutes は全部の setter を呼んだ後
// (recordCriticalWiring の直前) で代入する。
type routeParts struct {
	admin            *apiadmin.Handler
	signup           *apisignup.Handler
	signin           *apisignin.Handler
	resetPassword    *apiresetpassword.Handler
	oauth            *oauth.Handler
	emojiApplication *emojiapplication.Service
	abuseCreated     *coreabuse.CreatedNotifier
}
