package server

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/notification"
	coresignup "github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/repository"
)

// overlayNode is the part of an overlay of elythia_overlays.json this test
// reads.
type overlayNode struct {
	Enum        []any                  `json:"enum"`
	EnumPattern string                 `json:"x-elythia-enumPattern"`
	Properties  map[string]overlayNode `json:"properties"`
	Items       *overlayNode           `json:"items"`
}

// upstreamEnum returns the enum of the property name of the upstream
// paramDef of ep, or nil.
func upstreamEnum(t *testing.T, ep, name string) []any {
	t.Helper()
	var g struct {
		Endpoints map[string]struct {
			ParamDef struct {
				Properties map[string]struct {
					Enum []any `json:"enum"`
				} `json:"properties"`
			} `json:"paramDef"`
		} `json:"endpoints"`
	}
	require.NoError(t, json.Unmarshal(paramschema.UpstreamGolden(), &g))
	return g.Endpoints[ep].ParamDef.Properties[name].Enum
}

func stringSet(t *testing.T, vs []any) []string {
	t.Helper()
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		s, ok := v.(string)
		require.True(t, ok, "%v is not a string", v)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sorted(vs []string) []string {
	out := slices.Clone(vs)
	sort.Strings(out)
	return out
}

// TestElythiaOverlaysMatchTheImplementation keeps the values declared on top of
// upstream's paramDef (paramschema's elythia_overlays.json) to the values the
// implementation handles (#3865): a value the handler does not take must not
// pass the validation, and a value it takes must not be rejected by it.
func TestElythiaOverlaysMatchTheImplementation(t *testing.T) {
	var ov map[string]overlayNode
	require.NoError(t, json.Unmarshal(paramschema.ElythiaOverlays(), &ov))

	// キュー: 本家のenumに無い、Elythiaが回しているキューと、プラグインの
	// キュー。queueを受けるadmin/queue/*のすべてに重ねる。プラグインのキューは
	// admin/queue/queuesの一覧に載り、同梱のジョブキューの画面がその名前のまま
	// 統計・jobの一覧・jobの操作を呼ぶので、pause・resumeに限らない。
	queueEPs := 0
	eps, err := paramschema.Upstream()
	require.NoError(t, err)
	for name := range eps {
		if !strings.HasPrefix(name, "admin/queue/") {
			continue
		}
		up := upstreamEnum(t, name, "queue")
		if up == nil {
			assert.NotContains(t, ov, name, "%s takes no queue; nothing to overlay", name)
			continue
		}
		queueEPs++
		var want []string
		for _, q := range queue.AllQueueNames() {
			if !slices.Contains(up, any(q)) {
				want = append(want, q)
			}
		}
		require.NotEmpty(t, want)
		q := ov[name].Properties["queue"]
		assert.Equal(t, sorted(want), stringSet(t, q.Enum), "%s: the queues Elythia operates besides upstream's", name)
		assert.NotEmpty(t, q.EnumPattern, "%s does not take the queues of plugins", name)
	}
	assert.Equal(t, 10, queueEPs, "the admin/queue/* endpoints that take a queue")

	// 通知の型: Elythia独自の型(KindMkGo)。
	for _, name := range []string{"i/notifications", "i/notifications-grouped"} {
		for _, prop := range []string{"includeTypes", "excludeTypes"} {
			items := ov[name].Properties[prop].Items
			require.NotNil(t, items, "%s %s", name, prop)
			assert.Equal(t, sorted(notification.MkGoTypeNames()), stringSet(t, items.Enum), "%s %s", name, prop)
		}
	}

	// 並べ替え: repositoryが本家の外で扱う値。
	assert.Equal(t, sorted([]string{repository.InstanceSortHostAsc, repository.InstanceSortHostDesc}),
		stringSet(t, ov["federation/instances"].Properties["sort"].Enum))

	// 同梱のfrontendが送るキューの名前(misskey-jsのqueueTypes)は、本家のenumと
	// 重ねた値のどちらかに入っている。
	src, err := os.ReadFile("../../frontend/packages/misskey-js/src/consts.ts")
	require.NoError(t, err)
	m := regexp.MustCompile(`(?s)export const queueTypes = \[(.*?)\] as const;`).FindSubmatch(src)
	require.NotNil(t, m, "queueTypes not found in misskey-js consts.ts")
	sent := regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(m[1], -1)
	require.NotEmpty(t, sent)
	accepted := append(stringSet(t, upstreamEnum(t, "admin/queue/jobs", "queue")),
		stringSet(t, ov["admin/queue/jobs"].Properties["queue"].Enum)...)
	for _, s := range sent {
		assert.Contains(t, accepted, string(s[1]), "the bundled frontend sends the queue %q", s[1])
	}
}

// TestIUpdateOverlay pins what elythia_overlays.json adds to i/update
// (#3886): the follow approval action of #3466, and the emoji decorations of
// #2975, whose new entry the bundled frontend sends with an empty id
// (pages/settings/avatar-decoration.vue) and a scale the handler bounds to
// 0.1..1.
func TestIUpdateOverlay(t *testing.T) {
	eff, err := paramschema.Effective()
	require.NoError(t, err)
	up, err := paramschema.Upstream()
	require.NoError(t, err)
	validate := func(eps map[string]paramschema.Endpoint, body string) *paramschema.Error {
		t.Helper()
		var data any
		require.NoError(t, json.Unmarshal([]byte(body), &data), body)
		return eps["i/update"].Schema.Validate(data)
	}
	for _, a := range []string{model.FollowApprovalActionRequest, model.FollowApprovalActionSilentRequest, model.FollowApprovalActionSilentFollow} {
		assert.True(t, model.IsFollowApprovalAction(a))
		assert.Nil(t, validate(eff, `{"followApprovalAction":"`+a+`"}`), a)
	}
	// null は省略と同じ (handler は *string で受けて変えない)。
	assert.Nil(t, validate(eff, `{"followApprovalAction":null}`))
	for _, body := range []string{`{"followApprovalAction":"follow"}`, `{"followApprovalAction":""}`, `{"followApprovalAction":1}`} {
		assert.NotNil(t, validate(eff, body), body)
	}

	newEmoji := `{"avatarDecorations":[{"id":"","url":"https://example.test/e.png","emojiName":"blob","scale":0.5,"angle":0,"flipH":false,"offsetX":0,"offsetY":0}]}`
	assert.Nil(t, validate(eff, newEmoji))
	if err := validate(up, newEmoji); assert.NotNil(t, err) {
		assert.Equal(t, "#/properties/avatarDecorations/items/properties/id/format", err.SchemaPath)
	}
	for body, ok := range map[string]bool{
		`{"avatarDecorations":[{"id":"abc123","scale":0.1}]}`:   true,
		`{"avatarDecorations":[{"id":"abc123","scale":1}]}`:     true,
		`{"avatarDecorations":[{"id":"abc123","scale":null}]}`:  true,
		`{"avatarDecorations":[{"id":"abc123","scale":0.09}]}`:  false,
		`{"avatarDecorations":[{"id":"abc123","scale":1.01}]}`:  false,
		`{"avatarDecorations":[{"id":"abc123","scale":"1"}]}`:   false,
		`{"avatarDecorations":[{"id":"abc123","emojiName":1}]}`: false,
		`{"avatarDecorations":[{"id":"a-b","emojiName":"x"}]}`:  false,
	} {
		assert.Equal(t, ok, validate(eff, body) == nil, body)
	}
}

// TestUpdateMetaOverlay pins the settings elythia_overlays.json adds to
// admin/update-meta (#3888) to the bounds the handler and the services use:
// the chunked upload (core/drive), the minimum username length (core/signup),
// the signup application form (core/signupapplication) and the relay
// settings (queue/processors for the grace days; the bundled frontend's
// relays.vue for both). Each bound is checked from both sides.
func TestUpdateMetaOverlay(t *testing.T) {
	eff, err := paramschema.Effective()
	require.NoError(t, err)
	up, err := paramschema.Upstream()
	require.NoError(t, err)
	validate := func(eps map[string]paramschema.Endpoint, body string) *paramschema.Error {
		t.Helper()
		var data any
		require.NoError(t, json.Unmarshal([]byte(body), &data), body)
		return eps["admin/update-meta"].Schema.Validate(data)
	}
	ints := map[string][2]int{
		"minimumUsernameLength":          {coresignup.MinUsernameLength, coresignup.MaxUsernameLength},
		"chunkedUploadChunkSizeMb":       {coredrive.MinChunkSizeMb, coredrive.MaxChunkSizeMb},
		"chunkedUploadSessionTtlMinutes": {coredrive.MinSessionTTLMinutes, coredrive.MaxSessionTTLMinutes},
		// 上限は列 (int4) の範囲と、使う側で溢れない長さ (handler の
		// updateMetaNumericRanges と同じ値)。
		"chunkedUploadMaxSessionsPerUser":  {1, math.MaxInt32},
		"chunkedUploadMaxPendingMbPerUser": {1, math.MaxInt32},
		"relayOrphanUserGraceDays":         {processors.MinOrphanUserGraceDays, 3650},
		"ephemeralRelayNoteTtlMinutes":     {1, 525600},
	}
	for key, r := range ints {
		ok := func(v string) {
			body := fmt.Sprintf(`{%q:%s}`, key, v)
			assert.Nil(t, validate(eff, body), body)
			// 本家は知らない項目として通す。宣言を足したことで初めて検証される。
			assert.Nil(t, validate(up, body), body)
		}
		ng := func(v string) {
			body := fmt.Sprintf(`{%q:%s}`, key, v)
			assert.NotNil(t, validate(eff, body), body)
		}
		ok(fmt.Sprint(r[0]))
		ok(fmt.Sprintf("%d.0", r[0]))
		ng(fmt.Sprint(r[0] - 1))
		ok(fmt.Sprint(r[1]))
		ng(fmt.Sprint(r[1] + 1))
		ng(fmt.Sprintf("%d.5", r[0]))
		ng(`null`)
		ng(fmt.Sprintf("%q", fmt.Sprint(r[0])))
	}
	for _, key := range []string{"chunkedUploadEnabled", "enableRelayOrphanUserCleanup", "enableEphemeralRelayNotes"} {
		assert.Nil(t, validate(eff, `{"`+key+`":true}`), key)
		for _, v := range []string{`null`, `0`, `"true"`} {
			assert.NotNil(t, validate(eff, `{"`+key+`":`+v+`}`), key+" "+v)
		}
	}
	// 登録のゲートの null は、handler が省略として扱う (normalizeSignupGateBools)。
	for _, key := range []string{"approvalRequiredForSignup", "registrationClosed"} {
		for _, v := range []string{`true`, `false`, `null`} {
			assert.Nil(t, validate(eff, `{"`+key+`":`+v+`}`), key+" "+v)
		}
		assert.NotNil(t, validate(eff, `{"`+key+`":"false"}`), key)
	}

	field := func(label string, extra string) string {
		return fmt.Sprintf(`{"label":%q,"type":"text"%s}`, label, extra)
	}
	form := func(fields ...string) string {
		return `{"signupApplicationForm":[` + strings.Join(fields, ",") + `]}`
	}
	maxFields := make([]string, signupapplication.MaxFormFields)
	for i := range maxFields {
		maxFields[i] = field("q", "")
	}
	for body, ok := range map[string]bool{
		`{"signupApplicationForm":null}`:                   true,
		`{"signupApplicationForm":[]}`:                     true,
		form(maxFields...):                                 true,
		form(append(maxFields, field("q", ""))...):         false,
		form(field("q", `,"required":true,"maxLength":0`)): true,
		form(field("q", fmt.Sprintf(`,"maxLength":%d`, signupapplication.MaxAnswerLength))):   true,
		form(field("q", fmt.Sprintf(`,"maxLength":%d`, signupapplication.MaxAnswerLength+1))): false,
		form(field("q", `,"maxLength":-1`)):                                                   false,
		form(field("q", `,"maxLength":1.5`)):                                                  false,
		form(field("q", `,"required":null,"maxLength":null`)):                                 true,
		form(field("q", `,"required":"yes"`)):                                                 false,
		form(field(strings.Repeat("あ", signupapplication.MaxFormLabelLength), "")):            true,
		form(field(strings.Repeat("あ", signupapplication.MaxFormLabelLength+1), "")):          false,
		form(field("", "")): false,
		// 宣言は送られたままの長さで数え、handler は前後の空白を削ってから数える
		// (signupapplication.ValidateForm)。前後に空白を付けた 100 文字は宣言が
		// 拒否し、空白だけの label は宣言を通って handler が 400 にする。
		form(field(" "+strings.Repeat("あ", signupapplication.MaxFormLabelLength)+" ", "")): false,
		form(field(" ", "")): true,
		form(`{"label":"q","type":"` + signupapplication.FieldTypeTextarea + `"}`): true,
		form(`{"label":"q","type":"select"}`):                                      false,
		form(`{"label":"q"}`):                                                      false,
		form(`{"type":"text"}`):                                                    false,
		form(`"q"`):                                                                false,
		`{"signupApplicationForm":{}}`:                                             false,
	} {
		assert.Equal(t, ok, validate(eff, body) == nil, body)
	}

	for body, ok := range map[string]bool{
		`{"inboxSuspendedSoftware":null}`:                                           true,
		`{"inboxSuspendedSoftware":[]}`:                                             true,
		`{"inboxSuspendedSoftware":[{"software":"x","versionRange":"*"}]}`:          true,
		`{"inboxSuspendedSoftware":[{"software":"x","versionRange":"*","note":1}]}`: true,
		`{"inboxSuspendedSoftware":[{"software":"x"}]}`:                             false,
		`{"inboxSuspendedSoftware":[{"software":1,"versionRange":"*"}]}`:            false,
		`{"inboxSuspendedSoftware":["x"]}`:                                          false,
		`{"inboxSuspendedSoftware":{}}`:                                             false,
	} {
		assert.Equal(t, ok, validate(eff, body) == nil, body)
	}
}

// The job queue page of the bundled frontend opens a queue by the name
// admin/queue/queues lists, the queues of plugins included, and calls the
// endpoints below with it (job-queue.vue, job-queue.job.vue). The effective
// paramDefs must accept the name a plugin's queue gets (#3865).
func TestPluginQueueNamesPassTheValidation(t *testing.T) {
	eff, err := paramschema.Effective()
	require.NoError(t, err)
	name := queue.PluginQueueName("example")
	for ep, body := range map[string]string{
		"admin/queue/queue-stats":   `{"queue":%q}`,
		"admin/queue/jobs":          `{"queue":%q,"state":["active"]}`,
		"admin/queue/clear":         `{"queue":%q,"state":"wait"}`,
		"admin/queue/promote-jobs":  `{"queue":%q}`,
		"admin/queue/show-job":      `{"queue":%q,"jobId":"1"}`,
		"admin/queue/retry-job":     `{"queue":%q,"jobId":"1"}`,
		"admin/queue/remove-job":    `{"queue":%q,"jobId":"1"}`,
		"admin/queue/show-job-logs": `{"queue":%q,"jobId":"1"}`,
		"admin/queue/pause":         `{"queue":%q}`,
		"admin/queue/resume":        `{"queue":%q}`,
	} {
		var data any
		require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(body, name)), &data))
		assert.Nil(t, eff[ep].Schema.Validate(data), ep)
	}
}

