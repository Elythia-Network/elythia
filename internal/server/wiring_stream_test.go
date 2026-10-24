package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	corebubbleversus "github.com/elythia-network/elythia/internal/core/bubbleversus"
	"github.com/elythia-network/elythia/internal/core/cache"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/stream/channels"
)

// allStreamChannelDeps fills every field with a non-nil zero value. The
// factories are only compared and looked up, never called.
func allStreamChannelDeps() streamChannelDeps {
	return streamChannelDeps{
		roles:        &corerole.Service{},
		serverStats:  &stream.ServerStatsPublisher{},
		queueStats:   &stream.QueueStatsPublisher{},
		reversi:      &corereversi.Service{},
		bubbleVersus: &corebubbleversus.Service{},
		chatRoom:     &channels.ChatRoomFactory{},
		chat:         &corechat.Service{},
	}
}

// wantStreamChannels is every built-in channel name and whether it requires
// an authenticated connection (upstream requireCredential).
//
// #3692 の前に setupRoutes が登録していた 22 行 (queueStats は queueInspector の
// 有無で 2 通り) と同じ。起動したサーバーの registry の中身 (名前・factory・
// credentialed の組) を前後で突き合わせて確かめた。
var wantStreamChannels = map[string]bool{
	"homeTimeline":      true,
	"localTimeline":     false,
	"globalTimeline":    false,
	"hybridTimeline":    true,
	"notifications":     true,
	"main":              true,
	"drive":             true,
	"hashtag":           false,
	"antenna":           true,
	"channel":           false,
	"userList":          false,
	"clip":              false,
	"roleTimeline":      false,
	"admin":             true,
	"serverStats":       false,
	"queueStats":        false,
	"reversiGame":       false,
	"reversi":           true,
	"bubbleVersus":      true,
	"bubbleVersusMatch": true,
	"chatRoom":          true,
	"chatUser":          true,
}

// **組み込みのチャンネルは、すべて 1 回ずつ一覧に載り、credentialed が決まった
// 値であること (#3692)。**
//
// 一覧から落ちたチャンネルは、クライアントが connect しても factory が無く黙って
// 無視される。同じ名前を 2 回載せると、registry の map で後の factory が前のものを
// 上書きし、Register は前の RegisterCredentialed の印を消さない。credentialed を
// 取り違えると、匿名の接続が main や chat を購読できる (#1944)。どれもビルドは通る。
func TestBuiltinStreamChannelsListEveryChannelOnce(t *testing.T) {
	for _, withQueueStats := range []bool{true, false} {
		d := allStreamChannelDeps()
		if !withQueueStats {
			d.queueStats = nil
		}
		got := map[string]bool{}
		seen := map[string]int{}
		for _, c := range builtinStreamChannels(d) {
			seen[c.name]++
			got[c.name] = c.credentialed
			assert.NotNil(t, c.factory, "%s の factory が nil", c.name)
		}
		for name, n := range seen {
			assert.Equal(t, 1, n, "%s が一覧に %d 回ある。後の factory が前のものを上書きする", name, n)
		}
		assert.Equal(t, wantStreamChannels, got, "queueStats の publisher あり=%v", withQueueStats)
	}
}

// closureNumberRe matches the closure number the compiler appends to the name
// of a function literal ("NewServerStatsFactory.func1").
var closureNumberRe = regexp.MustCompile(`\.func[0-9]+$`)

// streamFactoryName returns the name of the function f runs, relative to
// internal/stream/channels: "NewMain", "(*AdminFactory).New" for a method
// value, "NewServerStatsFactory.func" for a closure a constructor returns.
//
// **閉包の番号は落とす。** コンストラクタの中に関数リテラルを 1 つ足しただけで
// func1 が func2 になり、配線は同じなのに落ちる。どのコンストラクタの閉包かは
// 番号を落としても残るので、取り違えは引き続き落ちる。
func streamFactoryName(f stream.ChannelFactory) string {
	const pkg = "github.com/elythia-network/elythia/internal/stream/channels."
	// method value の関数名は "<pkg>.(*T).Method-fm" になる。
	name := runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
	name = strings.TrimSuffix(strings.TrimPrefix(name, pkg), "-fm")
	return closureNumberRe.ReplaceAllString(name, ".func")
}

