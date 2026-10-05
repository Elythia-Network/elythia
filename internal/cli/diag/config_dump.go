package diag

import (
	"fmt"

	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/server"
)

// ConfigDump implements "elythia config-dump": it prints the resolved
// configuration (secrets masked) and returns the process exit code.
func ConfigDump(args []string) int { return configDump(defaultEnv(), args) }

// configDump is ConfigDump with its dependencies passed in.
//
// **サーバーを起動しない。** 設定を読むだけなので DB / Redis に繋がらなくても
// 動く。新規構築時や「本当にこの値で動いているのか」を確かめたいときに、
// 起動できない状態でも使えることが要点 (#2469)。
func configDump(e env, args []string) int {
	path, parse := configFlag("config-dump", e.stderr)
	if code, ok := parse(args); !ok {
		return code
	}
	cfg, ok := loadConfig(e, "config-dump", *path)
	if !ok {
		return 1
	}
	role, err := config.ResolveProcessRole()
	if err != nil {
		// role の解決に失敗しても設定は出す。何が矛盾しているかを見たい
		// 場面なので、ここで止めると診断の役に立たない。
		fmt.Fprintf(e.stderr, "config-dump: %v\n", err)
		role = config.RoleBoth
	}
	fmt.Fprint(e.stdout, server.RenderConfigDump(server.BuildConfigDump(cfg, role)))
	return 0
}