// The queue name of a plugin passes the effective paramDefs exactly when the
// queue package makes a queue for it (PluginQueueNames skips the names its
// pluginNamePattern refuses). The pattern lives twice, in the queue package
// and in elythia_overlays.json; compared by behavior, widening one of them
// alone fails here (#3865).
func TestPluginQueueNamePatternMatchesTheQueuePackage(t *testing.T) {
	eff, err := paramschema.Effective()
	require.NoError(t, err)
	for _, n := range []string{"a", "a-b", "a-", "1a", "status", "claude-bot", "a_b", "A", "-a", "a.b", "a b", "a:b", "a/b", ""} {
		made := len(queue.PluginQueueNames([]string{n})) == 1
		for _, ep := range []string{"admin/queue/queue-stats", "admin/queue/pause"} {
			var data any
			require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"queue":%q}`, queue.PluginQueueName(n))), &data))
			assert.Equal(t, made, eff[ep].Schema.Validate(data) == nil, "%s %q", ep, n)
		}
	}
}

// TestAddedPropertiesMatchElythiaJS keeps the properties Elythia adds to
// upstream endpoints (x-elythia-properties of elythia_overlays.json, #3867)
// and the direct-route properties in the generated golden (#3852) to the same
// names as the request fields elythia-js declares for them
// (ElythiaRequestExtensions of extensions.ts).
func TestAddedPropertiesMatchElythiaJS(t *testing.T) {
	src, err := os.ReadFile("../../frontend/packages/elythia-js/src/extensions.ts")
	require.NoError(t, err)
	block := regexp.MustCompile(`(?s)export type ElythiaRequestExtensions = \{\n(.*?)\n\};`).FindSubmatch(src)
	require.NotNil(t, block, "ElythiaRequestExtensions not found in extensions.ts")
	ts := map[string][]string{}
	ep := ""
	for _, line := range strings.Split(string(block[1]), "\n") {
		if m := regexp.MustCompile(`^\t'([^']+)': \{`).FindStringSubmatch(line); m != nil {
			ep = m[1]
			ts[ep] = []string{}
			continue
		}
		if m := regexp.MustCompile(`^\t\t'?([A-Za-z0-9_]+)'?\??: `).FindStringSubmatch(line); m != nil {
			require.NotEmpty(t, ep, line)
			ts[ep] = append(ts[ep], m[1])
		}
	}
	require.NotEmpty(t, ts)

	var ov map[string]struct {
		Added map[string]json.RawMessage `json:"x-elythia-properties"`
	}
	require.NoError(t, json.Unmarshal(paramschema.ElythiaOverlays(), &ov))
	added := map[string][]string{}
	for name, o := range ov {
		for prop := range o.Added {
			added[name] = append(added[name], prop)
		}
	}
	require.NotEmpty(t, added)
	var upstream struct {
		DirectRoutes []string `json:"directRoutes"`
	}
	raw, err := os.ReadFile("../../internal/api/paramschema/upstream_paramdefs.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &upstream))
	directRoutes := make(map[string]bool, len(upstream.DirectRoutes))
	for _, name := range upstream.DirectRoutes {
		directRoutes[name] = true
	}
	directProperties := map[string][]string{"signup": {"federationStatus"}}
	for name, fields := range ts {
		if directFields, ok := directProperties[name]; ok {
			assert.True(t, directRoutes[name], "%s must be an upstream direct route", name)
			assert.NotContains(t, added, name, "%s is schema-less in the upstream golden and must not be overlaid", name)
			assert.Equal(t, sorted(directFields), sorted(fields), "%s: direct-route fields and extensions.ts differ", name)
			continue
		}
		assert.Equal(t, sorted(fields), sorted(added[name]), "%s: extensions.ts and x-elythia-properties differ", name)
	}
	for name := range directProperties {
		assert.Contains(t, ts, name, "%s direct-route fields are missing from extensions.ts", name)
		assert.True(t, directRoutes[name], "%s must be an upstream direct route", name)
	}
	for name := range added {
		assert.Contains(t, ts, name, "%s adds properties that extensions.ts does not declare", name)
	}
}
