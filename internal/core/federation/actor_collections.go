package federation

import "github.com/elythia-network/elythia/internal/activitypub"

// actorCollectionsOnActorHost reports whether the actor's outbox, followers and
// following collections (when present) are on the actor's own host. When one
// is not, it returns that collection's name and false.
//
// 本家 ApPersonService.validateActor は 3 つの collection のどれかが actor と
// 別のホストなら `invalid Actor: ${collection} has different host` で actor ごと
// 拒否する (作成・更新とも)。mk-go は followers を followersUri として保存し、
// 受信したノートの可視性の判定に使う (#3330) ので、別ホストの値を受け入れると
// 判定の基準を他所のホストに置けてしまう。比較は inbox と同じ sameDeliveryHost
// (本家の punyHost と同じ正規形)。
func actorCollectionsOnActorHost(actor *activitypub.Person) (string, bool) {
	for _, c := range []struct {
		name  string
		value string
	}{
		{"outbox", actor.Outbox.String()},
		{"followers", actor.Followers.String()},
		{"following", actor.Following.String()},
	} {
		if c.value == "" {
			continue
		}
		if !sameDeliveryHost(c.value, actor.ID) {
			return c.name, false
		}
	}
	return "", true
}