// **チャンネル名ごとに、factory の関数が決まっていること (#3692)。**
//
// 型が同じ stream.ChannelFactory なので、取り違えてもビルドは通る (home と hybrid、
// local と global、reversi と reversiGame、bubbleVersus と bubbleVersusMatch など)。
// queueStats は queue の inspector の有無 (publisher が nil かどうか) で factory が
// 変わるので、両方を見る。
func TestBuiltinStreamChannelsBindTheExpectedFactories(t *testing.T) {
	want := map[string]string{
		"homeTimeline":      "NewHomeTimeline",
		"localTimeline":     "NewLocalTimeline",
		"globalTimeline":    "NewGlobalTimeline",
		"hybridTimeline":    "NewHybridTimeline",
		"notifications":     "NewNotifications",
		"main":              "NewMain",
		"drive":             "NewDrive",
		"hashtag":           "NewHashtag",
		"antenna":           "(*AntennaFactory).New",
		"channel":           "NewChannelTimeline",
		"userList":          "(*UserListFactory).New",
		"clip":              "(*ClipFactory).New",
		"roleTimeline":      "(*RoleTimelineFactory).New",
		"admin":             "(*AdminFactory).New",
		"serverStats":       "NewServerStatsFactory.func",
		"queueStats":        "NewQueueStatsFactory.func",
		"reversiGame":       "(*ReversiGameFactory).New",
		"reversi":           "NewReversi",
		"bubbleVersus":      "NewBubbleVersus",
		"bubbleVersusMatch": "(*BubbleVersusMatchFactory).New",
		"chatRoom":          "(*ChatRoomFactory).New",
		"chatUser":          "(*ChatUserFactory).New",
	}
	names := func(d streamChannelDeps) map[string]string {
		got := map[string]string{}
		for _, c := range builtinStreamChannels(d) {
			got[c.name] = streamFactoryName(c.factory)
		}
		return got
	}
	assert.Equal(t, want, names(allStreamChannelDeps()))

	// queueInspector が無い起動では、publisher を使わない fallback の factory。
	d := allStreamChannelDeps()
	d.queueStats = nil
	want["queueStats"] = "NewQueueStats"
	assert.Equal(t, want, names(d))
}

// registerStreamChannels は一覧をそのまま registry へ登録し、credentialed の印を付ける。
func TestRegisterStreamChannelsRegistersTheList(t *testing.T) {
	r := stream.NewRegistry()
	cs := builtinStreamChannels(allStreamChannelDeps())
	registerStreamChannels(r, cs)

	for _, c := range cs {
		got := r.Lookup(c.name)
		require.NotNil(t, got, "%s が登録されていない", c.name)
		assert.Equal(t, reflect.ValueOf(c.factory).Pointer(), reflect.ValueOf(got).Pointer(), c.name)
		assert.Equal(t, c.credentialed, r.RequiresCredential(c.name), "%s の credentialed", c.name)
	}
	assert.Nil(t, r.Lookup("unknownChannel"))
}

