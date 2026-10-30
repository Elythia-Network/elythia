package federation

import (
	"net/url"
	"strings"

	"github.com/elythia-network/elythia/internal/activitypub"
)

// ExtractLocalAPReferences delegates to the shared semantic ActivityPub reference
// extractor using this resolver's canonical local origin.
func (r *Resolver) ExtractLocalAPReferences(serialized string) (userIDs []string, noteIDs []string) {
	if r == nil || r.urls == nil {
		return nil, nil
	}
	return activitypub.ExtractLocalAPReferences(r.urls.BaseURL(), serialized)
}

// ExtractLocalUserID returns the user ID a local URI names the way upstream
// ApPersonService.fetchPerson reads it: a URI under `{url}/` (a plain string
// prefix, not a host comparison) names the user whose ID is its last path
// segment (`uri.split('/').pop()`). It returns "" for any other URI.
//
// 以前は `/users/{id}` の直後で切っていたので、`/users/{id}/followers` のような
// ローカルの利用者の collection の URI がその利用者に解決され、メンション・
// specified の宛先の対象になっていた。本家は最後の段 (`followers`) を ID として
// 引くので誰にも解決しない (#3330)。
//
// **host ではなく文字列の接頭辞で見る。** 本家 fetchPerson は
// `uri.startsWith(`${config.url}/`)` で、`config.url` は `new URL(...).origin`
// (scheme + 小文字の host + 既定でない port)。`http://` を名乗るものや
// `:443` を付けたものは、host が自分でも接頭辞に当たらず、続く createPerson が
// isUriLocal (host 比較) で `cannot resolve local user` を投げるので、誰にも
// 解決しない。mk-go の呼び出し側 (resolveMentionedUserIDs) も、ここで "" なら
// FindByURI に回り、ローカルの利用者は uri を持たないので同じく解決しない。
// host で見るように変えると、本家が捨てるそれらの URI をローカルの利用者に
// 解決してしまう。
//
// 接頭辞は `{url}/users/` ではなく `{url}/` (本家と同じ)。通報 (Flag) は本家が
// `{url}/users/` で絞るので flagTargetUserID を使う。Accept の actor のように
// 本家が getUserFromApId で読む箇所 (WebFinger の self を含む) と、本家に対応の
// 無い chat の宛先は localUserIDFromAPID / LocalUserIDFromURI (parseLocalURI) を使う。
func (r *Resolver) ExtractLocalUserID(uri string) string {
	if r.urls == nil {
		return ""
	}
	// UserURI("") = `{url}/users/`。URLBuilder は baseURL を公開しないので、
	// 既存の組み立てから `{url}/` を取り出す。
	return lastSegmentUnder(uri, strings.TrimSuffix(r.urls.UserURI(""), "users/"))
}

// ExtractLocalNoteID returns a note ID from a canonical local note URI,
// including activity and fragment variants.
func (r *Resolver) ExtractLocalNoteID(uri string) string {
	if r.urls == nil {
		return ""
	}
	return r.urls.LocalNoteIDFromURI(uri)
}

// flagTargetUserID returns the user ID an inbound Flag object URI names the
// way upstream ApInboxService.flag reads it: a URI under `{url}/users/` names
// the user whose ID is its last path segment (`uri.split('/').at(-1)`).
func (r *Resolver) flagTargetUserID(uri string) string {
	if r.urls == nil {
		return ""
	}
	return lastSegmentUnder(uri, r.urls.UserURI(""))
}

// lastSegmentUnder returns the last `/`-separated segment of uri when uri
// starts with prefix, and "" otherwise.
func lastSegmentUnder(uri, prefix string) string {
	rest, ok := strings.CutPrefix(uri, prefix)
	if !ok {
		return ""
	}
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		rest = rest[i+1:]
	}
	return rest
}

// parseLocalURI reads uri the way upstream ApDbResolverService.parseUri does
// for this instance: local is whether the host is this instance's (any
// scheme, like UtilityService.isUriLocal), and for a local URI typ and id are
// the first and second path segments (`/users/{id}/...` → "users", id).
func (r *Resolver) parseLocalURI(uri string) (typ, id string, local bool) {
	if !r.isSelfHostURI(uri) {
		return "", "", false
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", true
	}
	// 本家は `new URL(...).pathname` (エスケープされたまま) を `/` で割る。
	parts := strings.Split(u.EscapedPath(), "/")
	if len(parts) > 1 {
		typ = parts[1]
	}
	if len(parts) > 2 {
		id = parts[2]
	}
	return typ, id, true
}

// localUserIDFromAPID returns the user ID a local `/users/{id}[/...]` URI
// names the way upstream ApDbResolverService.getUserFromApId reads it
// (parseUri: the segment right after `users`, ignoring the rest). It returns
// "" for a URI that is not local or not under `/users/`.
func (r *Resolver) localUserIDFromAPID(uri string) string {
	id, _ := r.LocalUserIDFromURI(uri)
	return id
}

// LocalUserIDFromURI reports whether uri is served by this instance and, for
// a `/users/{id}[/...]` URI, the user ID, both as upstream parseUri reads them
// (see parseLocalURI). The WebFinger self link of a remote acct is read this
// way (RemoteUserResolveService.resolveUser → getUserFromApId).
func (r *Resolver) LocalUserIDFromURI(uri string) (string, bool) {
	typ, id, local := r.parseLocalURI(uri)
	if typ != "users" {
		id = ""
	}
	return id, local
}

// extractLocalNoteID returns the trailing note ID for a URI rooted at the
// local instance, or "" if the URI does not match the local pattern.
func (r *Resolver) extractLocalNoteID(uri string) string {
	if r.urls == nil {
		return ""
	}
	prefix := r.urls.NoteURI("")
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	rest := uri[len(prefix):]
	// /notes/{id} の "/{id}" 部分しか持たないように、追加のスラッシュは無視する
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// pointerStringsEqual returns true if both *string point to equal values
// (nil/nil 同士も等しい)。upsertEmojis の license diff など、`*string` で
// 「未指定」と「明示空文字列」を区別する経路で使う。
func pointerStringsEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
