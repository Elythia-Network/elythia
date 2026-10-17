package entitycompat

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// benchABCompose is the compose file of the base / head comparison stack.
const benchABCompose = "tests/bench/ab/compose.yml"

// benchPostgresNoDiskWait are the postgres settings that keep the runner's
// disk out of the bench numbers (#3659).
var benchPostgresNoDiskWait = []string{"fsync=off", "synchronous_commit=off", "full_page_writes=off"}

// anyComposePattern matches compose file names anywhere in the repository,
// including docker-compose.yml, overlays such as compose.ts.yaml, and
// templates such as compose.uds.yaml.example (the same form as
// rootComposePattern).
var anyComposePattern = regexp.MustCompile(`^(docker-)?compose[^/]*\.ya?ml(\.example)?$`)

// benchPostgresSettingName matches the names of the settings in
// benchPostgresNoDiskWait as whole words, whatever value or spelling follows
// them.
//
// 値の書き方 (`fsync=false`、`fsync = 0` など) は PostgreSQL が何通りも受け付けるので、
// 値ではなく名前で見る。語の境界を付け、valkey の `--appendfsync` などは拾わない。
var benchPostgresSettingName = regexp.MustCompile(`(?i)\b(fsync|synchronous_commit|full_page_writes)\b`)

// benchPostgresSettingKeys are the service keys through which a compose file
// can hand settings to PostgreSQL: the server's arguments, and environment
// variables such as PGOPTIONS (read by clients, including the app) and
// POSTGRES_INITDB_ARGS.
var benchPostgresSettingKeys = []string{"command", "entrypoint", "environment"}

