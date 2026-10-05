package entitycompat

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/entitycompat/ipscanfixture"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publicShapeDirs は「利用者と連合が受け取る JSON の形」を作るディレクトリ。
//
// **`internal/entity` / `internal/activitypub` だけでは足りない。** handler が自分の
// ファイルに宣言した response struct もそのままレスポンスの shape になるし、stream の
// イベント payload も利用者に届く。実測で `internal/api` だけで json タグは 1,539 行
// あり、うち IP を指すキーを持つのは `internal/api/admin` の IP 照会 API だけだった。
var publicShapeDirs = []string{
	"internal/entity",
	"internal/activitypub",
	"internal/api",
	"internal/server",
	"internal/stream",
}

// レスポンス DTO と連合の shape に IP が現れないことを固定する (#3136)。
//
// **reflect で型を手で並べない。** 最初そう書いたところ、敵対的レビューで
// `entity.MeDetailed` (= `/api/i`) が対象から漏れていること、入れ子 struct・map の
// 値型・匿名埋め込みを辿れていないことが実測された。**AST で全 struct のタグを読む**
// ほうが、型を列挙せずに済むぶん射程が広く、入れ子も別 struct として自然に拾える。
//
// **「一般利用者に IP が出ない」ではなく「他人の IP は出ない」が正確な主張。**
// IP を意図的に出す経路は 2 つあり、どちらもこの検査とは別に守られている —
// `entity.PackSignin` (本人の main stream と `admin/show-user`。後者は #3114 の
// `TestShowUser_SigninIPsRequirePolicy`)、`admin/ip/*` (モデレーター + policy +
// scope の 3 段。allowlist に列挙してある)。
//
// **射程の外** (どれもテストでは見えない。増やすなら別の手当てが要る):
//
//   - handler が `map[string]any` を手で組んで返す経路 (`entity.PackSignin` / nodeinfo)
//   - `datatypes.JSON` / `json.RawMessage` のような不透明な列の中身 (形が実行時に決まる)
//   - **`publicShapeDirs` の外に宣言された型を handler が `c.JSON` にそのまま渡す形。**
//     `/api/server-info` (**未認証**) が返す `serverstats.PublicStats` が実例で、
//     あそこに IP のキーを足しても落ちない (実測)。`internal/core` を走査に足すのは
//     採らなかった — IP を持つ内部の入力構造体が 11 件流れ込んで allowlist が倍増し、
//     本物の signal が埋もれる (実測)
//   - `remoteAddr` / `xForwardedFor` のように `ip` の語を含まない綴り (名前で判定するため)。
//     `CDNIPs` / `ip4s` / `XIp` のような語割りの残りも同じ (実測)
//   - **フィールドの型が走査対象の外**にある形は、型の参照として
//     `TestPublicShapesDoNotReferenceIPBearingTypes` が見る (推移的に追う)
func TestResponseAndFederationShapesHaveNoIPField(t *testing.T) {
	root := repoRoot(t)

	var all []taggedField
	for _, d := range publicShapeDirs {
		all = append(all, scanJSONTags(t, filepath.Join(root, d), root)...)
	}

	// **下限を持たせる。** 違反 0 件が正常な状態なので、抽出が壊れても「検出 0 件」と
	// 区別が付かない。実測 2,332 (2026-09-21。数え方は `scanJSONTags` が
	// `publicShapeDirs` 全体で返した**要素数**。キーのユニーク数ではない)。
	require.GreaterOrEqualf(t, len(all), 2200,
		"json のキーを %d 件しか拾えていない。抽出が壊れている", len(all))

	// **ファイル単位でも取りこぼしを見る。** 件数の下限も代表キーも「大きなファイルさえ
	// 残っていれば通る」ので、走査が縮んでも気付けない (実測: 2 ファイルだけを残す変異が、
	// 落としたファイルに置いた本物の漏れごと素通りした)。`json:"` を含むファイルは
	// 1 件以上寄与していなければならない。truth は AST ではなく**テキスト走査**から
	// 採るので、AST 側が壊れれば必ず食い違う。
	contributed := map[string]bool{}
	for _, f := range all {
		contributed[f.File] = true
	}
	want := filesWithJSONTag(t, root, publicShapeDirs)
	// **truth 側にも下限が要る。** 「1 件でもあればよい」だと、テキスト走査を
	// 1 ファイルに縮めるだけでこの検査が丸ごと無意味になる (実測で素通りした)。
	// 実測 156 (2026-09-21)。
	require.GreaterOrEqualf(t, len(want), 150,
		"json タグを持つファイルを %d 件しか拾えていない。テキスト走査が壊れている", len(want))
	for _, f := range want {
		assert.Truef(t, contributed[f], "%s から 1 件も収集していない。走査が縮んでいる", f)
	}

	for _, f := range all {
		if !looksLikeIPKey(f.Key) {
			continue
		}
		reason, ok := ipShapeAllowlist[f.ref()]
		assert.Truef(t, ok,
			"%s の %s.%s (json:%q) が IP を指している。**他人の IP をレスポンスや連合へ"+
				"出さないこと** (#3136)。出す必要があるなら、モデレーション権限でゲートした"+
				"経路か、本人だけが引ける経路 (`i/signin-history`) を通し、ここに理由付きで"+
				"足すこと。",
			f.File, f.Struct, f.Field, f.Key)
		if ok {
			assert.NotEmptyf(t, reason, "%s の allowlist に理由が無い", f.ref())
		}
	}

	for ref := range ipShapeAllowlist {
		found := false
		for _, f := range all {
			if f.ref() == ref {
				found = true
				break
			}
		}
		assert.Truef(t, found, "allowlist の %q が実在しない。移動したか消えた", ref)
	}
}

