package server

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// requiredDepHandlers are the apiHandlers fields whose handlers take their
// dependencies as required constructor arguments (#3817).
//
// i の handler は stream の節で作る値に依存するので、まだ setter で入れている
// (#3812 の段 (4))。それ以外の apiHandlers の欄 (wellknown・nodeinfo など) は
// #3817 の対象ではないので見ない。
var requiredDepHandlers = []string{
	"webhook", "notifications", "hashtags", "gallery", "blocking", "mute", "renoteMute",
	"drive", "federation", "flash", "following", "userList", "auth", "bubbleGame",
	"invite", "pages",
	"users", "channels", "antennas", "clips", "roles", "ap",
}

// handlerNilAllowed lists the fields that stay nil on a server built by
// newServerWithConfig, keyed by "<apiHandlers field>.<handler field>" or
// "<routeParts field>.<component field>". The reason says on which path the
// field is actually nil.
//
// #3817 の作業中に clips.noteRepo の未配線が見つかり、#3818 で配線した。
var handlerNilAllowed = map[string]string{
	// テストだけが SetCaptchaVerifierFactory で入れる (外部の HTTP を避けるため)。
	// setupRoutes は呼ばないので本番では常に nil で、buildCaptchaVerifier は
	// provider ごとの本物の verifier を組む。
	"admin.captchaVerifierFactory": "テスト専用の seam。setupRoutes は入れず、nil のとき本物の verifier を組む",
	// テストだけが SetClockForTest (export_test.go) で入れる。本番では常に nil で、
	// now() は time.Now を使う。
	"admin.clock": "テスト専用の seam。setupRoutes は入れず、nil のとき time.Now を使う",
}

// **組み立てたサーバーの handler と部品に、nil の依存が残っていないこと (#3817、#3823)。**
//
// 依存を構築子の必須の引数にすると、渡し忘れはコンパイルで落ちる。ただ nil は
// 渡せる。以前の起動時の検査 (criticalWiring の表) と wiring-check の文字列の照合は、
// nil を渡す形も落としていたので、その代わりにここで見る。対象の handler の、nil に
// なりうるフィールド (ポインタ・interface・func・map・slice・chan) がすべて nil で
// ないことを確かめる。setter で入れるフィールドも、組み立て (setupRoutes) を全部
// 通った後なので同じ規則で見る。
//
// 対象は、buildHandlers が作った handler のうち requiredDepHandlers に並べたもの
// (#3817) と、setupRoutes の本文で作る部品のうち routeParts に持たせたもの全部 (#3823)。
//
// フィールドは unexported だが、同じ型の値を reflect で辿り IsNil を見るだけなので
// unsafe は要らない (IsNil は unexported なフィールドの値にも使える)。構造体の値の
// フィールド (sync.Mutex・singleflight.Group・userrelation.Repos など) の中までは
// 辿らない。
func TestBuiltHandlersHaveNoNilDependencies(t *testing.T) {
	srv := newServerWithConfig(t, func(*config.Config) {})

	hv := reflect.ValueOf(srv.builtHandlers)
	c := nilDepCheck{used: map[string]bool{}, checked: map[string]bool{}}
	for _, name := range requiredDepHandlers {
		c.component(t, "apiHandlers", name, hv.FieldByName(name))
	}
	// routeParts の欄は全部見る。欄を足したら、そのまま検査の対象になる。
	pv := reflect.ValueOf(srv.builtRouteParts)
	require.Positive(t, pv.NumField(), "routeParts に欄が 1 つも無い")
	for i := range pv.NumField() {
		name := pv.Type().Field(i).Name
		// 許可リストの key は欄の名前から作るので、apiHandlers と同じ名前があると
		// どちらの項目か分からなくなる。
		require.False(t, hv.FieldByName(name).IsValid(),
			"routeParts.%s が apiHandlers の欄と同じ名前 (handlerNilAllowed の key が衝突する)", name)
		c.component(t, "routeParts", name, pv.Field(i))
	}
	used, checked := c.used, c.checked

	// 依存の中まで見る。以前の起動時の検査の述語が、依存のポインタだけでなく、その
	// 中のフィールドまで見ていたもの。
	for key, inner := range nestedDepChecks {
		comp, field, ok := strings.Cut(key, ".")
		require.True(t, ok, "nestedDepChecks の key %q が <部品>.<フィールド> でない", key)
		v := pv.FieldByName(comp)
		if !v.IsValid() {
			v = hv.FieldByName(comp)
		}
		require.True(t, v.IsValid(), "nestedDepChecks の %s の部品が無い", key)
		dep := v.Elem().FieldByName(field)
		require.True(t, dep.IsValid(), "nestedDepChecks の %s のフィールドが無い", key)
		require.Equal(t, reflect.Pointer, dep.Kind(), "%s がポインタでない", key)
		require.False(t, dep.IsNil(), "%s が nil", key)
		for _, name := range inner {
			f := dep.Elem().FieldByName(name)
			require.True(t, f.IsValid(), "%s に %s のフィールドが無い (rename した?)", key, name)
			assert.False(t, isNilDep(f), "%s.%s が nil のまま組み立てが終わった", key, name)
		}
	}

	// 抽出の下限: 置き換えた依存を名指しで要求する。書き方が変わって何も見なく
	// なっても緑になるのを防ぐ。
	for _, key := range requiredDepChecks {
		assert.True(t, checked[key], "%s を検査していない (フィールド名を変えた?)", key)
	}

	var unused []string
	for key := range handlerNilAllowed {
		if !used[key] {
			unused = append(unused, key)
		}
	}
	sort.Strings(unused)
	assert.Empty(t, unused, "handlerNilAllowed に、nil にならなかった項目がある (配線したなら消す)")
}