// **起動したサーバーの manager が引く registry に、一覧のチャンネルがすべて
// 載っていること (#3692)。**
//
// registry は buildStream で作り、manager はその参照を持つ。登録は setupRoutes の
// 別名 streamRegistry に対して行うので、別名を別の registry (`stream.NewRegistry()`
// など) に書き換えると、ビルドも構文木のテストも通ったまま、manager の registry は
// 空になり、どのチャンネルへの connect も黙って無視される。なので実際にサーバーを
// 作り、dispatcher が引く manager の registry を見る。queue の inspector がある場合と
// 無い場合 (queueStats の factory が変わる) の両方を見る。
func TestSetupRoutesRegistersChannelsOnTheManagersRegistry(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB

	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	redisClients := &cache.RedisClients{
		Default: redisClient, Pubsub: redisClient, JobQueue: redisClient,
		Timelines: redisClient, Reactions: redisClient,
	}
	redisPort, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	redisOptions := config.RedisOptions{Host: mr.Host(), Port: redisPort}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		MediaProxySecret: []byte("test-secret"),
		Redis:            redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
	}

	for _, withInspector := range []bool{true, false} {
		var hooks []func(*Server)
		if !withInspector {
			hooks = append(hooks, func(s *Server) { s.queueInspector = nil })
		}
		srv, err := newServer(cfg, db, redisClients, nil, noopStorage, hooks...)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		require.Equal(t, withInspector, srv.queueInspector != nil, "queueInspector の有無を作り分けられていない")

		d := allStreamChannelDeps()
		if !withInspector {
			d.queueStats = nil
		}
		want := builtinStreamChannels(d)
		r := srv.streamManager.Registry()
		require.NotNil(t, r, "manager が registry を持っていない")
		for _, c := range want {
			got := r.Lookup(c.name)
			if !assert.NotNil(t, got, "queueInspector=%v: manager の registry に %s が無い", withInspector, c.name) {
				continue
			}
			assert.Equal(t, streamFactoryName(c.factory), streamFactoryName(got),
				"queueInspector=%v: %s の factory", withInspector, c.name)
			assert.Equal(t, c.credentialed, r.RequiresCredential(c.name),
				"queueInspector=%v: %s の credentialed", withInspector, c.name)
		}
	}
}

// streamChannelEntries returns the entries of the list builtinStreamChannels
// returns, keyed by channel name, from the git-tracked sources of this
// package.
func streamChannelEntries(t *testing.T) (*token.FileSet, map[string]*ast.CompositeLit) {
	t.Helper()
	fset := token.NewFileSet()
	entries := map[string]*ast.CompositeLit{}
	found := 0
	for _, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, src.name, src.src, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != "builtinStreamChannels" {
				continue
			}
			found++
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, res := range ret.Results {
					list, ok := res.(*ast.CompositeLit)
					require.True(t, ok, "builtinStreamChannels が一覧のリテラルを返していない")
					for _, el := range list.Elts {
						item, ok := el.(*ast.CompositeLit)
						require.True(t, ok && len(item.Elts) == 3, "%s: 項目が {名前, factory, credentialed} の形でない", fset.Position(el.Pos()))
						lit, ok := item.Elts[0].(*ast.BasicLit)
						require.True(t, ok && lit.Kind == token.STRING, "%s: 項目の名前が文字列リテラルでない", fset.Position(item.Pos()))
						name, err := strconv.Unquote(lit.Value)
						require.NoError(t, err)
						entries[name] = item
					}
				}
				return true
			})
		}
	}
	require.Equal(t, 1, found, "internal/server に builtinStreamChannels が 1 つだけあること (rename した?)")
	require.NotEmpty(t, entries, "builtinStreamChannels の項目を 1 つも読めない")
	return fset, entries
}

// **chatRoom には、起動時の検査が見るのと同じ factory を登録すること (#3692)。**
//
// 起動時の検査 (streamChannels.chatRoom.service) は setupRoutes の chatRoomFactory の
// HasService を見る。一覧で `channels.NewChatRoomFactory(d.chat).New` と書き直すと、
// 関数名も型も同じなのでほかのテストは緑のまま、検査と登録が別の factory になる。
func TestBuiltinStreamChannelsRegisterTheCheckedChatRoomFactory(t *testing.T) {
	_, entries := streamChannelEntries(t)
	item, ok := entries["chatRoom"]
	require.True(t, ok, "一覧に chatRoom が無い")
	sel, ok := item.Elts[1].(*ast.SelectorExpr)
	require.True(t, ok && sel.Sel.Name == "New", "chatRoom の factory が d.chatRoom.New でない")
	inner, ok := sel.X.(*ast.SelectorExpr)
	require.True(t, ok && inner.Sel.Name == "chatRoom", "chatRoom の factory が d.chatRoom.New でない")
	_, ok = inner.X.(*ast.Ident)
	assert.True(t, ok, "chatRoom の factory が d.chatRoom.New でない")
}