// ipShapeAllowlist は IP を指すキーを持ってよい shape と、その理由。
//
// **`internal/entity` / `internal/activitypub` / `internal/stream` からは 1 件も
// 載っていない** — 利用者向けの DTO と連合の出力に他人の IP は出ない。載っているのは
// IP 照会の admin API と、そもそもレスポンスではない入力構造体だけ。
//
// **件数やフラグも載る。** 判定は語で見るので `sharedIpCount` / `targetIpsTruncated`
// のような「IP そのものではない」キーも拾う。fail-closed なので危険側には倒れないが、
// 理由にはそう書いておく。
var ipShapeAllowlist = map[string]string{
	// --- admin/ip/* (#3104 / #3105 / #3106) ---
	//
	// 3 段で守られている (`internal/server/router.go`): `RequireModerator` +
	// `RequireRolePolicy(canSearchIpHistory)` + `RequireScope("read:admin:user-ips")`。
	// policy の既定は false で admin は bypass するので、既定の挙動は upstream と
	// 同じ「管理者のみ」。
	"internal/api/admin/ip_search.go#func IPAccounts.IP": "" +
		"`admin/ip/accounts` がリクエストを bind する構造体。**入力**であって出力ではない。",
	"internal/api/admin/ip_search.go#ipAccountsResponse.IP": "" +
		"同 API の応答。検索に実際に使った正規化後の IP を返す (入力そのままではない)。",
	"internal/api/admin/ip_related.go#ipRelatedSharedIP.IP": "" +
		"`admin/ip/related-accounts` が返す共有 IP 本体 (#3105)。順位の根拠として出す。",
	"internal/api/admin/ip_related.go#ipRelatedSharedIP.IPAccountCount": "" +
		"IP そのものではない。窓の中でその IP を使ったアカウント数 (共有回線かの判断材料)。",
	"internal/api/admin/ip_related.go#ipRelatedSharedIP.IPAccountCountIsLowerBound": "" +
		"IP そのものではない。上の件数が「これ以上」であることを示すフラグ。",
	"internal/api/admin/ip_related.go#ipRelatedCandidate.SharedIPs": "" +
		"共有 IP の一覧 (要素は上の `ipRelatedSharedIP`)。",
	"internal/api/admin/ip_related.go#ipRelatedCandidate.SharedIPCount": "" +
		"IP そのものではない。共有した IP の数 (重複なし)。",
	"internal/api/admin/ip_related.go#ipRelatedResponse.TargetIPCount": "" +
		"IP そのものではない。対象が使った IP の数。",
	"internal/api/admin/ip_related.go#ipRelatedResponse.TargetIPsTruncated": "" +
		"IP そのものではない。対象の IP を打ち切ったかのフラグ。",
	"internal/api/admin/ip_lookup_log.go#ipLookupLogEntity.IP": "" +
		"`admin/ip/lookup-log` が返す照会の監査記録 (#3106)。**応答自体が機密**なので" +
		"照会 API と同じ 3 段で守ってある。",

	// --- ここから下はレスポンスではない ---
	"internal/api/drive/url_upload.go#URLUploadInput.RequestIP": "" +
		"`drive/files/upload-from-url` の処理に渡す入力。`Process(ctx, in)` に直接渡すだけで " +
		"`encoding/json` を通らない (タグが無いので収集側からはキーに見える)。",
}

// allowlist の中身そのものを固定する。
//
// **entry を足すのは「他人の IP をレスポンスに出す」と決めたときだけ。** 上の
// 「死んだ entry を落とす」検査は、実在する漏れと対になった entry は落とさないので、
// それだけだと足し放題になる (実測: entry と漏れを対で足すと全テストが緑のまま通った)。
func TestIPShapeAllowlistMatchesExpected(t *testing.T) {
	var got []string
	for k := range ipShapeAllowlist {
		got = append(got, k)
	}
	sort.Strings(got)
	assert.Equal(t, []string{
		"internal/api/admin/ip_lookup_log.go#ipLookupLogEntity.IP",
		"internal/api/admin/ip_related.go#ipRelatedCandidate.SharedIPCount",
		"internal/api/admin/ip_related.go#ipRelatedCandidate.SharedIPs",
		"internal/api/admin/ip_related.go#ipRelatedResponse.TargetIPCount",
		"internal/api/admin/ip_related.go#ipRelatedResponse.TargetIPsTruncated",
		"internal/api/admin/ip_related.go#ipRelatedSharedIP.IP",
		"internal/api/admin/ip_related.go#ipRelatedSharedIP.IPAccountCount",
		"internal/api/admin/ip_related.go#ipRelatedSharedIP.IPAccountCountIsLowerBound",
		"internal/api/admin/ip_search.go#func IPAccounts.IP",
		"internal/api/admin/ip_search.go#ipAccountsResponse.IP",
		"internal/api/drive/url_upload.go#URLUploadInput.RequestIP",
	}, got,
		"IP を指すキーを持つ shape が増減している。**他人の IP をレスポンスや連合へ出す判断**"+
			"なので、経路と権限ゲートを確かめてからこの一覧を更新すること")
}

