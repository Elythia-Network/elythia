// Command elythia is the single executable of the server. The work is chosen
// by a subcommand: serve, migrate, backfill <name>, doctor, fsck, config-dump,
// healthcheck and dump-routes. Run "elythia help" for the list.
//
// 振り分けと各サブコマンドの処理は internal/cli にある。ここに処理を足さない
// (main パッケージは import できず、テストから振る舞いを確かめられない)。
// 同じディレクトリには、tools/pluginbuild がプラグインを組み込むための
// plugins_generated.go を生成する (gitignore 済み)。
package main

import (
	"os"

	"github.com/elythia-network/elythia/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[0], os.Args[1:]))
}
