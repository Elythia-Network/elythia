package middleware

import "github.com/labstack/echo/v5"

// IPObserver records the IP an authenticated request arrived from.
// Implemented by core/iplog.Service。interface で受けるのは、middleware から
// core を直接参照させないためと、テストで差し替えるため。
type IPObserver interface {
	Record(userID, ip string)
}

// RecordClientIP notes the client IP of every authenticated request (#3103).
//
// **`Authenticate` の後ろに置く。** 前に置くと `UserContextKey` がまだ積まれて
// おらず、常に何もしない middleware になる (エラーにもならない)。
//
// upstream は `ApiCallService` から `logIp` を呼ぶので契機は「API 呼び出し」だが、
// こちらは「認証済みリクエスト」でわずかに広い。observer 側が (利用者, IP) を
// 窓で重複排除するので、書き込み回数としての差は出ない。
//
// 記録するかどうか (`meta.enableIpLogging`) の判断は observer 側に任せる。
// ここで meta を読むと、middleware が meta repository を持つことになる。
func RecordClientIP(observer IPObserver) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			// **プロセス内の呼び出しは記録しない。** 実在しない
			// `127.0.0.1` が `user_ip` に入ると、関連アカウント検索
			// (#3105) の材料が汚れる。
			if observer != nil && !IsInternalCall(c.Request().Context()) {
				if u := GetUser(c); u != nil {
					// **`RealIP()` をそのまま渡す。** 正規化は observer 側で行う
					// (検索側と同じ実装を通すため)。
					observer.Record(u.ID, c.RealIP())
				}
			}
			return next(c)
		}
	}
}