// 入れ子の型まで IP が届いていないことを見る。
//
// **キーの走査だけでは足りない。** `Session *model.Signin` のように**走査対象外の
// パッケージの struct** を 1 フィールド持つだけで、`encoding/json` はその中の `ip` を
// 出すのに、こちらからは中が見えない (実測で素通りした)。IP を持つ型は実在する
// (`model.Signin` / `model.UserIP` / `model.IPLookupLog` / `model.DriveFile`)。
//
// そこで `internal/` 全体から「自分の JSON キーに IP を持つ名前付き struct」を集め、
// 公開 shape がそれを参照していないことを要求する。**一覧は導出する** — 手で持つと
// それ自体が同期を要する第 2 の一覧になる。
func TestPublicShapesDoNotReferenceIPBearingTypes(t *testing.T) {
	root := repoRoot(t)

	tainted := ipBearingTypes(t, filepath.Join(root, "internal"), root)
	// 抽出が壊れると「参照 0 件」と区別が付かないので、実在する型を要求する。
	for _, want := range []string{"model.Signin", "model.UserIP", "model.IPLookupLog", "model.DriveFile"} {
		require.Containsf(t, tainted, want, "IP を持つ型として %q を拾えていない", want)
	}

	// **参照側にも下限が要る。** tainted が正しくても、型の参照を 1 つも解決できて
	// いなければ照合は空回りする。`typeRefs` から package 修飾の収集を落とす 1 行の
	// 変異が、`*model.Signin` を足す漏れごと素通りした (実測)。実測 9。
	crossPkg := 0
	referenced := map[string]bool{}
	for _, d := range publicShapeDirs {
		for _, f := range scanJSONTags(t, filepath.Join(root, d), root) {
			for _, ref := range f.TypeRefs {
				if strings.HasPrefix(ref, "model.") {
					crossPkg++
				}
				if _, bad := tainted[ref]; !bad {
					continue
				}
				reason, ok := ipRefAllowlist[f.ref()]
				assert.Truef(t, ok,
					"%s の %s.%s が %s を参照している。**その型は自分の JSON キーに IP を"+
						"持つ**ので、`encoding/json` は入れ子として IP を出す (#3136)。"+
						"出す必要があるなら権限でゲートしたうえで理由付きで足すこと。",
					f.File, f.Struct, f.Field, ref)
				if ok {
					assert.NotEmptyf(t, reason, "%s の allowlist に理由が無い", f.ref())
				}
				referenced[f.ref()] = true
			}
		}
	}

	require.GreaterOrEqualf(t, crossPkg, 9,
		"公開 shape から `model.*` への参照を %d 件しか解決できていない。型の照合が壊れている", crossPkg)

	// **死んだ entry も落とす。** これが無いと、実在しない場所を書いた entry が
	// 誰にも検出されずに残る (実測: 捏造した `driveFileRow.RequestIP` が全テスト
	// 緑のまま通った)。
	for ref := range ipRefAllowlist {
		assert.Truef(t, referenced[ref], "allowlist の %q が実在しない。移動したか消えた", ref)
	}
}

// 「IP を持つ型」の判定を人工ソースで固定する。
//
// 実データの IP 保持型は 4 つとも**タグ付き**なので、タグ無しの枝も `json:"-"` の枝も
// 非公開の枝も一度も実行されない。そこを潰しても実データからは何も起こらないのに、
// 潰した状態では本物の漏れが通る (実測)。
func TestIPBearingTypesPinsEveryBranch(t *testing.T) {
	root := repoRoot(t)
	got := ipBearingTypes(t, filepath.Join(root, "internal/entitycompat/ipbearingfixture"), root)
	var names []string
	for k := range got {
		names = append(names, k)
	}
	sort.Strings(names)
	assert.Equal(t, []string{
		"ipbearingfixture.AliasedIP",     // 別名も同じものを指す
		"ipbearingfixture.DefinedIP",     // 下敷きが carrier
		"ipbearingfixture.NestedIP",      // 推移的に汚れる
		"ipbearingfixture.TaggedEmbedIP", // タグ付きの匿名埋め込みはタグ名がキー
		"ipbearingfixture.TaggedIP",
		"ipbearingfixture.UntaggedEmbedIP", // タグ無しの匿名埋め込みは中身が昇格する
		"ipbearingfixture.UntaggedIP",      // タグ無しはフィールド名で出る
	}, names, "IP を持つ型の判定が変わっている")
}