// **setupRoutes が chatRoom に渡す factory は、起動時の検査が見るのと同じ変数で
// あること (#3692)。**
//
// 上のテストは一覧の側だけを見る。setupRoutes で `otherFactory :=
// channels.NewChatRoomFactory(chatService)` を作って `chatRoom: otherFactory` と渡すと、
// 下のテスト (欄の値が := で 1 回だけ定義した変数か) も通ったまま、検査
// (streamChannels.chatRoom.service の HasService) と登録が別の factory になる。
func TestSetupRoutesPassesTheCheckedChatRoomFactory(t *testing.T) {
	_, fd := setupRoutesDecl(t)

	// 起動時の検査の項目 {"streamChannels.chatRoom.service", X.HasService(), …} の X。
	var checked []string
	var depsChatRoom []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "streamChannelDeps" {
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "chatRoom" {
					v, ok := kv.Value.(*ast.Ident)
					require.True(t, ok, "streamChannelDeps の chatRoom に変数でない式を渡している")
					depsChatRoom = append(depsChatRoom, v.Name)
				}
			}
			return true
		}
		if len(cl.Elts) < 2 {
			return true
		}
		lit, ok := cl.Elts[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || lit.Value != `"streamChannels.chatRoom.service"` {
			return true
		}
		call, ok := cl.Elts[1].(*ast.CallExpr)
		require.True(t, ok, "streamChannels.chatRoom.service の検査が X.HasService() の形でない")
		sel, ok := call.Fun.(*ast.SelectorExpr)
		require.True(t, ok && sel.Sel.Name == "HasService", "streamChannels.chatRoom.service の検査が X.HasService() の形でない")
		x, ok := sel.X.(*ast.Ident)
		require.True(t, ok, "streamChannels.chatRoom.service の検査が X.HasService() の形でない")
		checked = append(checked, x.Name)
		return true
	})
	require.Len(t, checked, 1, "setupRoutes に起動時の検査 streamChannels.chatRoom.service が 1 つだけあること (rename した?)")
	require.Len(t, depsChatRoom, 1, "setupRoutes の streamChannelDeps に chatRoom が 1 つだけあること")
	assert.Equal(t, checked[0], depsChatRoom[0],
		"chatRoom に登録する factory (%s) が、起動時の検査が見る factory (%s) と別の変数", depsChatRoom[0], checked[0])
}

// unregisteredChannelConstructors lists the exported constructors of
// internal/stream/channels that the list does not use, with the path by which
// each is actually used instead.
var unregisteredChannelConstructors = map[string]string{
	"NewServerStats": "serverStats は常に publisher を持つ factory (NewServerStatsFactory) で登録する。publisher の無い版は channels のテストだけが使う",
}

// **channels のチャンネルの実装は、どれも一覧から使われていること (#3692)。**
//
// チャンネルを足して一覧に載せ忘れると、実装もテストもあるのにクライアントからは
// 購読できない。internal/stream/channels の tracked な非テストのファイルから、
// stream.Channel / stream.ChannelFactory / *…Factory を返す exported な New… を集め、
// builtinStreamChannels の本文が channels.<名前> として参照していることを確かめる。
// 使わない許可の一覧には、使われ始めた項目を落とす検査も付ける。
func TestBuiltinStreamChannelsUseEveryChannelConstructor(t *testing.T) {
	// go test はパッケージのディレクトリで走るので、出力は ../stream/channels/<file> の形。
	out, err := exec.Command("git", "ls-files", "-z", "--", "../stream/channels").Output()
	require.NoError(t, err, "git ls-files ../stream/channels")
	fset := token.NewFileSet()
	ctors := map[string]bool{}
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" || filepath.ToSlash(filepath.Dir(name)) != "../stream/channels" ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "New") || !fd.Name.IsExported() ||
				fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
				continue
			}
			switch r := fd.Type.Results.List[0].Type.(type) {
			case *ast.SelectorExpr:
				if x, ok := r.X.(*ast.Ident); ok && x.Name == "stream" && (r.Sel.Name == "Channel" || r.Sel.Name == "ChannelFactory") {
					ctors[fd.Name.Name] = true
				}
			case *ast.StarExpr:
				if id, ok := r.X.(*ast.Ident); ok && strings.HasSuffix(id.Name, "Factory") {
					ctors[fd.Name.Name] = true
				}
			}
		}
	}
	// 抽出が空振りしたら落とす (書式が変わっても緑にしない)。
	require.Contains(t, ctors, "NewHomeTimeline", "channels から New… を読めていない")
	require.Contains(t, ctors, "NewChatRoomFactory", "channels から *…Factory を返す New… を読めていない")

	used := map[string]bool{}
	found := 0
	for _, src := range packageSources(t) {
		f, err := parser.ParseFile(token.NewFileSet(), src.name, src.src, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != "builtinStreamChannels" {
				continue
			}
			found++
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "channels" {
						used[sel.Sel.Name] = true
					}
				}
				return true
			})
		}
	}
	require.Equal(t, 1, found, "internal/server に builtinStreamChannels が 1 つだけあること")
	// chatRoom の factory は setupRoutes が作って渡す (起動時の検査と同じ値を登録するため)。
	used["NewChatRoomFactory"] = true

	for name := range ctors {
		if _, ok := unregisteredChannelConstructors[name]; ok {
			assert.False(t, used[name], "unregisteredChannelConstructors の %s は一覧が使っている。許可から消すこと", name)
			continue
		}
		assert.True(t, used[name], "channels.%s を builtinStreamChannels が使っていない。チャンネルを一覧に足すこと", name)
	}
	for name := range unregisteredChannelConstructors {
		assert.True(t, ctors[name], "unregisteredChannelConstructors の %s は channels に無い。許可から消すこと", name)
	}
}

