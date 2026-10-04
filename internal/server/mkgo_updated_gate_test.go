package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// 配線の照合に使う正規表現。
//
// **識別子と整形を固定しない。** 1 周目の指摘を受けて「式まで見る」形にしたが、
// 今度は `clientUpdate` をリネームしただけ・ループ変数を `[k, v]` にしただけで
// 落ちるようになり、しかも診断は「渡していない」と**事実と逆**を出した
// (2 周目の敵対的レビューで 4 形を実測)。#2940 が High に挙げたのと同じ型なので、
// 変わってよい部分は `\w+` にしてある。
var (
	// 呼び出しの `{}` の中に mkGoVersion が shorthand で現れること。
	//
	// **1 段のネストを許す。** `[^}]*` だと、前の引数に `}` を含む式
	// (`(instance as typeof instance & { x?: string }).x ?? null` など) を
	// 足しただけで一致しなくなる。引数を足すのはごく自然な拡張なので、
	// そこで事実と逆の診断を出さないようにする。
	resolveCallPassesMkGoVersionRe = regexp.MustCompile(`(?s)resolveClientUpdate\(\{(?:[^{}]|\{[^{}]*\})*?\bmkGoVersion,`)

	// 判定結果が persist として取り出され、その中身が書かれること。
	//
	// **`for` 文ごと見る。** `persist` と `setItem` の部分文字列だけを見る形では、
	// 先頭 1 件だけ書く実装 (`persist[0]`) が素通りする — upstream 追従回のように
	// 両方の版が動いたとき lastMkGoVersion が書かれず、毎回ダイアログが出る。
	// **ループ変数を捕まえて、書き込みをそれに紐付ける。** 自由な
	// `setItem(\w+, \w+)` だと、将来 common.ts に同じ形の書き込みが 1 つ
	// 増えただけでこの検査が静かに空振りする。
	persistLoopRe = regexp.MustCompile(`for\s*\(\s*const\s*\[\s*(\w+)\s*,\s*(\w+)\s*\]\s+of\s+\w+\.persist\s*\)`)

	// ダイアログへ判定結果が渡ること。変数名は問わないが、**version は
	// isMkGo と同じオブジェクトから取ること**を要求する。
	//
	// `updatedVersion: \w+\.version` だけだと `instance.version` が通る
	// (`instance` は main-boot が既に import しているのでコンパイルも通る)。
	// それは「✨mk-go 2026.9.0🚀」と表示して CHANGELOG#202690 へ飛ぶ配線で、
	// **main-boot 自身のコメントが名指しで警告している唯一の live hazard**。
	// 識別子を捕まえて突き合わせる (RE2 に後方参照が無いので 2 段)。
	passesIsMkGoRe = regexp.MustCompile(`isMkGo:\s*(\w+)\.isMkGo\b`)

	// 三項の向き。`props.` は template では省ける (224 本中 209 本が省いている)。
	mkGoUpdatedBranchRe = regexp.MustCompile(`(?:props\.)?isMkGo\s*\?\s*i18n\.ts\.mkGoUpdated`)
)

