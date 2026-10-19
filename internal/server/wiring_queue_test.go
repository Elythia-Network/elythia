package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

// allQueueHandlerProcs fills every field with a distinct zero-value processor.
// The handlers are only compared, never called.
func allQueueHandlerProcs() queueHandlerProcs {
	return queueHandlerProcs{
		webPush:                 &processors.WebPushProcessor{},
		webhook:                 &processors.WebhookProcessor{},
		dataExport:              &processors.ExportProcessor{},
		dataImport:              &processors.ImportProcessor{},
		emojiImport:             &processors.ImportCustomEmojisProcessor{},
		resendQuoteRequests:     &processors.ResendQuoteRequestsProcessor{},
		deliver:                 &processors.DeliverProcessor{},
		inbox:                   &processors.InboxProcessor{},
		cleanRemoteNotes:        &processors.CleanRemoteNotesProcessor{},
		orphanUserCleanup:       &processors.OrphanUserCleanupProcessor{},
		orphanAttachmentCleanup: &processors.OrphanAttachmentCleanupProcessor{},
		checkExpiredMutings:     &processors.CheckExpiredMutingsProcessor{},
		clean:                   &processors.CleanProcessor{},
		chunkedUploadGC:         &processors.ChunkedUploadGCProcessor{},
		objectStorage:           &processors.ObjectStorageProcessor{},
		reactionFlush:           &processors.ReactionFlushProcessor{},
		deleteAccount:           &processors.DeleteAccountProcessor{},
		unfollow:                &processors.UnfollowProcessor{},
		follow:                  &processors.FollowProcessor{},
		block:                   &processors.BlockProcessor{},
		unblock:                 &processors.UnblockProcessor{},
		chart:                   &processors.ChartProcessor{},
		instanceRefresh:         &processors.InstanceRefreshProcessor{},
		retentionAggregate:      &processors.RetentionAggregateProcessor{},
		postScheduledNote:       &processors.PostScheduledNoteProcessor{},
		checkModeratorsActivity: &processors.CheckModeratorsActivityProcessor{},
	}
}

// declaredTaskTypes reads every TaskType* constant declared in the non-test,
// git-tracked .go files of internal/queue, keyed by value.
//
// **パッケージ全体を読む。** tasks.go だけを読むと、別のファイルに足した
// TaskType の定数が一覧の検査から黙って漏れる。**値が文字列リテラルでない
// 定数は落とす。** 式や iota で書かれた値をここでは評価しないので、拾えない
// まま緑になるのを防ぐ。
func declaredTaskTypes(t *testing.T) map[string]string {
	t.Helper()
	// go test はパッケージのディレクトリで走るので、出力は ../queue/<file> の形。
	out, err := exec.Command("git", "ls-files", "-z", "--", "../queue").Output()
	require.NoError(t, err, "git ls-files ../queue")
	fset := token.NewFileSet()
	got := map[string]string{}
	files := 0
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" || filepath.ToSlash(filepath.Dir(name)) != "../queue" ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, "TaskType") {
						continue
					}
					require.Less(t, i, len(vs.Values), "%s: %s の値が書かれていない (iota などは読めない)", name, id.Name)
					lit, ok := vs.Values[i].(*ast.BasicLit)
					require.True(t, ok && lit.Kind == token.STRING,
						"%s: %s の値が文字列リテラルでない。この検査は式を評価しないので、リテラルで書くこと", name, id.Name)
					v, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					got[v] = id.Name
				}
			}
		}
	}
	require.NotZero(t, files, "git ls-files が internal/queue の .go を 1 つも返さない")
	return got
}

// **組み込みの job 種別は、すべて 1 回ずつ一覧に載っていること (#3690)。**
//
// 一覧に載せ忘れた種別は handler が無いまま積まれ、worker が処理できずに失敗し続ける。
// 同じ種別を 2 回載せると、driver の map で後の handler が前のものを黙って
// 上書きする。どちらもビルドは通るので、tasks.go の定数と突き合わせる。
func TestBuiltinQueueHandlersCoverEveryTaskType(t *testing.T) {
	declared := declaredTaskTypes(t)
	// 抽出が空振りしたら落とす (書式が変わっても緑にしない)。
	require.Contains(t, declared, queue.TaskTypeDeliver, "tasks.go から TaskType の定数を読めていない")
	require.Contains(t, declared, queue.TaskTypeUnblock, "tasks.go から TaskType の定数を読めていない")

	listed := map[string]int{}
	for _, h := range builtinQueueHandlers(allQueueHandlerProcs()) {
		listed[h.taskType]++
		assert.NotNil(t, h.handle, "%s の handler が nil", h.taskType)
	}
	for typ, n := range listed {
		assert.Equal(t, 1, n, "%s が一覧に %d 回ある。後の handler が前のものを上書きする", typ, n)
		_, ok := declared[typ]
		assert.True(t, ok, "%s は tasks.go の TaskType の定数に無い", typ)
	}
	for typ, name := range declared {
		assert.Contains(t, listed, typ, "queue.%s (%q) を受ける handler が一覧に無い", name, typ)
	}
}