// 型の参照の解決そのものを人工ソースで固定する。
//
// **実データには import の別名が 1 つも無い**ので、別名を解決する枝は実データでは
// 一度も実行されない。`[]T` / `map[K]V` / `*T` / generic も同じ。
func TestTypeRefsResolvesNamedTypes(t *testing.T) {
	const src = `package p

import (
	m "github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/entity"
)

type Box[T any] struct{ V T }

type S struct {
	A *m.Signin            ` + "`json:\"a\"`" + `
	B []entity.UserLite    ` + "`json:\"b\"`" + `
	C map[m.Signin]Local   ` + "`json:\"c\"`" + `
	D Box[m.UserIP]        ` + "`json:\"d\"`" + `
	E string               ` + "`json:\"e\"`" + `
}

type Local struct{}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "s.go", src, 0)
	require.NoError(t, err)
	al := importAliases(f)
	require.Equal(t, map[string]string{"m": "model"}, al, "import の別名を解決できていない")

	var st *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if ts, ok := n.(*ast.TypeSpec); ok && ts.Name.Name == "S" {
			st = ts.Type.(*ast.StructType)
		}
		return true
	})
	require.NotNil(t, st)

	got := map[string][]string{}
	for _, fld := range st.Fields.List {
		got[fld.Names[0].Name] = typeRefs(fld.Type, "p", al)
	}
	assert.Equal(t, []string{"model.Signin"}, got["A"], "別名越しのポインタ")
	assert.Equal(t, []string{"entity.UserLite"}, got["B"], "slice の要素型")
	assert.Equal(t, []string{"model.Signin", "p.Local"}, got["C"], "map の鍵と値")
	assert.Equal(t, []string{"p.Box", "model.UserIP"}, got["D"], "generic の実体化")
	assert.Empty(t, got["E"], "組み込み型は名前付き型ではない")
}

// 入れ子まで歩くことを人工の JSON で固定する。
//
// **代表型はどれもゼロ値で marshal するので、object の入れ子が 1 つも生まれない。**
// 再帰を潰しても実データからは何も起こらない (実測で素通りした)。
func TestAllJSONKeysWalksNestedObjects(t *testing.T) {
	got := allJSONKeys(t, []byte(`{"a":1,"b":{"ip":"1.2.3.4"},"c":[{"d":{"e":2}}]}`))
	assert.Equal(t, []string{"a", "b", "c", "d", "e", "ip"}, got)
}

// ipRefAllowlist は IP を持つ型を参照してよい場所と、その理由。
//
// 2 系統ある。**IP 照会 API が自分の行を入れ子にしているもの** (出すのが目的なので
// 上の `ipShapeAllowlist` と同じ 3 段でゲート済み) と、**marshal されない構造体**
// (packer に渡す入力や context で `encoding/json` を通る経路が無い)。レスポンスの形に
// `*model.User` を 1 つ足すような変更は、ここに書かない限り落ちる。
var ipRefAllowlist = map[string]string{
	// --- admin/ip/* の応答が自分の行を入れ子にしている (3 段でゲート済み) ---
	"internal/api/admin/ip_lookup_log.go#ipLookupLogResponse.Entries": "" +
		"`admin/ip/lookup-log` の応答が監査記録の行を並べる。行が IP を持つのは意図どおり。",
	"internal/api/admin/ip_related.go#ipRelatedResponse.Candidates": "" +
		"`admin/ip/related-accounts` の応答が候補を並べる。",
	"internal/api/admin/ip_related.go#ipRelatedCandidate.SharedIPs": "" +
		"候補が共有 IP の行を並べる。",

	// --- ここから下は marshal されない構造体 ---
	"internal/api/drive/url_upload.go#URLUploadInput.User": "" +
		"`drive/files/upload-from-url` の処理に渡す入力。handler は 204 を返すので " +
		"`encoding/json` を通らない。",
	"internal/entity/notification.go#NotificationItem.User": "" +
		"`PackNotifications` に渡す入力。解決済みの通知者をまとめて渡すための器で、" +
		"出力は packer が組み立てる map。",
	"internal/entity/notification.go#NotificationItem.Note": "同上 (参照先のノート)。",
	"internal/entity/page.go#PackPageContext.Owner": "" +
		"`PackPageWithContext` に渡す context。ページの所有者を解決して渡すためのもので、" +
		"出力は packer が組み立てる map。",
}

// allowlist の中身そのものを固定する (上の allowlist と同じ理由)。
func TestIPRefAllowlistMatchesExpected(t *testing.T) {
	var got []string
	for k := range ipRefAllowlist {
		got = append(got, k)
	}
	sort.Strings(got)
	assert.Equal(t, []string{
		"internal/api/admin/ip_lookup_log.go#ipLookupLogResponse.Entries",
		"internal/api/admin/ip_related.go#ipRelatedCandidate.SharedIPs",
		"internal/api/admin/ip_related.go#ipRelatedResponse.Candidates",
		"internal/api/drive/url_upload.go#URLUploadInput.User",
		"internal/entity/notification.go#NotificationItem.Note",
		"internal/entity/notification.go#NotificationItem.User",
		"internal/entity/page.go#PackPageContext.Owner",
	}, got,
		"IP を持つ型を入れ子にした shape が増減している。**その型が本当に marshal されないか**"+
			"を確かめてから更新すること")
}

// 静的なタグ走査に加えて**実際に `json.Marshal` する**。
//
// 静的走査は自前 marshaler を持つ型の中身を見られず、実 Marshal は `omitempty` で
// 消えるフィールドを見られない。どちらか片方では足りないので両方置く (1 稿目にあった
// この検査を 2 稿目で落としてしまい、敵対的レビューで指摘された)。
//
// **この検査は変異で殺せない。** 実データに「静的走査からは見えないが Marshal には
// 出る IP」が 1 つも無いので、アサーションを外しても緑のままになる (実測)。**それが
// 示すのは「いまは陽性が無い」ことだけ**で、自前 marshaler が IP を吐くようになった
// ときに落ちるのはここだけ。静的走査側は `TestCustomJSONMarshalersAreKnown` が
// 一覧で押さえる。
func TestPublicShapesMarshalWithoutIP(t *testing.T) {
	for _, v := range []any{
		entity.UserLite{},
		entity.UserDetailed{},
		entity.MeDetailed{},
		activitypub.Person{},
		activitypub.Note{},
	} {
		b, err := json.Marshal(v)
		require.NoErrorf(t, err, "marshal %T", v)
		keys := allJSONKeys(t, b)
		require.NotEmptyf(t, keys, "%T が 1 つもキーを出していない (検査が空振りしている)", v)
		for _, k := range keys {
			assert.Falsef(t, looksLikeIPKey(k), "%T が %q を出している", v, k)
		}
	}
}

// 収集ロジックそのものを、**`encoding/json` の実挙動と突き合わせて**固定する。
//
// **期待値を手で書かない。** `ipscanfixture` は普通にコンパイルされるパッケージなので、
// 同じ型を実際に `json.Marshal` して、そのキー集合と `scanJSONTags` の結果を比べられる。
// 手書きの期待値だと、収集側と `encoding/json` が食い違っても両方を直すまで気付けない
// (実測: 食い違う形を足しても緑のままだった)。`testdata/` に置くとコンパイルされない
// ので、この突き合わせができない。
func TestScanJSONTagsCollectsWhatEncodingJSONEmits(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal/entitycompat/ipscanfixture")

	samples := []any{ipscanfixture.Outer{}, ipscanfixture.Inner{}, ipscanfixture.Labeled{}}

	// 対象の型を数え落としていないこと (`samples` は手で並べているので)。
	var sampleNames []string
	for _, v := range samples {
		sampleNames = append(sampleNames, reflect.TypeOf(v).Name())
	}
	sort.Strings(sampleNames)
	require.Equal(t, exportedStructNames(t, dir), sampleNames,
		"fixture の struct が増減している。samples を更新すること")

	emitted := map[string]bool{}
	for _, v := range samples {
		b, err := json.Marshal(v)
		require.NoErrorf(t, err, "marshal %T", v)
		var obj map[string]json.RawMessage
		require.NoErrorf(t, json.Unmarshal(b, &obj), "unmarshal %T", v)
		for k := range obj {
			emitted[k] = true
		}
	}

	collected := map[string]bool{}
	for _, f := range scanJSONTags(t, dir, root) {
		collected[f.Key] = true
	}

	assert.Equal(t, sortedKeySet(emitted), sortedKeySet(collected),
		"`encoding/json` が出すキーと収集したキーが食い違っている")
	// fixture が「実データには無い形」を本当に含んでいることも見る。
	for _, k := range []string{"Untagged", "deep", "labeled", "IPList"} {
		require.Containsf(t, emitted, k, "fixture が %q を出していない。形が減っている", k)
	}
}

// 自前の marshaler を持つ型は AST のタグ走査から外れる。一覧を固定して、増えたら
// 気付けるようにする (増えた型が IP を吐くかは人が見る)。
//
// **`MarshalText` も見る。** `encoding/json` は `MarshalJSON` が無くても
// `encoding.TextMarshaler` を優先するので、そちらでも中身は見えなくなる。
//
// **走査は `internal/` 全体。** `publicShapeDirs` に絞ると、**走査対象の外に宣言した型に
// marshaler を付けて IP を吐かせる**形が見えない (実測で素通りした)。実データの marshaler は
// `internal/` 全体で 1 件しか無いので、広げても一覧は増えない。
//
// **走査そのものを人工のソースで固定する。** 実データには `MarshalJSON` が 1 件
// あるだけなので、`MarshalText` を見ない変異も generic なレシーバを捨てる変異も、
// 実データ相手には何も起こさない (実測で両方素通りした)。fixture の 4 件が一覧に
// 載っているのはそのため。
func TestCustomJSONMarshalersAreKnown(t *testing.T) {
	root := repoRoot(t)
	found := scanCustomMarshalers(t, filepath.Join(root, "internal"), root)
	sort.Strings(found)
	assert.Equal(t, []string{
		// `[]Multikey` をそのまま出すだけ。IP は持たない。
		"internal/activitypub/types.go#MultikeyList.MarshalJSON",
		// 以下は走査そのものを固定するための人工ソース。
		"internal/entitycompat/marshalerfixture/marshalers.go#Generic.MarshalJSON",
		"internal/entitycompat/marshalerfixture/marshalers.go#Plain.MarshalJSON",
		"internal/entitycompat/marshalerfixture/marshalers.go#Pointer.MarshalJSON",
		"internal/entitycompat/marshalerfixture/marshalers.go#Text.MarshalText",
	}, found,
		"自前の marshaler を持つ型が増減した。**タグ走査では中身が見えない**ので、"+
			"新しい型が IP を吐かないことを人が確かめてから一覧を更新すること")
}

// 判定そのものを固定する。
//
// **実データは全て陰性なので、これが無いと判定の枝が一度も実行されない。** 緩めても
// 厳しくしても「違反 0 件」で緑のままになる。陰性側は実在するキーから採ってある。
//
// **`IPAddr` 系が要点。** 2 稿目で語の切り方を「小文字/数字の直後の大文字」だけに
// したところ、`IPAddr` / `IPHash` / `IPList` が 1 語に潰れて素通りする回帰を作った
// (敵対的レビューで実測)。しかも当時の表は `IPAddress` しか持っておらず、正規表現に
// `address` が書いてあったせいで**表が回帰を隠していた**。
func TestLooksLikeIPKey(t *testing.T) {
	for _, c := range []struct {
		key  string
		want bool
	}{
		{"ip", true},
		{"IP", true},
		{"ips", true},
		{"IPs", true}, // Go の複数形イニシャリズム
		{"lastIp", true},
		{"lastIPs", true},
		{"ipAddress", true},
		{"ipaddress", true},
		{"IPaddress", true},
		{"IPADDRESS", true},
		{"ipaddr", true},
		{"lastIPaddressHash", true},
		{"IPAddress", true},
		{"IPAddresses", true},
		{"IPAddr", true}, // 2 稿目の回帰はここ
		{"IPHash", true},
		{"IPList", true},
		{"IPRange", true},
		{"lastIPAddr", true},
		{"signinIPHash", true},
		{"clientIP", true},
		{"signinIps", true},
		{"signinIPs", true},
		{"last_ip", true},
		{"ip4", true},
		{"ip6", true},
		{"ipv4", true},
		{"IPv4", true},
		{"IPv4s", true},
		{"IPv6s", true},
		{"IPv4Address", true},
		{"lastIpv4", true},
		{"ipv6Address", true},
		// 偽陽性にしてはいけないもの (実在する / しうるキー)。
		{"description", false},
		{"flipH", false},
		{"clipId", false},
		{"participants", false},
		{"recipientId", false},
		{"membership", false},
		{"relationship", false},
		{"municipality", false},
		{"encryption", false},
		{"manuscript", false},
		{"zip", false},
		{"emojis", false},
		{"script", false},
		{"multiple", false},
		{"HTTPSProxy", false},
		{"IPFS", false},
		{"IPsec", false},
		{"IPSec", true}, // fail-closed 側の既知の偽陽性 (allowlist 1 行で済む)
		{"VoIP", true},  // 同上
	} {
		assert.Equalf(t, c.want, looksLikeIPKey(c.key), "looksLikeIPKey(%q) / 正規化=%q", c.key, spaceCamelKey(c.key))
	}
}

type taggedField struct {
	File   string // repo 相対
	Struct string
	Field  string
	Key    string // JSON に出る名前
	// TypeRefs は型式に現れた名前付き型 (`<package>.<Type>`)。走査対象の外にある
	// struct を入れ子にした形を見つけるために持つ。
	TypeRefs []string
}

func (f taggedField) ref() string { return f.File + "#" + f.Struct + "." + f.Field }

// looksLikeIPKey はキーを語に割ってから `ip` 系を探す。
//
// **素朴な部分一致は使えない** (`description` / `flipH` / `clipId` を誤検出する)。
func looksLikeIPKey(key string) bool { return ipTokenRe.MatchString(spaceCamelKey(key)) }

// **`address` 系の alternative は要る。** `spaceCamelKey` が割るのは `IPAddress` の
// ように A が大文字の綴りだけで、`IPaddress` / `ipaddress` / `IPADDRESS` は 1 語に
// 潰れる。**これを「割るから要らない」と書いて一度落とし、敵対的レビューで実測された**
// (合成コーパス 12,113 件のうち 310 件がその形だった)。
var ipTokenRe = regexp.MustCompile(`(?:^| )ip(?:s|4|6|v4|v6|v4s|v6s|addr|address|addresses)?(?:$| )`)

// spaceCamelKey は camelCase / snake_case を空白区切りの小文字へ均す。
//
// 境界は 2 つある。**どちらも要る。**
//
//   - 小文字または数字の直後の大文字 (`lastIp` → `last Ip`)
//   - **大文字が 2 つ以上続いた後の、小文字が続く大文字** (`IPAddr` → `IP Addr`)。
//     「大文字のたびに割る」だと `lastIPs` が `last I Ps` になって Go の命名規約に
//     素直な名前だけが素通りし、逆に割らないと `IPAddr` が 1 語に潰れて素通りする。
//     **2 稿にわたって片側ずつ落とした** (実測で両方の回帰を確認した)。連なりが
//     1 文字のときに割らないのが要点で、これが `IPv4` / `IPs` を 1 語に保つ。
func spaceCamelKey(s string) string {
	runes := []rune(s)
	var b strings.Builder
	upperRun := 0
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prevLowerOrDigit := unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLowerOrDigit || (upperRun >= 2 && nextLower) {
				b.WriteRune(' ')
			}
		}
		if unicode.IsUpper(r) {
			upperRun++
		} else {
			upperRun = 0
		}
		b.WriteRune(r)
	}
	return strings.ToLower(strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(b.String()))
}

// parsedGoFile is one non-test Go file with the information the scanners need.
type parsedGoFile struct {
	Rel  string // repo 相対
	Dir  string // 絶対パス。package の単位
	Pkg  string
	File *ast.File
}

// marshalableFieldType reports whether `encoding/json` could put the field's
// type on the wire. 関数とチャネルは marshal できない (`json.Marshal` は
// `UnsupportedTypeError` を返す) ので、そこから型の参照を集めると DI 用の
// struct が軒並み汚れる (実測)。
func marshalableFieldType(expr ast.Expr) bool {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ArrayType:
			expr = e.Elt
		case *ast.FuncType, *ast.ChanType:
			return false
		default:
			return true
		}
	}
}

// importAliases maps a file's import aliases to the real package name
// (import path の末尾で代用する)。
func importAliases(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, im := range f.Imports {
		if im.Name == nil || im.Name.Name == "_" || im.Name.Name == "." {
			continue
		}
		path := strings.Trim(im.Path.Value, `"`)
		seg := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			seg = path[i+1:]
		}
		out[im.Name.Name] = seg
	}
	return out
}

