package federation

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ErrSignedFetchUnknownSigner is returned by SignedFetchAuthenticator when the
// signer is not a remote user already stored in the database.
var ErrSignedFetchUnknownSigner = errors.New("signed fetch: signer is not a known remote user")

// signedFetchKeyLookup is the part of Resolver that returns a stored public key
// for an actor. It reads the database (and Resolver's in-memory copy of it) only.
type signedFetchKeyLookup interface {
	PublicKeyForKeyID(actorID, keyID string) (string, error)
}

// SignedFetchAuthenticator identifies the remote user who signed a GET for an
// ActivityPub object, so the object can be shown to that user when it is not
// public (#3914).
//
// It never fetches anything. The signer and its key must already be in the
// database; an unknown keyId is rejected rather than resolved.
type SignedFetchAuthenticator struct {
	users        repository.UserRepository
	keys         signedFetchKeyLookup
	keyCache     *activitypub.PublicKeyCache
	expectedHost string
}

// NewSignedFetchAuthenticator returns an authenticator that binds signatures to
// expectedHost (config host, host:port). keys is normally the Resolver.
func NewSignedFetchAuthenticator(users repository.UserRepository, keys signedFetchKeyLookup, expectedHost string) *SignedFetchAuthenticator {
	return &SignedFetchAuthenticator{
		users:        users,
		keys:         keys,
		keyCache:     activitypub.NewPublicKeyCache(0),
		expectedHost: expectedHost,
	}
}

// AuthenticateFetch returns the remote user whose stored key signed req.
//
// The signature must cover (request-target), host and date, Host must be this
// server, and Date must be readable and within the clock skew
// (activitypub.VerifyFetchAdmission). The signer is looked up by the keyId
// without its fragment, the same actor URI the inbox resolves.
func (a *SignedFetchAuthenticator) AuthenticateFetch(req *http.Request) (*model.User, error) {
	parsed, err := activitypub.ParseSignatureHeader(req.Header.Get("Signature"))
	if err != nil {
		return nil, err
	}
	// 鍵を引く前に、この GET に縛られた署名かを見る。通らない要求で DB を引かない。
	if err := activitypub.VerifyFetchAdmission(parsed, req.Host, a.expectedHost,
		activitypub.InboxDateHeader(req.Header, parsed.Headers)); err != nil {
		return nil, err
	}
	// **知らない actor を取りに行かない。** GET のたびに外へ取りに行くと、
	// 署名を付けるだけで任意のホストへの取得をこちらにさせられる。受信と同じく
	// keyId の fragment を落とした URI を actor とみなし、DB にある利用者だけを
	// 相手にする (keyId が actor の URI の下の path になっている実装は、ここでは
	// 署名した人として扱えない。公開の投稿は署名を見ずに返るので、困るのは
	// 見せない投稿だけ)。
	signer, err := a.users.FindByURI(activitypub.ResolveKeyURL(parsed.KeyID))
	if err != nil {
		if repository.IsNotFound(err) {
			return nil, ErrSignedFetchUnknownSigner
		}
		return nil, fmt.Errorf("signed fetch: find signer: %w", err)
	}
	if signer.IsLocal() {
		return nil, ErrSignedFetchUnknownSigner
	}
	pem, err := a.keys.PublicKeyForKeyID(signer.ID, parsed.KeyID)
	if err != nil {
		return nil, fmt.Errorf("signed fetch: public key: %w", err)
	}
	if _, err := a.keyCache.VerifyRequestCached(req, parsed.KeyID, pem); err != nil {
		return nil, fmt.Errorf("signed fetch: verify: %w", err)
	}
	return signer, nil
}
