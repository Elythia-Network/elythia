package entitycompat

import (
	"regexp"
	"testing"
)

// legacyMigrateDockerfiles are the distributed images that must keep
// /app/migrate as a symlink to elythia during 2.x (#3394).
//
// **名指しで要求する。** 配布イメージ (`ghcr.io/shiroha-a/mk` の latest / bundled) は
// develop への push ごとに出るので、古い compose の migrate サービス
// (`entrypoint: ["/app/migrate"]`) のまま pull した運営者の migration は、この 2 つの
// Dockerfile が symlink を持っているかどうかだけで決まる。行を消しても build は
// 通り、壊れるのは運営者の手元で初めて分かる。UDS の image は entrypoint が
// `elythia migrate` を呼ぶので対象外。3.0 で互換を撤去するときにこのテストも消す。
var legacyMigrateDockerfiles = []string{"Dockerfile", "Dockerfile.bundled"}

var (
	// legacyMigrateSymlink matches `ln -s elythia <dir>/migrate` (with or without -f).
	legacyMigrateSymlink = regexp.MustCompile(`\bln -sf? elythia (\S+)/migrate(?:\s|&&|;|$)`)
	// binDirCopy matches a COPY of a whole directory (trailing slash) into /app/.
	binDirCopy = regexp.MustCompile(`(?m)^COPY --from=\S+ (?:--chown=\S+ )?(\S+)/ /app/\s*$`)
)

func TestDistributedImagesKeepLegacyMigrateSymlink(t *testing.T) {
	for _, path := range legacyMigrateDockerfiles {
		t.Run(path, func(t *testing.T) {
			// コメントの行と行末のシェルコメントを落とし、行継続を畳んでから見る。
			// コメントアウトして残した行は消したのと同じ。
			body := stripShellLineComments(foldContinuations(stripDockerfileComments(readRepoFile(t, path))))
			if problem := legacyMigrateSymlinkProblem(body); problem != "" {
				t.Errorf("%s: %s", path, problem)
			}
		})
	}
}

// legacyMigrateSymlinkProblem describes what is missing from one Dockerfile
// body, or returns "" when the symlink is created and copied as a symlink.
func legacyMigrateSymlinkProblem(body string) string {
	ln := legacyMigrateSymlink.FindStringSubmatch(body)
	if ln == nil {
		return "`ln -s elythia <dir>/migrate` で /app/migrate の symlink を作っていません。" +
			"古い compose (`entrypoint: [\"/app/migrate\"]`) の migration が新しい image で落ちます (#3394、3.0 まで残す)"
	}
	dir := ln[1]
	for _, m := range binDirCopy.FindAllStringSubmatch(body, -1) {
		if m[1] == dir {
			return ""
		}
	}
	// ファイル単体の COPY は symlink を辿り、実体の複製になる (/app/migrate が
	// 別バイナリになり、image も膨らむ)。ディレクトリごと COPY しないと symlink は残らない。
	return dir + "/migrate の symlink を作っていますが、" + dir + "/ をディレクトリごと /app/ へ COPY していません。" +
		"ファイル単体の COPY は symlink を辿るので、/app/migrate が symlink として残りません"
}

// 検査の枝そのものを固定する。実際の Dockerfile は今は全部通るので、壊れた形を
// 直接渡して落ちることを確かめる。
func TestLegacyMigrateSymlinkProblem_DetectsBrokenForms(t *testing.T) {
	const ok = "RUN go build -o /out/bin/elythia ./cmd/elythia && ln -s elythia /out/bin/migrate\n" +
		"COPY --from=builder /out/bin/ /app/\n"
	if p := legacyMigrateSymlinkProblem(ok); p != "" {
		t.Fatalf("the correct form must pass: %s", p)
	}
	cases := map[string]string{
		"no symlink":       "RUN go build -o /out/bin/elythia ./cmd/elythia\nCOPY --from=builder /out/bin/ /app/\n",
		"single-file copy": "RUN ln -s elythia /out/bin/migrate\nCOPY --from=builder /out/bin/elythia /app/elythia\n",
		"other directory":  "RUN ln -s elythia /out/bin/migrate\nCOPY --from=builder /out/other/ /app/\n",
		"no copy at all":   "RUN ln -s elythia /out/bin/migrate\n",
	}
	for name, body := range cases {
		if legacyMigrateSymlinkProblem(body) == "" {
			t.Errorf("%s: the broken form passed", name)
		}
	}
}
