// Command pluginspec regenerates the golden snapshot of plugin/'s exported
// surface, used by the drift gate (TestPluginSurfaceDrift).
//
// 抽出そのものは internal/pluginspec にある。gate から同じコードを呼べるように
// するため、CLI 側は薄く保つ (別実装にすると「ツールの出力」と「gate が見る
// もの」がずれる)。
//
// Usage:
//
//	go run ./tools/pluginspec              # 現在の公開面を表示する
//	go run ./tools/pluginspec -write       # golden を更新する
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/elythia-network/elythia/internal/pluginspec"
)

func main() {
	root := flag.String("root", ".", "リポジトリのルート")
	out := flag.String("out", pluginspec.DefaultGolden, "golden の出力先")
	write := flag.Bool("write", false, "golden を更新する")
	flag.Parse()

	surface, err := pluginspec.SurfaceAll(*root, pluginspec.TrackedDirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pluginspec:", err)
		os.Exit(1)
	}
	rendered := pluginspec.Render(surface)

	if *write {
		if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "pluginspec:", err)
			os.Exit(1)
		}
		fmt.Printf("pluginspec: %s を更新しました (%d 項目)\n", *out, len(surface))
		return
	}
	fmt.Print(rendered)
}