// **job 種別ごとに、受ける processor のメソッドが決まっていること (#3690)。**
//
// 型が同じ HandlerFunc なので、取り違えてもビルドは通る (webhook の user と
// system、chart の tick / resync / clean、follow と unfollow など)。
func TestBuiltinQueueHandlersBindTheExpectedMethods(t *testing.T) {
	want := map[string]string{
		queue.TaskTypeWebPush:                 "(*WebPushProcessor).Handle",
		queue.TaskTypeUserWebhook:             "(*WebhookProcessor).HandleUser",
		queue.TaskTypeSystemWebhook:           "(*WebhookProcessor).HandleSystem",
		queue.TaskTypeExport:                  "(*ExportProcessor).Handle",
		queue.TaskTypeImport:                  "(*ImportProcessor).Handle",
		queue.TaskTypeImportCustomEmojis:      "(*ImportCustomEmojisProcessor).Handle",
		queue.TaskTypeResendQuoteRequests:     "(*ResendQuoteRequestsProcessor).Handle",
		queue.TaskTypeDeliver:                 "(*DeliverProcessor).Handle",
		queue.TaskTypeInbox:                   "(*InboxProcessor).Handle",
		queue.TaskTypeCleanRemoteNotes:        "(*CleanRemoteNotesProcessor).Handle",
		queue.TaskTypeOrphanUserCleanup:       "(*OrphanUserCleanupProcessor).Handle",
		queue.TaskTypeOrphanAttachmentCleanup: "(*OrphanAttachmentCleanupProcessor).Handle",
		queue.TaskTypeCheckExpiredMutings:     "(*CheckExpiredMutingsProcessor).Handle",
		queue.TaskTypeClean:                   "(*CleanProcessor).Handle",
		queue.TaskTypeChunkedUploadGC:         "(*ChunkedUploadGCProcessor).Handle",
		queue.TaskTypeObjectStorageDeleteFile: "(*ObjectStorageProcessor).HandleDeleteFile",
		queue.TaskTypeCleanRemoteFiles:        "(*ObjectStorageProcessor).HandleCleanRemoteFiles",
		queue.TaskTypeReactionFlush:           "(*ReactionFlushProcessor).Handle",
		queue.TaskTypeDeleteAccount:           "(*DeleteAccountProcessor).Handle",
		queue.TaskTypeUnfollow:                "(*UnfollowProcessor).Handle",
		queue.TaskTypeFollow:                  "(*FollowProcessor).Handle",
		queue.TaskTypeBlock:                   "(*BlockProcessor).Handle",
		queue.TaskTypeUnblock:                 "(*UnblockProcessor).Handle",
		queue.TaskTypeChartTick:               "(*ChartProcessor).HandleTick",
		queue.TaskTypeChartResync:             "(*ChartProcessor).HandleResync",
		queue.TaskTypeChartClean:              "(*ChartProcessor).HandleClean",
		queue.TaskTypeInstanceRefresh:         "(*InstanceRefreshProcessor).Handle",
		queue.TaskTypeRetentionAggregate:      "(*RetentionAggregateProcessor).Handle",
		queue.TaskTypePostScheduledNote:       "(*PostScheduledNoteProcessor).Handle",
		queue.TaskTypeCheckModeratorsActivity: "(*CheckModeratorsActivityProcessor).Handle",
	}
	const pkg = "github.com/elythia-network/elythia/internal/queue/processors."
	got := map[string]string{}
	for _, h := range builtinQueueHandlers(allQueueHandlerProcs()) {
		// method value の関数名は "<pkg>.(*T).Method-fm" になる。
		name := runtime.FuncForPC(reflect.ValueOf(h.handle).Pointer()).Name()
		got[h.taskType] = strings.TrimSuffix(strings.TrimPrefix(name, pkg), "-fm")
	}
	assert.Equal(t, want, got)
}

// registerQueueHandlers は一覧をそのまま queueServer へ登録する。
func TestRegisterQueueHandlersRegistersTheList(t *testing.T) {
	d := newFakeQueueDriver()
	s := &Server{queueServer: queue.NewServer(d)}
	hs := builtinQueueHandlers(allQueueHandlerProcs())
	s.registerQueueHandlers(hs)

	require.Len(t, d.server.handlers, len(hs))
	for _, h := range hs {
		got, ok := d.server.handlers[h.taskType]
		require.True(t, ok, "%s が登録されていない", h.taskType)
		assert.Equal(t, reflect.ValueOf(h.handle).Pointer(), reflect.ValueOf(got).Pointer(), h.taskType)
	}
}

