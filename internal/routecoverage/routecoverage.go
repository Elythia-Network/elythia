// Package routecoverage counts the /api endpoints that a test layer hit, from
// the access log of the Elythia server it ran against (#4037).
//
// The denominator is the set of endpoints the server routes: upstream's
// endpoints and direct routes (upstream_paramdefs.json), Elythia's own
// endpoints (elythia_paramdefs.json) and the few routes that have no paramDef
// on purpose. internal/server checks that this set matches the routes of the
// built server, so it can be read without starting a stack.
package routecoverage

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// Origin tells an endpoint that upstream has from one only Elythia has.
type Origin string

const (
	Upstream Origin = "upstream"
	Elythia  Origin = "elythia"
)

// undeclared are the routes that have no paramDef on purpose
// (undeclaredAPIRoutes of internal/server, without the catch-all "*").
//
// internal/serverのTestRouteCoverageEndpointsMatchRoutesが、ここと組み立てた
// サーバーのルートの一致を確かめる。
var undeclared = map[string]Origin{
	// 本家の旧いpathの互換
	"signin": Upstream,
}

// Endpoints returns every endpoint the server routes, by the path after
// /api/ ("notes/create", "miauth/:session/check"), with its origin.
func Endpoints() (map[string]Origin, error) {
	return endpointsFrom(paramschema.UpstreamGolden(), paramschema.ElythiaParamDefs())
}

func endpointsFrom(upstreamGolden, elythiaParamDefs []byte) (map[string]Origin, error) {
	var up struct {
		DirectRoutes []string                   `json:"directRoutes"`
		Endpoints    map[string]json.RawMessage `json:"endpoints"`
	}
	if err := json.Unmarshal(upstreamGolden, &up); err != nil {
		return nil, fmt.Errorf("routecoverage: upstream golden: %w", err)
	}
	var ely struct {
		Endpoints map[string]json.RawMessage `json:"endpoints"`
	}
	if err := json.Unmarshal(elythiaParamDefs, &ely); err != nil {
		return nil, fmt.Errorf("routecoverage: elythia paramdefs: %w", err)
	}
	out := make(map[string]Origin, len(up.Endpoints)+len(up.DirectRoutes)+len(ely.Endpoints)+len(undeclared))
	for name := range up.Endpoints {
		out[name] = Upstream
	}
	for _, name := range up.DirectRoutes {
		out[name] = Upstream
	}
	for name := range ely.Endpoints {
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("routecoverage: %s is both upstream's and Elythia's", name)
		}
		out[name] = Elythia
	}
	for name, o := range undeclared {
		out[name] = o
	}
	return out, nil
}

// Count is how a layer's requests to one endpoint ended.
type Count struct {
	// OK counts 2xx and 3xx: the handler ran through.
	OK int `json:"ok"`
	// ClientError counts 4xx. A 400 or 401 alone means the request stopped
	// before the body of the handler.
	ClientError int `json:"clientError"`
	// ServerError counts 5xx.
	ServerError int `json:"serverError"`
}

// Input is one access log that was read, with its number of /api lines.
type Input struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

// Hits is the result of reading the access logs of one or more layers.
type Hits struct {
	// Layers names the test layers the hits came from.
	Layers []string `json:"layers"`
	// Inputs lists the access logs read, so that a log that was not
	// captured (a shard that timed out, a stack that did not start) shows.
	Inputs []Input `json:"inputs"`
	// Lines is the number of /api access log lines read.
	Lines int `json:"lines"`
	// Endpoints counts the requests by endpoint.
	Endpoints map[string]*Count `json:"endpoints"`
	// Unknown counts the requests to /api paths that are no endpoint (the
	// catch-all, plugins, typos), by path.
	Unknown map[string]int `json:"unknown"`
}

// NewHits returns empty hits of the given layer.
func NewHits(layer string) *Hits {
	h := &Hits{Endpoints: map[string]*Count{}, Unknown: map[string]int{}}
	if layer != "" {
		h.Layers = []string{layer}
	}
	return h
}

// accessLine matches a line of the access log (internal/server.accessLogConfig):
// "<RFC3339> <METHOD> <URI> <STATUS> <latency>". docker compose logs prefixes
// each line with "<service>  | " unless --no-log-prefix is given, so the
// prefix is allowed.
//
// slogの行と同じ出力に混ざるので、行の形で拾う。時刻はRFC3339で始まる。
var accessLine = regexp.MustCompile(`^(?:\S+\s+\|\s)?\d{4}-\d{2}-\d{2}T\S+ (GET|POST|HEAD|PUT|DELETE|PATCH|OPTIONS) /api/(\S*) (\d{3}) `)

