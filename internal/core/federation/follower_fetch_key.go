package federation

import (
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// followerKeyCandidates bounds how many local followers LocalFollowerKeys looks
// at before giving up.
//
// 取り直しは失敗の経路でだけ走るが、見つからない投稿を指す Announce は外から
// 送れる。フォロワーの読み出しも、フォロワーごとの利用者・ブロック・鍵の
// 問い合わせも、この数で頭打ちにする。先頭の人たちが使えないのは稀 (凍結・
// ブロック・鍵が無い) なので、取りこぼしは小さい。
const followerKeyCandidates = 16

// LocalFollowerKeys picks a local follower of a remote actor whose key signs
// the GET that retries a note the instance actor could not see (#3916): the
// target of the actor's Announce.
//
// A follower is used only when they are local, neither suspended nor deleted,
// externally available (localFederationObserver), have a keypair, and there is
// no block between them and the actor in either direction.
type LocalFollowerKeys struct {
	following repository.FollowingRepository
	users     repository.UserRepository
	keypairs  repository.UserKeypairRepository
	blocking  repository.BlockingRepository
	urls      *activitypub.URLBuilder
	observer  activitypub.LocalFederationObserver
	// hostAllowed は鍵を見せてよいホストか (配送を止めていない相手か) を答える。
	hostAllowed func(host string) bool
}

// NewLocalFollowerKeys returns a picker. observer may be nil (every local user
// is then treated as externally available). hostAllowed reports whether a
// local user's key may sign a GET to the host; with nil no key is picked.
//
// **配送を止めたホストには鍵を見せない。** 署名付きの GET を受け入れる側
// (ap の signedFetchHostAllowed) と同じく、配送する相手にだけローカルの利用者の
// keyId を出す。
func NewLocalFollowerKeys(
	following repository.FollowingRepository,
	users repository.UserRepository,
	keypairs repository.UserKeypairRepository,
	blocking repository.BlockingRepository,
	urls *activitypub.URLBuilder,
	observer activitypub.LocalFederationObserver,
	hostAllowed func(host string) bool,
) *LocalFollowerKeys {
	return &LocalFollowerKeys{following: following, users: users, keypairs: keypairs, blocking: blocking, urls: urls, observer: observer, hostAllowed: hostAllowed}
}

// KeyForFollowerOf returns the key of the first usable local follower of actor
// (in follower id order), or nil when there is none.
//
// **いつも同じ人を選ぶ。** 相手のサーバーに伝わる「こちらの誰か」を、取り直す
// たびに変えない。
func (k *LocalFollowerKeys) KeyForFollowerOf(actor *model.User) *activitypub.PrivateKey {
	if k == nil || actor == nil || actor.IsLocal() || k.hostAllowed == nil || !k.hostAllowed(*actor.Host) {
		return nil
	}
	ids, err := k.following.ListLocalFollowerIDsLimit(actor.ID, followerKeyCandidates)
	if err != nil {
		slog.Warn("federation: list local followers for a retry key failed", "actor", actor.ID, "err", err)
		return nil
	}
	for _, id := range ids {
		if key := k.keyFor(id, actor.ID); key != nil {
			return key
		}
	}
	return nil
}

// keyFor returns follower's key when they may sign on the actor's behalf.
func (k *LocalFollowerKeys) keyFor(followerID, actorID string) *activitypub.PrivateKey {
	u, err := k.users.FindByID(followerID)
	if err != nil || u == nil || !u.IsLocal() || u.IsSuspended || u.IsDeleted {
		return nil
	}
	if k.observer != nil && !k.observer.IsExternallyAvailable(u) {
		return nil
	}
	for _, pair := range [2][2]string{{followerID, actorID}, {actorID, followerID}} {
		blocked, err := k.blocking.Exists(pair[0], pair[1])
		if err != nil || blocked {
			return nil
		}
	}
	kp, err := k.keypairs.FindByUserID(followerID)
	if err != nil || kp == nil || kp.PrivateKey == "" {
		return nil
	}
	key, err := activitypub.NewPrivateKey(k.urls.UserKeyURI(followerID), kp.PrivateKey)
	if err != nil {
		return nil
	}
	return key
}

// OptionalRemoteDataGate reports whether data the server does not need may be
// exchanged with a host (the instance is neither blocked nor suspended).
// core/instance.Service implements it.
type OptionalRemoteDataGate interface {
	CanFetchOptionalRemoteData(host string) bool
}

// SoftwareSuspension reports whether a host runs software the server suspended
// (from delivery or from the inbox). *SuspendedChecker implements it.
type SoftwareSuspension interface {
	IsSuspended(host string) bool
}

// FetchKeyHostAllowed returns the hostAllowed of NewLocalFollowerKeys: a local
// user's key may sign a GET only to a host the server delivers to, the same
// hosts ap accepts a signed GET from (instance, delivery-suspended software
// and inbox-suspended software). A check passed as an untyped nil makes it
// refuse every host (a typed nil pointer is not caught).
//
// ap の signedFetchHostAllowed と違い、受信拒否のソフトウェアの判定が無くても断る
// (あちらは配線されていなければ通す)。本番はどちらも配線している。
func FetchKeyHostAllowed(instances OptionalRemoteDataGate, deliverSoftware, inboxSoftware SoftwareSuspension) func(host string) bool {
	return func(host string) bool {
		if host == "" || instances == nil || deliverSoftware == nil || inboxSoftware == nil {
			return false
		}
		return instances.CanFetchOptionalRemoteData(host) && !deliverSoftware.IsSuspended(host) &&
			!inboxSoftware.IsSuspended(host)
	}
}
