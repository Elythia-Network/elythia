package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ajvCorpusFile is the corpus of upstream's ajv verdicts (#3673).
const ajvCorpusFile = "../api/paramschema/testdata/ajv_corpus.jsonl"

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
	b, err := os.ReadFile(ajvCorpusFile)
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
	// 本家のsortのenumに、Elythia独自の+host / -hostが無い。enumに値を
	// 重ねる仕組みは、独自の引数の宣言(#3667の(4))で作る(#3770)。
	"federation/instances": `{"sort":"+host"}`,
	// userId に @user@host の形 (acct) を受ける。cherrypick 由来の拡張で、
	// tests/e2e-federation の対局が使う。本家の misskey:id の形には無いので、
	// 独自の引数の宣言(#3667の(4))で重ねるまで載せない(#3843)。
	"reversi/match": `{"userId":"@alice@remote.example"}`,
}

// enforcementDeferred lists the endpoints of an enforced package that are
// left to a later stage, with the reason. Unlike enforcementPending, they
// have no Elythia-only input that upstream's paramDef rejects.
var enforcementDeferred = map[string]string{
	// 独自の引数は無いが、本家と判定が分かれる入力 (U+0085 だけの text、NUL を含む
	// text、poll.choices の __proto__ の重複など) の扱いを、admin/update-meta・
	// i/update と一緒に最後の段で決める (#3667、#3847)。
	"notes/create": "decided with admin/update-meta and i/update in the last stage of #3667",
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
// of every note; see docs/divergence/approximations.md). Each entry must match
// at least one corpus input.
var handlerStricterThanUpstream = map[string]stricterCase{
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

// TestEnforcedEndpointsAreUpstreamEndpoints keeps paramEnforcedEndpoints to
// names upstream has, and to whole packages: an endpoint of a package left
// out would keep the handwritten checks only, unless enforcementPending or
// enforcementDeferred names it.
func TestEnforcedEndpointsAreUpstreamEndpoints(t *testing.T) {
	eps, err := paramschema.Upstream()
	require.NoError(t, err)
	packages := map[string]bool{}
	for _, name := range paramEnforcedEndpoints {
		ep, ok := eps[name]
		require.True(t, ok, "%s is not an upstream endpoint", name)
		require.NoError(t, ep.CompileErr, name)
		pkg, _, _ := strings.Cut(name, "/")
		packages[pkg] = true
	}
	require.NotEmpty(t, packages)
	for name := range eps {
		pkg, _, _ := strings.Cut(name, "/")
		if packages[pkg] && enforcementPending[name] == "" && enforcementDeferred[name] == "" {
			assert.Contains(t, paramEnforcedEndpoints, name, "package %s is enforced, but not %s", pkg, name)
		}
	}
	// 使われなくなった項目を残さない: 載せたか、パッケージごと外したら消す。
	for name, body := range enforcementPending {
		pkg, _, _ := strings.Cut(name, "/")
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
		pkg, _, _ := strings.Cut(name, "/")
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
		// chat/* の受理の入力で実際に書き込むのは chat/rooms/create だけ (#3841)。ほかの
		// 書き込み系は、corpus の id の利用者・部屋・メッセージが無いので、書き込みや
		// 外向きの配送の前に NO_SUCH_* で止まる。chat_room は利用者の削除で消える
		// (ON DELETE CASCADE) が、何を作るかが読めるように明示して消す。
		require.NoError(t, db.Exec(`DELETE FROM chat_room WHERE "ownerId" = ?`, userID).Error)
		// export-custom-emojis・fetch-external-resources・request-reset-password・
		// reset-password・verify-email・v2/admin/emoji/list の受理の入力は書き込まない
		// (#3845)。export はテストごとの Redis にジョブを積むだけで、worker がいないので
		// 走らない。fetch-external-resources の corpus の url には host が無く、取得の
		// 前に止まる。request-reset-password は corpus の名前の利用者がいないので、
		// password_reset_request の行を作らない。
		// notes/* の受理の入力は、入力ごとに置き直すノート・投票・下書き・リストと、
		// そこへのお気に入り・リアクション・投票・スレッドのミュートを書き込む
		// (#3847)。どれも利用者の削除で消える (ON DELETE CASCADE) が、何を作るかが
		// 読めるように明示して消す。hashtag の行を作る notes/create は載せていない
		// (enforcementDeferred) ので、hashtag は書き込まない。
		resetNotes()
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
	// 確かめたいので、管理者は ap/get だけに使う (#3768)。
	now := time.Now()
	require.NoError(t, db.Create(&model.Role{
		ID: roleID, Name: "penforce-role", UpdatedAt: now, LastUsedAt: now,
		Policies: datatypes.JSON(`{"canInvite":{"useDefault":false,"priority":0,"value":true},` +
			`"canSearchNotes":{"useDefault":false,"priority":0,"value":true},` +
			`"canUseTranslator":{"useDefault":false,"priority":0,"value":true}}`),
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
	const seedID = "abc123"
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
	for _, tc := range cases {
		if tc.ok && (tc.ep == "notes" || strings.HasPrefix(tc.ep, "notes/")) {
			seedNotes()
		}
		req := httptest.NewRequest(http.MethodPost, "/api/"+tc.ep, bytes.NewReader(tc.in))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		bearer := token
		if tc.ep == "ap/get" || tc.ep == "v2/admin/emoji/list" {
			bearer = adminToken
		}
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+bearer)
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
	// 使われなくなった項目を残さない: corpus のどの入力にも当たらない例外は消す。
	for ep := range handlerStricterThanUpstream {
		assert.Positive(t, stricterSeen[ep], "handlerStricterThanUpstream[%s] matches no accepted corpus input", ep)
	}
}

// TestEnforcedGETMatchesUpstream sends GET queries to the enforced allowGet
// endpoints of a real server (charts/*, bubble-game/ranking, federation/stats,
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
