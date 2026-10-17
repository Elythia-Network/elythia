package cachebus

import "github.com/elythia-network/elythia/internal/core/relay"

// RelayInvalidation is the payload of TopicRelay. The cache is a single list,
// so there is nothing to name.
type RelayInvalidation struct{}

// ConnectRelays makes svc forward its accepted-relay list invalidations to the
// other processes and apply theirs (#3664). sender identifies this process. It
// returns the function that stops receiving.
//
// 通知を取りこぼしたプロセスでは、今までどおり一覧の TTL (10 分) が上限になる。
func ConnectRelays(svc *relay.Service, bus Bus, sender string) func() {
	if svc == nil || bus == nil {
		return func() {}
	}
	svc.SetInvalidationHook(func() {
		publishSync(bus, TopicRelay, sender, RelayInvalidation{})
	})
	return subscribe(bus, TopicRelay, sender,
		func(RelayInvalidation) { svc.InvalidateCacheLocally() },
		svc.InvalidateCacheLocally)
}
