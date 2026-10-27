package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/config"
	coretwofactor "github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/misc/ipnorm"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ajvCorpusFiles are the corpora of ajv's verdicts: on upstream's paramDefs
// (#3673), and on Elythia's own (elythia_paramdefs.json, #3869).
var ajvCorpusFiles = []string{
	"../api/paramschema/testdata/ajv_corpus.jsonl",
	"../api/paramschema/testdata/ajv_corpus_elythia.jsonl",
}

// ajvCase is one input of the corpus and ajv's verdict on it.
type ajvCase struct {
	ep   string
	line int
	in   json.RawMessage
	ok   bool
	path string
	msg  string
}

// readAJVCorpus returns the corpus cases of the endpoints in eps.
func readAJVCorpus(t *testing.T, eps []string) []ajvCase {
	t.Helper()
	var cases []ajvCase
	for _, f := range ajvCorpusFiles {
		cases = append(cases, readAJVCorpusFile(t, f, eps)...)
	}
	return cases
}

func readAJVCorpusFile(t *testing.T, file string, eps []string) []ajvCase {
	t.Helper()
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var cases []ajvCase
	ep := ""
	for n := 1; sc.Scan(); n++ {
		if n == 1 {
			continue // header
		}
		var l struct {
			EP   string          `json:"ep"`
			In   json.RawMessage `json:"in"`
			OK   *bool           `json:"ok"`
			Path string          `json:"path"`
			Msg  string          `json:"msg"`
		}
		require.NoError(t, json.Unmarshal(sc.Bytes(), &l), "corpus line %d", n)
		if l.EP != "" {
			ep = l.EP
			continue
		}
		if !slices.Contains(eps, ep) {
			continue
		}
		require.NotNil(t, l.OK, "corpus line %d", n)
		cases = append(cases, ajvCase{ep: ep, line: n, in: l.In, ok: *l.OK, path: l.Path, msg: l.Msg})
	}
	require.NoError(t, sc.Err())
	return cases
}

// enforcementPending lists the endpoints of an enforced package that are not
// enforced yet, with a JSON body Elythia accepts but upstream's paramDef
// rejects.
var enforcementPending = map[string]string{
	// 保留していた federation/instances・reversi/match・i/notifications・
	// i/notifications-grouped・admin/drive/files は、Elythia 独自の値を本家の
	// paramDef に重ねて(paramschema.Effective、elythia_overlays.json)載せた。
	// admin/emoji/add の url だけの経路は消した(#3865)。
}

// enforcementDeferred lists the endpoints of an enforced package that are
// left to a later stage, with the reason. Unlike enforcementPending, they
// have no Elythia-only input that upstream's paramDef rejects.
var enforcementDeferred = map[string]string{
	// 独自の引数は無いが、本家と判定が分かれる入力 (U+0085 だけの text、NUL を含む
	// text、poll.choices の __proto__ の重複など) の扱いを、admin/update-meta・
	// i/update と一緒に最後の段で決める (#3667、#3847)。
	"notes/create": "decided with admin/update-meta and i/update in the last stage of #3667",
	// 親 issue の方針どおり、admin/update-meta・notes/create と一緒に最後の段で
	// 決める (#3667、#3851)。
	"i/update": "decided with admin/update-meta and notes/create in the last stage of #3667",
}

// stricterCase is an entry of handlerStricterThanUpstream: the accepted corpus
// inputs it matches, and the id of the INVALID_PARAM the handler answers.
type stricterCase struct {
	match func(in map[string]any) bool
	id    string
}

// ajvInvalidParamID is upstream's id of the INVALID_PARAM ajv answers.
const ajvInvalidParamID = "3d81ceae-475f-4600-b2a8-2bc116157532"

// handlerStricterThanUpstream lists the accepted corpus inputs an enforced
// endpoint still rejects with INVALID_PARAM on purpose, because upstream
// answers them with a 500 (or, for notes/search's empty query, with a search
// of every note; see docs/divergence/approximations.md), or, for an Elythia
// endpoint (#3869), because the check is one a paramDef cannot express. Each
// entry must match at least one corpus input.
var handlerStricterThanUpstream = map[string]stricterCase{
	// IP アドレス (と ip:port) として読めるかは、paramDef の pattern では書き切れない
	// (IPv6 の省略形・zone・IPv4-mapped)。宣言は文字列までにし、読めない値は handler
	// が 400 INVALID_PARAM にする (#3869)。corpus の ip はどれも IP として読めない。
	"admin/ip/accounts": {id: ajvInvalidParamID, match: func(in map[string]any) bool {
		ip, ok := in["ip"].(string)
		if !ok {
			return false
		}
		_, valid := ipnorm.Normalize(ip)
		return !valid
	}},
	// リモートの利用者は対象にしない (user_ip は自インスタンスで認証した利用者しか
	// 書かないので、必ず「候補なし」になり、関連が無いと読まれる)。利用者がリモート
	// かは paramDef では書けないので、handler が 400 INVALID_PARAM にする (#3869)。
	// corpus の userId は、置いたリモートの利用者 (abc123) を指す。
	"admin/ip/related-accounts": {id: ajvInvalidParamID, match: func(in map[string]any) bool {
		return in["userId"] == "abc123"
	}},
	// 本家は絵文字を含まない reaction を normalizeEmojiString が素の Error で
	// 投げて 500 にする (ChatService.react)。利用者の入力の誤りなので、Elythia は
	// 400 INVALID_PARAM にしている (#3841)。corpus の値は ASCII だけなので、
	// ASCII だけで :name: の形でないものを絵文字の無い値とみなす。
	"chat/messages/react": {id: ajvInvalidParamID, match: func(in map[string]any) bool {
		r, ok := in["reaction"].(string)
		if !ok || (strings.HasPrefix(r, ":") && strings.HasSuffix(r, ":") && len(r) > 2) {
			return false
		}
		for _, c := range r {
			if c > 0x7f {
				return false
			}
		}
		return true
	}},
	// 本家は note_visibility_enum に無い visibility を PostgreSQL の型変換で 500 に
	// する。Elythia は引く前に 400 にしている (#3518)。空文字は未指定と同じに扱う。
	"notes/mentions": {id: ajvInvalidParamID, match: func(in map[string]any) bool {
		v, ok := in["visibility"].(string)
		return ok && !slices.Contains([]string{"", "public", "home", "followers", "specified"}, v)
	}},
	// 本家は空の検索語で絞り込まずに返す (LIKE '%%' でも meilisearch でも)。検索語の
	// 無い検索は検索として意味を持たないので、Elythia は以前からの 400 のままにし、
	// 残す差として docs/divergence/approximations.md に書いている (#3847)。
	"notes/search": {id: ajvInvalidParamID, match: func(in map[string]any) bool {
		return in["query"] == ""
	}},
	// 本家は相対の url や http(s) 以外の scheme を fetch に渡し、例外で 500 になる。
	// 取得の前に 400 にしている (#3845)。corpus の url はどれもこの形で、本家でも
	// 取得できる data: の url は無い。
	"fetch-external-resources": {id: ajvInvalidParamID, match: func(in map[string]any) bool {
		raw, ok := in["url"].(string)
		if !ok {
			return false
		}
		u, err := url.Parse(raw)
		return err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https")
	}},
	// 本家は存在しない token を findOneByOrFail で 500 にする。Elythia は独自の id の
	// 「Invalid token.」を返す (#3845)。corpus の token はどれも存在しない。
	"reset-password": {id: "6382759e-0a0d-4e32-893e-0e1e66cec4d5", match: func(map[string]any) bool { return true }},
	// 本家は整数でない displayOrder を、integer の列への書き込みで PostgreSQL の型
	// 変換の 500 にする見込み (ソースからの推定)。Elythia は書き込む前に 400 にして
	// いる (#3859)。paramDef の displayOrder は number なので、ajv は通す。
	"admin/roles/create": {id: ajvInvalidParamID, match: nonIntegerDisplayOrder},
	"admin/roles/update": {id: ajvInvalidParamID, match: nonIntegerDisplayOrder},
	// 本家は 129 文字以上の name を、varchar(128) の列への書き込みで 500 にする
	// 見込み。Elythia は書き込む前に 400 にしている (#2998、#3865)。
	"admin/emoji/add": {id: ajvInvalidParamID, match: func(in map[string]any) bool {
		name, ok := in["name"].(string)
		return ok && utf8.RuneCountInString(name) > 128
	}},
	// 本家は日付として読めない updatedAtFrom / updatedAtTo を PostgreSQL の型変換で
	// 500 にする。Elythia は引く前に 400 にしている (#3518)。
	"v2/admin/emoji/list": {id: ajvInvalidParamID, match: func(in map[string]any) bool {
		q, ok := in["query"].(map[string]any)
		if !ok {
			return false
		}
		for _, k := range []string{"updatedAtFrom", "updatedAtTo"} {
			if v, ok := q[k].(string); ok && !repository.ValidEmojiUpdatedAtFilter(v) {
				return true
			}
		}
		return false
	}},
}

// nonIntegerDisplayOrder matches an input whose displayOrder is a number
// without an integer value (1.5).
func nonIntegerDisplayOrder(in map[string]any) bool {
	v, ok := in["displayOrder"].(float64)
	return ok && v != math.Trunc(v)
}

// sixDigits matches a token that could be a valid TOTP code (i/2fa/done
// removes the whitespace first).
var sixDigits = regexp.MustCompile(`^[0-9]{6}$`)

// passwordCheckedEndpoints are the i/* endpoints whose accepted corpus inputs
// TestEnforcedEndpointsMatchTheCorpus expects to stop at the password check:
// the test user's password is not in the corpus.
var passwordCheckedEndpoints = []string{
	"i/2fa/key-done",
	"i/2fa/register",
	"i/2fa/register-key",
	"i/2fa/remove-key",
	"i/2fa/unregister",
	"i/change-password",
	"i/delete-account",
	"i/regenerate-token",
	"i/update-email",
}

