package ap

import (
	"github.com/elythia-network/elythia/internal/activitypub"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
)

// KeyedRemoteFetcher fetches a remote object with a GET signed by the given
// key, without an unsigned fallback (#3915). Implemented by
// federation.APFetcher.
type KeyedRemoteFetcher interface {
	FetchObjectSignedBy(uri string, key *activitypub.PrivateKey) ([]byte, error)
}

// KeyedNoteResolver resolves a remote note, retrying with the given key when
// the instance actor cannot see it (#3915). Implemented by federation.Resolver.
type KeyedNoteResolver interface {
	ResolveNoteAllowCrossHostWithKey(uri string, key *activitypub.PrivateKey) (*model.Note, error)
}

// refetchAsViewer fetches uri again signed with viewer's own key after the
// instance actor's fetch failed with fetchErr (#3915).
//
// It only retries when fetchErr is 401, 403 or 404, the answers of a server
// that decides by the signer, and when viewer follows someone on uri's host.
// ok is false when it did not retry, or when the retry was refused or not found
// too; the caller then keeps fetchErr. key is the viewer's key when the retry
// succeeded, and nil otherwise.
func (h *Handler) refetchAsViewer(viewer *model.User, uri string, fetchErr error) (data []byte, key *activitypub.PrivateKey, err error, ok bool) {
	if fetchErr == nil || !corefederation.HiddenFromInstanceActor(fetchErr) {
		return nil, nil, nil, false
	}
	kf, isKeyed := h.remoteFetcher.(KeyedRemoteFetcher)
	if !isKeyed || !h.viewerFollowsHostOf(viewer, uri) {
		return nil, nil, nil, false
	}
	vk := h.viewerFetchKey(viewer)
	if vk == nil {
		return nil, nil, nil, false
	}
	data, err = kf.FetchObjectSignedBy(uri, vk)
	if err != nil {
		// 鍵でも見せてもらえなかったときは、instance actor で取ったときのエラーを
		// そのまま使う (404 なら NO_SUCH_OBJECT、401・403 なら REQUEST_FAILED で、
		// 取り直さなかったときと同じ)。
		if corefederation.HiddenFromInstanceActor(err) {
			return nil, nil, nil, false
		}
		return nil, nil, err, true
	}
	return data, vk, nil, true
}

// viewerFollowsHostOf reports whether viewer follows at least one user on the
// host of uri.
//
// **フォローしているホストにだけ鍵を見せる。** 404 は相手がいつでも返せるので、
// 「見えなかったときだけ」では絞れない。リンクを踏ませるだけで ap/show が呼ばれる
// 経路 (frontend の share ページ) もあり、条件が無いと、任意のホストが照会した
// 人の actor を知れる。フォロワー限定の投稿を見られるのはフォローしている人だけ
// なので、この条件で目的は損なわない。
func (h *Handler) viewerFollowsHostOf(viewer *model.User, uri string) bool {
	host := apShowGateHost(uri)
	if viewer == nil || host == "" || h.followingRepo == nil {
		return false
	}
	follows, err := h.followingRepo.FollowsAnyOnHost(viewer.ID, host)
	return err == nil && follows
}

// resolveRemoteNote resolves a remote note for ap/show. With key (the viewer's
// key, set when only the viewer could fetch the note) the resolver retries the
// note with it (not its quote targets).
func (h *Handler) resolveRemoteNote(uri string, key *activitypub.PrivateKey) (*model.Note, error) {
	if key != nil {
		if kr, ok := h.remoteResolver.(KeyedNoteResolver); ok {
			return kr.ResolveNoteAllowCrossHostWithKey(uri, key)
		}
	}
	return h.remoteResolver.ResolveNoteAllowCrossHost(uri)
}

// viewerFetchKey returns viewer's signing key for fetching on their behalf, or
// nil when their key must not be used.
//
// 相手のサーバーには、この利用者の actor が見える (keyId を引きに来る)。連合に
// 出していない利用者 (localFederationObserver が外に出さないと判断した人) の鍵は
// 使わない。凍結・削除された利用者も同じ。
func (h *Handler) viewerFetchKey(viewer *model.User) *activitypub.PrivateKey {
	if viewer == nil || !viewer.IsLocal() || viewer.IsSuspended || viewer.IsDeleted || h.keypairRepo == nil {
		return nil
	}
	if h.localFederationObserver != nil && !h.localFederationObserver.IsExternallyAvailable(viewer) {
		return nil
	}
	kp, err := h.keypairRepo.FindByUserID(viewer.ID)
	if err != nil || kp == nil || kp.PrivateKey == "" {
		return nil
	}
	key, err := activitypub.NewPrivateKey(h.renderer.URLs().UserKeyURI(viewer.ID), kp.PrivateKey)
	if err != nil {
		return nil
	}
	return key
}
