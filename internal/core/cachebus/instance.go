package cachebus

import "github.com/elythia-network/elythia/internal/core/instance"

// InstanceSuspensionInvalidation is the payload of TopicInstanceSuspension.
type InstanceSuspensionInvalidation struct {
	// Host is the host whose suspend decision must be dropped. Empty never
	// travels on the wire; a malformed message drops every host instead.
	Host string `json:"host"`
}

// ConnectInstanceSuspension makes svc forward its suspend-cache invalidations
// to the other processes and apply theirs (#3664). sender identifies this
// process. It returns the function that stops receiving.
//
// **配送の判定 (`ShouldSkipDelivery`) は配送のプロセスで走り、停止の操作は
// Web のプロセスで受ける。** 繋がないと、管理画面で停止しても配送のプロセスは
// 停止判定のキャッシュ (5 分) が切れるまで送り続ける。
//
// 通知を取りこぼしたプロセス (publish の失敗、購読の再接続の間) では、今までどおり
// cacheTTL が上限になる。受け側の待ち行列が溢れて捨てた分は、全体を捨てて埋める
// (subscribe)。
func ConnectInstanceSuspension(svc *instance.Service, bus Bus, sender string) func() {
	if svc == nil || bus == nil {
		return func() {}
	}
	svc.SetSuspendInvalidationHook(func(host string) {
		publishSync(bus, TopicInstanceSuspension, sender, InstanceSuspensionInvalidation{Host: host})
	})
	return subscribe(bus, TopicInstanceSuspension, sender,
		func(inv InstanceSuspensionInvalidation) {
			// 空の host は「全部」を意味するので、届いた空はそのまま全体を捨てる
			// (送る側は空を送らない。捨て損ねるより捨てすぎる側に倒す)。
			svc.DropSuspendCacheLocally(inv.Host)
		},
		func() { svc.DropSuspendCacheLocally("") })
}