// parseGoTree parses every non-test Go file under root. 同じ木を何度も舐めるので
// 結果を使い回す (`internal/` 全体を 4 回パースすると実測で 7 秒以上かかる)。
var (
	parseGoTreeMu    sync.Mutex
	parseGoTreeCache = map[string][]parsedGoFile{}
)

func parseGoTree(t *testing.T, root, repo string) []parsedGoFile {
	t.Helper()
	parseGoTreeMu.Lock()
	defer parseGoTreeMu.Unlock()
	if cached, ok := parseGoTreeCache[root+"\x00"+repo]; ok {
		return cached
	}
	var out []parsedGoFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// テストの固定資産は対象外 (Go ツールチェーンも `testdata` を無視する)。
			// コンパイルされない Go を置ける場所なので、parse させると無関係な理由で
			// このゲートが落ちる。
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(repo, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, parsedGoFile{
			Rel:  filepath.ToSlash(rel),
			Dir:  filepath.Dir(path),
			Pkg:  f.Name.Name,
			File: f,
		})
		return nil
	})
	require.NoError(t, err, "walk %s", root)
	parseGoTreeCache[root+"\x00"+repo] = out
	return out
}

// scanJSONTags は dir 以下の非テスト Go から、`encoding/json` が出すキーを集める。
//
// 拾う形は `encoding/json` の規則に合わせてある:
//
//   - タグの無い exported フィールドは**フィールド名**で出る
//   - `json:"-"` は出ない / 非公開フィールドは出ない
//   - タグの**無い**匿名埋め込みは中身が昇格する。埋め込まれた型が同じディレクトリで
//     宣言された struct なら、そちらを別途走査するのでキーは作らない
//   - タグの**ある**匿名埋め込みは昇格せず、**タグ名 1 つ**がキーになる
//   - struct でない名前付き型を埋め込むと**型名**がキーになる (`net.IP` など)
//
// **走査対象の外にある struct を埋め込む / 入れ子にする形は、ここでは中が見えない。**
// そちらは `TestPublicShapesDoNotReferenceIPBearingTypes` が型の参照として見る。
func scanJSONTags(t *testing.T, dir, repo string) []taggedField {
	t.Helper()
	files := parseGoTree(t, dir, repo)

	// 同じディレクトリ (= package) で宣言された struct 型。匿名埋め込みが昇格するか
	// どうかの判定に使う。`internal/api` のようにサブパッケージを含む木もあるので、
	// 走査した木の単位ではなく**ディレクトリ単位**で持つ。
	localStructs := map[string]bool{}
	for _, pf := range files {
		for _, d := range pf.File.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, isStruct := ts.Type.(*ast.StructType); isStruct {
					localStructs[pf.Dir+"\x00"+ts.Name.Name] = true
				}
			}
		}
	}

	var out []taggedField
	for _, pf := range files {
		eachStruct(pf.File, func(owner string, st *ast.StructType) {
			for _, fld := range st.Fields.List {
				key, hasTag := jsonTagFromLit(fld.Tag)
				if key == "-" {
					continue
				}
				var refs []string
				if marshalableFieldType(fld.Type) {
					refs = typeRefs(fld.Type, pf.Pkg, importAliases(pf.File))
				}
				if len(fld.Names) == 0 {
					name := embeddedTypeName(fld.Type)
					switch {
					case hasTag && key != "":
						// タグ付きの埋め込みは昇格せず、タグ名 1 つがキーになる。
					case localStructs[pf.Dir+"\x00"+name]:
						// 昇格する。埋め込まれた struct 自体を別途走査する。
						continue
					default:
						// struct でない名前付き型、または走査対象外の型。前者は型名が
						// キーになる。後者は中が見えないので、**型名で立てておく**
						// (見えないまま黙るより、名前で落ちるほうがよい)。
						key = name
					}
					if key == "" {
						continue
					}
					out = append(out, taggedField{
						File: pf.Rel, Struct: owner, Field: name, Key: key, TypeRefs: refs,
					})
					continue
				}
				for _, id := range fld.Names {
					if !id.IsExported() {
						continue
					}
					k := key
					if k == "" {
						k = id.Name
					}
					out = append(out, taggedField{
						File: pf.Rel, Struct: owner, Field: id.Name, Key: k, TypeRefs: refs,
					})
				}
			}
		})
	}
	return out
}