// Matcher resolves a request path to the endpoint it was routed to.
type Matcher struct {
	exact  map[string]bool
	params []paramRoute
}

type paramRoute struct {
	name     string
	segments []string
}

// NewMatcher returns a matcher over the endpoints.
func NewMatcher(endpoints map[string]Origin) *Matcher {
	m := &Matcher{exact: map[string]bool{}}
	for name := range endpoints {
		if !strings.Contains(name, ":") {
			m.exact[name] = true
			continue
		}
		m.params = append(m.params, paramRoute{name: name, segments: strings.Split(name, "/")})
	}
	sort.Slice(m.params, func(i, j int) bool { return m.params[i].name < m.params[j].name })
	return m
}

// Match returns the endpoint of path (the part after /api/, without the
// query), or false.
func (m *Matcher) Match(path string) (string, bool) {
	if m.exact[path] {
		return path, true
	}
	segs := strings.Split(path, "/")
	for _, r := range m.params {
		if len(r.segments) != len(segs) {
			continue
		}
		ok := true
		for i, s := range r.segments {
			if strings.HasPrefix(s, ":") {
				if segs[i] == "" {
					ok = false
					break
				}
				continue
			}
			if s != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return r.name, true
		}
	}
	return "", false
}

// ReadLog adds the /api access log lines of r, which is named name, to h.
func (h *Hits) ReadLog(name string, r io.Reader, m *Matcher) error {
	before := h.Lines
	defer func() { h.Inputs = append(h.Inputs, Input{Name: name, Lines: h.Lines - before}) }()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		g := accessLine.FindStringSubmatch(sc.Text())
		if g == nil {
			continue
		}
		path, _, _ := strings.Cut(g[2], "?")
		// 正規表現が3桁の数字に絞っているので失敗しない
		status, _ := strconv.Atoi(g[3])
		h.Lines++
		name, ok := m.Match(path)
		if !ok {
			h.Unknown[path]++
			continue
		}
		c := h.Endpoints[name]
		if c == nil {
			c = &Count{}
			h.Endpoints[name] = c
		}
		switch {
		case status >= 500:
			c.ServerError++
		case status >= 400:
			c.ClientError++
		default:
			c.OK++
		}
	}
	return sc.Err()
}