// jobEnqueuingEndpoints are the enforced admin/* endpoints whose accepted
// corpus inputs reach the job queue. The test server's Redis (miniredis) has
// no cmsgpack, so the enqueue fails with a 500.
var jobEnqueuingEndpoints = []string{"admin/drive/clean-remote-files", "admin/emoji/import-zip"}

// metaWritingEndpoints are the enforced admin/* endpoints whose accepted
// corpus inputs write meta. TestEnforcedEndpointsMatchTheCorpus puts the row
// back after each of them.
var metaWritingEndpoints = []string{"admin/roles/update-default-policies", "admin/captcha/save"}

// moderationEndpoint reports whether TestEnforcedEndpointsMatchTheCorpus seeds
// the remote user, role, system webhook, notification recipient and abuse
// report for the accepted inputs of the admin/* endpoint ep (#3859), and the
// admin/* endpoints that take the userId of the remote user (#3861).
func moderationEndpoint(ep string) bool {
	for _, prefix := range []string{"admin/roles/", "admin/abuse-report/", "admin/system-webhook/"} {
		if strings.HasPrefix(ep, prefix) {
			return true
		}
	}
	return slices.Contains([]string{"admin/abuse-user-reports", "admin/forward-abuse-user-report",
		"admin/resolve-abuse-user-report", "admin/update-abuse-user-report"}, ep) || slices.Contains(userTargetEndpoints, ep)
}

// userTargetEndpoints are the enforced admin/* endpoints that act on, or look
// up, the user named by userId (#3861; admin/ip/related-accounts in #3869).
// The corpus names the seeded remote user (abc123).
var userTargetEndpoints = []string{
	"admin/accounts/delete", "admin/delete-account", "admin/delete-all-files-of-a-user", "admin/get-user-ips",
	"admin/reset-password", "admin/show-user", "admin/suspend-user", "admin/unset-mfa", "admin/unset-user-avatar",
	"admin/unset-user-banner", "admin/unsuspend-user", "admin/update-user-note", "admin/ip/related-accounts",
}

// testNet1 is TEST-NET-1 (RFC 5737), where the seeded urls point.
var testNet1 = &net.IPNet{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)}

// enforcementPackage returns the package TestEnforcedEndpointsAreDeclared
// counts the endpoint in: the first segment of the path (notes for notes and
// notes/*), and for admin the first two (admin/ad for admin/ad/*). A top-level
// endpoint (meta, admin/meta) is a package of its own.
//
// admin/* は 102 件あり、性質ごとに段を分けて載せる (#3854)。1 段目で数えると、
// 一部を載せた時点で残り (#3854 の 20 件なら 82 件) を enforcementDeferred に並べる
// ことになるので、admin だけは 2 段目までを 1 つのパッケージとして数える。
func enforcementPackage(name string) string {
	first, rest, ok := strings.Cut(name, "/")
	if !ok || first != "admin" {
		return first
	}
	second, _, ok := strings.Cut(rest, "/")
	if !ok {
		return name
	}
	return first + "/" + second
}

func TestEnforcementPackage(t *testing.T) {
	for name, want := range map[string]string{
		"meta":                          "meta",
		"notes":                         "notes",
		"notes/create":                  "notes",
		"notes/polls/vote":              "notes",
		"v2/admin/emoji/list":           "v2",
		"admin/meta":                    "admin/meta",
		"admin/ad/create":               "admin/ad",
		"admin/emoji/set-category-bulk": "admin/emoji",
		"admin/abuse-report/notification-recipient/create": "admin/abuse-report",
	} {
		assert.Equal(t, want, enforcementPackage(name), name)
	}
}

// TestEnforcedEndpointsAreDeclared keeps paramEnforcedEndpoints to names
// with a paramDef: upstream's, or Elythia's own (elythia_paramdefs.json,
// #3869). The upstream ones go by whole packages (see enforcementPackage): an
// endpoint of a package left out would keep the handwritten checks only,
// unless enforcementPending or enforcementDeferred names it. Every Elythia
// endpoint with a paramDef is enforced: the declaration is written to be
// enforced, in steps of #3667.
func TestEnforcedEndpointsAreDeclared(t *testing.T) {
	eps, err := paramschema.Upstream()
	require.NoError(t, err)
	eff, err := paramschema.Effective()
	require.NoError(t, err)
	packages := map[string]bool{}
	elythia := 0
	for _, name := range paramEnforcedEndpoints {
		if ep, ok := eff[name]; ok && ep.Elythia {
			require.NoError(t, ep.CompileErr, name)
			elythia++
			continue
		}
		ep, ok := eps[name]
		require.True(t, ok, "%s has no paramDef: neither upstream nor elythia_paramdefs.json declares it", name)
		require.NoError(t, ep.CompileErr, name)
		packages[enforcementPackage(name)] = true
	}
	// 独自のendpointを1件も数えなくなる(Elythiaの印が付かなくなる)と、下の
	// 検査が空振りする。
	require.Positive(t, elythia, "no Elythia endpoint is enforced")
	for name, ep := range eff {
		if ep.Elythia {
			assert.Contains(t, paramEnforcedEndpoints, name, "%s is declared in elythia_paramdefs.json, but not enforced", name)
		}
	}
	require.NotEmpty(t, packages)
	// admin は 2 段目までで数える。1 段目で数える形に戻ると、admin/* の残りが
	// 載っていないことで落ちるが、数え方が別の形で崩れて admin のパッケージを
	// 1 つも数えなくなると、下の検査が空振りする。載せたパッケージを名指しで
	// 要求する (#3854、#3857、#3859、#3861、#3865)。
	for _, pkg := range []string{"admin/ad", "admin/announcements", "admin/avatar-decorations", "admin/invite", "admin/promo", "admin/meta",
		"admin/drive", "admin/emoji", "admin/roles", "admin/abuse-report", "admin/abuse-user-reports", "admin/system-webhook", "admin/captcha",
		"admin/accounts", "admin/federation", "admin/relays", "admin/show-users", "admin/update-proxy-account", "admin/queue"} {
		require.True(t, packages[pkg], "package %s is not counted", pkg)
	}
	for name := range eps {
		pkg := enforcementPackage(name)
		if packages[pkg] && enforcementPending[name] == "" && enforcementDeferred[name] == "" {
			assert.Contains(t, paramEnforcedEndpoints, name, "package %s is enforced, but not %s", pkg, name)
		}
	}
	// 使われなくなった項目を残さない: 載せたか、パッケージごと外したら消す。
	for name, body := range enforcementPending {
		pkg := enforcementPackage(name)
		ep, ok := eps[name]
		if assert.True(t, ok, "pending %s is not an upstream endpoint", name) {
			// 本家がその入力を通すようになったら、除外の理由は無くなる。
			var in any
			require.NoError(t, json.Unmarshal([]byte(body), &in), name)
			assert.NotNil(t, ep.Schema.Validate(in), "pending %s: upstream now accepts %s; enforce it", name, body)
		}
		assert.NotContains(t, paramEnforcedEndpoints, name, "pending %s is enforced; drop it from enforcementPending", name)
		assert.True(t, packages[pkg], "pending %s: package %s is not enforced; drop it from enforcementPending", name, pkg)
	}
	for name, reason := range enforcementDeferred {
		pkg := enforcementPackage(name)
		assert.NotEmpty(t, reason, name)
		_, ok := eps[name]
		assert.True(t, ok, "deferred %s is not an upstream endpoint", name)
		assert.NotContains(t, paramEnforcedEndpoints, name, "deferred %s is enforced; drop it from enforcementDeferred", name)
		assert.Empty(t, enforcementPending[name], "%s is both pending and deferred", name)
		assert.True(t, packages[pkg], "deferred %s: package %s is not enforced; drop it from enforcementDeferred", name, pkg)
	}
}