// eachStruct calls fn for every struct type literal in f, naming the enclosing
// declaration. 入れ子の匿名 struct も、それを囲む宣言の名前で報告する。
//
// **`ast.Inspect` で `TypeSpec` を見るたびに名前を上書きする形では駄目** — 関数の中で
// 組み立てる無名 struct が、直前に宣言された無関係な型の名前で報告される (実測で
// `renderer.go` の関数内 struct が `Renderer` のフィールドとして出ていた)。allowlist の
// キーはこの名前で作るので、誤った名前を指したまま運用することになる。
func eachStruct(f *ast.File, fn func(owner string, st *ast.StructType)) {
	visit := func(n ast.Node, owner string) {
		ast.Inspect(n, func(n ast.Node) bool {
			if st, ok := n.(*ast.StructType); ok {
				fn(owner, st)
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			visit(d.Type, "func "+d.Name.Name)
			if d.Body != nil {
				visit(d.Body, "func "+d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					visit(s.Type, s.Name.Name)
				case *ast.ValueSpec:
					name := "var"
					if len(s.Names) > 0 {
						name = "var " + s.Names[0].Name
					}
					visit(s, name)
				}
			}
		}
	}
}

// jsonTagFromLit returns the name part of the json struct tag and whether the
// tag was present at all.
func jsonTagFromLit(tag *ast.BasicLit) (string, bool) {
	if tag == nil {
		return "", false
	}
	// **backtick 以外のリテラルも剥がす。** `"json:\"ip\""` は合法な Go で gofmt も
	// 書き換えないが、backtick だけを削る実装ではタグとして読めず、フィールド名を
	// キーとして記録してしまう (実測)。
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		raw = strings.Trim(tag.Value, "`")
	}
	v, ok := reflect.StructTag(raw).Lookup("json")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(v, ",")
	return name, true
}

// embeddedTypeName returns the type name an anonymous field embeds, or "" when
// it cannot be named.
func embeddedTypeName(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.IndexExpr: // generic instantiation
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.SelectorExpr:
			return e.Sel.Name
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
}

// typeRefs collects the named types a field type mentions, as `<package>.<Type>`.
// 同じ package の型は selfPkg で修飾し、import の別名は実際の package 名へ直す。
//
// **別名を直さないと 1 行で穴が開く。** `import m ".../internal/model"` と書いて
// `*m.Signin` を持たせると、字面のままでは `m.Signin` になって照合から外れる
// (実測で素通りした)。package 名は import path の末尾で代用する — このリポジトリの
// `internal/**` は全てそうなっている。
func typeRefs(expr ast.Expr, selfPkg string, aliases map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	ast.Inspect(expr, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := e.X.(*ast.Ident); ok {
				name := pkg.Name
				if real, ok := aliases[name]; ok {
					name = real
				}
				add(name + "." + e.Sel.Name)
				return false
			}
		case *ast.Ident:
			// **非公開の型名も拾う。** `type mkAlias = model.Signin` のように
			// ローカルの別名を挟むと、exported だけを見る実装では参照が消える
			// (実測で素通りした)。組み込み型だけ除く。
			if !predeclaredIdents[e.Name] {
				add(selfPkg + "." + e.Name)
			}
		}
		return true
	})
	return out
}

