package mfm

import (
	"encoding/json"
	"strings"
)

// MentionedRemoteUser is one element of a note's `mentionedRemoteUsers`
// column (upstream IMentionedRemoteUsers in models/Note.ts).
//
// JSON のキーと順序は本家 NoteCreateService.insertNote の JSON.stringify と同じ
// (uri, url, username, host)。url は利用者のプロフィールに url が無いと
// undefined になり、キーごと出ない。
type MentionedRemoteUser struct {
	URI      string  `json:"uri"`
	URL      *string `json:"url,omitempty"`
	Username string  `json:"username"`
	Host     *string `json:"host"`
}

// ParseMentionedRemoteUsers decodes a `mentionedRemoteUsers` column value.
// A value that is not a JSON array of objects yields nil.
//
// 本家は JSON.parse が投げるとノートの HTML を作れない。列は mk-go か TS 版が
// 書いたものしか入らないが、壊れていても配送やフィードを止めないよう、
// メンション先が分からないもの (全て自サーバーのリンク) として扱う。
func ParseMentionedRemoteUsers(column string) []MentionedRemoteUser {
	if column == "" {
		return nil
	}
	var users []MentionedRemoteUser
	if err := json.Unmarshal([]byte(column), &users); err != nil {
		return nil
	}
	return users
}

// mentionHref returns the href upstream MfmService.toHtml builds for a mention
// before normalizing it with `new URL()`.
//
// 本家 (MfmService.ts の mention) は mentionedRemoteUsers から、username と host を
// それぞれ toLowerCase して一致する最初の要素を探す。host は `?.` で比べるので、
// メンションに host が無いときは host が null の要素にだけ一致する (リモートの
// 利用者は host を必ず持つので、実際には一致しない)。見つかれば url (空なら uri)、
// 見つからなければ `${config.url}/${acct}` にする。acct は書かれたままの
// `@user` / `@user@host` なので、大文字小文字も保たれる。本家にはさらに acct が
// `@${config.url}` で終わるときにそれを外す分岐があるが、mfm-js の host は `:` も
// `/` も含まないので、config.url (scheme 付き) で終わることは無い。
func (hc *htmlContext) mentionHref(username, host, acct string) string {
	for _, u := range hc.mentioned {
		if strings.ToLower(u.Username) != strings.ToLower(username) { //nolint:staticcheck // JS の toLowerCase と同じ比較にする。EqualFold は ſ と s も一致させる
			continue
		}
		if u.Host == nil {
			if host != "" {
				continue
			}
		} else if host == "" || strings.ToLower(*u.Host) != strings.ToLower(host) { //nolint:staticcheck // JS の toLowerCase と同じ比較にする。EqualFold は ſ と s も一致させる
			continue
		}
		if u.URL != nil && *u.URL != "" {
			return *u.URL
		}
		return u.URI
	}
	return "https://" + hc.host + "/" + acct
}
