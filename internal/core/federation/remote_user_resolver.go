package federation

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"golang.org/x/sync/singleflight"
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
	// RefreshActor re-fetches a stored remote actor and updates its row
	// (upstream ApPersonService.updatePerson), reporting a failed fetch.
	RefreshActor(uri string) (*model.User, error)
	// LocalUserIDFromURI reports whether uri is served by this instance and,
	// when it is a `/users/<id>` URI, the user ID (upstream
	// ApDbResolverService.parseUri).
	LocalUserIDFromURI(uri string) (id string, local bool)
}

// RemoteUserResyncInterval is how old a stored remote user's lastFetchedAt
// may get before resolving it by acct re-syncs it through WebFinger. 本家
// RemoteUserResolveService.resolveUser の `1000 * 60 * 60 * 24` と同じ。
const RemoteUserResyncInterval = 24 * time.Hour

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
	// group は同じ acct への並行な解決・再同期を 1 回にまとめる。users/show は
	// 未認証でも叩けるので、同じ acct を並べて WebFinger を何度も焚かせない。
	group singleflight.Group
	// now は再同期の要否の判定に使う時計 (テストで差し替える)。
	now func() time.Time
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
		now:       time.Now,
	}
}

// SetClock overrides the clock used to decide whether a stored user is stale.
func (r *RemoteUserResolver) SetClock(now func() time.Time) {
	if now != nil {
		r.now = now
	}
}

// ErrRemoteUserURIMismatch is returned when a re-sync's WebFinger points the
// acct at an actor URI on another host (upstream throws `Invalid uri`).
var ErrRemoteUserURIMismatch = errors.New("remote user resolver: webfinger self link points to another host")

// ErrLocalUserNotFound is returned when the WebFinger self link of a remote
// acct points at this instance but not at an existing local user.
var ErrLocalUserNotFound = errors.New("remote user resolver: local user not found")

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
	if !r.hostAllowed(host) {
		return nil, ErrRemoteUserHostNotAllowed
	}
	acct := strings.ToLower(username) + "@" + idnhost.Puny(host)
	v, err, _ := r.group.Do("new\x00"+acct, func() (any, error) {
		uri, err := r.webfinger.LookupActorURI(username, host)
		if err != nil {
			return nil, fmt.Errorf("webfinger lookup: %w", err)
		}
		// WebFinger が自ホストの利用者を指していたら、その利用者を返す
		// (本家 resolveUser の `isUriLocal(self.href)` → getUserFromApId)。
		// `/users/<id>` 以外の自ホスト URI は本家の createPerson と同じく失敗にする。
		if id, local := r.resolver.LocalUserIDFromURI(uri); local {
			return r.localUserByID(id)
		}
		return r.resolver.ResolveActor(uri)
	})
	if err != nil {
		return nil, err
	}
	u, _ := v.(*model.User)
	return u, nil
}

// hostAllowed reports whether the instance federates with host.
func (r *RemoteUserResolver) hostAllowed(host string) bool {
	return r.hostBlocker == nil || (!r.hostBlocker.IsBlocked(host) && r.hostBlocker.IsAllowed(host))
}

// localUserByID returns the non-deleted local user with the given ID, as
// upstream ApDbResolverService.getUserFromApId does for a local URI.
func (r *RemoteUserResolver) localUserByID(id string) (*model.User, error) {
	if id == "" || r.userRepo == nil {
		return nil, ErrLocalUserNotFound
	}
	u, err := r.userRepo.FindByID(id)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil, ErrLocalUserNotFound
		}
		return nil, fmt.Errorf("remote user resolver: local user: %w", err)
	}
	if u == nil || u.IsDeleted || !u.IsLocal() {
		return nil, ErrLocalUserNotFound
	}
	return u, nil
}

// NeedsResync reports whether u is a remote user whose lastFetchedAt is older
// than RemoteUserResyncInterval (or unset), i.e. whether ResyncIfStale would
// contact the remote server.
func (r *RemoteUserResolver) NeedsResync(u *model.User) bool {
	if u == nil || u.IsLocal() {
		return false
	}
	return u.LastFetchedAt == nil || r.now().Sub(*u.LastFetchedAt) > RemoteUserResyncInterval
}

