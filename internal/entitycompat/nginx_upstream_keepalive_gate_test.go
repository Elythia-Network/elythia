package entitycompat

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 同梱の nginx 設定が、mk-go への接続を要求ごとに張り直さないこと (#3963)。
//
// upstream に `keepalive` が無いか、location が `Connection` を指定しないと
// (nginx の既定は `close`)、API や `/files/` の要求のたびに UDS の接続を
// 張って閉じる。隔離した nginx + mk-go で数えると、`POST /api/meta` 300 件で
// mk-go の accept が 300 回 (直した設定では 1 回) だった。`/files/` だけ
// `Connection ""` を外した設定でも 20 件で 20 回になった。
func TestBundledNginxReusesUpstreamConnections(t *testing.T) {
	t.Parallel()

	// **拾えた location を名指しで要求する。** 抽出が空振りしても「違反 0 件」で
	// 緑にならないように、今ある 3 つの委譲先を下限にする。
	mustProxy := []string{"/files/", "= /backup-download", "/"}
	require.NotEmpty(t, bundledNginxConfigs)

	locRe := regexp.MustCompile(`location\s+([^{]+?)\s*\{`)
	keepaliveRe := regexp.MustCompile(`(?m)^\s*keepalive\s+[1-9][0-9]*\s*;`)
	connRe := regexp.MustCompile(`(?m)^\s*proxy_set_header\s+Connection\s+(""|\$connection_upgrade)\s*;`)

	for _, path := range bundledNginxConfigs {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		body := stripNginxComments(string(raw))

		upstream, ok := nginxBlockBody(body, "upstream mkgo {")
		require.True(t, ok, "%s: upstream mkgo が無い", path)
		require.Regexp(t, keepaliveRe, upstream,
			"%s: upstream mkgo に keepalive が無い。要求ごとに mk-go への接続を張り直す", path)

		seen := map[string]bool{}
		for _, m := range locRe.FindAllStringSubmatch(body, -1) {
			loc := strings.TrimSpace(m[1])
			inner, ok := nginxLocationBody(body, loc)
			require.True(t, ok, "%s: location %s の中身が読めない", path, loc)
			if !strings.Contains(inner, "proxy_pass http://mkgo") {
				continue
			}
			seen[loc] = true
			require.Regexp(t, `(?m)^\s*proxy_http_version\s+1\.1\s*;`, inner,
				"%s: location %s が HTTP/1.1 で委譲していない (1.0 では keepalive が効かない)", path, loc)
			require.Regexp(t, connRe, inner,
				"%s: location %s が Connection を \"\" か $connection_upgrade にしていない。"+
					"指定しないと nginx は close を送り、要求ごとに接続を閉じる。"+
					"\"upgrade\" の固定は WebSocket でない要求にも付く", path, loc)
		}
		for _, loc := range mustProxy {
			require.True(t, seen[loc], "%s: mk-go へ委譲する location %s を拾えなかった", path, loc)
		}

		// **`/streaming` の WebSocket は location / を通る。** 上の検査は
		// `Connection ""` でも通すので、/files/ の形を書き写すと WebSocket が
		// 101 にならないのに緑になる (#3963 のレビューで変異して確かめた)。
		// location / だけは Upgrade と $connection_upgrade の両方を名指しで求める。
		root, ok := nginxLocationBody(body, "/")
		require.True(t, ok, "%s: location / の中身が読めない", path)
		require.Regexp(t, `(?m)^\s*proxy_set_header\s+Upgrade\s+\$http_upgrade\s*;`, root,
			"%s: location / が Upgrade を渡していない。/streaming の WebSocket がつながらない", path)
		require.Regexp(t, `(?m)^\s*proxy_set_header\s+Connection\s+\$connection_upgrade\s*;`, root,
			"%s: location / の Connection が $connection_upgrade でない。/streaming の WebSocket がつながらない", path)

		// $connection_upgrade は WebSocket でない要求で空になること。
		// default だけだと全ての要求に upgrade が付く。接続の使い回しはそれでも
		// 効くが (実測)、Upgrade の無い要求に upgrade を付けるのは誤りなので塞ぐ。
		m, ok := nginxBlockBody(body, "map $http_upgrade $connection_upgrade {")
		require.True(t, ok, "%s: $connection_upgrade の map が無い", path)
		require.Regexp(t, `(?m)^\s*''\s+''\s*;`, m,
			"%s: $connection_upgrade の map が、Upgrade の無い要求で空にならない", path)
		require.Regexp(t, `(?m)^\s*default\s+upgrade\s*;`, m,
			"%s: $connection_upgrade の map が、WebSocket の要求で upgrade にならない", path)
	}
}

// nginxBlockBody returns the body after head up to the first closing brace.
//
// nginxLocationBody と同じく入れ子は想定しない。
func nginxBlockBody(body, head string) (string, bool) {
	i := strings.Index(body, head)
	if i < 0 {
		return "", false
	}
	rest := body[i+len(head):]
	j := strings.Index(rest, "}")
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}