// TestEnforcedEndpointsMatchTheCorpus sends every corpus input of the
// enforced endpoints to a real server and compares the answer with ajv's
// verdict: an input ajv rejects gets upstream's 400 INVALID_PARAM with the
// same info.param and info.reason, and an input ajv accepts gets no
// INVALID_PARAM (the handler may still answer NO_SUCH_CLIP and the like).
// The second half catches a handwritten check that is stricter than
// upstream, which the validator in front cannot fix.
func TestEnforcedEndpointsMatchTheCorpus(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB
	const userID, token = "penforce-user", "penforcetoken001"
	const roleID, assignID = "penforce-role", "penforce-assign"
	const adminID, adminToken = "penforce-admin", "penforceadmintk1"
	const adminRoleID, adminAssignID = "penforce-admin-role", "penforce-admin-assign"
	const iPassword = "penforce-not-in-corpus"
	iEmail := "penforce@example.com"
	// resetNotes removes what the notes/* inputs write: the seeded note, poll,
	// draft and list, and the favorites, reactions, votes and thread mutings
	// of the test user.
	resetNotes := func() {
		require.NoError(t, db.Exec(`DELETE FROM note_favorite WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM note_reaction WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM poll_vote WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM note_thread_muting WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM note_draft WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM poll WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM note WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM user_list WHERE "userId" = ?`, userID).Error)
	}
	// resetI removes what the i/* inputs write: the profile, webhooks, registry
	// items and pinned notes of the test user.
	resetI := func() {
		require.NoError(t, db.Exec(`DELETE FROM webhook WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM registry_item WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM user_note_pining WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM user_profile WHERE "userId" = ?`, userID).Error)
	}
	// resetAdmin removes what the admin/* inputs write to the tables without
	// an owner column: the ads, announcements, avatar decorations, emojis and
	// drive files made after the test started (keep holds the ids that were
	// there before), the seeded ones (abc123, abc1231) included, and the
	// promotion of the seeded note.
	const seedID, seedID2 = "abc123", "abc1231"
	// guardFileID is a file of the test user that admin/drive/cleanup must not
	// delete (see the check after the loop).
	const guardFileID = "penforce-guard-file"
	var keepAds, keepAnnouncements, keepDecorations, keepEmojis, keepFiles []string
	require.NoError(t, db.Raw(`SELECT id FROM ad WHERE id <> ?`, seedID).Scan(&keepAds).Error)
	require.NoError(t, db.Raw(`SELECT id FROM announcement WHERE id <> ?`, seedID).Scan(&keepAnnouncements).Error)
	require.NoError(t, db.Raw(`SELECT id FROM avatar_decoration WHERE id <> ?`, seedID).Scan(&keepDecorations).Error)
	require.NoError(t, db.Raw(`SELECT id FROM emoji WHERE id NOT IN (?, ?)`, seedID, seedID2).Scan(&keepEmojis).Error)
	require.NoError(t, db.Raw(`SELECT id FROM drive_file WHERE id NOT IN (?, ?)`, seedID, guardFileID).Scan(&keepFiles).Error)
	keepFiles = append(keepFiles, guardFileID)
	var keepRoles, keepWebhooks, keepRecipients, keepReports []string
	require.NoError(t, db.Raw(`SELECT id FROM role WHERE id NOT IN (?, ?, ?)`, seedID, roleID, adminRoleID).Scan(&keepRoles).Error)
	keepRoles = append(keepRoles, roleID, adminRoleID)
	require.NoError(t, db.Raw(`SELECT id FROM system_webhook WHERE id <> ?`, seedID).Scan(&keepWebhooks).Error)
	require.NoError(t, db.Raw(`SELECT id FROM abuse_report_notification_recipient WHERE id <> ?`, seedID).Scan(&keepRecipients).Error)
	require.NoError(t, db.Raw(`SELECT id FROM abuse_user_report WHERE id <> ?`, seedID).Scan(&keepReports).Error)
	// admin/accounts/create は本物のローカルの利用者を作り、名前を used_username に
	// 記録する (#3861)。テストの前にあった利用者と名前を覚えておき、それ以外を消す。
	// テストの利用者・管理者は cleanup が消し、システムアカウント (instance.actor・
	// proxy.actor) は作り直さずに使い回されるので残す。
	// 中断された前回の実行 (タイムアウトの panic など) が t.Cleanup を通らずに残した、
	// accounts/create の利用者と名前を、控えを取る前に消す。控えに入ると消されずに
	// 残り続け、以後の実行が 409 で止まる。
	var createdNames []string
	for _, c := range readAJVCorpus(t, []string{"admin/accounts/create"}) {
		var in struct{ Username string }
		if c.ok && json.Unmarshal(c.in, &in) == nil && in.Username != "" {
			createdNames = append(createdNames, strings.ToLower(in.Username))
		}
	}
	require.NotEmpty(t, createdNames)
	require.NoError(t, db.Exec(`DELETE FROM "user" u WHERE u.host IS NULL AND u."usernameLower" = ANY(?::varchar[])
		AND NOT EXISTS (SELECT 1 FROM system_account s WHERE s."userId" = u.id)`, model.StringArray(createdNames)).Error)
	require.NoError(t, db.Exec(`DELETE FROM used_username WHERE lower(username) = ANY(?::varchar[])`, model.StringArray(createdNames)).Error)
	var keepUsers, keepUsedNames []string
	require.NoError(t, db.Raw(`SELECT id FROM "user" WHERE id NOT IN (?, ?, ?)`, seedID, userID, adminID).Scan(&keepUsers).Error)
	keepUsers = append(keepUsers, userID, adminID)
	require.NoError(t, db.Raw(`SELECT username FROM used_username`).Scan(&keepUsedNames).Error)
	// dropUserCache drops the users the server process caches. The seeded
	// remote user is deleted by SQL, which the cache does not see: a later
	// input would find it there and talk to its host (reversi/match fetches the
	// nodeinfo of the opponent's host, #3865). It is set once the server is up.
	var dropUserCache func()
	resetAdmin := func() {
		if dropUserCache != nil {
			defer dropUserCache()
		}
		require.NoError(t, db.Exec(`DELETE FROM promo_note WHERE "noteId" = ? OR "userId" = ?`, seedID, userID).Error)
		// 一覧が nil だと NULL を渡すことになり、NOT (id = ANY(NULL)) は何も消さない。
		// 空でも配列 ('{}') として渡す。
		keep := func(ids []string) model.StringArray { return append(model.StringArray{}, ids...) }
		require.NoError(t, db.Exec(`DELETE FROM ad WHERE NOT (id = ANY(?::varchar[]))`, keep(keepAds)).Error)
		require.NoError(t, db.Exec(`DELETE FROM announcement WHERE NOT (id = ANY(?::varchar[]))`, keep(keepAnnouncements)).Error)
		require.NoError(t, db.Exec(`DELETE FROM avatar_decoration WHERE NOT (id = ANY(?::varchar[]))`, keep(keepDecorations)).Error)
		require.NoError(t, db.Exec(`DELETE FROM emoji WHERE NOT (id = ANY(?::varchar[]))`, keep(keepEmojis)).Error)
		require.NoError(t, db.Exec(`DELETE FROM drive_file WHERE NOT (id = ANY(?::varchar[]))`, keep(keepFiles)).Error)
		require.NoError(t, db.Exec(`DELETE FROM abuse_user_report WHERE NOT (id = ANY(?::varchar[]))`, keep(keepReports)).Error)
		require.NoError(t, db.Exec(`DELETE FROM abuse_report_notification_recipient WHERE NOT (id = ANY(?::varchar[]))`, keep(keepRecipients)).Error)
		require.NoError(t, db.Exec(`DELETE FROM system_webhook WHERE NOT (id = ANY(?::varchar[]))`, keep(keepWebhooks)).Error)
		require.NoError(t, db.Exec(`DELETE FROM role_assignment WHERE "userId" = ? OR NOT ("roleId" = ANY(?::varchar[]))`, seedID, keep(keepRoles)).Error)
		require.NoError(t, db.Exec(`DELETE FROM role WHERE NOT (id = ANY(?::varchar[]))`, keep(keepRoles)).Error)
		require.NoError(t, db.Exec(`DELETE FROM "user" WHERE id = ?`, seedID).Error)
		require.NoError(t, db.Exec(`DELETE FROM "user" u WHERE NOT (u.id = ANY(?::varchar[]))
			AND NOT EXISTS (SELECT 1 FROM system_account s WHERE s."userId" = u.id)`, keep(keepUsers)).Error)
		require.NoError(t, db.Exec(`DELETE FROM used_username WHERE NOT (username = ANY(?::varchar[]))`, keep(keepUsedNames)).Error)
	}
	cleanup := func() {
		require.NoError(t, db.Where("id IN ?", []string{assignID, adminAssignID}).Delete(&model.RoleAssignment{}).Error)
		require.NoError(t, db.Where("id IN ?", []string{roleID, adminRoleID}).Delete(&model.Role{}).Error)
		require.NoError(t, db.Exec(`DELETE FROM clip WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM antenna WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM flash WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM page WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM channel WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM registration_ticket WHERE "createdById" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM access_token WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM app WHERE "userId" = ?`, userID).Error)
		// users/* の受理の入力で実際に書き込むのは users/lists/create だけ (#3837)。ほかの
		// 書き込み系は、corpus の id の利用者やリストが無いので、書き込みや外向きの通信の前に
		// NO_SUCH_USER / NO_SUCH_LIST で止まる。user_list は利用者の削除で消える (ON DELETE
		// CASCADE) が、何を作るかが読めるように明示して消す。
		require.NoError(t, db.Exec(`DELETE FROM user_list WHERE "userId" = ?`, userID).Error)
		// drive/* の受理の入力で実際に書き込むのは drive/folders/create だけ (#3839)。ほかの
		// 書き込み系は、corpus の id のファイルやフォルダが無いので、書き込みの前に
		// NO_SUCH_FILE / NO_SUCH_FOLDER で止まる。upload-from-url の受理の入力の url には
		// host が無く、取得の前に invalid url で止まるので、外へは出ない。
		require.NoError(t, db.Exec(`DELETE FROM drive_folder WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Exec(`DELETE FROM drive_file WHERE id = ?`, guardFileID).Error)
		// chat/* の受理の入力で実際に書き込むのは chat/rooms/create だけ (#3841)。ほかの
		// 書き込み系は、corpus の id の利用者・部屋・メッセージが無いので、書き込みや
		// 外向きの配送の前に NO_SUCH_* で止まる。chat_room は利用者の削除で消える
		// (ON DELETE CASCADE) が、何を作るかが読めるように明示して消す。
		require.NoError(t, db.Exec(`DELETE FROM chat_room WHERE "ownerId" = ?`, userID).Error)
		// export-custom-emojis・fetch-external-resources・request-reset-password・
		// reset-password・verify-email・v2/admin/emoji/list の受理の入力は書き込まない
		// (#3845)。export はジョブを積もうとするが、テストの Redis (miniredis) には
		// cmsgpack が無く、mkq の投入が失敗して 500 になる (ジョブは積まれない。
		// i/export-* も同じ。#3851)。fetch-external-resources の corpus の url には
		// host が無く、取得の前に止まる。request-reset-password は corpus の名前の
		// 利用者がいないので、password_reset_request の行を作らない。
		// notes/* の受理の入力は、入力ごとに置き直すノート・投票・下書き・リストと、
		// そこへのお気に入り・リアクション・投票・スレッドのミュートを書き込む
		// (#3847)。どれも利用者の削除で消える (ON DELETE CASCADE) が、何を作るかが
		// 読めるように明示して消す。hashtag の行を作る notes/create は載せていない
		// (enforcementDeferred) ので、hashtag は書き込まない。
		resetNotes()
		// i/* の受理の入力は、入力ごとに置き直すプロフィール・Webhook・レジストリの値と、
		// webhooks/create・registry/set・pin が足す行を書き込む (#3851)。パスワードを
		// 要る操作 (削除・トークンの再発行・パスワードやメールの変更・2 段階認証) は、
		// corpus に無いパスワードで「パスワードが違う」に止まるので書き込まない
		// (ループの中で確かめる)。どれも利用者の削除で消える (ON DELETE CASCADE) が、
		// 何を作るかが読めるように明示して消す。
		resetI()
		// admin/* の受理の入力は、入力ごとに置き直す corpus の id (abc123) の広告・
		// お知らせ・デコレーションと、作成の endpoint が足す広告・お知らせ・デコレーション・
		// 招待・プロモーション、それらを記録するモデレーションログを書き込む (#3854)。
		// 広告・お知らせ・デコレーションには作った人の列が無いので、テストの前に
		// あった id を覚えておき、それ以外を消す。
		// admin/drive/*・admin/emoji/* の受理の入力は、入力ごとに置き直すドライブの
		// ファイルと絵文字 (abc123・abc1231)、emoji/copy が足す絵文字、それらを記録する
		// モデレーションログを書き込む (#3857)。絵文字とシステム所有のファイルにも
		// 作った人の列が無いので、同じくテストの前にあった id 以外を消す。
		// ロール・通報・通報先・システム Webhook の受理の入力は、入力ごとに置き直す
		// リモートの利用者・ロール・通報・通報先・システム Webhook (abc123) と、作成の
		// endpoint が足すロール・通報先・システム Webhook、ロールの付与を書き込む
		// (#3859)。どれも作った人の列が無いので、テストの前にあった id 以外を消す。
		// forward-abuse-user-report が作るインスタンスのシステムアカウント
		// (instance.actor) は、作り直さずに使い回されるので残す。
		// 利用者の管理・連合・リレー・メールの受理の入力は、置き直すリモートの利用者
		// (abc123) と accounts/create が作るローカルの利用者、プロキシのアカウントの
		// 説明、IP の閲覧の記録 (ip_lookup_log) を書き込む (#3861)。説明は入力ごとに
		// 戻す (ループの中)。ip_lookup_log には外部キーが無いので、明示して消す。
		resetAdmin()
		require.NoError(t, db.Exec(`DELETE FROM ip_lookup_log WHERE "userId" = ?`, adminID).Error)
		require.NoError(t, db.Exec(`DELETE FROM registration_ticket WHERE "createdById" = ?`, adminID).Error)
		require.NoError(t, db.Exec(`DELETE FROM moderation_log WHERE "userId" = ?`, adminID).Error)
		require.NoError(t, db.Where("id IN ?", []string{userID, adminID}).Delete(&model.User{}).Error)
	}
	cleanup()
	t.Cleanup(cleanup)
	tok := token
	require.NoError(t, repository.NewUserRepository(db).Create(&model.User{
		ID: userID, Username: "penforce", UsernameLower: "penforce", Token: &tok,
	}))
	// notes/search と notes/translate は、本家と同じく canSearchNotes・canUseTranslator
	// が無いと引数を見る前に UNAVAILABLE を返す。handler の検査まで届かせるため、
	// 利用者に与える (#3847)。
	// invite/* は route の canInvite の検査が引数の検証より先に 403 を返す。本家も
	// requiredRolePolicy を引数の検証より先に見るので順は同じで、検証まで届かせる
	// には利用者に canInvite が要る (#3762)。管理者限定の ap/get も同じ理由で、
	// 管理者のトークンで送る。他のendpointは、管理者でない利用者の経路で
	// 確かめたいので、管理者は ap/get・v2/admin/emoji/list と admin/* だけに使う
	// (#3768、#3854)。
	now := time.Now()
	require.NoError(t, db.Create(&model.Role{
		ID: roleID, Name: "penforce-role", UpdatedAt: now, LastUsedAt: now,
		Policies: datatypes.JSON(`{"canInvite":{"useDefault":false,"priority":0,"value":true},` +
			`"canSearchNotes":{"useDefault":false,"priority":0,"value":true},` +
			`"canUseTranslator":{"useDefault":false,"priority":0,"value":true},` +
			`"canImportAntennas":{"useDefault":false,"priority":0,"value":true},` +
			`"canImportBlocking":{"useDefault":false,"priority":0,"value":true},` +
			`"canImportFollowing":{"useDefault":false,"priority":0,"value":true},` +
			`"canImportMuting":{"useDefault":false,"priority":0,"value":true},` +
			`"canImportUserLists":{"useDefault":false,"priority":0,"value":true}}`),
	}).Error)
	require.NoError(t, db.Create(&model.RoleAssignment{ID: assignID, UserID: userID, RoleID: roleID}).Error)
	adminTok := adminToken
	require.NoError(t, repository.NewUserRepository(db).Create(&model.User{
		ID: adminID, Username: "penforceadmin", UsernameLower: "penforceadmin", Token: &adminTok,
	}))
	require.NoError(t, db.Create(&model.Role{
		ID: adminRoleID, Name: "penforce-admin-role", UpdatedAt: now, LastUsedAt: now, IsAdministrator: true,
	}).Error)
	require.NoError(t, db.Create(&model.RoleAssignment{ID: adminAssignID, UserID: adminID, RoleID: adminRoleID}).Error)
	// admin/drive/cleanup が消してよいのは、参照されていないシステム所有のファイル
	// だけ。利用者のファイルを 1 つ置き、ループの後に残っていることを確かめる (#3857)。
	guardOwner := userID
	require.NoError(t, db.Create(&model.DriveFile{
		ID: guardFileID, UserID: &guardOwner, Name: "guard.png", Type: "image/png", URL: "http://192.0.2.1/guard.png",
	}).Error)
	// 外向きの通信が起きないことを確かめるため、外へ出る経路の proxy に、届いた
	// リクエストを数えるだけのサーバーを置く (#3847)。
	var outbound atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		outbound.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	_, on, _ := newParamShadowServers(t, func(cfg *config.Config) {
		cfg.Proxy = proxy.URL
		// 下の確かめに使う宛先。TEST-NET-1 で、どこへも届かない。
		cfg.AllowedPrivateNetworks = []string{"192.0.2.0/24"}
	})
	dropUserCache = on.userCache.DropLocal
	// proxy が外向きの通信を実際に拾えることを先に確かめる。拾えないと、下の 0 件は
	// 何も見ていないのと同じになる。
	if res, err := on.outboundClient(5 * time.Second).Get("http://192.0.2.1/probe"); err == nil {
		require.NoError(t, res.Body.Close())
	}
	require.EqualValues(t, 1, outbound.Load(), "the recording proxy does not see the outbound requests")
	outbound.Store(0)

	// notes/* の受理の入力の多くは、corpus の id (abc123) のノートや下書きが無いと
	// handler の検査より先に NO_SUCH_NOTE などで止まる。受理の入力ごとに、利用者が持つ
	// 公開のノート (投票付き)・下書き・リストを abc123 で置き直し、handler の検査まで
	// 届かせる (#3847)。notes/delete などが消したり書き足したりするので、毎回作り直す。
	seedNotes := func() {
		resetNotes()
		text := "hello"
		require.NoError(t, db.Create(&model.Note{
			ID: seedID, UserID: userID, Text: &text, Visibility: model.NoteVisibilityPublic, HasPoll: true,
		}).Error)
		require.NoError(t, db.Create(&model.Poll{
			NoteID: seedID, UserID: userID, Choices: model.StringArray{"a", "b"}, Votes: model.Int64Array{0, 0},
			NoteVisibility: model.NoteVisibilityPublic,
		}).Error)
		require.NoError(t, db.Create(&model.NoteDraft{ID: seedID, UserID: userID, Text: &text, Visibility: "public"}).Error)
		require.NoError(t, db.Create(&model.UserList{ID: seedID, UserID: userID, Name: "penforce"}).Error)
	}

	// i/* の受理の入力の多くは、プロフィール (パスワード)・corpus の id (abc123) の
	// Webhook・key が hello のレジストリの値が無いと、handler の検査より先に止まる。
	// 受理の入力ごとに置き直し、handler の検査まで届かせる (#3851)。i/pin のために
	// ノートも置き直す。webhooks/create は 1 人 3 件までなので、毎回消してから作る。
	// パスワードは corpus に無い値にし、パスワードを要る操作を「パスワードが違う」で
	// 止める。照合に失敗した回数の制限 (passwordguard、10 回) に当たらないよう、
	// その記録も毎回消す。
	//
	// i/2fa/done は、2 段階認証の設定を始めた状態 (仮の秘密と、その期限と試行の
	// 回数の記録) を置き、token の照合 (403 INVALID_TOKEN) まで届かせる (#3851)。
	// 置かないと「設定を始めていない」の 400 で止まり、照合の手前で受理の入力を
	// 拒否する回帰を見逃す。テストのサーバーは testMode で、そのままでは任意の
	// token を受けて 2 段階認証が有効になってしまうので、本家と同じ
	// MISSKEY_TEST_CHECK_DUPLICATED_TOTP=1 でテスト用のバイパスを止める。corpus の
	// token は 6 桁の数字でない (下で確かめる) ので、たまたま正しい TOTP にはならない。
	t.Setenv("MISSKEY_TEST_CHECK_DUPLICATED_TOTP", "1")
	const tempSecret = "JBSWY3DPEHPK3PXP"
	pending := coretwofactor.NewRedisPendingGuard(on.redis.Default)
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(iPassword), bcrypt.MinCost)
	require.NoError(t, err)
	hash := string(passwordHash)
	seedI := func(ep string) {
		seedNotes()
		resetI()
		profile := &model.UserProfile{UserID: userID, Password: &hash, Email: &iEmail}
		if ep == "i/2fa/done" {
			secret := tempSecret
			profile.TwoFactorTempSecret = &secret
			require.NoError(t, pending.Start(t.Context(), userID, tempSecret))
		}
		require.NoError(t, db.Create(profile).Error)
		require.NoError(t, db.Create(&model.Webhook{
			ID: seedID, UserID: userID, Name: "penforce", URL: "https://remote.example/hook", Secret: "s", On: model.StringArray{"note"},
		}).Error)
		require.NoError(t, db.Create(&model.RegistryItem{
			ID: seedID, UpdatedAt: time.Now(), UserID: userID, Key: "hello", Value: datatypes.JSON(`"v"`), Scope: model.StringArray{},
		}).Error)
		keys, err := on.redis.Default.Keys(t.Context(), "passwordguard:failures:"+userID+"*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, on.redis.Default.Del(t.Context(), keys...).Err())
		}
	}

	// admin/* の受理の入力の多くは、corpus の id (abc123) の広告・お知らせ・
	// デコレーションが無いと、handler の検査より先に NOT_FOUND などで止まる。受理の
	// 入力ごとに置き直し、handler の検査まで届かせる (#3854)。promo/create のために、
	// 利用者が持つ公開のノート (abc123) も置き直し、前の入力が作ったプロモーションを
	// 消す (ALREADY_PROMOTED で止めない)。プロモーションはノートを消すと消える
	// (ON DELETE CASCADE) が、何を消すかが読めるように明示して消す。
	// admin/drive/*・admin/emoji/* の受理の入力の多くは、corpus の id (abc123) の
	// ファイルや絵文字が無いと、handler の検査より先に NO_SUCH_FILE・NO_SUCH_EMOJI で
	// 止まる。受理の入力ごとに、システム所有の画像のファイル (abc123) と、ローカルの
	// 絵文字 abc123 (名前は hello)・abc1231 (hello1) を置き直す (#3857)。ファイルは
	// accessKey を持たないので、emoji/update の fileId で使っても、削除系で消しても、
	// ストレージへの書き込みも削除も起きない。システム所有なので、emoji/update は
	// 複製を作らない。emoji/copy には、abc123 をリモートの絵文字として置く。
	// originalUrl が空なので、画像を取りに行かずに複製する (取りに行くと外向きの
	// 通信になる)。
	//
	// 種の URL にホスト名を使わない。外向きの HTTP クライアント (safehttp) は、
	// proxy に渡す前に宛先を手元で名前解決するので、下の proxy では名前解決を
	// 数えられない。ホスト名の無い URL (TEST-NET-1 の IP) にしておけば、取りに
	// 行っても名前解決は起きず、proxy に届いて数えられる。
	seedDriveEmoji := func(remoteCopySource bool) {
		const base = "http://192.0.2.1/penforce/"
		thumb := base + "thumb.png"
		require.NoError(t, db.Create(&model.DriveFile{
			ID: seedID, Name: "penforce.png", Type: "image/png", URL: base + "file.png", ThumbnailURL: &thumb,
		}).Error)
		if remoteCopySource {
			host := "192.0.2.1"
			require.NoError(t, db.Create(&model.Emoji{
				ID: seedID, Name: "hello", Host: &host, OriginalURL: "", PublicURL: base + "remote.png",
			}).Error)
		} else {
			for _, e := range []struct{ id, name string }{{seedID, "hello"}, {seedID2, "hello1"}} {
				require.NoError(t, db.Create(&model.Emoji{
					ID: e.id, Name: e.name, OriginalURL: base + "file.png", PublicURL: base + "file.png",
				}).Error)
			}
		}
		// 置いた行の URL に、ホスト名が無いことを確かめる。
		var urls []string
		require.NoError(t, db.Raw(`SELECT u FROM (
			SELECT url AS u FROM drive_file WHERE id = ?
			UNION ALL SELECT "thumbnailUrl" FROM drive_file WHERE id = ?
			UNION ALL SELECT "webpublicUrl" FROM drive_file WHERE id = ?
			UNION ALL SELECT uri FROM drive_file WHERE id = ?
			UNION ALL SELECT "originalUrl" FROM emoji WHERE id IN (?, ?)
			UNION ALL SELECT "publicUrl" FROM emoji WHERE id IN (?, ?)
			UNION ALL SELECT uri FROM emoji WHERE id IN (?, ?)
		) s WHERE u IS NOT NULL`, seedID, seedID, seedID, seedID, seedID, seedID2, seedID, seedID2, seedID, seedID2).Scan(&urls).Error)
		require.NotEmpty(t, urls)
		for _, raw := range urls {
			if raw == "" {
				continue
			}
			u, err := url.Parse(raw)
			require.NoError(t, err, raw)
			ip := net.ParseIP(u.Hostname())
			require.True(t, ip != nil && testNet1.Contains(ip), "seeded url %q has a host name or an address outside TEST-NET-1", raw)
		}
	}
	// ロール・通報・通報先・システム Webhook の受理の入力の多くは、corpus の id
	// (abc123) の利用者・ロール・通報・Webhook が無いと、handler の検査より先に
	// NO_SUCH_ROLE などで止まる。受理の入力ごとに、リモートの利用者 (abc123)、
	// ロール、システム Webhook、通報先、その利用者への通報を置き直す (#3859)。
	// roles/unassign には、利用者へのロールの付与も置く (置かないと NOT_ASSIGNED で
	// 止まる)。ロールの condFormula と policies は空にし、テストの利用者の権限を
	// 変えない。利用者・Webhook の URL は TEST-NET-1 の IP にし、配送をキューに積む
	// system-webhook/test・forward-abuse-user-report でも名前解決が起きないように
	// する。
	seedModeration := func(ep string) {
		const base = "http://192.0.2.1"
		host := "192.0.2.1"
		inbox, uri := base+"/inbox", base+"/users/abc123"
		require.NoError(t, repository.NewUserRepository(db).Create(&model.User{
			ID: seedID, Username: "penforceremote", UsernameLower: "penforceremote", Host: &host, Inbox: &inbox, URI: &uri,
		}))
		require.NoError(t, db.Create(&model.Role{
			ID: seedID, Name: "penforce-seed-role", UpdatedAt: now, LastUsedAt: now,
			CondFormula: datatypes.JSON(`{}`), Policies: datatypes.JSON(`{}`),
		}).Error)
		if ep == "admin/roles/unassign" {
			require.NoError(t, db.Create(&model.RoleAssignment{ID: seedID, UserID: seedID, RoleID: seedID}).Error)
		}
		require.NoError(t, db.Create(&model.SystemWebhook{
			ID: seedID, Name: "penforce", URL: base + "/hook", Secret: "s", On: model.StringArray{},
		}).Error)
		require.NoError(t, db.Create(&model.AbuseReportNotificationRecipient{ID: seedID, Name: "penforce", Method: "email"}).Error)
		require.NoError(t, db.Create(&model.AbuseUserReport{
			ID: seedID, TargetUserID: seedID, ReporterID: userID, Comment: "penforce", TargetUserHost: &host,
		}).Error)
		// 置いた行の URL に、ホスト名が無いことを確かめる。
		var urls []string
		require.NoError(t, db.Raw(`SELECT u FROM (
			SELECT inbox AS u FROM "user" WHERE id = ?
			UNION ALL SELECT "sharedInbox" FROM "user" WHERE id = ?
			UNION ALL SELECT uri FROM "user" WHERE id = ?
			UNION ALL SELECT url FROM system_webhook WHERE id = ?
		) s WHERE u IS NOT NULL`, seedID, seedID, seedID, seedID).Scan(&urls).Error)
		require.Len(t, urls, 3)
		for _, raw := range urls {
			u, err := url.Parse(raw)
			require.NoError(t, err, raw)
			ip := net.ParseIP(u.Hostname())
			require.True(t, ip != nil && testNet1.Contains(ip), "seeded url %q has a host name or an address outside TEST-NET-1", raw)
		}
	}
	seedAdmin := func(ep string) {
		seedNotes()
		resetAdmin()
		future := time.Now().Add(24 * time.Hour)
		require.NoError(t, db.Create(&model.Ad{
			ID: seedID, URL: "https://remote.example/ad", ImageURL: "https://remote.example/ad.png", Place: "square",
			Priority: "middle", Ratio: 1, StartsAt: time.Now(), ExpiresAt: future,
		}).Error)
		require.NoError(t, db.Create(&model.Announcement{ID: seedID, Title: "penforce", Text: "penforce", IsActive: true}).Error)
		require.NoError(t, db.Create(&model.AvatarDecoration{
			ID: seedID, Name: "penforce", URL: "https://remote.example/deco.png", RoleIDs: model.StringArray{},
		}).Error)
		if strings.HasPrefix(ep, "admin/drive/") || strings.HasPrefix(ep, "admin/emoji/") {
			seedDriveEmoji(ep == "admin/emoji/copy")
		}
		if moderationEndpoint(ep) {
			seedModeration(ep)
		}
	}
	// テストの管理者と meta が、admin/* の入力で変わらないことを最後に確かめるため、
	// 流す前の状態を覚えておく (#3854)。
	type adminState struct {
		Token       *string
		IsDeleted   bool
		IsSuspended bool
		Roles       []string
		Tokens      int64
	}
	readAdmin := func() adminState {
		var u model.User
		require.NoError(t, db.First(&u, "id = ?", adminID).Error)
		st := adminState{Token: u.Token, IsDeleted: u.IsDeleted, IsSuspended: u.IsSuspended}
		require.NoError(t, db.Raw(`SELECT "roleId" FROM role_assignment WHERE "userId" = ? ORDER BY "roleId"`, adminID).Scan(&st.Roles).Error)
		require.NoError(t, db.Raw(`SELECT count(*) FROM access_token WHERE "userId" = ?`, adminID).Scan(&st.Tokens).Error)
		return st
	}
	readMeta := func() []string {
		var rows []string
		require.NoError(t, db.Raw(`SELECT row_to_json(m)::text FROM meta m ORDER BY id`).Scan(&rows).Error)
		return rows
	}
	adminBefore := readAdmin()
	metaBefore := readMeta()
	// 行が無いと、変わらないことの確かめが空振りする。
	require.Len(t, metaBefore, 1)
	// admin/send-email は、メールが有効だと SMTP で外へ送る。無効であることを
	// 確かめる (#3861)。
	var enableEmail bool
	require.NoError(t, db.Raw(`SELECT "enableEmail" FROM meta`).Scan(&enableEmail).Error)
	require.False(t, enableEmail, "admin/send-email would send mail")
	// admin/update-proxy-account はプロキシのアカウントの説明を書き換えるので、
	// 入力ごとに流す前の値へ戻す (#3861)。アカウントがまだ無ければ、作られたときの
	// 説明は空 (NULL) になる。
	readProxyDescription := func() *string {
		var rows []struct{ Description *string }
		require.NoError(t, db.Raw(`SELECT p.description FROM user_profile p
			JOIN system_account s ON s."userId" = p."userId" WHERE s.type = 'proxy'`).Scan(&rows).Error)
		require.LessOrEqual(t, len(rows), 1)
		if len(rows) == 0 {
			return nil
		}
		return rows[0].Description
	}
	proxyDescriptionBefore := readProxyDescription()
	// roles/update-default-policies と captcha/save の受理の入力は meta を書き換える
	// (#3859)。入力ごとに、流す前の行へ戻す。列は流す前の行の JSON から読み直す。
	// 戻すと、それより前の入力が meta を書き換えていても消えてしまうので、流す前に
	// 変わっていないことを確かめてから戻す。
	var metaCols []string
	require.NoError(t, db.Raw(`SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'meta' AND column_name <> 'id' ORDER BY column_name`).Scan(&metaCols).Error)
	require.NotEmpty(t, metaCols)
	quoted := make([]string, len(metaCols))
	for i, c := range metaCols {
		quoted[i] = `"` + c + `"`
	}
	restoreMetaSQL := `UPDATE meta SET (` + strings.Join(quoted, ", ") + `) = (SELECT ` + strings.Join(quoted, ", ") +
		` FROM json_populate_record(NULL::meta, ?::json))`
	restoreMeta := func() {
		require.NoError(t, db.Exec(restoreMetaSQL, metaBefore[0]).Error)
	}

	eps, err := paramschema.Upstream()
	require.NoError(t, err)
	cases := readAJVCorpus(t, paramEnforcedEndpoints)
	seen := map[string][2]int{}
	for _, tc := range cases {
		n := seen[tc.ep]
		if tc.ok {
			n[0]++
		} else {
			n[1]++
		}
		seen[tc.ep] = n
	}
	for _, name := range paramEnforcedEndpoints {
		// 何も流さずに緑にならないよう、各endpointに受理と拒否の両方を要求する。
		// paramDef が {} で何でも通す endpoint (reversi/invitations) には拒否の
		// 入力が無いので、受理だけを要求する (#3843)。
		require.Positive(t, seen[name][0], "the corpus has no accepted input for %s", name)
		if anyBodyEndpoints()[name] {
			require.Zero(t, seen[name][1], "the corpus rejects an input of %s, whose paramDef accepts any body", name)
			continue
		}
		require.Positive(t, seen[name][1], "the corpus has no rejected input for %s", name)
	}

	stricterSeen := map[string]int{}
	// pausedQueues counts the queues the accepted admin/queue/pause inputs
	// actually paused.
	pausedQueues := 0
	adminPost := func(ep, body string, wantStatus int) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/"+ep, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		on.Handler().ServeHTTP(rec, req)
		require.Equal(t, wantStatus, rec.Code, "%s %s: %s", ep, body, rec.Body.String())
		return rec.Body.Bytes()
	}
	queuePaused := func(name string) bool {
		t.Helper()
		var stats struct {
			IsPaused bool `json:"isPaused"`
		}
		require.NoError(t, json.Unmarshal(adminPost("admin/queue/queue-stats", `{"queue":`+strconv.Quote(name)+`}`, http.StatusOK), &stats))
		return stats.IsPaused
	}
	seedReached := map[string]int{}
	jobReached := map[string]int{}
	adminSeeded := false
	for _, tc := range cases {
		if tc.ok && (tc.ep == "notes" || strings.HasPrefix(tc.ep, "notes/")) {
			seedNotes()
		}
		isAdmin := strings.HasPrefix(tc.ep, "admin/")
		switch {
		case tc.ok && isAdmin:
			seedAdmin(tc.ep)
			adminSeeded = true
		case adminSeeded && !isAdmin:
			// 置いたお知らせなどを、admin/* でない endpoint (i/read-announcement など)
			// に見せない。
			resetAdmin()
			adminSeeded = false
		}
		isI := tc.ep == "i" || strings.HasPrefix(tc.ep, "i/")
		if isI {
			require.NotContains(t, string(tc.in), iPassword, "corpus line %d has the test user's password", tc.line)
		}
		if tc.ok && tc.ep == "i/2fa/done" {
			var in struct{ Token string }
			require.NoError(t, json.Unmarshal(tc.in, &in), "corpus line %d", tc.line)
			require.False(t, sixDigits.MatchString(strings.Join(strings.Fields(in.Token), "")), "corpus line %d: the token may be a valid TOTP", tc.line)
		}
		if tc.ok && isI {
			seedI(tc.ep)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/"+tc.ep, bytes.NewReader(tc.in))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		bearer := token
		if tc.ep == "ap/get" || tc.ep == "v2/admin/emoji/list" || isAdmin {
			bearer = adminToken
		}
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+bearer)
		metaWriting := slices.Contains(metaWritingEndpoints, tc.ep)
		if metaWriting {
			require.Equal(t, metaBefore, readMeta(), "meta changed before %s (corpus line %d)", tc.ep, tc.line)
		}
		if tc.ok {
			// 外へ出る手前で止まることを確かめる (#3861)。refresh-remote-instance-metadata
			// は instance があると、その host の nodeinfo を取りに行く (名前解決が起きる)。
			// relays/add は https の inbox だと、リレーへ Follow を送る。
			var in struct {
				Host  string `json:"host"`
				Inbox string `json:"inbox"`
			}
			_ = json.Unmarshal(tc.in, &in)
			switch tc.ep {
			case "admin/federation/refresh-remote-instance-metadata":
				var n int64
				require.NoError(t, db.Raw(`SELECT count(*) FROM instance WHERE host = lower(?)`, in.Host).Scan(&n).Error)
				require.Zero(t, n, "corpus line %d: the instance %q exists and would be fetched", tc.line, in.Host)
			case "admin/relays/add":
				require.False(t, strings.HasPrefix(strings.ToLower(in.Inbox), "https://"), "corpus line %d: relays/add would follow %q", tc.line, in.Inbox)
			}
		}
		rec := httptest.NewRecorder()
		on.Handler().ServeHTTP(rec, req)
		if metaWriting {
			restoreMeta()
		}
		if tc.ep == "admin/update-proxy-account" {
			require.NoError(t, db.Exec(`UPDATE user_profile SET description = ?
				WHERE "userId" IN (SELECT "userId" FROM system_account WHERE type = 'proxy')`, proxyDescriptionBefore).Error)
		}
		// admin/queue/pause の受理の入力は、テストの Redis のキューを止める (Elythia が
		// 運用していないキューは何もせずに 204)。実際に止まったキューは、すぐに同じ
		// 名前で再開し、動いていることを確かめる。後の入力や最後の確かめに残さない
		// (#3865)。
		if tc.ok && tc.ep == "admin/queue/pause" && rec.Code == http.StatusNoContent {
			var in struct{ Queue string }
			require.NoError(t, json.Unmarshal(tc.in, &in))
			if queuePaused(in.Queue) {
				pausedQueues++
				adminPost("admin/queue/resume", `{"queue":`+strconv.Quote(in.Queue)+`}`, http.StatusNoContent)
				assert.False(t, queuePaused(in.Queue), "resume did not restart %s", in.Queue)
			}
		}

		var env struct {
			Error struct {
				Code string `json:"code"`
				ID   string `json:"id"`
				Info struct {
					Param  string `json:"param"`
					Reason string `json:"reason"`
				} `json:"info"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		name := tc.ep + " " + string(tc.in)
		if eps[tc.ep].RequireFile {
			// 本家は requireFile の endpoint (drive/files/create) の引数を multipart の
			// field から読み、JSON の body には ajv より先に本文の無い 400 を返す
			// (handleMultipartRequest)。Elythia は RequireMultipartFile が同じ 400 を返す。
			// 検証器は multipart を検証しないので、ajv の判定によらずこの応答になる (#3839)。
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s (corpus line %d)", name, tc.line)
			assert.Empty(t, rec.Body.String(), name)
			continue
		}
		if tc.ok && isI {
			// パスワードを要る操作が、handler の照合まで届いて止まったことと、
			// 利用者に壊す変更 (削除・トークンの再発行・パスワードやメールの変更・
			// 2 段階認証の設定・移行) が起きていないことを確かめる (#3851)。
			if slices.Contains(passwordCheckedEndpoints, tc.ep) {
				assert.Equal(t, "INCORRECT_PASSWORD", env.Error.Code, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
			}
			if tc.ep == "i/2fa/done" {
				assert.Equal(t, "INVALID_TOKEN", env.Error.Code, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
			}
			var u model.User
			require.NoError(t, db.First(&u, "id = ?", userID).Error, name)
			assert.Equal(t, &tok, u.Token, name)
			assert.False(t, u.IsDeleted, name)
			assert.Nil(t, u.MovedToURI, name)
			var p model.UserProfile
			require.NoError(t, db.First(&p, `"userId" = ?`, userID).Error, name)
			assert.Equal(t, &hash, p.Password, name)
			assert.Equal(t, &iEmail, p.Email, name)
			if tc.ep == "i/2fa/done" {
				// 照合に失敗したので、置いた仮の秘密はそのまま残る。
				secret := tempSecret
				assert.Equal(t, &secret, p.TwoFactorTempSecret, name)
			} else {
				assert.Nil(t, p.TwoFactorTempSecret, name)
			}
			assert.Nil(t, p.TwoFactorSecret, name)
			assert.False(t, p.TwoFactorEnabled, name)
		}
		if tc.ok && isAdmin {
			// 置いた広告・お知らせ・デコレーション・ノートを指す入力が、handler の
			// 検査まで届いたこと (見つからないで止まっていないこと) を確かめる (#3854)。
			// ファイルと絵文字 (#3857) も同じ。
			// ロール・通報・Webhook (#3859) も同じ。
			var in struct {
				ID        string `json:"id"`
				NoteID    string `json:"noteId"`
				EmojiID   string `json:"emojiId"`
				FileID    string `json:"fileId"`
				RoleID    string `json:"roleId"`
				UserID    string `json:"userId"`
				ReportID  string `json:"reportId"`
				WebhookID string `json:"webhookId"`
			}
			if json.Unmarshal(tc.in, &in) == nil && (in.ID == seedID || in.NoteID == seedID || in.EmojiID == seedID || in.FileID == seedID ||
				in.RoleID == seedID || in.ReportID == seedID || in.WebhookID == seedID || in.UserID == seedID) {
				notFound := []string{"NOT_FOUND", "NO_SUCH_AD", "NO_SUCH_ANNOUNCEMENT", "NO_SUCH_NOTE", "ALREADY_PROMOTED", "NO_SUCH_EMOJI",
					"NO_SUCH_ABUSE_REPORT", "NO_SUCH_SYSTEM_WEBHOOK", "NO_SUCH_WEBHOOK", "NO_SUCH_RECIPIENT"}
				if in.FileID == seedID {
					notFound = append(notFound, "NO_SUCH_FILE")
				}
				// roleId・userId は、置いたロール・利用者を指すときだけ見る (roles/assign の
				// 置いていない roleId は NO_SUCH_ROLE で止まってよい)。
				if in.RoleID == seedID {
					notFound = append(notFound, "NO_SUCH_ROLE")
				}
				if in.UserID == seedID {
					notFound = append(notFound, "NO_SUCH_USER", "NOT_ASSIGNED")
				}
				assert.NotContains(t, notFound, env.Error.Code, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
				seedReached[tc.ep]++
			}
			// ジョブを積む 2 件は、テストの Redis (miniredis) に cmsgpack が無いので
			// 積めずに 500 になる。積む手前の検査を通り、積むところまで届いたことを
			// 確かめる (#3857)。
			if slices.Contains(jobEnqueuingEndpoints, tc.ep) {
				assert.Equal(t, http.StatusInternalServerError, rec.Code, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
				assert.Equal(t, "INTERNAL_ERROR", env.Error.Code, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
				jobReached[tc.ep]++
			}
			// accounts/create の受理の入力は、利用者を作るところまで届く (#3861)。前の
			// 入力や前の実行が作った利用者と名前 (used_username) が残っていると、409
			// USERNAME_ALREADY_EXISTS で止まる。
			if tc.ep == "admin/accounts/create" {
				assert.Equal(t, http.StatusOK, rec.Code, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
			}
		}
		if tc.ok {
			var in map[string]any
			if stricter, ok := handlerStricterThanUpstream[tc.ep]; ok && json.Unmarshal(tc.in, &in) == nil && stricter.match(in) {
				stricterSeen[tc.ep]++
				assert.Equal(t, "INVALID_PARAM", env.Error.Code, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
				assert.Equal(t, stricter.id, env.Error.ID, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
				continue
			}
			assert.NotEqual(t, "INVALID_PARAM", env.Error.Code, "%s (corpus line %d): ajv accepts it; body %s", name, tc.line, rec.Body.String())
			continue
		}
		if assert.Equal(t, http.StatusBadRequest, rec.Code, "%s (corpus line %d)", name, tc.line) {
			assert.Equal(t, "INVALID_PARAM", env.Error.Code, name)
			assert.Equal(t, tc.path, env.Error.Info.Param, name)
			assert.Equal(t, tc.msg, env.Error.Info.Reason, name)
		}
	}
	assert.Zero(t, outbound.Load(), "the corpus inputs made outbound requests")
	// 全部を流した後も、利用者が残り、トークンが変わっていないこと (#3851)。
	var after model.User
	require.NoError(t, db.First(&after, "id = ?", userID).Error)
	assert.Equal(t, &tok, after.Token)
	assert.False(t, after.IsDeleted)
	// 置き直しが空振りしていないこと: 置いたものを指す受理の入力が、各 endpoint に
	// あること (#3854)。
	for _, ep := range []string{"admin/ad/delete", "admin/ad/update", "admin/announcements/delete", "admin/announcements/update",
		"admin/avatar-decorations/delete", "admin/avatar-decorations/update", "admin/promo/create",
		"admin/drive/show-file", "admin/emoji/copy", "admin/emoji/delete", "admin/emoji/import-zip", "admin/emoji/update",
		"admin/roles/assign", "admin/roles/delete", "admin/roles/show", "admin/roles/unassign", "admin/roles/update", "admin/roles/users",
		"admin/abuse-report/notification-recipient/delete", "admin/abuse-report/notification-recipient/show",
		"admin/abuse-report/notification-recipient/update", "admin/forward-abuse-user-report", "admin/resolve-abuse-user-report",
		"admin/update-abuse-user-report", "admin/system-webhook/delete", "admin/system-webhook/show", "admin/system-webhook/test",
		"admin/system-webhook/update",
		"admin/accounts/delete", "admin/delete-account", "admin/delete-all-files-of-a-user", "admin/get-user-ips",
		"admin/reset-password", "admin/show-user", "admin/suspend-user", "admin/unset-mfa", "admin/unset-user-avatar",
		"admin/unset-user-banner", "admin/unsuspend-user", "admin/update-user-note"} {
		assert.Positive(t, seedReached[ep], "no accepted corpus input of %s names the seeded id", ep)
	}
	for _, ep := range jobEnqueuingEndpoints {
		assert.Positive(t, jobReached[ep], "no accepted corpus input of %s", ep)
	}
	// admin/drive/cleanup が、置いたもの以外のファイル (テストの前からあったものと、
	// 利用者のファイル) を消していないこと (#3857)。
	var keptFiles int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM drive_file WHERE id = ANY(?::varchar[])`, model.StringArray(keepFiles)).Scan(&keptFiles).Error)
	assert.EqualValues(t, len(keepFiles), keptFiles, "admin/drive/cleanup deleted a file it does not own")
	// テストの管理者 (削除・凍結・トークン・ロール) と meta が変わっていないこと (#3854)。
	assert.Equal(t, adminBefore, readAdmin())
	assert.Equal(t, proxyDescriptionBefore, readProxyDescription())
	// どのキューも止まっていないこと (#3865)。実際に止まった入力が無いと、ループの
	// 中の再開の確かめは何も見ていないのと同じになる。
	assert.Positive(t, pausedQueues, "no accepted corpus input of admin/queue/pause paused a queue")
	var queues []struct {
		Name     string `json:"name"`
		IsPaused bool   `json:"isPaused"`
	}
	require.NoError(t, json.Unmarshal(adminPost("admin/queue/queues", `{}`, http.StatusOK), &queues))
	require.NotEmpty(t, queues)
	for _, q := range queues {
		assert.False(t, q.IsPaused, "the queue %s is left paused", q.Name)
	}
	assert.Equal(t, []string{adminRoleID}, adminBefore.Roles)
	assert.Equal(t, metaBefore, readMeta())
	// 使われなくなった項目を残さない: corpus のどの入力にも当たらない例外は消す。
	for ep := range handlerStricterThanUpstream {
		assert.Positive(t, stricterSeen[ep], "handlerStricterThanUpstream[%s] matches no accepted corpus input", ep)
	}
}

// TestEnforcedGETMatchesUpstream sends GET queries to the enforced allowGet
// endpoints of a real server (charts/*, bubble-game/ranking, federation/stats,
// federation/instances,
// the top-level emoji, emojis, fetch-rss, get-online-users-count,
// retention and server-info, users/featured-notes and notes/featured) and compares the answers with upstream's: the cast step (JSON.parse of integer params, 0b5f1631-… on failure), then
// ajv on the cast values. The corpus has JSON bodies only, so the queries
// are written out here (#3744).
func TestEnforcedGETMatchesUpstream(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	_, on, _ := newParamShadowServers(t)

	const castID = "0b5f1631-7c1a-41a6-b399-cce335f34d85"
	const ajvID = "3d81ceae-475f-4600-b2a8-2bc116157532"
	for _, tc := range []struct {
		query string
		// ok: upstream accepts. Otherwise the INVALID_PARAM id, info.param
		// and info.reason upstream answers with.
		ok                bool
		id, param, reason string
		// code: the error the handler answers an accepted query with,
		// instead of 200 (an emoji that does not exist and the like).
		code string
	}{
		{query: "charts/notes?span=day", ok: true},
		{query: "charts/notes?span=day&limit=10.0", ok: true},
		{query: "charts/notes?span=day&limit=1e1", ok: true},
		{query: "charts/notes?span=day&limit=%2010", ok: true},
		{query: "charts/notes?span=day&offset=null", ok: true},
		{query: "charts/notes?span=day&offset=10.0", ok: true},
		{query: "charts/notes?span=day&unknown=1", ok: true},
		{query: "charts/instance?span=day&host=", ok: true},
		{query: "charts/instance?span=day&host=remote.example", ok: true},
		{query: "charts/user/notes?span=day&userId=abc123", ok: true},
		{query: "charts/notes?span=day&limit=abc", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "charts/notes?span=day&limit=", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "charts/notes?span=day&offset=x", id: castID, param: "offset", reason: "cannot cast to integer"},
		{query: "charts/notes?span=day&limit=abc&offset=x", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "charts/notes?span=day&limit=1&limit=2", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "charts/notes?span=day&span=hour", id: ajvID, param: "#/properties/span/type", reason: "must be string"},
		{query: "charts/notes?span=day&limit=null", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "charts/notes?span=day&limit=%2210%22", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "charts/notes?span=day&limit=10.5", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "charts/notes?span=day&limit=0", id: ajvID, param: "#/properties/limit/minimum", reason: "must be >= 1"},
		{query: "charts/notes?span=day&limit=501", id: ajvID, param: "#/properties/limit/maximum", reason: "must be <= 500"},
		{query: "charts/notes?span=week", id: ajvID, param: "#/properties/span/enum", reason: "must be equal to one of the allowed values"},
		{query: "charts/notes", id: ajvID, param: "#/required", reason: "must have required property 'span'"},
		{query: "charts/instance?span=day", id: ajvID, param: "#/required", reason: "must have required property 'host'"},
		{query: "charts/user/notes?span=week", id: ajvID, param: "#/required", reason: "must have required property 'userId'"},
		{query: "charts/user/notes?span=day&userId=x-y", id: ajvID, param: "#/properties/userId/format", reason: `must match format "misskey:id"`},
		{query: "charts/user/notes?span=day&userId=", id: ajvID, param: "#/properties/userId/format", reason: `must match format "misskey:id"`},
		// bubble-game/rankingは本家もallowGetで、文字列の引数は変換しない(#3768)。
		{query: "bubble-game/ranking?gameMode=normal", ok: true},
		{query: "bubble-game/ranking?gameMode=", ok: true},
		{query: "bubble-game/ranking", id: ajvID, param: "#/required", reason: "must have required property 'gameMode'"},
		{query: "bubble-game/ranking?gameMode=a&gameMode=b", id: ajvID, param: "#/properties/gameMode/type", reason: "must be string"},
		// federation/statsはadmin/overviewがGETで呼ぶ(#3770)。
		{query: "federation/stats", ok: true},
		{query: "federation/stats?limit=10.0", ok: true},
		{query: "federation/stats?limit=1e1", ok: true},
		{query: "federation/stats?limit=abc", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "federation/stats?limit=null", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "federation/stats?limit=0", id: ajvID, param: "#/properties/limit/minimum", reason: "must be >= 1"},
		{query: "federation/stats?limit=101", id: ajvID, param: "#/properties/limit/maximum", reason: "must be <= 100"},
		// federation/instancesはwelcome画面とadmin/federationがGETで呼ぶ。sortの
		// +host・-hostはElythiaが重ねた値(#3865)。queryの+は%2Bで送る。
		{query: "federation/instances", ok: true},
		{query: "federation/instances?limit=10.0&offset=1e1", ok: true},
		{query: "federation/instances?sort=%2Bhost", ok: true},
		{query: "federation/instances?sort=-host", ok: true},
		{query: "federation/instances?sort=%2BpubSub", ok: true},
		{query: "federation/instances?sort=bogus", id: ajvID, param: "#/properties/sort/enum", reason: "must be equal to one of the allowed values"},
		{query: "federation/instances?limit=abc", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "federation/instances?limit=0", id: ajvID, param: "#/properties/limit/minimum", reason: "must be >= 1"},
		{query: "federation/instances?limit=101", id: ajvID, param: "#/properties/limit/maximum", reason: "must be <= 100"},
		// 参照だけのトップレベルのendpoint(#3774)。
		{query: "emoji?name=hello", ok: true, code: "NO_SUCH_EMOJI"},
		{query: "emoji?name=", ok: true, code: "NO_SUCH_EMOJI"},
		{query: "emoji", id: ajvID, param: "#/required", reason: "must have required property 'name'"},
		{query: "emoji?name=a&name=b", id: ajvID, param: "#/properties/name/type", reason: "must be string"},
		{query: "fetch-rss?url=hello", ok: true, code: "INVALID_URL"},
		{query: "fetch-rss?url=", ok: true, code: "INVALID_URL"},
		{query: "fetch-rss", id: ajvID, param: "#/required", reason: "must have required property 'url'"},
		{query: "fetch-rss?url=a&url=b", id: ajvID, param: "#/properties/url/type", reason: "must be string"},
		{query: "emojis", ok: true},
		{query: "emojis?x=1", ok: true},
		{query: "get-online-users-count", ok: true},
		{query: "retention", ok: true},
		{query: "server-info", ok: true},
		// users/featured-notesは本家もallowGetで、存在しない利用者には空の配列を返す(#3837)。
		{query: "users/featured-notes?userId=abc123", ok: true},
		{query: "users/featured-notes?userId=abc123&limit=10.0", ok: true},
		{query: "users/featured-notes?userId=abc123&limit=1e1", ok: true},
		{query: "users/featured-notes?userId=abc123&limit=100e0", ok: true},
		{query: "users/featured-notes?userId=abc123&untilId=abc124", ok: true},
		{query: "users/featured-notes?userId=abc123&unknown=1", ok: true},
		{query: "users/featured-notes?userId=abc123&limit=abc", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "users/featured-notes?userId=abc123&limit=", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "users/featured-notes?userId=abc123&limit=null", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "users/featured-notes?userId=abc123&limit=10.5", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "users/featured-notes?userId=abc123&limit=0", id: ajvID, param: "#/properties/limit/minimum", reason: "must be >= 1"},
		{query: "users/featured-notes?userId=abc123&limit=101", id: ajvID, param: "#/properties/limit/maximum", reason: "must be <= 100"},
		{query: "users/featured-notes", id: ajvID, param: "#/required", reason: "must have required property 'userId'"},
		{query: "users/featured-notes?userId=", id: ajvID, param: "#/properties/userId/format", reason: `must match format "misskey:id"`},
		{query: "users/featured-notes?userId=a&userId=b", id: ajvID, param: "#/properties/userId/type", reason: "must be string"},
		{query: "users/featured-notes?userId=abc123&untilId=x-y", id: ajvID, param: "#/properties/untilId/format", reason: `must match format "misskey:id"`},
		// notes/featuredは本家もallowGetで、本家のparamDefに無いoffset・untilDateは読まない(#3847)。
		{query: "notes/featured", ok: true},
		{query: "notes/featured?limit=10.0", ok: true},
		{query: "notes/featured?limit=1e1", ok: true},
		{query: "notes/featured?limit=100e0", ok: true},
		{query: "notes/featured?untilId=abc123", ok: true},
		{query: "notes/featured?channelId=abc123", ok: true},
		{query: "notes/featured?unknown=1", ok: true},
		{query: "notes/featured?offset=x&untilDate=x", ok: true},
		{query: "notes/featured?limit=abc", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "notes/featured?limit=", id: castID, param: "limit", reason: "cannot cast to integer"},
		{query: "notes/featured?limit=null", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "notes/featured?limit=10.5", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "notes/featured?limit=0", id: ajvID, param: "#/properties/limit/minimum", reason: "must be >= 1"},
		{query: "notes/featured?limit=101", id: ajvID, param: "#/properties/limit/maximum", reason: "must be <= 100"},
		{query: "notes/featured?limit=1&limit=2", id: ajvID, param: "#/properties/limit/type", reason: "must be integer"},
		{query: "notes/featured?untilId=x-y", id: ajvID, param: "#/properties/untilId/format", reason: `must match format "misskey:id"`},
		{query: "notes/featured?channelId=", id: ajvID, param: "#/properties/channelId/format", reason: `must match format "misskey:id"`},
		{query: "notes/featured?channelId=a&channelId=b", id: ajvID, param: "#/properties/channelId/type", reason: "must be string"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/"+tc.query, nil)
		rec := httptest.NewRecorder()
		on.Handler().ServeHTTP(rec, req)
		var env struct {
			Error struct {
				Code string `json:"code"`
				ID   string `json:"id"`
				Info struct {
					Param  string `json:"param"`
					Reason string `json:"reason"`
				} `json:"info"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if tc.ok && tc.code != "" {
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.query, rec.Body.String())
			assert.Equal(t, tc.code, env.Error.Code, "%s: %s", tc.query, rec.Body.String())
			continue
		}
		if tc.ok {
			assert.Equal(t, http.StatusOK, rec.Code, "%s: %s", tc.query, rec.Body.String())
			continue
		}
		if assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.query, rec.Body.String()) {
			assert.Equal(t, "INVALID_PARAM", env.Error.Code, tc.query)
			assert.Equal(t, tc.id, env.Error.ID, tc.query)
			assert.Equal(t, tc.param, env.Error.Info.Param, tc.query)
			assert.Equal(t, tc.reason, env.Error.Info.Reason, tc.query)
		}
	}
}
