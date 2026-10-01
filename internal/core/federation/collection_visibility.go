package federation

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/model"
)

// errUnrecognizedCollection is returned when a remote actor's followers /
// following value does not resolve to a Collection or OrderedCollection.
// It mirrors upstream ApResolverService.resolveCollection's
// "unrecognized collection type" error, and like it is not a status error.
var errUnrecognizedCollection = errors.New("unrecognized collection type")

// errLocalCollection is returned when a remote actor's followers / following
// value points at this instance. upstream resolves such URLs locally, and
// nothing it renders there is a collection, so the result is the same
// non-status error as errUnrecognizedCollection.
var errLocalCollection = errors.New("collection points at the local instance")

// rawFollowCollections extracts the raw `followers` / `following` values of an
// actor document. Unreadable documents yield nil for both.
//
// キーは大文字小文字まで完全一致で引く。struct へ unmarshal すると
// encoding/json は `"Followers"` も拾うが、upstream (JS の `person.followers`)
// は区別する。
func rawFollowCollections(body []byte) (followers, following json.RawMessage) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, nil
	}
	return obj["followers"], obj["following"]
}

// jsonTruthy reports whether a raw JSON value is truthy in the JavaScript
// sense. Absent values, null, false, 0 and "" are falsy; everything else,
// including empty arrays and objects, is truthy.
//
// upstream の判定 (`if (collection)` / `resolved.first || resolved.items ||
// resolved.orderedItems`) は JS の truthiness なので、空配列 `[]` も「ある」に
// 数える。
func jsonTruthy(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// 数値のレンジ外 (`1e999`) などは JS では Infinity で truthy。
		return true
	}
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	default:
		return true
	}
}

// isPublicCollection reports whether a remote actor's followers / following
// collection is publicly listable.
//
// Mirrors upstream ApPersonService.isPublicCollection: an absent value is
// private; otherwise the collection (an IRI, fetched through the regular
// signed and SSRF-guarded AP fetch path, or an embedded object) must be a
// Collection / OrderedCollection, and it is public when it carries `first`,
// `items` or `orderedItems`.
//
// The collection id must be on the actor's host. upstream validateActor
// rejects the whole actor otherwise; mk-go keeps the actor and only treats
// the collection as unusable.
func (r *Resolver) isPublicCollection(actorID string, raw json.RawMessage) (bool, error) {
	if !jsonTruthy(raw) {
		return false, nil
	}
	var obj map[string]json.RawMessage
	var uri string
	switch {
	case json.Unmarshal(raw, &uri) == nil:
		// 別ホストの collection は取りに行かない。upstream では actor ごと
		// 弾かれるので、ここで取りに行くのは upstream に無い外向き取得になる。
		if !sameDeliveryHost(uri, actorID) {
			return false, ErrObjectHostMismatch
		}
		body, err := r.fetchCollection(uri)
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
			return false, errUnrecognizedCollection
		}
	case json.Unmarshal(raw, &obj) == nil && obj != nil:
		// 埋め込みの collection は upstream と同じく取りに行かずにそのまま見る。
		// id が無い / 別ホストのものは upstream では actor ごと弾かれる。
		var embeddedID string
		if err := json.Unmarshal(obj["id"], &embeddedID); err != nil || !sameDeliveryHost(embeddedID, actorID) {
			return false, ErrObjectHostMismatch
		}
	default:
		return false, errUnrecognizedCollection
	}
	switch singleAPType(obj["type"]) {
	case "Collection", "OrderedCollection":
	default:
		return false, errUnrecognizedCollection
	}
	return jsonTruthy(obj["first"]) || jsonTruthy(obj["items"]) || jsonTruthy(obj["orderedItems"]), nil
}