// ResyncIfStale implements the "user found in the DB" branch of upstream
// RemoteUserResolveService.resolveUser: a remote user whose lastFetchedAt is
// older than RemoteUserResyncInterval is re-synced through WebFinger (fixing
// the stored actor URI when the acct now points elsewhere on the same host)
// and then re-fetched. A fresh or local user is returned as is.
//
// 本家と同じく、試す前に lastFetchedAt を進める。繋がらないホストへ何度も
// 試さないため (本家のコメント「後続の同様処理の連続試行を防ぐ」)。そのため
// 失敗しても次の 24 時間は保存済みの行がそのまま返る。
func (r *RemoteUserResolver) ResyncIfStale(u *model.User) (*model.User, error) {
	if !r.NeedsResync(u) {
		return u, nil
	}
	if r.webfinger == nil || r.resolver == nil || r.userRepo == nil {
		return nil, errors.New("remote user resolver: webfinger or resolver not configured")
	}
	host := idnhost.Puny(*u.Host)
	acct := strings.ToLower(u.Username) + "@" + host
	v, err, _ := r.group.Do("resync\x00"+acct, func() (any, error) {
		return r.resync(u.ID, host)
	})
	if err != nil {
		return nil, err
	}
	out, _ := v.(*model.User)
	return out, nil
}

// resync is the body of ResyncIfStale, run once per acct at a time.
func (r *RemoteUserResolver) resync(userID, host string) (*model.User, error) {
	// 並行して待っていた呼び出しが先に済ませているかもしれないので引き直す。
	u, err := r.userRepo.FindByID(userID)
	if err != nil {
		return nil, fmt.Errorf("remote user resolver: resync: %w", err)
	}
	if !r.NeedsResync(u) {
		return u, nil
	}
	now := r.now()
	if err := r.userRepo.UpdateUser(u.ID, map[string]any{"lastFetchedAt": &now}); err != nil {
		return nil, fmt.Errorf("remote user resolver: resync: %w", err)
	}
	// 本家は連合しないホストにも WebFinger を投げ、updatePerson の取得で失敗する。
	// 結果 (失敗) は同じなので、外向きのリクエストを出さずに失敗させる。
	if !r.hostAllowed(host) {
		return nil, ErrRemoteUserHostNotAllowed
	}
	usernameLower := strings.ToLower(u.Username)
	href, err := r.webfinger.LookupActorURI(usernameLower, host)
	if err != nil {
		return nil, fmt.Errorf("webfinger lookup: %w", err)
	}
	if u.URI == nil || *u.URI != href {
		// acct と actor URI の対応を直す (本家 resolveUser の uri missmatch)。
		// 別のホストを指していたら拒否する。認めると、あるホストの WebFinger が
		// 他所の actor をその acct に結び付けられる。
		parsed, perr := url.Parse(href)
		if perr != nil || idnhost.Puny(parsed.Hostname()) != host {
			return nil, ErrRemoteUserURIMismatch
		}
		// 同じ URI の行が既にあれば付け替えない。付け替えると 1 つの actor に
		// 2 つの行が結び付く (本家も uri に unique 制約は無いが、mk-go は uri で
		// 行を 1 つに引く経路が多い)。
		switch holder, ferr := r.userRepo.FindByURI(href); {
		case ferr == nil && holder != nil && holder.ID != u.ID:
			return nil, ErrRemoteUserURIMismatch
		case ferr != nil && !repository.IsNotFound(ferr):
			return nil, fmt.Errorf("remote user resolver: resync: %w", ferr)
		}
		slog.Info("remote user resolver: recovering mismatched actor uri",
			"username", usernameLower, "host", host, "to", href)
		if err := r.userRepo.UpdateUser(u.ID, map[string]any{"uri": href}); err != nil {
			return nil, fmt.Errorf("remote user resolver: update uri: %w", err)
		}
		// 新しい URI を「無い」と覚えている cache があれば落とす。落とさないと
		// 直後の RefreshActor が行を引けない。
		if inv, ok := r.userRepo.(interface{ InvalidateURI(string) }); ok {
			inv.InvalidateURI(href)
		}
	}
	return r.resolver.RefreshActor(href)
}