// **チャンネルは setupRoutes の本文の直下で 1 回だけ、/streaming を生やす前に
// 登録すること (#3692)。**
//
// registry は factory を map に入れるだけで、引くのは接続を受けたときなので、登録の
// 位置は接続を受け始める前ならどこでもよい。ただし、関数リテラルの中 (呼ばれない
// かもしれない)、`go` 文 (接続と競合する)、`if` の中 (条件次第で登録されない) では
// 登録されない経路ができるので、本文の直下に限る。setupRoutes の途中の return より
// 前、/streaming の handler (streaming.NewHandler) を作るより前に置く。逆に
// Register / RegisterCredentialed を直接呼ぶ行が戻ると、チャンネル名と factory の
// 対応が一覧の外にもできる。こちらは関数リテラルの中も数える。
func TestSetupRoutesRegistersStreamChannelsOnce(t *testing.T) {
	fset, fd := setupRoutesDecl(t)
	body := fd.Body

	isRegister := func(call *ast.CallExpr) bool {
		id, ok := call.Fun.(*ast.Ident)
		return ok && id.Name == "registerStreamChannels"
	}

	var topLevel []int
	handlerLine := 0
	for _, st := range body.List {
		switch st := st.(type) {
		case *ast.ExprStmt:
			call, ok := st.X.(*ast.CallExpr)
			if !ok || !isRegister(call) {
				continue
			}
			topLevel = append(topLevel, fset.Position(call.Pos()).Line)
			require.Len(t, call.Args, 2)
			reg, ok := call.Args[0].(*ast.Ident)
			require.True(t, ok && reg.Name == "streamRegistry", "registerStreamChannels の第 1 引数が streamRegistry でない")
			inner, ok := call.Args[1].(*ast.CallExpr)
			require.True(t, ok, "registerStreamChannels の第 2 引数が builtinStreamChannels(...) でない")
			id, ok := inner.Fun.(*ast.Ident)
			require.True(t, ok && id.Name == "builtinStreamChannels", "registerStreamChannels の第 2 引数が builtinStreamChannels(...) でない")
		case *ast.AssignStmt:
			for _, rhs := range st.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok {
					continue
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewHandler" {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "streaming" {
						require.Zero(t, handlerLine, "streaming.NewHandler が 2 回ある")
						handlerLine = fset.Position(call.Pos()).Line
					}
				}
			}
		}
	}

	all, direct := 0, 0
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isRegister(call) {
			all++
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Register" || sel.Sel.Name == "RegisterCredentialed") {
			direct++
		}
		return true
	})
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
	require.Len(t, topLevel, 1, "setupRoutes の本文の直下で registerStreamChannels(streamRegistry, builtinStreamChannels(...)) を 1 回だけ呼ぶ")
	require.Equal(t, 1, all, "registerStreamChannels を本文の直下以外 (関数リテラル・go 文・if の中など) でも呼んでいる")
	require.NotZero(t, handlerLine, "setupRoutes の本文の直下に streaming.NewHandler(...) の代入が無い (rename した?)")
	assert.Less(t, topLevel[0], handlerLine, "チャンネルの登録は /streaming の handler を作るより前に置く")
	require.NotEmpty(t, returnLines, "setupRoutes の return を 1 つも拾えていない")
	for _, l := range returnLines {
		assert.Less(t, topLevel[0], l, "チャンネルの登録が %d 行目の return より後にある", l)
	}
	assert.Zero(t, direct, "setupRoutes で Register / RegisterCredentialed を直接呼んでいる。builtinStreamChannels の一覧に足すこと")
}