// fetchCollection fetches a collection document with the same guards the
// resolver applies to other fetched AP objects: no fragments, no local URLs,
// federation policy, ActivityStreams @context, and the id bound to both the
// responding host and the requested host.
func (r *Resolver) fetchCollection(uri string) ([]byte, error) {
	uri = trimWHATWGURL(uri)
	if strings.Contains(uri, "#") {
		return nil, ErrResolveFragment
	}
	// 呼び出し元 (isPublicCollection) が URI を actor のホストに縛っており、
	// actor のホストは取り込みの時点で自ホストでないことと連合の許可を確かめて
	// ある。下の 2 つは、その前提が崩れたときのための重ねた防御。
	if r.isSelfHostURI(uri) {
		return nil, errLocalCollection
	}
	if !r.hostAllowedForURI(uri) {
		return nil, ErrHostNotAllowed
	}
	body, finalURL, err := r.fetchObjectWithFinalURL(uri)
	if err != nil {
		return nil, err
	}
	var head struct {
		Context any    `json:"@context"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, errUnrecognizedCollection
	}
	if !hasActivityStreamsContext(head.Context) {
		return nil, errUnrecognizedCollection
	}
	id := trimWHATWGURL(head.ID)
	if err := assertResponseHostMatches(finalURL, id); err != nil {
		return nil, err
	}
	if err := assertRequestHostMatches(uri, id); err != nil {
		return nil, err
	}
	return body, nil
}

// isPermanentCollectionError reports whether a collection lookup failure is
// a non-retryable HTTP status (4xx other than 429). Mirrors upstream
// StatusError.isRetryable: every other failure (network errors, 5xx, 429,
// malformed documents) is treated as transient.
func isPermanentCollectionError(err error) bool {
	var se *activitypub.StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.StatusCode >= 400 && se.StatusCode < 500 && se.StatusCode != 429
}

// collectionVisibility converts isPublicCollection's verdict to the stored
// visibility.
func collectionVisibility(public bool) model.FollowingVisibility {
	if public {
		return model.FollowingVisibilityPublic
	}
	return model.FollowingVisibilityPrivate
}

// followVisibilitiesOnCreate resolves both collections concurrently for a
// newly created actor. upstream createPerson も 2 本を Promise.all で並べる。
// 直列だと、遅い相手で取り込みが最大 2 回分の timeout だけ延びる。
func (r *Resolver) followVisibilitiesOnCreate(actorURI string, followingRaw, followersRaw json.RawMessage) (following, followers model.FollowingVisibility) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 子 goroutine の panic は job / HTTP の recover に届かずプロセスごと
		// 落ちるので、ここで止めて失敗と同じく非公開に倒す。
		defer func() {
			if p := recover(); p != nil {
				slog.Error("federation: recovered panic while resolving follow collection",
					"actor", truncateRunes(actorURI, userURIMaxRunes), "collection", "followers", "panic", p)
				followers = model.FollowingVisibilityPrivate
			}
		}()
		followers = r.followVisibilityOnCreate(actorURI, "followers", followersRaw)
	}()
	following = r.followVisibilityOnCreate(actorURI, "following", followingRaw)
	<-done
	return following, followers
}

// followVisibilitiesOnUpdate is followVisibilitiesOnCreate for the update
// path; ok is false when the stored value should be kept.
func (r *Resolver) followVisibilitiesOnUpdate(actorURI string, followingRaw, followersRaw json.RawMessage) (following model.FollowingVisibility, followingOK bool, followers model.FollowingVisibility, followersOK bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// panic は一時的な失敗と同じく保存値を残す (followersOK=false)。
		defer func() {
			if p := recover(); p != nil {
				slog.Error("federation: recovered panic while resolving follow collection",
					"actor", truncateRunes(actorURI, userURIMaxRunes), "collection", "followers", "panic", p)
				followers, followersOK = "", false
			}
		}()
		followers, followersOK = r.followVisibilityOnUpdate(actorURI, "followers", followersRaw)
	}()
	following, followingOK = r.followVisibilityOnUpdate(actorURI, "following", followingRaw)
	<-done
	return following, followingOK, followers, followersOK
}

// followVisibilityOnCreate computes the followers / following visibility of
// a newly imported remote actor. Any failure yields private, like upstream
// createPerson.
func (r *Resolver) followVisibilityOnCreate(actorURI, which string, raw json.RawMessage) model.FollowingVisibility {
	public, err := r.isPublicCollection(actorURI, raw)
	if err != nil {
		r.logCollectionError(actorURI, which, err)
		return model.FollowingVisibilityPrivate
	}
	return collectionVisibility(public)
}

// followVisibilityOnUpdate computes the followers / following visibility of
// a refreshed remote actor. ok=false means a transient failure: the stored
// value must be kept, like upstream updatePerson. A non-retryable status
// yields private.
func (r *Resolver) followVisibilityOnUpdate(actorURI, which string, raw json.RawMessage) (model.FollowingVisibility, bool) {
	public, err := r.isPublicCollection(actorURI, raw)
	if err != nil {
		if isPermanentCollectionError(err) {
			return model.FollowingVisibilityPrivate, true
		}
		r.logCollectionError(actorURI, which, err)
		return "", false
	}
	return collectionVisibility(public), true
}

// logCollectionError logs transient collection lookup failures. upstream も
// retryable な失敗だけをログに出す (4xx は非公開の意思表示として扱う)。
func (r *Resolver) logCollectionError(actorURI, which string, err error) {
	if isPermanentCollectionError(err) {
		return
	}
	slog.Warn("federation: failed to resolve follow collection",
		"actor", truncateRunes(actorURI, userURIMaxRunes), "collection", which, "err", err)
}
