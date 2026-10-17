package cachebus

// AuthInvalidation is the payload of TopicAuth.
//
// **token も hash も載せない。** 受けた側は利用者単位で捨てる。
type AuthInvalidation struct {
	// UserIDs are the users whose cached authentications must be dropped.
	UserIDs []string `json:"userIds,omitempty"`
	// All drops every cached authentication.
	All bool `json:"all,omitempty"`
}

// AuthCache is the auth cache of one process (server/middleware.AuthMiddleware).
type AuthCache interface {
	SetInvalidationHook(fn func(userID string))
	InvalidateTokensForUserLocally(userID string)
	InvalidateAllLocally()
}

// ConnectAuth makes auth forward the users whose cached authentications it
// dropped to the other processes and apply theirs (#3664). sender identifies
// this process. It returns the function that stops receiving.
//
// 捨てる契機は凍結・解除・削除・token の再生成と失効・i/update・i/move で、どれも
// 人の操作なので、publish は同期で行う。凍結・削除・token の失効は、WebSocket を
// 閉じる失効の通知 (stream の credential:revoke) にも相乗りして捨てているので、
// 二重に捨てることになるが害は無い。通知を取りこぼしたプロセスでは、今までどおり
// TTL (30 秒) が上限になる。
func ConnectAuth(auth AuthCache, bus Bus, sender string) func() {
	if auth == nil || bus == nil {
		return func() {}
	}
	auth.SetInvalidationHook(func(userID string) {
		publishSync(bus, TopicAuth, sender, AuthInvalidation{UserIDs: []string{userID}})
	})
	return subscribe(bus, TopicAuth, sender, func(inv AuthInvalidation) {
		if inv.All {
			auth.InvalidateAllLocally()
			return
		}
		for _, id := range inv.UserIDs {
			auth.InvalidateTokensForUserLocally(id)
		}
	}, auth.InvalidateAllLocally)
}