// nilDepCheck records which fields TestBuiltHandlersHaveNoNilDependencies
// checked and which handlerNilAllowed entries it used.
type nilDepCheck struct {
	used    map[string]bool
	checked map[string]bool
}

// component checks every nilable field of the struct f points to. container
// and name say where f came from ("apiHandlers" / "routeParts" and its field).
func (c nilDepCheck) component(t *testing.T, container, name string, f reflect.Value) {
	t.Helper()
	require.True(t, f.IsValid(), "%s に %s の欄が無い (rename した?)", container, name)
	require.Equal(t, reflect.Pointer, f.Kind(), "%s.%s がポインタでない", container, name)
	require.False(t, f.IsNil(), "%s.%s が nil", container, name)
	h := f.Elem()
	require.Equal(t, reflect.Struct, h.Kind(), "%s.%s が構造体を指していない", container, name)
	n := 0
	for i := range h.NumField() {
		sf := h.Type().Field(i)
		switch sf.Type.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		default:
			continue
		}
		n++
		key := name + "." + sf.Name
		c.checked[key] = true
		if !isNilDep(h.Field(i)) {
			continue
		}
		if _, ok := handlerNilAllowed[key]; ok {
			c.used[key] = true
			continue
		}
		t.Errorf("%s が nil のまま組み立てが終わった。構築子か setter に nil を渡していないか確かめる。"+
			"nil でよいなら、実際にどの経路で nil になるかを handlerNilAllowed に書く", key)
	}
	assert.Positive(t, n, "%s.%s に nil になりうるフィールドが 1 つも無い (抽出が空振りしていないか)", container, name)
}

// requiredDepChecks names fields that #3817 and #3823 turned into constructor
// arguments. TestBuiltHandlersHaveNoNilDependencies must check each of them.
var requiredDepChecks = []string{
	"webhook.rolePolicyProvider",
	"notifications.entityPacker", "notifications.noteHider", "notifications.moderatorChecker",
	"notifications.abuseReportStates", "notifications.abuseUnresolvedCount", "notifications.signupApplicationLookup",
	"hashtags.entityPacker", "hashtags.extras", "hashtags.moderators", "hashtags.meta",
	"gallery.entityPacker",
	"blocking.entityPacker", "blocking.extras", "blocking.packer",
	"mute.entityPacker", "mute.extras",
	"renoteMute.entityPacker", "renoteMute.extras",
	"drive.entityPacker", "drive.noteHider", "drive.background",
	"federation.entityPacker", "federation.packer",
	"flash.entityPacker",
	"following.entityPacker", "following.packer",
	"userList.entityPacker", "userList.rolePolicyProvider",
	"auth.entityPacker", "auth.detailExtras", "auth.background",
	"bubbleGame.entityPacker",
	"invite.entityPacker", "invite.rolePolicyProvider", "invite.moderatorChecker",
	"pages.entityPacker", "pages.driveFileRepo", "pages.userPacker",
	"users.noteHider", "users.entityPacker", "users.rolePolicyProvider", "users.metaRepo",
	"users.ugcVisibilityFn", "users.channelMutingRepo", "users.mutingRepo", "users.blockingRepo",
	"users.relationReader",
	"channels.noteHider", "channels.entityPacker", "channels.metaRepo", "channels.ugcVisibilityFn",
	"antennas.noteHider", "antennas.entityPacker", "antennas.metaRepo",
	"antennas.mutingRepo", "antennas.blockingRepo", "antennas.channelMutingRepo",
	"clips.noteHider", "clips.entityPacker", "clips.metaRepo", "clips.ugcVisibilityFn",
	"clips.mutingRepo", "clips.blockingRepo",
	"roles.noteHider", "roles.entityPacker", "roles.metaRepo", "roles.packer",
	"ap.noteHider", "ap.entityPacker", "ap.hostBlocker", "ap.softwareBlocker", "ap.detailExtras",
	// ここから #3823 (setupRoutes の本文で作る部品)。
	"admin.background", "admin.entityPacker", "admin.detailExtras", "admin.listPacker",
	"admin.suspensionOriginRepo", "admin.userTokenInvalidator", "admin.userStreamRevoker",
	"admin.ipLookupAudit", "admin.emojiImageFetcher",
	"signup.background", "signup.entityPacker", "signup.userPolicies",
	"signin.background", "signin.metaRepo", "signin.totpReplayGuard",
	"resetPassword.background", "resetPassword.metaRepo",
	"oauth.authInvalidator",
	"emojiApplication.policies", "emojiApplication.resets",
	"abuseCreated.background", "abuseCreated.entityPacker",
}

// nestedDepChecks names dependencies whose own fields must be non-nil too,
// keyed like handlerNilAllowed.
//
// iplookuplog.Service は repo と idGen のどちらかが nil だと、Record が黙って
// 何もしない。以前の起動時の検査の述語 (Wired) は両方を見ていたので、ここでも
// 両方を見る (#3823)。
var nestedDepChecks = map[string][]string{
	"admin.ipLookupAudit": {"repo", "idGen"},
}

// isNilDep reports whether v holds no dependency: v is nil, or v is an
// interface holding a nil pointer. 型付きの nil のポインタを interface の引数に
// 渡すと、interface としては nil でないが、呼べば panic するか未配線と同じ扱いになる。
func isNilDep(v reflect.Value) bool {
	if v.IsNil() {
		return true
	}
	if v.Kind() == reflect.Interface {
		e := v.Elem()
		switch e.Kind() {
		case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
			return e.IsNil()
		}
	}
	return false
}
