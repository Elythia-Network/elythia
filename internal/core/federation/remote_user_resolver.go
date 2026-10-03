package federation

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shiroha-a/mk/internal/misc/idnhost"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// WebFinger performs the outbound WebFinger step used by RemoteUserResolver to
// turn a (username, host) pair into an actor URI. 循環依存を避けるため
// interface で受け取る (実装は activitypub.WebFingerClient)。
type WebFinger interface {
	LookupActorURI(username, host string) (string, error)
}

// actorResolver is the slice of Resolver used by RemoteUserResolver. Declared
// as an interface so tests can inject a fake without building a full Resolver.
type actorResolver interface {
	ResolveActor(uri string) (*model.User, error)
}

// RemoteUserResolver implements user.RemoteUserResolver by combining a
// WebFinger discovery step with an ActivityPub actor resolver. Used to
// populate users/show with remote users that are not yet cached locally.
type RemoteUserResolver struct {
	webfinger WebFinger
	resolver  actorResolver
	userRepo  repository.UserRepository
	localHost string // 自ホスト. host が一致したら webfinger をスキップする.
	// hostBlocker, when set, skips hosts the instance does not federate with
	// before the WebFinger request.
	hostBlocker HostBlockChecker
}

// ErrRemoteUserHostNotAllowed is returned by ResolveByUsernameHost for a host
// the instance does not federate with (blocked, or outside the allowlist).
var ErrRemoteUserHostNotAllowed = errors.New("remote user resolver: host is not allowed")

// SetHostBlockChecker makes ResolveByUsernameHost skip blocked / not-allowed
// hosts without sending the WebFinger request.
func (r *RemoteUserResolver) SetHostBlockChecker(c HostBlockChecker) {
	r.hostBlocker = c
}

// NewRemoteUserResolver constructs a RemoteUserResolver.
//
// localHost は自インスタンスのホスト (例: "example.com")。呼び出し側が
// host = 自ホストで誤って resolver 経由にしてしまった際、WebFinger で自分に
// HTTP 往復するのを避けるための短絡路。空文字なら短絡しない。
func NewRemoteUserResolver(webfinger WebFinger, resolver actorResolver, userRepo repository.UserRepository, localHost string) *RemoteUserResolver {
	return &RemoteUserResolver{
		webfinger: webfinger,
		resolver:  resolver,
		userRepo:  userRepo,
		localHost: localHost,
	}
}

// ResolveByUsernameHost implements user.RemoteUserResolver.
func (r *RemoteUserResolver) ResolveByUsernameHost(username, host string) (*model.User, error) {
	if username == "" || host == "" {
		return nil, errors.New("remote user resolver: username and host required")
	}
	// ホストがローカルなら webfinger を踏まず user table の local 行を引く。
	// ここに来るのは host 正規化を怠った呼び出し側へのセーフティネット。
	// **両辺を正規化する。** upstream も toPuny を掛けた host を
	// `toPuny(config.host)` と比べる (RemoteUserResolveService.ts:59)。片側だけ
	// だと、`url` を Unicode IDN で書いた instance で自ホスト短絡が効かなくなり、
	// 自分自身への WebFinger 往復を誘発する (#2704 review)。
	if r.localHost != "" && strings.EqualFold(idnhost.Puny(host), idnhost.Puny(r.localHost)) {
		if r.userRepo == nil {
			return nil, errors.New("remote user resolver: local host without userRepo")
		}
		return r.userRepo.FindByUsernameLower(strings.ToLower(username), nil)
	}
	if r.webfinger == nil || r.resolver == nil {
		return nil, errors.New("remote user resolver: webfinger or resolver not configured")
	}
	// 連合しないホストには WebFinger も投げない。本家 resolveUser は WebFinger を
	// 投げてから createPerson で弾くが、mk-go の ResolveActor も同じホストを
	// 弾くので結果 (解決できない) は変わらず、外向きのリクエストだけが減る。
	if r.hostBlocker != nil && (r.hostBlocker.IsBlocked(host) || !r.hostBlocker.IsAllowed(host)) {
		return nil, ErrRemoteUserHostNotAllowed
	}
	uri, err := r.webfinger.LookupActorURI(username, host)
	if err != nil {
		return nil, fmt.Errorf("webfinger lookup: %w", err)
	}
	return r.resolver.ResolveActor(uri)
}