// TestMkGoUpdatedDialogIsWired asserts that the "client was updated" dialog
// still keys off the mk-go release version (#2939).
//
// **壊れても誰も気付かない。** 症状は「更新してもダイアログが出ない」だけで、
// エラーもログも出ない。まさにそれが #2939 の状態で、`mk.1` から `mk.9a` まで
// 一度も出ていなかったのに誰も気付かなかった。#2762 が `wiring-check` を
// 作ったのと同じ型。
//
// **upstream 追従で落ちるのが一番ありうる。** `boot/common.ts` も
// `MkUpdated.vue` も upstream のファイルで、fork は差分を載せているだけなので、
// `git rebase --onto` の衝突解消で消えても気付けない。
//
// **見るのは 3 層。** どれが欠けても「出ないダイアログ」に戻る:
//
//   - 判定の実装 (`utility/check-client-update.ts`)
//   - boot の配線 (呼び出しと localStorage のキー)
//   - 表示側 (props と CHANGELOG へのリンク)
//
// コメントは落としてから探す — コメントアウトして残すのは消すのと同じ
// (#2856 が `wiring-check` で `/* */` に対して踏んだ型)。
func TestMkGoUpdatedDialogIsWired(t *testing.T) {
	root := filepath.Join(repoRootDir(t), "frontend", "packages", "frontend")
	utility := filepath.Join(root, "src", "utility", "check-client-update.ts")
	boot := filepath.Join(root, "src", "boot", "common.ts")
	mainBoot := filepath.Join(root, "src", "boot", "main-boot.ts")
	dialog := filepath.Join(root, "src", "components", "MkUpdated.vue")

	if _, err := os.Stat(boot); err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", boot)
	}

	read := func(path string) string {
		raw, err := os.ReadFile(path)
		require.NoErrorf(t, err, "%s を読めない", path)
		return stripComments(string(raw))
	}

	// 判定の実装そのもの。単体テストはこのファイルに対して書いてあるので、
	// 消えればそちらも落ちるが、gate の診断のほうが原因に近い。
	utilitySrc := read(utility)
	require.Containsf(t, utilitySrc, "export function resolveClientUpdate",
		"%s に resolveClientUpdate が無い。更新判定の実装はここにある (#2939)", utility)
	require.Containsf(t, utilitySrc, "export function mkGoChangelogUrl",
		"%s に mkGoChangelogUrl が無い。「更新情報を見る」のリンク先を組み立てている", utility)

	// **単体テストの存在も見る。** どのキーをいつ書くかの担保はこの 1 本に
	// 全面的に乗っており、消えても vitest は件数が減るだけで緑になる
	// (`gaterun-check` が Go 側で塞いでいる「黙って検査が止まる」型が、
	// frontend の vitest には無い)。
	// **場所は固定しない。** vitest の include は `test/unit/**` なので、
	// サブディレクトリへ移しても実行は続く。パスを決め打つと、移動しただけで
	// 「試していない」という事実と逆の診断が出る。
	spec := findSpec(t, filepath.Join(root, "test", "unit"), "check-client-update.test.ts")
	specSrc := read(spec)
	// **import だけでは足りない。** 中身を空にして import 行だけ残しても
	// 部分文字列は充足される。実際に何を固定しているかまで見る。
	require.Containsf(t, specSrc, "resolveClientUpdate(",
		"%s が resolveClientUpdate を呼んでいない。保存条件の担保がこのファイルにしか無い", spec)
	require.Containsf(t, specSrc, ".persist",
		"%s が persist を検査していない。どのキーをいつ書くかを固定しているのはここだけで、"+
			"反転させても型もゲートも通る", spec)

	// boot の配線。**識別子の存在だけを見ない。** 呼び出しは残したまま引数を
	// `mkGoVersion: null` に固定しても、`const mkGoVersion = ...` の宣言行が
	// あるので部分文字列の検査は充足されてしまう。それは #2939 の元の状態
	// (何度更新してもダイアログが出ない) そのもので、vue-tsc も eslint も
	// vitest も赤くならない (敵対的レビューで実測された)。
	// **渡している式まで見る。**
	bootSrc := read(boot)
	require.Containsf(t, bootSrc, "resolveClientUpdate(",
		"%s が resolveClientUpdate を呼んでいない。更新判定が upstream の版に戻り、"+
			"mk-go を更新してもダイアログが出なくなる (#2939)", boot)
	require.Regexpf(t, resolveCallPassesMkGoVersionRe, bootSrc,
		"%s が resolveClientUpdate へ mkGoVersion をそのまま渡していない。"+
			"呼び出しが残っていても引数を固定すれば判定は死ぬ", boot)

	// **localStorage のキーを分けていること。** 同じキーに入れると
	// compareVersions('2026.9.0', '1.3.0') === 1 で、backend を差し替えた
	// だけで更新と誤検知する。
	// **キーのリテラルで見る。** 変数名だけを探す形だと、getItem の引数を
	// 'lastVersion' に戻しても `const lastMkGoVersion` が残っていれば通ってしまう
	// (実測: 変異検証でこの緩さを踏んだ)。
	require.Containsf(t, bootSrc, `getItem('lastMkGoVersion')`,
		"%s が localStorage から 'lastMkGoVersion' を読んでいない。lastVersion と混ぜると"+
			"compareVersions('2026.9.0', '1.3.0') === 1 で backend の差し替えを更新と誤検知する", boot)
	// **保存も配線されていること。** 書かないと lastMkGoVersion が更新されず、
	// ログイン中の全利用者にページ遷移のたびにダイアログが出続ける。何を書くかは
	// resolveClientUpdate が決めて vitest で固定してあるので、ここでは
	// 「決まったものを実際に書いているか」だけを見る。
	loop := persistLoopRe.FindStringSubmatch(bootSrc)
	require.NotNilf(t, loop, "%s が persist を回して書いていない。先頭 1 件だけ書く形も含めて不可 — "+
		"両方の版が動いたリリースで lastMkGoVersion を書き損ね、毎回ダイアログが出る", boot)
	writeRe := regexp.MustCompile(`miLocalStorage\.setItem\(\s*` +
		regexp.QuoteMeta(loop[1]) + `\s*,\s*` + regexp.QuoteMeta(loop[2]) + `\s*\)`)
	require.Regexpf(t, writeRe, bootSrc,
		"%s が persist のループ変数を localStorage へ書いていない", boot)

	// 表示側へ判定結果が渡っていること。渡らないと props が既定値のままで、
	// mk-go の更新なのに「Misskeyが更新されました！」が出る。
	// **キー名だけでは足りない。** `isMkGo: false` に固定しても `isMkGo:` は
	// 残るので、値まで含めて照合する。
	mainBootSrc := read(mainBoot)
	passed := passesIsMkGoRe.FindStringSubmatch(mainBootSrc)
	require.NotNilf(t, passed, "%s が MkUpdated へ判定結果の isMkGo を渡していない。値を固定すると"+
		"mk-go の更新でも upstream の文言と misskey-hub へのリンクが出る", mainBoot)
	sameSourceVersionRe := regexp.MustCompile(`updatedVersion:\s*` + regexp.QuoteMeta(passed[1]) + `\.version\b`)
	require.Regexpf(t, sameSourceVersionRe, mainBootSrc,
		"%s が isMkGo と同じ判定結果から version を渡していない。instance を読み直すと"+
			"判定した版と表示する版が食い違う (mk-go の更新なのに Misskey の版が出る)", mainBoot)

	// 表示側。**CHANGELOG へのリンクが残っていること** — ここが落ちると
	// 「更新情報を見る」が misskey-hub の Misskey リリースノートへ飛び、
	// mk-go の変更点はそこに載っていない。
	dialogSrc := read(dialog)
	require.Containsf(t, dialogSrc, "mkGoChangelogUrl(",
		"%s が mkGoChangelogUrl を呼んでいない。「更新情報を見る」が Misskey の"+
			"リリースノートへ飛び、mk-go の変更点は載っていない (#2939)", dialog)
	// **三項の向きまで見る。** 参照の有無だけだと、分岐を入れ替えて
	// mk-go のときに misskeyUpdated を出す変異が素通りする (実測)。
	require.Regexpf(t, mkGoUpdatedBranchRe, dialogSrc,
		"%s が mk-go のときに mkGoUpdated を出していない。「Misskeyが更新されました！」に戻る", dialog)
}

// findSpec locates a spec file anywhere under dir.
//
// **`.spec.ts` へのリネームは意図的に落とす。** vitest の include は
// `*.test.ts` なので、リネームすると黙って実行されなくなる (memory の
// 「`_spec.ts` は silent skip」と同じ形)。落ちてよい。
func findSpec(t *testing.T, dir, name string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			found = path
		}
		return nil
	})
	require.NoErrorf(t, err, "%s を走査できない", dir)
	require.NotEmptyf(t, found, "%s の下に %s が無い。更新判定の単体テストが消えると、"+
		"保存条件を反転させても全検査が緑になる", dir, name)
	return found
}
