package entitycompat

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// 配布する compose が全サービスに掛ける上限。
//
// **`max-size` は decimal で読まれる** (json-file は units.FromHumanSize を使う)
// ので、`50m` は 50,000,000 バイト = 47.7 MiB。`50mib` と書いても同じ扱い。
const (
	wantMaxSize = "50m"
	wantMaxFile = "3"
)

// 配布する compose の全サービスにログの上限があること (#2828)。
//
// **既定の json-file はローテーションしない。** 指定を忘れたサービスが 1 つでも
// あると、そのコンテナのログだけが増え続けてディスクを埋める。運用者は自分で
// 気付いて足すしかないので、配布物の側で担保する。
//
// **サービスを足したときが危ない。** anchor (`*default-logging`) を書き忘れても
// compose は通るし、起動もするので、埋まるまで気付けない。
//
// **コメントアウトされたサービスは見えない** (YAML パーサはコメントを読まない)。
// 既定無効のテンプレート (video-thumb) にも `#  logging: *default-logging` を
// 書いてあるが、それが正しいかはこの gate では担保できない。
//
// **一覧は `git ls-files` で直下から列挙する。** 検証用の compose は tests/ の
// 各スイートへ移した (#3373) ので、直下にあるのは配布物だけになった
// (TestRootComposeFilesAreOperatorOnly が固定する)。
func TestComposeServicesHaveLogLimits(t *testing.T) {
	root := repoRoot(t)
	for _, name := range rootComposeFiles(t, root) {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			var doc struct {
				Services map[string]struct {
					Logging *struct {
						Driver  string            `yaml:"driver"`
						Options map[string]string `yaml:"options"`
					} `yaml:"logging"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			if len(doc.Services) == 0 {
				// **空なら落とす。** 書式が変わってサービスを 1 つも拾えなく
				// なると、検査していないのに緑になる。
				t.Fatalf("%s から service を 1 つも読めない (書式が変わった?)", name)
			}

			for svc, def := range doc.Services {
				if def.Logging == nil {
					t.Errorf("%s の service %q に logging が無い。"+
						"`logging: *default-logging` を足すこと (#2828)", name, svc)
					continue
				}
				// **値そのものを見る。** 「空でない」だけだと `max-size: 50g` の
				// ような「上限を書いたのに実質無制限」を通してしまう。
				if got := def.Logging.Options["max-size"]; got != wantMaxSize {
					t.Errorf("%s の service %q の max-size が %q (期待 %q)。"+
						"揃えないと 1 サービスだけがディスクを埋める", name, svc, got, wantMaxSize)
				}
				if got := def.Logging.Options["max-file"]; got != wantMaxFile {
					t.Errorf("%s の service %q の max-file が %q (期待 %q)", name, svc, got, wantMaxFile)
				}
			}
		})
	}
}