// **factory に渡す service と publisher は、setter を呼んだのと同じ変数であること (#3692)。**
//
// streamChannelDeps の欄に構築の式 (`reversi: corereversi.NewService(...)`) を書くと、
// ビルドも上のテストも通るのに、setter (アイコンの URL、対戦の記録、chat の配送・
// 権限・ブロックなど) を通っていない service でチャンネルが動く。なので欄の値は
// setupRoutes の本文の直下で 1 回だけ定義し (`:=` か、値付きの `var`)、どこでも `=` で
// 入れ替えていない名前に限る。
//
// **例外は queueStats の 1 つだけ。** publisher は queue の inspector があるときだけ
// 作るので、`var queueStatsPub *stream.QueueStatsPublisher` で nil に宣言し、本文の
// 直下の `if s.queueInspector != nil {` の中で 1 回だけ代入する形に限って許す。欄の
// 書き忘れ (nil の service) も落とす。
func TestSetupRoutesPassesSetterConfiguredStreamDeps(t *testing.T) {
	_, fd := setupRoutesDecl(t)

	defined := map[string]int{}     // 本文の直下の := と、値付きの var
	declaredNil := map[string]int{} // 本文の直下の、値の無い var
	for _, st := range fd.Body.List {
		switch st := st.(type) {
		case *ast.AssignStmt:
			if st.Tok != token.DEFINE {
				continue
			}
			for _, l := range st.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name != "_" {
					defined[id.Name]++
				}
			}
		case *ast.DeclStmt:
			gd, ok := st.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for _, id := range vs.Names {
					if len(vs.Values) == 0 {
						declaredNil[id.Name]++
					} else {
						defined[id.Name]++
					}
				}
			}
		}
	}
	require.NotEmpty(t, defined, "setupRoutes の := を 1 つも拾えていない")

	// 本文のどこか (関数リテラルの中を含む) で `=` などで書き換えている名前と、その文。
	reassigned := map[string][]*ast.AssignStmt{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok == token.DEFINE {
			return true
		}
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok {
				reassigned[id.Name] = append(reassigned[id.Name], as)
			}
		}
		return true
	})

	// 本文の直下の `if s.queueInspector != nil {` の本文の直下の代入。
	inspectorAssigns := map[*ast.AssignStmt]bool{}
	for _, st := range fd.Body.List {
		is, ok := st.(*ast.IfStmt)
		if !ok || is.Init != nil {
			continue
		}
		be, ok := is.Cond.(*ast.BinaryExpr)
		if !ok || be.Op != token.NEQ {
			continue
		}
		sel, ok := be.X.(*ast.SelectorExpr)
		nilID, okNil := be.Y.(*ast.Ident)
		if !ok || !okNil || nilID.Name != "nil" || sel.Sel.Name != "queueInspector" {
			continue
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "s" {
			continue
		}
		for _, inner := range is.Body.List {
			if as, ok := inner.(*ast.AssignStmt); ok && as.Tok == token.ASSIGN {
				inspectorAssigns[as] = true
			}
		}
	}

	var lits []*ast.CompositeLit
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if cl, ok := n.(*ast.CompositeLit); ok {
			if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "streamChannelDeps" {
				lits = append(lits, cl)
			}
		}
		return true
	})
	require.Len(t, lits, 1, "setupRoutes に streamChannelDeps{...} が 1 つだけあること")

	keys := map[string]bool{}
	for _, el := range lits[0].Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		require.True(t, ok, "streamChannelDeps{...} は欄名を付けて書く")
		key := kv.Key.(*ast.Ident).Name
		keys[key] = true
		val, ok := kv.Value.(*ast.Ident)
		if !assert.True(t, ok, "streamChannelDeps の %s に変数でない式を渡している。setter を呼んだ変数を渡すこと", key) {
			continue
		}
		if key == "queueStats" {
			as := reassigned[val.Name]
			assert.Equal(t, 1, declaredNil[val.Name],
				"streamChannelDeps の queueStats に渡す %s は、setupRoutes の本文の直下で値の無い var で 1 回だけ宣言すること", val.Name)
			assert.Zero(t, defined[val.Name], "streamChannelDeps の queueStats に渡す %s を := や値付きの var でも定義している", val.Name)
			if assert.Len(t, as, 1, "streamChannelDeps の queueStats に渡す %s への代入は 1 回だけ", val.Name) {
				assert.True(t, inspectorAssigns[as[0]],
					"streamChannelDeps の queueStats に渡す %s への代入は、本文の直下の if s.queueInspector != nil { の直下に置く", val.Name)
			}
			continue
		}
		assert.Equal(t, 1, defined[val.Name],
			"streamChannelDeps の %s に渡す %s は、setupRoutes の本文の直下で := (か値付きの var) で 1 回だけ定義した名前であること", key, val.Name)
		assert.Zero(t, declaredNil[val.Name], "streamChannelDeps の %s に渡す %s を値の無い var で宣言している", key, val.Name)
		assert.Empty(t, reassigned[val.Name],
			"streamChannelDeps の %s に渡す %s を、setupRoutes の中で = で書き換えている", key, val.Name)
	}
	typ := reflect.TypeOf(streamChannelDeps{})
	for i := 0; i < typ.NumField(); i++ {
		assert.True(t, keys[typ.Field(i).Name], "streamChannelDeps の %s を渡していない (nil の依存でチャンネルが動く)", typ.Field(i).Name)
	}
}