// Merge adds o to h.
func (h *Hits) Merge(o *Hits) {
	for _, l := range o.Layers {
		if !containsString(h.Layers, l) {
			h.Layers = append(h.Layers, l)
		}
	}
	h.Inputs = append(h.Inputs, o.Inputs...)
	h.Lines += o.Lines
	for name, c := range o.Endpoints {
		d := h.Endpoints[name]
		if d == nil {
			d = &Count{}
			h.Endpoints[name] = d
		}
		d.OK += c.OK
		d.ClientError += c.ClientError
		d.ServerError += c.ServerError
	}
	for p, n := range o.Unknown {
		h.Unknown[p] += n
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Problems returns why the hits cannot be trusted: no line was read, an
// access log had no line, or an endpoint that the layer surely passes through
// was not passed through (2xx).
//
// 書式が変わって1行も拾えないと、全部が「叩かれていない」になるだけで落ちない。
// 確実に通るエンドポイントを名指しで要求する。shardの1つのログが取れなかった
// ときも、ほかのshardで要求は満たされて緑のまま数だけが減るので、空のログを落とす。
func (h *Hits) Problems(require []string) []string {
	var out []string
	if h.Lines == 0 {
		out = append(out, "no /api access log line was read")
	}
	for _, in := range h.Inputs {
		if in.Lines == 0 {
			out = append(out, fmt.Sprintf("%s has no /api access log line", in.Name))
		}
	}
	for _, name := range require {
		if c := h.Endpoints[name]; c == nil || c.OK == 0 {
			out = append(out, fmt.Sprintf("%s was never passed through (2xx or 3xx)", name))
		}
	}
	return out
}

// Summary is the coverage of one origin.
type Summary struct {
	Origin Origin
	// Total is the number of endpoints of the origin.
	Total int
	// OK is the number of endpoints passed through at least once.
	OK int
	// ClientErrorOnly is the number of endpoints that only got 4xx (no 2xx,
	// 3xx or 5xx).
	ClientErrorOnly int
	// NotHit lists the endpoints with no request.
	NotHit []string
	// ClientErrorOnlyNames lists the endpoints that only got 4xx.
	ClientErrorOnlyNames []string
	// ServerError lists the endpoints that returned 5xx at least once.
	ServerError []string
}

// Summarize counts the hits by origin.
func (h *Hits) Summarize(endpoints map[string]Origin) []Summary {
	sums := map[Origin]*Summary{Upstream: {Origin: Upstream}, Elythia: {Origin: Elythia}}
	names := make([]string, 0, len(endpoints))
	for name := range endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := sums[endpoints[name]]
		s.Total++
		c := h.Endpoints[name]
		switch {
		case c == nil:
			s.NotHit = append(s.NotHit, name)
		case c.OK > 0:
			s.OK++
		case c.ServerError == 0:
			// 5xxが混ざったものは下の5xxの一覧に出るので、ここには数えない
			s.ClientErrorOnly++
			s.ClientErrorOnlyNames = append(s.ClientErrorOnlyNames, name)
		}
		if c != nil && c.ServerError > 0 {
			s.ServerError = append(s.ServerError, name)
		}
	}
	return []Summary{*sums[Upstream], *sums[Elythia]}
}

// WriteMarkdown writes the summary for $GITHUB_STEP_SUMMARY.
func (h *Hits) WriteMarkdown(w io.Writer, endpoints map[string]Origin) error {
	sums := h.Summarize(endpoints)
	var b strings.Builder
	fmt.Fprintf(&b, "## API の通過 (%s)\n\n", strings.Join(h.Layers, ", "))
	fmt.Fprintf(&b, "アクセスログの /api の行: %d (ログ %d 本)\n\n", h.Lines, len(h.Inputs))
	b.WriteString("| 区分 | エンドポイント | 2xx・3xx で通った | 4xx だけ | 叩かれていない | 5xx が出た |\n|---|---|---|---|---|---|\n")
	label := map[Origin]string{Upstream: "本家互換", Elythia: "Elythia 独自"}
	for _, s := range sums {
		fmt.Fprintf(&b, "| %s | %d | %d (%s) | %d | %d | %d |\n",
			label[s.Origin], s.Total, s.OK, percent(s.OK, s.Total), s.ClientErrorOnly, len(s.NotHit), len(s.ServerError))
	}
	b.WriteString("\n")
	for _, s := range sums {
		list(&b, label[s.Origin]+": 5xx が出たエンドポイント", s.ServerError)
		list(&b, label[s.Origin]+": 4xx だけのエンドポイント", s.ClientErrorOnlyNames)
		list(&b, label[s.Origin]+": 叩かれていないエンドポイント", s.NotHit)
	}
	inputs := make([]string, 0, len(h.Inputs))
	for _, in := range h.Inputs {
		inputs = append(inputs, fmt.Sprintf("%s (%d)", in.Name, in.Lines))
	}
	list(&b, "読んだログと /api の行", inputs)
	unknown := make([]string, 0, len(h.Unknown))
	for p, n := range h.Unknown {
		unknown = append(unknown, fmt.Sprintf("%s (%d)", p, n))
	}
	sort.Strings(unknown)
	list(&b, "エンドポイントでない /api のパス", unknown)
	_, err := io.WriteString(w, b.String())
	return err
}

func percent(n, total int) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(n)*100/float64(total))
}

func list(b *strings.Builder, title string, names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(b, "<details><summary>%s (%d)</summary>\n\n", title, len(names))
	for _, n := range names {
		fmt.Fprintf(b, "- `%s`\n", n)
	}
	b.WriteString("\n</details>\n\n")
}

// uncoveredTSV is uncovered.tsv: the endpoints that no test layer passes
// through yet, each with the issue that will cover it (#4039).
//
//go:embed uncovered.tsv
var uncoveredTSV string

// UncoveredList returns the embedded uncovered.tsv.
func UncoveredList() string {
	return uncoveredTSV
}

// UncoveredRow is a row of uncovered.tsv.
type UncoveredRow struct {
	Endpoint string
	Issue    string
	Line     int
}

// issueRef is the start of the second column: the issue that will cover the
// endpoint ("#4024(18)", "#4024").
var issueRef = regexp.MustCompile(`^#\d+`)