// setupRoutesDecl finds (*Server).setupRoutes among the git-tracked non-test
// files of this package.
//
// **router.go を名指ししない (#3672)。** setupRoutes は分割の途中なので、
// 宣言しているファイルをパッケージ全体から探す。見つからなければ落とす。
func setupRoutesDecl(t *testing.T) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	var found *ast.FuncDecl
	for _, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, src.name, src.src, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "setupRoutes" || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "Server" {
				require.Nil(t, found, "(*Server).setupRoutes が 2 つある")
				found = fd
			}
		}
	}
	require.NotNil(t, found, "internal/server に (*Server).setupRoutes が無い (rename した?)")
	return fset, found
}

// **組み込みの handler は setupRoutes の本文の直下で 1 回だけ登録すること (#3690)。**
//
// setupRoutes が registerQueueHandlers を呼ばなくなっても、上のテストは一覧
// しか見ないので緑のまま。**呼び出しは本文の直下の文に限る。** 関数リテラルの
// 中 (呼ばれないかもしれない)、`go` 文 (Start と競合する)、`if` の中 (条件次第で
// 登録されない、欄の名前が別の変数に隠れうる) では、組み込みの handler が登録
// されない経路ができる。逆に s.queueServer.Handle を直接呼ぶ行が戻ると、job 種別と
// processor の対応が一覧の外にもできる。こちらは関数リテラルの中も数える。
//
// **登録は setupRoutes のどの return よりも前に置く。** プラグインの準備に
// 失敗すると setupRoutes は途中で return する。元の 30 か所の登録はすべてその
// return より前にあったので、後ろへ置くとその経路で組み込みの handler が
// 1 つも登録されなくなる。プラグインの登録 (setupPlugins) も return の後なので、
// 組み込みが先という元の順もこれで保たれる。
func TestSetupRoutesRegistersBuiltinQueueHandlersOnce(t *testing.T) {
	fset, fd := setupRoutesDecl(t)
	body := fd.Body

	isRegister := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "registerQueueHandlers"
	}

	// 本文の直下の式文としての呼び出し。
	var topLevel []int
	for _, st := range body.List {
		es, ok := st.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok || !isRegister(call) {
			continue
		}
		topLevel = append(topLevel, fset.Position(call.Pos()).Line)
		require.Len(t, call.Args, 1)
		inner, ok := call.Args[0].(*ast.CallExpr)
		require.True(t, ok, "registerQueueHandlers の引数が builtinQueueHandlers(...) でない")
		id, ok := inner.Fun.(*ast.Ident)
		require.True(t, ok && id.Name == "builtinQueueHandlers", "registerQueueHandlers の引数が builtinQueueHandlers(...) でない")
	}

	// 関数リテラルの中も含めた、すべての呼び出し。
	all, direct := 0, 0
	var pluginLines []int
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isRegister(call) {
			all++
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "setupPlugins":
				pluginLines = append(pluginLines, fset.Position(call.Pos()).Line)
			case "Handle":
				if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "queueServer" {
					direct++
				}
			}
		}
		return true
	})
	// setupRoutes 自身の return だけを集める (関数リテラルの return は別の関数のもの)。
	var returnLines []int
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returnLines = append(returnLines, fset.Position(n.Pos()).Line)
		}
		return true
	})
	require.Len(t, topLevel, 1, "setupRoutes の本文の直下で s.registerQueueHandlers(builtinQueueHandlers(...)) を 1 回だけ呼ぶ")
	require.Equal(t, 1, all, "registerQueueHandlers を本文の直下以外 (関数リテラル・go 文・if の中など) でも呼んでいる")
	require.Len(t, pluginLines, 1, "setupRoutes に setupPlugins の呼び出しが無い (rename した?)")
	// 抽出が空振りしたら落とす。途中の return を本当に無くしたなら、この検査ごと見直す。
	require.NotEmpty(t, returnLines, "setupRoutes の return を 1 つも拾えていない")
	for _, l := range returnLines {
		assert.Less(t, topLevel[0], l, "組み込みの handler の登録が %d 行目の return より後にある", l)
	}
	assert.Less(t, topLevel[0], pluginLines[0], "組み込みの handler の登録は setupPlugins より前に置く")
	assert.Zero(t, direct, "setupRoutes で s.queueServer.Handle を直接呼んでいる。builtinQueueHandlers の一覧に足すこと")
}

