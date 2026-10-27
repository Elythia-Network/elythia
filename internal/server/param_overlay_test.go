package server

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/queue"
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
