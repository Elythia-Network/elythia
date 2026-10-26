package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/api/paramschema"
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

// handlerStricterThanUpstream lists the accepted corpus inputs an enforced
// endpoint still rejects with INVALID_PARAM on purpose, because upstream
// answers them with a 500. Each entry must match at least one corpus input.
var handlerStricterThanUpstream = map[string]func(in map[string]any) bool{
	// 本家は絵文字を含まない reaction を normalizeEmojiString が素の Error で
	// 投げて 500 にする (ChatService.react)。利用者の入力の誤りなので、Elythia は
	// 400 INVALID_PARAM にしている (#3841)。corpus の値は ASCII だけなので、
	// ASCII だけで :name: の形でないものを絵文字の無い値とみなす。
	"chat/messages/react": func(in map[string]any) bool {
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
	},
}

// TestEnforcedEndpointsAreUpstreamEndpoints keeps paramEnforcedEndpoints to
// names upstream has, and to whole packages: an endpoint of a package left
// out would keep the handwritten checks only, unless enforcementPending
// names it.
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
		if packages[pkg] && enforcementPending[name] == "" {
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
		require.NoError(t, db.Where("id IN ?", []string{userID, adminID}).Delete(&model.User{}).Error)
	}
	cleanup()
	t.Cleanup(cleanup)
	tok := token
	require.NoError(t, repository.NewUserRepository(db).Create(&model.User{
		ID: userID, Username: "penforce", UsernameLower: "penforce", Token: &tok,
	}))
	// invite/* は route の canInvite の検査が引数の検証より先に 403 を返す。本家も
	// requiredRolePolicy を引数の検証より先に見るので順は同じで、検証まで届かせる
	// には利用者に canInvite が要る (#3762)。管理者限定の ap/get も同じ理由で、
	// 管理者のトークンで送る。他のendpointは、管理者でない利用者の経路で
	// 確かめたいので、管理者は ap/get だけに使う (#3768)。
	now := time.Now()
	require.NoError(t, db.Create(&model.Role{
		ID: roleID, Name: "penforce-role", UpdatedAt: now, LastUsedAt: now,
		Policies: datatypes.JSON(`{"canInvite":{"useDefault":false,"priority":0,"value":true}}`),
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
	_, on, _ := newParamShadowServers(t)

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
		req := httptest.NewRequest(http.MethodPost, "/api/"+tc.ep, bytes.NewReader(tc.in))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		bearer := token
		if tc.ep == "ap/get" {
			bearer = adminToken
		}
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+bearer)
		rec := httptest.NewRecorder()
		on.Handler().ServeHTTP(rec, req)

		var env struct {
			Error struct {
				Code string `json:"code"`
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
			if stricter := handlerStricterThanUpstream[tc.ep]; stricter != nil && json.Unmarshal(tc.in, &in) == nil && stricter(in) {
				stricterSeen[tc.ep]++
				assert.Equal(t, "INVALID_PARAM", env.Error.Code, "%s (corpus line %d): body %s", name, tc.line, rec.Body.String())
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
	// 使われなくなった項目を残さない: corpus のどの入力にも当たらない例外は消す。
	for ep := range handlerStricterThanUpstream {
		assert.Positive(t, stricterSeen[ep], "handlerStricterThanUpstream[%s] matches no accepted corpus input", ep)
	}
}

// TestEnforcedGETMatchesUpstream sends GET queries to the enforced allowGet
// endpoints of a real server (charts/*, bubble-game/ranking, federation/stats,
// the top-level emoji, emojis, fetch-rss, get-online-users-count,
// retention and server-info, and users/featured-notes) and compares the answers with upstream's: the cast step (JSON.parse of integer params, 0b5f1631-… on failure), then
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
