package federation

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// accountCreatedAtFillTimeout bounds the Misskey `/api/users/show` lookup made
// while handling an inbound Follow.
//
// フォローの処理を待たせる上限。RemoteStatsFetcher の HTTP client の上限と
// 同じ値にして、フォローの処理がそれ以上は延びないようにする。
const accountCreatedAtFillTimeout = remoteStatsTimeout

// parseAccountPublished parses an actor's `published` (account creation time,
// #3465) leniently. It accepts RFC 3339 (with or without fractional seconds)
// and a bare date (`2006-01-02`, read as UTC midnight). It returns nil for an
// empty, unparsable, pre-2000 or future value.
//
// 相手が偽った日付はここでは防がない (#3465 の決定)。作成日時を偽るのは
// 悪意のあるサーバーで、ドメインのブロックなど別の手段で扱う。ここで落とす
// のは、明らかに作成日時として成り立たない値 (未来・2000 年より前) だけ。
func parseAccountPublished(raw string, now time.Time) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var (
		t   time.Time
		err error
	)
	for _, layout := range []string{time.RFC3339Nano, time.DateOnly} {
		if t, err = time.Parse(layout, raw); err == nil {
			break
		}
	}
	if err != nil {
		return nil
	}
	return acceptableAccountCreatedAt(t, now)
}

// accountCreatedAtFloor is the earliest accepted account creation time.
var accountCreatedAtFloor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// acceptableAccountCreatedAt returns t in UTC when it can be an account
// creation time, or nil otherwise (before 2000-01-01 UTC / later
// than now beyond the clock skew allowance).
func acceptableAccountCreatedAt(t, now time.Time) *time.Time {
	// 2000-01-01 より前は採らない。fediverse より前の日時は作成日時として
	// 成り立たず、単位の取り違え (APLenientTimestamp は数値を epoch ミリ秒と
	// 読むので、epoch 秒を送る実装は 1970-01 になる) と、「値が無い」を 0 で
	// 埋めた形を拾うため。偽装への防御ではない。
	if t.Before(accountCreatedAtFloor) {
		return nil
	}
	// 時計のずれは published (ノート) と同じ 5 分まで許す。
	if t.After(now.Add(publishedFutureSkew)) {
		return nil
	}
	u := t.UTC()
	return &u
}

// IsMisskeyFamilySoftware reports whether a remote instance's nodeinfo
// software name (`instance.softwareName`) denotes a Misskey-compatible server
// whose unauthenticated `/api/users/show` returns `createdAt`.
//
// 判定は保存済みのソフトウェア名で行い、推測で叩かない (相手が持たない
// endpoint へ毎回出すのは、こちらの待ち時間と相手の負荷の無駄)。大文字混じり
// の値が実在するので lowercase して比べる (emojimeta.SupportsHost と同じ)。
// Elythia 自身 (旧名 mk-go を含む) も Misskey 系として扱う。
func IsMisskeyFamilySoftware(softwareName string) bool {
	switch strings.ToLower(strings.TrimSpace(softwareName)) {
	case "misskey", config.SoftwareName, config.LegacySoftwareName,
		"cherrypick", "sharkey", "yojo-art", "cluckey", "firefish", "iceshrimp",
		"foundkey", "calckey", "meisskey", "catodon":
		return true
	default:
		return false
	}
}

// AccountCreatedAtSource fetches a remote account's creation time from its
// origin server. *RemoteStatsFetcher implements it.
type AccountCreatedAtSource interface {
	FetchAccountCreatedAt(ctx context.Context, host, username string) *time.Time
}

// InstanceSoftwareLookup finds the stored instance row for a host.
// repository.InstanceRepository satisfies it.
type InstanceSoftwareLookup interface {
	FindByHost(host string) (*model.Instance, error)
}

// AccountCreatedAtWriter stores the creation time only while the column is
// still NULL. repository.UserRepository satisfies it.
type AccountCreatedAtWriter interface {
	SetAccountCreatedAtIfNull(userID string, createdAt time.Time) (bool, error)
}

// AccountCreatedAtFiller fills `user.accountCreatedAt` for a remote account
// that did not send `published`, by asking a Misskey-family origin server's
// `/api/users/show` (#3465).
//
// 取りに行くのは、判定に要るのに列が空のとき (フォローを受け取ったとき)
// だけ。一度取れたら列に保存するので、同じ人へは二度と取りに行かない。
// 取れなかったときの再試行は RemoteStatsFetcher の負キャッシュ (5 分) で抑える。
type AccountCreatedAtFiller struct {
	source    AccountCreatedAtSource
	instances InstanceSoftwareLookup
	users     AccountCreatedAtWriter
	now       func() time.Time
}

// NewAccountCreatedAtFiller constructs a filler. Any nil dependency makes Fill
// a no-op.
func NewAccountCreatedAtFiller(source AccountCreatedAtSource, instances InstanceSoftwareLookup, users AccountCreatedAtWriter) *AccountCreatedAtFiller {
	return &AccountCreatedAtFiller{source: source, instances: instances, users: users, now: time.Now}
}

// Fill stores the account creation time of remote user u when it is unknown
// and u's server is Misskey-family. It is best-effort: every failure is logged
// at debug level and leaves the column NULL. On success u.AccountCreatedAt is
// updated in place.
func (f *AccountCreatedAtFiller) Fill(ctx context.Context, u *model.User) {
	if f == nil || f.source == nil || f.instances == nil || f.users == nil {
		return
	}
	if u == nil || u.IsLocal() || u.AccountCreatedAt != nil {
		return
	}
	host := *u.Host
	inst, err := f.instances.FindByHost(host)
	if err != nil || inst == nil || inst.SoftwareName == nil {
		if err != nil {
			slog.Debug("accountCreatedAt: instance lookup failed", "host", host, "err", err)
		}
		return
	}
	if !IsMisskeyFamilySoftware(*inst.SoftwareName) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, accountCreatedAtFillTimeout)
	defer cancel()
	fetched := f.source.FetchAccountCreatedAt(ctx, host, u.Username)
	if fetched == nil {
		slog.Debug("accountCreatedAt: users/show gave no createdAt", "host", host, "userId", u.ID)
		return
	}
	t := acceptableAccountCreatedAt(*fetched, f.now())
	if t == nil {
		return
	}
	// 列が空のときだけ書く。u は取りに行く前に読んだ行なので、その間に
	// refresh が actor の `published` を書いていたら、そちらを残す。
	set, err := f.users.SetAccountCreatedAtIfNull(u.ID, *t)
	if err != nil {
		slog.Debug("accountCreatedAt: store failed", "userId", u.ID, "err", err)
		return
	}
	if !set {
		slog.Debug("accountCreatedAt: already stored by another path", "userId", u.ID)
		return
	}
	u.AccountCreatedAt = t
}