// streamRegisterCallers lists the functions of this package allowed to call
// Register / RegisterCredentialed on a stream registry, with the path by which
// each actually registers.
var streamRegisterCallers = map[string]string{
	"registerStreamChannels": "builtinStreamChannels の一覧を setupRoutes から 1 回登録する (#3692)",
}

// **stream のチャンネルを登録する関数を増やさないこと (#3692)。**
//
// パッケージの tracked な非テストのファイルすべてで、Register / RegisterCredentialed を
// 引数 2 つで呼んでいる関数を集め、上の一覧と突き合わせる。import した package の
// 関数と、引数の数が違う Register (queue の metrics の `metrics.Register(registry)`)
// は stream の registry のメソッドではないので数えない。
// 一覧にあって呼んでいない項目も落とす (使われない許可が残ると、後で別の登録を
// 黙って許す)。
func TestOnlyAllowedFunctionsRegisterStreamChannels(t *testing.T) {
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, src.name, src.src, 0)
		require.NoError(t, err)
		imports := map[string]bool{}
		for _, im := range f.Imports {
			path, err := strconv.Unquote(im.Path.Value)
			require.NoError(t, err)
			name := path[strings.LastIndex(path, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = true
		}
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
				if !ok || (sel.Sel.Name != "Register" && sel.Sel.Name != "RegisterCredentialed") {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); ok && imports[x.Name] {
					return true
				}
				// 型は引かないので、引数の数で registry のメソッド (名前と factory の 2 つ) に
				// 絞る。queue の metrics の Register(registry) は 1 つなので数えない。
				if len(call.Args) != 2 {
					return true
				}
				found[name] = true
				assert.Contains(t, streamRegisterCallers, name,
					"%s が stream のチャンネルを登録している (%s)。組み込みのチャンネルは builtinStreamChannels の一覧に足すこと",
					name, fset.Position(call.Pos()))
				return true
			})
		}
	}
	for name := range streamRegisterCallers {
		assert.True(t, found[name], "streamRegisterCallers の %s はチャンネルを登録していない。一覧から消すこと", name)
	}
}