// **登録する processor は、setter を呼んだのと同じ変数であること (#3690)。**
//
// queueHandlerProcs の欄に構築の式 (`deliver: processors.NewDeliverProcessor(apClient)`)
// を書くと、ビルドも上のテストも通るのに、署名鍵・配送の関門・replay guard などの
// setter を通っていない processor が job を受ける。なので欄の値は setupRoutes の
// 本文で `:=` で 1 回だけ定義し、どこでも `=` で入れ替えていない名前に限る
// (入れ替えると、setter を呼んだ値と登録する値が分かれうる)。欄の書き忘れ
// (nil の processor) も落とす。
func TestSetupRoutesPassesSetterConfiguredProcessors(t *testing.T) {
	_, fd := setupRoutesDecl(t)

	// setupRoutes の本文の直下の `:=` で定義した名前と、その回数。
	defined := map[string]int{}
	for _, st := range fd.Body.List {
		as, ok := st.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE {
			continue
		}
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok && id.Name != "_" {
				defined[id.Name]++
			}
		}
	}
	require.NotEmpty(t, defined, "setupRoutes の := を 1 つも拾えていない")

	// 本文のどこか (関数リテラルの中を含む) で `=` や `+=` などで書き換えている名前。
	reassigned := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok == token.DEFINE {
			return true
		}
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok {
				reassigned[id.Name] = true
			}
		}
		return true
	})

	var lits []*ast.CompositeLit
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if cl, ok := n.(*ast.CompositeLit); ok {
			if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "queueHandlerProcs" {
				lits = append(lits, cl)
			}
		}
		return true
	})
	require.Len(t, lits, 1, "setupRoutes に queueHandlerProcs{...} が 1 つだけあること")

	keys := map[string]bool{}
	for _, el := range lits[0].Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		require.True(t, ok, "queueHandlerProcs{...} は欄名を付けて書く")
		key := kv.Key.(*ast.Ident).Name
		keys[key] = true
		val, ok := kv.Value.(*ast.Ident)
		if !assert.True(t, ok, "queueHandlerProcs の %s に変数でない式を渡している。setter を呼んだ変数を渡すこと", key) {
			continue
		}
		assert.Equal(t, 1, defined[val.Name],
			"queueHandlerProcs の %s に渡す %s は、setupRoutes の本文で := で 1 回だけ定義した名前であること", key, val.Name)
		assert.False(t, reassigned[val.Name],
			"queueHandlerProcs の %s に渡す %s を、setupRoutes の中で = で書き換えている", key, val.Name)
	}
	typ := reflect.TypeOf(queueHandlerProcs{})
	for i := 0; i < typ.NumField(); i++ {
		assert.True(t, keys[typ.Field(i).Name], "queueHandlerProcs の %s を渡していない (nil の processor が job を受ける)", typ.Field(i).Name)
	}
}

// queueHandleCallers lists the functions of this package allowed to call
// Handle on the queue server, with the path by which each actually registers.
//
// **許すのは、組み込みの一覧の登録とプラグインの登録だけ。** ほかの関数から
// 登録すると、setupRoutes から呼ばれたときに組み込みの handler を一覧の外で
// 上書きできる。理由の欄には、その関数が実際にどの経路で登録するかを書く。
var queueHandleCallers = map[string]string{
	"(*Server).registerQueueHandlers":       "builtinQueueHandlers の一覧を setupRoutes から 1 回登録する (#3690)",
	"(*Server).setupPlugins":                "Peered なプラグインの peer の job を queue ロールで登録する (plugin_wiring.go、#2819)",
	"(*Server).registerPluginNotifications": "プラグインの通知の job を登録する (plugin_notifications.go、#3469)",
	"(*pluginJobs).Handle":                  "プラグインが Jobs で宣言した job を、プラグインの接頭辞付きの種別で登録する (plugin_wiring.go)",
}

// **queue の handler を登録する関数を増やさないこと (#3690)。**
//
// パッケージの tracked な非テストのファイルすべてで、queue の server の Handle
// (`s.queueServer.Handle(...)` と、プラグインの `j.server.Handle(...)`) を呼んでいる
// 関数を集め、上の一覧と突き合わせる。一覧にあって呼んでいない項目も落とす
// (使われない許可が残ると、後で別の登録を黙って許す)。
func TestOnlyAllowedFunctionsRegisterQueueHandlers(t *testing.T) {
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, src.name, src.src, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				recv := fd.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = "(*" + id.Name + ")." + name
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Handle" {
					return true
				}
				if x, ok := sel.X.(*ast.SelectorExpr); ok && (x.Sel.Name == "queueServer" || x.Sel.Name == "server") {
					found[name] = true
					assert.Contains(t, queueHandleCallers, name,
						"%s が queue の handler を登録している (%s)。組み込みの job は builtinQueueHandlers の一覧に足すこと",
						name, fset.Position(call.Pos()))
				}
				return true
			})
		}
	}
	for name := range queueHandleCallers {
		assert.True(t, found[name], "queueHandleCallers の %s は queue の handler を登録していない。一覧から消すこと", name)
	}
}