// predeclaredIdents are Go's predeclared type names. 名前付き型ではないので
// 参照として数えない。
var predeclaredIdents = map[string]bool{
	"bool": true, "byte": true, "complex64": true, "complex128": true, "error": true,
	"float32": true, "float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true, "uint": true,
	"uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"any": true, "comparable": true,
}

// ipBearingTypes returns named types under root that put an IP on the wire,
// keyed as `<package>.<Type>`.
//
// **タグ無しの exported フィールドも数える。** `encoding/json` はそれをフィールド名で
// 出すので、拾わないと「タグを書き忘れた IP フィールドを持つ型」を参照する漏れが
// 素通りする (実測)。実データの IP 保持型は全てタグ付きなので、この枝は
// `ipbearingfixture` でしか固定できない。
//
// **匿名埋め込みも数える。** タグが無ければ中身が昇格するので、`type T struct{
// model.Signin }` は `ip` を出す。
//
// **推移的に追う。** `model.User` は自分では IP を持たないが `Avatar *model.DriveFile`
// を持ち、`json.Marshal` すると `avatar.requestIp` が出る (実測)。1 段だけ見る形だと、
// レスポンス struct に `*model.User` を 1 つ足す変更が素通りする。別名 (`type X = Y`) と
// 定義型 (`type X Y`) も同じものを指すので伝播させる。
func ipBearingTypes(t *testing.T, root, repo string) map[string]string {
	t.Helper()
	direct := map[string]string{}
	refs := map[string][]string{} // 型 -> その型が参照する型
	file := map[string]string{}

	for _, pf := range parseGoTree(t, root, repo) {
		al := importAliases(pf.File)
		for _, decl := range pf.File.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				self := pf.Pkg + "." + ts.Name.Name
				file[self] = pf.Rel
				if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
					// interface は JSON の形ではない。メソッドの引数と戻り値に
					// 型が現れるだけなので、伝播させると repository 一式が汚れる (実測)。
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					// `type X = Y` / `type X Y` は下敷きの型を指す。
					refs[self] = append(refs[self], typeRefs(ts.Type, pf.Pkg, al)...)
					continue
				}
				for _, fld := range st.Fields.List {
					key, hasTag := jsonTagFromLit(fld.Tag)
					if key == "-" {
						continue
					}
					if !marshalableFieldType(fld.Type) {
						continue
					}
					refs[self] = append(refs[self], typeRefs(fld.Type, pf.Pkg, al)...)
					if len(fld.Names) == 0 {
						// タグの無い匿名埋め込みは中身が昇格する。タグ付きは
						// タグ名 1 つがキーになる。
						if hasTag && looksLikeIPKey(key) {
							direct[self] = pf.Rel
						}
						continue
					}
					for _, id := range fld.Names {
						if !id.IsExported() {
							continue
						}
						k := key
						if k == "" {
							k = id.Name
						}
						if looksLikeIPKey(k) {
							direct[self] = pf.Rel
						}
					}
				}
			}
		}
	}

	out := map[string]string{}
	for k, v := range direct {
		out[k] = v
	}
	for changed := true; changed; {
		changed = false
		for self, rs := range refs {
			if _, done := out[self]; done {
				continue
			}
			for _, r := range rs {
				if _, bad := out[r]; bad {
					out[self] = file[self]
					changed = true
					break
				}
			}
		}
	}
	return out
}