// ParseUncovered parses uncovered.tsv: "<endpoint>\t<issue>" per line, with
// blank lines and lines starting with "#" skipped. It returns the rows and
// the malformed lines.
func ParseUncovered(text string) ([]UncoveredRow, []string) {
	var rows []UncoveredRow
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 2 || cols[0] == "" || !issueRef.MatchString(cols[1]) {
			problems = append(problems, fmt.Sprintf("uncovered.tsv:%d: not \"<endpoint>\\t#<issue>\": %q", i+1, line))
			continue
		}
		rows = append(rows, UncoveredRow{Endpoint: cols[0], Issue: cols[1], Line: i + 1})
	}
	return rows, problems
}

// CheckRows returns the rows that name no endpoint or name one twice. It
// needs no hits, so the required test runs it on every PR.
func CheckRows(endpoints map[string]Origin, rows []UncoveredRow) []string {
	var out []string
	seen := map[string]int{}
	for _, r := range rows {
		if _, ok := endpoints[r.Endpoint]; !ok {
			out = append(out, fmt.Sprintf("uncovered.tsv:%d: %s is no endpoint (renamed or removed?)", r.Line, r.Endpoint))
		}
		if first, dup := seen[r.Endpoint]; dup {
			out = append(out, fmt.Sprintf("uncovered.tsv:%d: %s is listed on line %d too", r.Line, r.Endpoint, first))
			continue
		}
		seen[r.Endpoint] = r.Line
	}
	return out
}

// CheckUncovered compares the hits of every layer with uncovered.tsv: an
// endpoint no layer passed through (2xx or 3xx) must be listed, and a listed
// endpoint must not be passed through. A 4xx alone is not passing through.
//
// 一覧に無いのに通っていないものは、新しいAPIがテスト無しで入ったか、既存の
// テストが叩かなくなったもの。一覧にあるのに通っているものは、テストが付いた
// ので行を消させる (使われない行を残さない)。
func (h *Hits) CheckUncovered(endpoints map[string]Origin, rows []UncoveredRow) []string {
	out := CheckRows(endpoints, rows)
	listed := map[string]bool{}
	for _, r := range rows {
		listed[r.Endpoint] = true
		if c := h.Endpoints[r.Endpoint]; c != nil && c.OK > 0 {
			out = append(out, fmt.Sprintf("uncovered.tsv:%d: %s is passed through now; remove the row", r.Line, r.Endpoint))
		}
	}
	names := make([]string, 0, len(endpoints))
	for name := range endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c := h.Endpoints[name]; (c == nil || c.OK == 0) && !listed[name] {
			out = append(out, fmt.Sprintf("%s (%s) is passed through by no layer and is not in uncovered.tsv", name, endpoints[name]))
		}
	}
	return out
}

// MissingLayers returns the layers of want that the hits do not come from.
func (h *Hits) MissingLayers(want []string) []string {
	var out []string
	for _, l := range want {
		if !containsString(h.Layers, l) {
			out = append(out, l)
		}
	}
	return out
}

// WriteUncovered writes uncovered.tsv for the hits: every endpoint no layer
// passed through, keeping the issue of the rows already listed and giving
// the others defaultIssue of their origin.
func (h *Hits) WriteUncovered(w io.Writer, endpoints map[string]Origin, rows []UncoveredRow, defaultIssue map[Origin]string) error {
	issue := map[string]string{}
	for _, r := range rows {
		issue[r.Endpoint] = r.Issue
	}
	names := make([]string, 0, len(endpoints))
	for name := range endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(uncoveredHeader)
	for _, name := range names {
		if c := h.Endpoints[name]; c != nil && c.OK > 0 {
			continue
		}
		is := issue[name]
		if is == "" {
			is = defaultIssue[endpoints[name]]
		}
		fmt.Fprintf(&b, "%s\t%s\n", name, is)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

const uncoveredHeader = `# どのテストの層 (upstream-backend-e2e・playwright・diff-e2e) でも 2xx・3xx で通っていない
# /api のエンドポイント (#4039)。` + "`<エンドポイント>\\t<付ける予定の issue>`" + `。4xx だけのものも載せる。
# テストを付けたら行を消す (残すと週次の api-coverage が落とす)。作り直し方は docs/ci.md の
# 「API の通過の集計」。
`