// TestBenchPostgresSkipsDiskWaits guards how the bench stack runs PostgreSQL
// (#3659).
//
// GitHub-hosted runner の一部の機種で、ノートの INSERT / COMMIT がディスクの待ちで
// 止まり、notes-create の p95 が揺れた。ベンチの postgres だけは fsync などを切った
// 設定で動かす。docker を使わずに次の 3 つを見る。
//
//   - tests/bench/ab/compose.yml の postgres の command が、3 つの設定を `-c` で渡す
//   - 同じディレクトリの overlay (compose.upstream.yml など) が postgres を定義せず、
//     どの service の command / entrypoint / environment にも 3 つの設定の名前が無い。
//     overlay は本家の側の回にだけかかるので、postgres を定義したり、app の PGOPTIONS
//     で synchronous_commit を戻したりすると、両側に同じ条件がかからない
//   - tests/bench/ の外の compose (本番の雛形 compose.uds.yaml.example、e2e、開発用) の
//     どの service の command / entrypoint / environment にも、3 つの設定の名前が無い。
//     本番の雛形にクラッシュで壊れる設定を持ち込まず、e2e も本番と同じ書き込みの経路で
//     試す。tests/bench/ の下のほかのベンチは本番と関係しないので見ない。コメントや
//     postgres 以外の設定 (valkey の appendfsync など) も見ない
func TestBenchPostgresSkipsDiskWaits(t *testing.T) {
	root := repoRoot(t)
	tracked := gitTrackedSet(t, root)

	var files []string
	for p := range tracked {
		if anyComposePattern.MatchString(path.Base(p)) {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	// **拾えなかったら落とす。** 実在する対象を名指しで要求する。
	for _, must := range []string{benchABCompose, "tests/bench/ab/compose.upstream.yml", "docker-compose.yml", "compose.uds.yaml.example", "tests/dropin/compose.yml"} {
		require.Containsf(t, files, must, "compose の列挙が %s を拾えていない", must)
	}

	pg, ok := benchComposeServices(t, root, benchABCompose)["postgres"]
	require.Truef(t, ok, "%s に postgres が無い", benchABCompose)
	var svc struct {
		Command []string `yaml:"command"`
	}
	require.NoErrorf(t, pg.Decode(&svc), "%s の postgres の command を読めない (配列で書く)", benchABCompose)
	command := svc.Command
	require.NotEmptyf(t, command, "%s の postgres に command が無い", benchABCompose)
	for _, setting := range benchPostgresNoDiskWait {
		require.Truef(t, passesSetting(command, setting),
			"%s の postgres の command が `-c %s` を渡していない: %q", benchABCompose, setting, command)
	}

	overlays, checked := 0, 0
	for _, f := range files {
		switch {
		case f == benchABCompose:
			continue
		case path.Dir(f) == path.Dir(benchABCompose):
			overlays++
			services := benchComposeServices(t, root, f)
			_, ok := services["postgres"]
			require.Falsef(t, ok, "overlay %s が postgres を定義している。base / head の両側に同じ設定をかけるため、postgres は %s にだけ書く", f, benchABCompose)
			for _, leak := range leakedPostgresSettings(services) {
				t.Errorf("overlay %s の %s に %s がある。overlay は片側の回にだけかかるので、postgres の設定を変えない", f, leak.where, leak.name)
			}
		case !postgresLeakChecked(f):
			continue
		default:
			checked++
			for _, leak := range leakedPostgresSettings(benchComposeServices(t, root, f)) {
				t.Errorf("%s の %s に %s がある。fsync などを切る設定はベンチのスタック (%s) だけに置く", f, leak.where, leak.name, benchABCompose)
			}
		}
	}
	require.Positivef(t, overlays, "%s の overlay を 1 つも拾えていない", path.Dir(benchABCompose))
	require.GreaterOrEqualf(t, checked, 3, "tests/bench/ の外の compose を拾えていない")
}

// postgresLeakChecked reports whether the leak check covers a compose file
// outside the bench comparison stack. The other benches under tests/bench/
// have nothing to do with production, so they may turn fsync off too.
func postgresLeakChecked(f string) bool {
	return !strings.HasPrefix(f, "tests/bench/")
}

// TestPostgresLeakChecked pins which compose files the leak check covers.
func TestPostgresLeakChecked(t *testing.T) {
	for f, want := range map[string]bool{
		"compose.uds.yaml.example":                       true,
		"docker-compose.yml":                             true,
		".devcontainer/docker-compose.yml":               true,
		"tests/dropin/compose.yml":                       true,
		"tests/playwright/compose.ts.yml":                true,
		"tests/bench/http/compose.yml":                   false,
		"tests/bench/queue-autoscale/docker-compose.yml": false,
	} {
		require.Equalf(t, want, postgresLeakChecked(f), "%s", f)
	}
}

// TestLeakedPostgresSettings checks which forms the leak check catches.
func TestLeakedPostgresSettings(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{"postgres command list", "services:\n  db:\n    image: postgres:18\n    command: [\"postgres\", \"-c\", \"fsync=off\"]\n", []string{"db.command: fsync"}},
		{"postgres command string with spaces", "services:\n  db:\n    command: postgres -c 'synchronous_commit = off'\n", []string{"db.command: synchronous_commit"}},
		{"upper case value", "services:\n  db:\n    command: postgres -c FULL_PAGE_WRITES=0\n", []string{"db.command: FULL_PAGE_WRITES"}},
		{"entrypoint", "services:\n  db:\n    entrypoint: [\"docker-entrypoint.sh\", \"postgres\", \"-c\", \"fsync=false\"]\n", []string{"db.entrypoint: fsync"}},
		{"PGOPTIONS map on the app", "services:\n  app:\n    environment:\n      PGOPTIONS: -c synchronous_commit=on\n", []string{"app.environment: synchronous_commit"}},
		{"PGOPTIONS list", "services:\n  app:\n    environment:\n      - PGOPTIONS=-c synchronous_commit=off\n", []string{"app.environment: synchronous_commit"}},
		{"initdb args", "services:\n  db:\n    environment:\n      POSTGRES_INITDB_ARGS: --no-sync -c fsync=off\n", []string{"db.environment: fsync"}},
		{"valkey appendfsync", "services:\n  valkey:\n    command: [\"valkey-server\", \"--appendfsync\", \"everysec\"]\n", nil},
		{"comment", "# postgres の fsync は切らない\nservices:\n  db:\n    image: postgres:18 # fsync=off にしない\n", nil},
		{"other keys", "services:\n  db:\n    labels:\n      note: fsync=off\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			services, err := parseComposeServices([]byte(tc.yaml))
			require.NoError(t, err)
			var got []string
			for _, l := range leakedPostgresSettings(services) {
				got = append(got, l.where+": "+l.name)
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// postgresSettingLeak is one place where a compose file names a setting in
// benchPostgresNoDiskWait.
type postgresSettingLeak struct {
	where, name string
}

// leakedPostgresSettings returns the settings of benchPostgresNoDiskWait
// named in the command, entrypoint or environment of any service.
func leakedPostgresSettings(services map[string]yaml.Node) []postgresSettingLeak {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	var leaks []postgresSettingLeak
	for _, name := range names {
		svc := services[name]
		if svc.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(svc.Content); i += 2 {
			key := svc.Content[i].Value
			if !slices.Contains(benchPostgresSettingKeys, key) {
				continue
			}
			for _, v := range scalarValues(svc.Content[i+1]) {
				if m := benchPostgresSettingName.FindString(v); m != "" {
					leaks = append(leaks, postgresSettingLeak{where: name + "." + key, name: m})
					break
				}
			}
		}
	}
	return leaks
}

// scalarValues returns every scalar (keys included) below n. Comments are not
// scalars, so they are left out.
func scalarValues(n *yaml.Node) []string {
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, scalarValues(c)...)
	}
	return out
}

// benchComposeServices parses the services of a compose file, leaving each
// service undecoded so that a service this test does not read can use any
// form compose accepts.
func benchComposeServices(t *testing.T, root, rel string) map[string]yaml.Node {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	require.NoErrorf(t, err, "read %s", rel)
	services, err := parseComposeServices(raw)
	require.NoErrorf(t, err, "parse %s", rel)
	require.NotEmptyf(t, services, "%s から service を 1 つも拾えない", rel)
	return services
}

// parseComposeServices parses the services of a compose file.
func parseComposeServices(raw []byte) (map[string]yaml.Node, error) {
	var doc struct {
		Services map[string]yaml.Node `yaml:"services"`
	}
	err := yaml.Unmarshal(raw, &doc)
	return doc.Services, err
}

// passesSetting reports whether command passes setting as "-c setting".
func passesSetting(command []string, setting string) bool {
	for i := 0; i+1 < len(command); i++ {
		if command[i] == "-c" && strings.TrimSpace(command[i+1]) == setting {
			return true
		}
	}
	return false
}