// exportedStructNames lists the exported struct type names declared in dir.
func exportedStructNames(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, pf := range parseGoTree(t, dir, dir) {
		for _, decl := range pf.File.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				if _, isStruct := ts.Type.(*ast.StructType); isStruct {
					out = append(out, ts.Name.Name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// filesWithJSONTag lists repo-relative non-test Go files under dirs that contain
// a json struct tag. **テキストで見るのが要点** — AST 側と独立した truth にする。
func filesWithJSONTag(t *testing.T, repo string, dirs []string) []string {
	t.Helper()
	var out []string
	for _, d := range dirs {
		err := filepath.WalkDir(filepath.Join(repo, d), func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				if e.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if !strings.Contains(string(b), `json:"`) {
				return nil
			}
			rel, rerr := filepath.Rel(repo, path)
			if rerr != nil {
				return rerr
			}
			out = append(out, filepath.ToSlash(rel))
			return nil
		})
		require.NoError(t, err, "walk %s", d)
	}
	sort.Strings(out)
	return out
}

// scanCustomMarshalers lists types under dir that implement their own JSON or
// text marshaler, as `<file>#<Type>.<Method>`.
func scanCustomMarshalers(t *testing.T, dir, repo string) []string {
	t.Helper()
	var out []string
	for _, pf := range parseGoTree(t, dir, repo) {
		for _, decl := range pf.File.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			if fn.Name.Name != "MarshalJSON" && fn.Name.Name != "MarshalText" {
				continue
			}
			// レシーバは `T` / `*T` / `T[U]` / `pkg.T` のいずれか。
			// `embeddedTypeName` がそれを全部剥がす (generic を剥がさない実装だと
			// generic な型の marshaler が黙って一覧から消える。実測で素通りした)。
			out = append(out, pf.Rel+"#"+embeddedTypeName(fn.Recv.List[0].Type)+"."+fn.Name.Name)
		}
	}
	return out
}

// allJSONKeys returns every object key in a JSON document, at any depth.
func allJSONKeys(t *testing.T, b []byte) []string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(b, &v))
	seen := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			for k, child := range x {
				seen[k] = true
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(v)
	return sortedKeySet(seen)
}

func sortedKeySet(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
