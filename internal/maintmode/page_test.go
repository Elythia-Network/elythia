package maintmode

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pageScriptHarness runs the script of the maintenance page with a fake
// fetch answering status/body once, and prints "reload" or "render".
const pageScriptHarness = `
var out = [];
global.document = { getElementById: function () { return { hidden: false, textContent: '' }; } };
global.location = { href: '/x', reload: function () { out.push('reload'); } };
global.setTimeout = function () {};
global.fetch = function (url, opts) {
	if (opts && opts.method === 'HEAD') {
		if (HEADSTATUS < 0) return Promise.reject(new Error('offline'));
		return Promise.resolve({ ok: HEADSTATUS >= 200 && HEADSTATUS < 300, status: HEADSTATUS });
	}
	return Promise.resolve({
		ok: STATUS >= 200 && STATUS < 300,
		json: function () { return Promise.resolve().then(function () { return JSON.parse(BODY); }); }
	});
};
SCRIPT
setTimeout(function () {}, 0);
var waits = 0;
(function wait() { if (++waits < 20) { setImmediate(wait); return; } console.log(out.join(',') || 'none'); })();
`

// TestPage_PollReloadsOnlyWhenTheServerSaysItIsOver fixes #3463 review L-6:
// a 200 with a body that is not JSON (a proxy's page) was read as "over" and
// the page reloaded every 5 seconds.
func TestPage_PollReloadsOnlyWhenTheServerSaysItIsOver(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	m := regexp.MustCompile(`(?s)<script nonce="\{\{nonce\}\}">(.*?)</script>`).FindStringSubmatch(pageHTML)
	require.Len(t, m, 2, "the page has its script")
	for name, c := range map[string]struct {
		status int
		body   string
		head   int // status of the HEAD to the page; -1 = network error
		want   string
	}{
		"over":                       {200, `{"maintenance":false}`, 0, "reload"},
		"still on":                   {200, `{"maintenance":true,"phase":"held"}`, 0, "none"},
		"not json, page still 503":   {200, `<html>proxy</html>`, 503, "none"},
		"not json, page served":      {200, `<html>spa</html>`, 200, "reload"},
		"not json, page unreachable": {200, `<html>proxy</html>`, -1, "none"},
		"json but not obj":           {200, `"ok"`, 0, "none"},
		"error status":               {502, `{"maintenance":false}`, 0, "none"},
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.NewReplacer(
				"HEADSTATUS", strconv.Itoa(c.head),
				"STATUS", strconv.Itoa(c.status),
				"BODY", jsString(c.body),
				"SCRIPT", m[1],
			).Replace(pageScriptHarness)
			path := filepath.Join(t.TempDir(), "page.js")
			require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
			got, err := exec.Command(node, path).CombinedOutput()
			require.NoError(t, err, string(got))
			assert.Equal(t, c.want, strings.TrimSpace(string(got)))
		})
	}
}

func jsString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
