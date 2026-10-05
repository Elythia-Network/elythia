package entitycompat

import "strings"

// filterGitEnv drops git's environment overrides so that the command always
// resolves against the repository at -C.
//
// hook の中 (pre-commit など) では git が GIT_DIR / GIT_INDEX_FILE を export する。
// 引き継ぐと、テストが作った使い捨てのリポジトリではなく、作業中のリポジトリへ
// commit してしまう。
func filterGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GIT_DIR="),
			strings.HasPrefix(kv, "GIT_WORK_TREE="),
			strings.HasPrefix(kv, "GIT_INDEX_FILE="):
			continue
		}
		out = append(out, kv)
	}
	return out
}
