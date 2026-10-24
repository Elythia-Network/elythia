package server

import (
	"reflect"
	"sort"
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

// handlerNilAllowed lists the handler fields that stay nil on a server built
// by newServerWithConfig, keyed by "<apiHandlers field>.<handler field>".
// The reason says on which path the field is actually nil.
// 今は空。#3817 の作業中に clips.noteRepo の未配線が見つかり、#3818 で配線した。
var handlerNilAllowed = map[string]string{}

// **組み立てたサーバーの handler に、nil の依存が残っていないこと (#3817)。**
//
// 依存を構築子の必須の引数にすると、渡し忘れはコンパイルで落ちる。ただ nil は
// 渡せる。以前の起動時の検査 (criticalWiring の表) と wiring-check の文字列の照合は、
// nil を渡す形も落としていたので、その代わりにここで見る。対象の handler の、nil に
// なりうるフィールド (ポインタ・interface・func・map・slice・chan) がすべて nil で
// ないことを確かめる。setter で入れるフィールドも、組み立て (setupRoutes) を全部
// 通った後なので同じ規則で見る。
//
// フィールドは unexported だが、同じ型の値を reflect で辿り IsNil を見るだけなので
// unsafe は要らない (IsNil は unexported なフィールドの値にも使える)。構造体の値の
// フィールド (sync.Mutex・singleflight.Group・userrelation.Repos など) の中までは
// 辿らない。
func TestBuiltHandlersHaveNoNilDependencies(t *testing.T) {
	srv := newServerWithConfig(t, func(*config.Config) {})

	hv := reflect.ValueOf(srv.builtHandlers)
	used := map[string]bool{}
	checked := map[string]bool{}
	for _, name := range requiredDepHandlers {
		f := hv.FieldByName(name)
		require.True(t, f.IsValid(), "apiHandlers に %s の欄が無い (rename した?)", name)
		require.Equal(t, reflect.Pointer, f.Kind(), "apiHandlers.%s がポインタでない", name)
		require.False(t, f.IsNil(), "apiHandlers.%s が nil", name)
		h := f.Elem()
		require.Equal(t, reflect.Struct, h.Kind(), "apiHandlers.%s が構造体を指していない", name)
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
			checked[key] = true
			if !isNilDep(h.Field(i)) {
				continue
			}
			if _, ok := handlerNilAllowed[key]; ok {
				used[key] = true
				continue
			}
			t.Errorf("%s が nil のまま組み立てが終わった。構築子か setter に nil を渡していないか確かめる。"+
				"nil でよいなら、実際にどの経路で nil になるかを handlerNilAllowed に書く", key)
		}
		assert.Positive(t, n, "apiHandlers.%s に nil になりうるフィールドが 1 つも無い (抽出が空振りしていないか)", name)
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

// requiredDepChecks names handler fields that #3817 turned into constructor
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
