package channels

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultNoteFilter(t *testing.T) {
	f := defaultNoteFilter()
	assert.True(t, f.WithRenotes)
	assert.False(t, f.WithReplies)
	assert.False(t, f.WithFiles)
}

func TestParseNoteFilter_Empty(t *testing.T) {
	f := parseNoteFilter(nil)
	assert.True(t, f.WithRenotes)
}

func TestParseNoteFilter_Override(t *testing.T) {
	f := parseNoteFilter(json.RawMessage(`{"withRenotes":false,"withReplies":true,"withFiles":true}`))
	assert.False(t, f.WithRenotes)
	assert.True(t, f.WithReplies)
	assert.True(t, f.WithFiles)
}

func TestShouldEmit_PureRenoteFiltered(t *testing.T) {
	f := noteFilter{WithRenotes: false, WithReplies: false, WithFiles: false}
	// 純リノート: text=nil, renoteId!=nil, fileIds=[]
	payload := []byte(`{"renoteId":"r1","fileIds":[]}`)
	assert.False(t, f.shouldEmit(payload, nil, ""))
}

func TestShouldEmit_QuoteRenoteAllowed(t *testing.T) {
	f := noteFilter{WithRenotes: false, WithReplies: false, WithFiles: false}
	// 引用リノート: text!=nil, renoteId!=nil → 通過
	payload := []byte(`{"text":"hello","renoteId":"r1"}`)
	assert.True(t, f.shouldEmit(payload, nil, ""))
}

func TestShouldEmit_RenoteWithFilesAllowed(t *testing.T) {
	f := noteFilter{WithRenotes: false, WithReplies: false, WithFiles: false}
	// ファイル添付付きリノート: text=nil, renoteId!=nil, fileIds非空 → 純リノートではない
	payload := []byte(`{"renoteId":"r1","fileIds":["f1"]}`)
	assert.True(t, f.shouldEmit(payload, nil, ""))
}

// #1888: cw / poll / reply を伴う renote は quote なので withRenotes=false でも通過する
// (SQL pureRenoteCondSQL と cache 経路の挙動を一致させる)。
func TestShouldEmit_RenoteWithCWAllowed(t *testing.T) {
	f := noteFilter{WithRenotes: false, WithReplies: false, WithFiles: false}
	assert.True(t, f.shouldEmit([]byte(`{"renoteId":"r1","cw":"warn"}`), nil, ""))
}

func TestShouldEmit_RenoteWithPollAllowed(t *testing.T) {
	f := noteFilter{WithRenotes: false, WithReplies: false, WithFiles: false}
	assert.True(t, f.shouldEmit([]byte(`{"renoteId":"r1","poll":{"choices":[]}}`), nil, ""))
}

func TestShouldEmit_RenoteWithReplyAllowed(t *testing.T) {
	f := noteFilter{WithRenotes: false, WithReplies: false, WithFiles: false}
	assert.True(t, f.shouldEmit([]byte(`{"renoteId":"r1","replyId":"p1"}`), nil, ""))
}

// #1063: shouldEmit は reply blanket-drop を持たない (upstream Misskey TS の
// home/hybrid/local channel に揃った semantics で、reply 表示可否は
// replyShouldEmit が channel ごとに判定する)。WithReplies フィールドは
// connect param 互換のために残っているが、shouldEmit からは参照しない。
// 旧テスト `TestShouldEmit_ReplyFiltered` (`WithReplies=false → drop`) /
// `TestShouldEmit_ReplyAllowed` (`WithReplies=true → pass`) は drift 挙動の
// assertion だったので、`WithReplies=false` でも reply が pass-through する
// ことだけ assert すれば不変条件として十分。`WithReplies=true` 側は shouldEmit
// から見ると同じコードパスを通るので別 case にしない。
//
// 将来 shouldEmit が reply gate を再導入したら本テストが失敗する想定で、
// reply 判定の責務が `replyShouldEmit` に集約されている前提を守る regression
// guard として機能する。
func TestShouldEmit_ReplyPassesIndependentOfWithReplies(t *testing.T) {
	f := noteFilter{WithRenotes: true, WithReplies: false, WithFiles: false}
	payload := []byte(`{"text":"reply","replyId":"p1"}`)
	assert.True(t, f.shouldEmit(payload, nil, ""),
		"shouldEmit must not gate reply based on WithReplies (#1063); reply decisions belong to replyShouldEmit")
}

// #1063: replyShouldEmit の 3 escape hatch と followee snapshot gate を覆う。
// upstream `home-timeline.ts` / `hybrid-timeline.ts` / `local-timeline.ts` /
// `global-timeline.ts` の reply 判定を Go で再実装したものなので、ここで
// drift regression を吸収する。
func TestReplyShouldEmit_NonReplyAlwaysPasses(t *testing.T) {
	payload := []byte(`{"userId":"author","text":"hello"}`)
	for _, mode := range []replyGateMode{replyGateHome, replyGateLocal, replyGateGlobal} {
		assert.True(t, replyShouldEmit(payload, "viewer", nil, false, mode))
	}
}

func TestReplyShouldEmit_GlobalNeverFilters(t *testing.T) {
	// reply embed が無くても (= drop 候補のはず) globalTimeline は通す。
	payload := []byte(`{"userId":"author","replyId":"p1","text":"r"}`)
	assert.True(t, replyShouldEmit(payload, "viewer", nil, false, replyGateGlobal))
}

func TestReplyShouldEmit_IsMeEscapeHatch(t *testing.T) {
	// 自分の reply は snapshot 無しでも常に pass (#1047 → #1055 → #1063 の本丸)。
	payload := []byte(`{"userId":"viewer","replyId":"p1","reply":{"userId":"someone","visibility":"public"}}`)
	assert.True(t, replyShouldEmit(payload, "viewer", nil, false, replyGateHome))
	assert.True(t, replyShouldEmit(payload, "viewer", nil, false, replyGateLocal))
}

func TestReplyShouldEmit_ReplyToMeEscapeHatch(t *testing.T) {
	// 自分宛 reply は author を follow していなくても pass。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"viewer","visibility":"public"}}`)
	assert.True(t, replyShouldEmit(payload, "viewer", nil, false, replyGateHome))
	assert.True(t, replyShouldEmit(payload, "viewer", nil, false, replyGateLocal))
}

func TestReplyShouldEmit_SelfThreadEscapeHatch(t *testing.T) {
	// author が自分自身に返信した self-thread は snapshot 無しでも pass。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"author","visibility":"public"}}`)
	assert.True(t, replyShouldEmit(payload, "viewer", nil, false, replyGateHome))
	assert.True(t, replyShouldEmit(payload, "viewer", nil, false, replyGateLocal))
}

func TestReplyShouldEmit_HomeDropsForeignReplyWithoutFolloweeWithReplies(t *testing.T) {
	// 他人 → 他人 reply (3 escape hatch どれも該当しない) は default で drop。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	snap := map[string]bool{"author": false}
	assert.False(t, replyShouldEmit(payload, "viewer", snap, false, replyGateHome))
}

func TestReplyShouldEmit_HomeAllowsForeignReplyWhenFolloweeWithReplies(t *testing.T) {
	// follow している author の reply は followee.withReplies=true なら pass。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	snap := map[string]bool{"author": true}
	assert.True(t, replyShouldEmit(payload, "viewer", snap, false, replyGateHome))
}

func TestReplyShouldEmit_HomeFollowersVisibilityNeedsTargetFollow(t *testing.T) {
	// followee.withReplies=true でも reply 先の visibility=followers で、
	// reply.userId を自分が follow していない & 自分宛でもないなら drop。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"followers"}}`)
	snap := map[string]bool{"author": true}
	assert.False(t, replyShouldEmit(payload, "viewer", snap, false, replyGateHome))

	// 同条件で target を follow していれば pass。
	snap2 := map[string]bool{"author": true, "target": false}
	assert.True(t, replyShouldEmit(payload, "viewer", snap2, false, replyGateHome))
}

func TestReplyShouldEmit_HomeIgnoresConnectParamWithReplies(t *testing.T) {
	// upstream home-timeline には withReplies connect param が無い。
	// paramWithReplies=true でも snapshot が空なら drop される。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	assert.False(t, replyShouldEmit(payload, "viewer", nil, true, replyGateHome))
}

func TestReplyShouldEmit_LocalAnonymousPassesAllReplies(t *testing.T) {
	// upstream local-timeline は anonymous viewer に対して reply gate を
	// かけない (`if (note.reply && this.user && ...)` の this.user が false)。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	assert.True(t, replyShouldEmit(payload, "", nil, false, replyGateLocal))
}

// #1063 follow-up: hybridTimeline は upstream で `requireCredential=true` なので
// 「anonymous hybrid」状態は本来発生しないが、mk-go は legacy 互換で anonymous
// viewer にも hybrid 購読を許している。snap=nil + selfThread 以外で全 reply
// drop すると旧 mk-go (= `WithReplies=true → 全 pass`) より厳しくなる方向の
// リグレッションになるので、anonymous local と同じく pass-through に揃える。
func TestReplyShouldEmit_HybridAnonymousPassesAllReplies(t *testing.T) {
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	assert.True(t, replyShouldEmit(payload, "", nil, false, replyGateHybrid))
	// followers visibility の reply も同様に pass する。anonymous なので
	// followers-visibility check を強行しても snap が無く必ず drop してしまうため。
	payloadFollowers := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"followers"}}`)
	assert.True(t, replyShouldEmit(payloadFollowers, "", nil, false, replyGateHybrid))
}

func TestReplyShouldEmit_LocalConnectParamWithRepliesPasses(t *testing.T) {
	// authenticated viewer & withReplies=true connect param で reply pass。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	assert.True(t, replyShouldEmit(payload, "viewer", nil, true, replyGateLocal))
}

func TestReplyShouldEmit_HybridConnectParamWithRepliesPasses(t *testing.T) {
	// upstream hybrid-timeline は followee.withReplies OR param.withReplies。
	// snapshot が空 (= follow していない) でも param.withReplies=true なら pass。
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	assert.True(t, replyShouldEmit(payload, "viewer", nil, true, replyGateHybrid))
}

// TestReplyShouldEmit_MentionedViewerEscapesReplyFilter guards #1195:
// viewer が note.mentions に含まれている場合、3 default escape hatch に
// 該当しなくても reply gate を pass する (mk-go 独自 escape、上流 TS は
// この escape を意図的に持たない)。
func TestReplyShouldEmit_MentionedViewerEscapesReplyFilter(t *testing.T) {
	// 他人 → 他人 reply (isMe / replyToMe / selfThread どれも false) で、
	// viewer が note.mentions に含まれる payload。
	payload := []byte(`{"userId":"author","replyId":"p1","mentions":["viewer"],"reply":{"userId":"target","visibility":"public"}}`)

	// snapshot 空 (= author を follow していても withReplies=false)。upstream TS
	// 互換 path なら drop されるが、mention escape で home / hybrid / local
	// すべての mode で pass する。
	snap := map[string]bool{"author": false}
	assert.True(t, replyShouldEmit(payload, "viewer", snap, false, replyGateHome),
		"home: mentioned viewer should bypass reply gate")
	assert.True(t, replyShouldEmit(payload, "viewer", snap, false, replyGateHybrid),
		"hybrid: mentioned viewer should bypass reply gate")
	assert.True(t, replyShouldEmit(payload, "viewer", snap, false, replyGateLocal),
		"local: mentioned viewer should bypass reply gate")
}

// TestReplyShouldEmit_MentionEscapeRequiresAuthenticatedViewer: anonymous
// viewer (= viewerID="") は note.mentions に空文字 ID が混入しても escape
// しない (空文字 viewer に対する誤誘発防止)。全 reply gate mode で対称に
// pin する: home は snapshot 経由の default drop、local/hybrid は anonymous
// 早期 return での pass-through、いずれも mention escape が誤発火しないこと
// を確認する。
func TestReplyShouldEmit_MentionEscapeRequiresAuthenticatedViewer(t *testing.T) {
	// mentions に "" が含まれる悪意ある / 破損 payload。
	payload := []byte(`{"userId":"author","replyId":"p1","mentions":[""],"reply":{"userId":"target","visibility":"public"}}`)
	snap := map[string]bool{"author": false}

	// home: anonymous + snapshot 有りでも default drop path に乗る。mention
	// escape は viewerID 空で発火しないので drop が維持される。
	assert.False(t, replyShouldEmit(payload, "", snap, false, replyGateHome),
		"home: anonymous viewer must not be escaped by empty-string mention")

	// local / hybrid: anonymous は anyway pass-through (`viewerID == ""` の
	// 早期 return)。mention escape の有無に関係なく true を返す挙動が、
	// 空文字 mention の混入で乱れないことを pin。
	assert.True(t, replyShouldEmit(payload, "", nil, false, replyGateLocal),
		"local: anonymous viewer pass-through must not depend on mention escape")
	assert.True(t, replyShouldEmit(payload, "", nil, false, replyGateHybrid),
		"hybrid: anonymous viewer pass-through must not depend on mention escape")
}

// TestReplyShouldEmit_MentionEscapeRespectsFollowersVisibility: mention
// escape は basic reply gate を pass させるが、followers-visibility 経路
// 自体は別 gate なのでそちらの judgment は upstream 互換のまま (= mention
// escape は basic gate 専用)。今後 followers-visibility にも mention
// escape を広げるなら別 issue で扱う。
func TestReplyShouldEmit_MentionEscapeDoesNotOverrideFollowersVisibility(t *testing.T) {
	// followee.withReplies=true、reply.visibility=followers、viewer は
	// target を follow していない & 自分宛でもない & mentions に含まれる。
	// followers-visibility branch は mention を見ないので drop が保持される。
	payload := []byte(`{"userId":"author","replyId":"p1","mentions":["viewer"],"reply":{"userId":"target","visibility":"followers"}}`)
	snap := map[string]bool{"author": true} // followee.withReplies=true
	assert.False(t, replyShouldEmit(payload, "viewer", snap, false, replyGateHome),
		"followers-visibility branch should not be bypassed by mention escape (mk-go #1195 scope)")
}

func TestReplyShouldEmit_MissingReplyEmbedIsConservative(t *testing.T) {
	// reply embed が無い場合 (= reply.userId 参照できない) は 3 escape hatch の
	// うち isMe しか判定できない。conservative に drop して fanout / DB の
	// fallback (= リロード時に表示) に倒す。
	payload := []byte(`{"userId":"author","replyId":"p1"}`)
	assert.False(t, replyShouldEmit(payload, "viewer", nil, false, replyGateHome))
	// isMe escape hatch は機能する。
	payload2 := []byte(`{"userId":"viewer","replyId":"p1"}`)
	assert.True(t, replyShouldEmit(payload2, "viewer", nil, false, replyGateHome))
}

func TestShouldEmit_WithFilesNoFiles(t *testing.T) {
	f := noteFilter{WithRenotes: true, WithReplies: true, WithFiles: true}
	payload := []byte(`{"text":"hello","fileIds":[]}`)
	assert.False(t, f.shouldEmit(payload, nil, ""))
}

func TestShouldEmit_WithFilesHasFiles(t *testing.T) {
	f := noteFilter{WithRenotes: true, WithReplies: true, WithFiles: true}
	payload := []byte(`{"text":"hello","fileIds":["f1"]}`)
	assert.True(t, f.shouldEmit(payload, nil, ""))
}

func TestShouldEmit_InvalidJSON(t *testing.T) {
	f := noteFilter{WithRenotes: false}
	// パース失敗時はそのまま送信
	assert.True(t, f.shouldEmit([]byte(`{invalid`), nil, ""))
}

func TestShouldEmit_NormalNote(t *testing.T) {
	f := noteFilter{WithRenotes: true, WithReplies: false, WithFiles: false}
	payload := []byte(`{"text":"hello"}`)
	assert.True(t, f.shouldEmit(payload, nil, ""))
}

// #787: hardMutedWords による per-publish filter (streaming path)。
func TestShouldEmit_HardMute_TextHit(t *testing.T) {
	f := noteFilter{WithRenotes: true, WithReplies: true, WithFiles: false}
	payload := []byte(`{"userId":"author","text":"this contains politics"}`)
	rules := []byte(`[["politics"]]`)
	assert.False(t, f.shouldEmit(payload, rules, "viewer"))
}

func TestShouldEmit_HardMute_CWHit(t *testing.T) {
	f := noteFilter{WithRenotes: true, WithReplies: true, WithFiles: false}
	payload := []byte(`{"userId":"author","text":"clean body","cw":"spoiler ahead"}`)
	rules := []byte(`[["spoiler"]]`)
	assert.False(t, f.shouldEmit(payload, rules, "viewer"))
}

func TestShouldEmit_HardMute_ViewerOwnNoteExempt(t *testing.T) {
	f := noteFilter{WithRenotes: true, WithReplies: true, WithFiles: false}
	// note.userId == viewerID なので self skip で送信される
	payload := []byte(`{"userId":"viewer","text":"politics by self"}`)
	rules := []byte(`[["politics"]]`)
	assert.True(t, f.shouldEmit(payload, rules, "viewer"))
}

func TestShouldEmit_HardMute_NoRulesIsNoOp(t *testing.T) {
	f := noteFilter{WithRenotes: true, WithReplies: true, WithFiles: false}
	payload := []byte(`{"userId":"author","text":"politics"}`)
	assert.True(t, f.shouldEmit(payload, nil, "viewer"))
	assert.True(t, f.shouldEmit(payload, []byte("[]"), "viewer"))
}

// #3369: 本家の checkNoteWordMute(getAppearNote(note)) と同じ対象を見る。
// 期待値は本家 frontend use-note.ts / utility/check-word-mute.ts から書き下した。
func TestHardMutedPayload_UpstreamAppearNote(t *testing.T) {
	rules := []byte(`[["muted"]]`)
	const now = int64(1_800_000_000_000)
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"pure renote is judged by the renoted note",
			`{"userId":"booster","renoteId":"r1","renote":{"userId":"author","text":"a muted word"}}`, true},
		{"pure renote of a clean note is shown",
			`{"userId":"booster","renoteId":"r1","renote":{"userId":"author","text":"clean"}}`, false},
		{"pure renote of the viewer's own note is not muted by it",
			`{"userId":"booster","renoteId":"r1","renote":{"userId":"viewer","text":"muted by me"}}`, false},
		{"pure renote by the viewer of a muted note is hidden",
			`{"userId":"viewer","renoteId":"r1","renote":{"userId":"author","text":"muted"}}`, true},
		{"pure renote of a reply checks the renoted note's reply target",
			`{"userId":"booster","renoteId":"r1","renote":{"userId":"author","text":"clean","replyId":"p1","reply":{"userId":"other","text":"muted parent"}}}`, true},
		{"pure renote of a quote checks the quoted note",
			`{"userId":"booster","renoteId":"r1","renote":{"userId":"author","text":"clean","renoteId":"q1","renote":{"userId":"other","cw":"muted cw"}}}`, true},
		{"quote checks the quoted note",
			`{"userId":"author","text":"clean","renoteId":"q1","renote":{"userId":"other","text":"muted"}}`, true},
		{"quote's own text is checked",
			`{"userId":"author","text":"muted comment","renoteId":"q1","renote":{"userId":"other","text":"clean"}}`, true},
		{"quote does not look two levels into the quoted note",
			`{"userId":"author","text":"clean","renoteId":"q1","renote":{"userId":"other","text":"clean","renoteId":"q2","renote":{"userId":"third","text":"muted"}}}`, false},
		{"reply checks the reply target",
			`{"userId":"author","text":"clean","replyId":"p1","reply":{"userId":"other","text":"muted"}}`, true},
		{"reply to the viewer's own muted note is shown",
			`{"userId":"author","text":"clean","replyId":"p1","reply":{"userId":"viewer","text":"muted"}}`, false},
		{"followers-only embed the viewer cannot see is not judged",
			`{"userId":"author","text":"clean","replyId":"p1","reply":{"userId":"other","visibility":"followers","text":"muted"}}`, false},
		{"specified embed addressed to the viewer is judged",
			`{"userId":"author","text":"clean","replyId":"p1","reply":{"userId":"other","visibility":"specified","visibleUserIds":["viewer"],"text":"muted"}}`, true},
		{"pure renote of a quote whose target the viewer cannot see",
			`{"userId":"booster","renoteId":"r1","renote":{"userId":"author","text":"clean","renoteId":"q1","renote":{"userId":"other","visibility":"followers","text":"muted"}}}`, false},
		{"pure renote of a reply whose target the viewer cannot see",
			`{"userId":"booster","renoteId":"r1","renote":{"userId":"author","text":"clean","replyId":"p1","reply":{"userId":"other","visibility":"followers","text":"muted"}}}`, false},
		{"specified embed not addressed to the viewer is not judged",
			`{"userId":"author","text":"clean","replyId":"p1","reply":{"userId":"other","visibility":"specified","visibleUserIds":["x"],"text":"muted"}}`, false},
		{"renote with a poll is not pure, its own empty text does not match but the target does",
			`{"userId":"author","renoteId":"q1","poll":{"choices":[]},"renote":{"userId":"other","text":"muted"}}`, true},
		{"poll choices are not judged",
			`{"userId":"author","text":"clean","poll":{"choices":[{"text":"muted"}]}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hardMutedPayload([]byte(tc.payload), rules, "viewer", now))
		})
	}
}

func TestHardMutedPayload_TopLevelHiddenByAuthorPrefsIsNotJudged(t *testing.T) {
	// makeNotesHiddenBefore で本文が消されるノートは、本家と同じく判定しない。
	hidden := -1
	payload := []byte(`{"userId":"author","text":"muted","visibility":"public","createdAt":"2020-01-01T00:00:00.000Z","user":{"id":"author","makeNotesHiddenBefore":` + strconv.Itoa(hidden) + `}}`)
	assert.False(t, hardMutedPayload(payload, []byte(`[["muted"]]`), "viewer", 1_800_000_000_000))
	visible := []byte(`{"userId":"author","text":"muted","visibility":"public","createdAt":"2020-01-01T00:00:00.000Z","user":{"id":"author"}}`)
	assert.True(t, hardMutedPayload(visible, []byte(`[["muted"]]`), "viewer", 1_800_000_000_000))
}

func TestHardMutedPayload_DowngradedReplyToViewerIsJudged(t *testing.T) {
	// makeNotesFollowersOnlyBefore で降格しても、viewer への返信なら見えるので判定する。
	base := `{"userId":"author","text":"muted","visibility":"public","createdAt":"2020-01-01T00:00:00.000Z","user":{"id":"author","makeNotesFollowersOnlyBefore":-1},"replyId":"p1","reply":{"userId":"%s","text":"q"}}`
	rules := []byte(`[["muted"]]`)
	assert.True(t, hardMutedPayload([]byte(fmt.Sprintf(base, "viewer")), rules, "viewer", 1_800_000_000_000))
	assert.False(t, hardMutedPayload([]byte(fmt.Sprintf(base, "someone")), rules, "viewer", 1_800_000_000_000))
}

func TestHardMutedPayload_FollowersOnlyTopLevelIsJudged(t *testing.T) {
	// チャンネルの配信先はトップのノートを見られる人だけなので、フォロワー限定の
	// トップは判定する (トップに当てるのは著者の設定の判定だけ)。
	payload := []byte(`{"userId":"author","visibility":"followers","text":"muted"}`)
	assert.True(t, hardMutedPayload(payload, []byte(`[["muted"]]`), "viewer", 1_800_000_000_000))
}

func TestHardMutedPayload_RenoteWithReplyIsNotPure(t *testing.T) {
	// 返信でもあるリノートは純粋なリノートではないので、リノート元の返信先は見ない。
	payload := []byte(`{"userId":"booster","renoteId":"t","replyId":"p","reply":{"userId":"other","text":"clean"},"renote":{"userId":"author","text":"clean","replyId":"i","reply":{"userId":"third","text":"muted"}}}`)
	assert.False(t, hardMutedPayload(payload, []byte(`[["muted"]]`), "viewer", 1_800_000_000_000))
	pure := []byte(`{"userId":"booster","renoteId":"t","renote":{"userId":"author","text":"clean","replyId":"i","reply":{"userId":"third","text":"muted"}}}`)
	assert.True(t, hardMutedPayload(pure, []byte(`[["muted"]]`), "viewer", 1_800_000_000_000))
}

func TestShouldEmit_HardMuteRulesKeepConnectParamFilters(t *testing.T) {
	// 規則を持つ閲覧者でも、withRenotes / withFiles の条件は同じく効く。
	rules := []byte(`[["nomatch"]]`)
	noRenotes := noteFilter{WithRenotes: false}
	assert.False(t, noRenotes.shouldEmit([]byte(`{"userId":"a","renoteId":"r1","renote":{"userId":"b","text":"x"}}`), rules, "viewer"))
	filesOnly := noteFilter{WithRenotes: true, WithFiles: true}
	assert.False(t, filesOnly.shouldEmit([]byte(`{"userId":"a","text":"x","fileIds":[]}`), rules, "viewer"))
	assert.True(t, filesOnly.shouldEmit([]byte(`{"userId":"a","text":"x","fileIds":["f1"]}`), rules, "viewer"))
	// 規則があっても、読めない payload は以前どおり送る。
	assert.True(t, filesOnly.shouldEmit([]byte(`{invalid`), rules, "viewer"))
}

func TestHardMutedPayload_InvalidJSONIsNotMuted(t *testing.T) {
	assert.False(t, hardMutedPayload([]byte(`{invalid`), []byte(`[["x"]]`), "viewer", 0))
}

func TestShouldEmit_HardMute_PureRenoteOfMutedNote(t *testing.T) {
	f := noteFilter{WithRenotes: true}
	payload := []byte(`{"userId":"booster","renoteId":"r1","renote":{"userId":"author","text":"muted"}}`)
	assert.False(t, f.shouldEmit(payload, []byte(`[["muted"]]`), "viewer"))
}

func TestAnonRequireSigninDrop(t *testing.T) {
	t.Run("authed viewer always false", func(t *testing.T) {
		p := []byte(`{"user":{"requireSigninToViewContents":true}}`)
		assert.False(t, anonRequireSigninDrop(p, "viewer"))
	})
	t.Run("anon + top-level requireSignin drops", func(t *testing.T) {
		p := []byte(`{"user":{"requireSigninToViewContents":true}}`)
		assert.True(t, anonRequireSigninDrop(p, ""))
	})
	t.Run("anon + renote requireSignin drops", func(t *testing.T) {
		p := []byte(`{"user":{"id":"u"},"renote":{"user":{"requireSigninToViewContents":true}}}`)
		assert.True(t, anonRequireSigninDrop(p, ""))
	})
	t.Run("anon + reply requireSignin drops", func(t *testing.T) {
		p := []byte(`{"user":{"id":"u"},"reply":{"user":{"requireSigninToViewContents":true}}}`)
		assert.True(t, anonRequireSigninDrop(p, ""))
	})
	t.Run("anon + flag false passes", func(t *testing.T) {
		p := []byte(`{"user":{"requireSigninToViewContents":false}}`)
		assert.False(t, anonRequireSigninDrop(p, ""))
	})
	t.Run("anon + flag absent passes", func(t *testing.T) {
		p := []byte(`{"user":{"id":"author"}}`)
		assert.False(t, anonRequireSigninDrop(p, ""))
	})
	t.Run("malformed payload passes (fail to other gates)", func(t *testing.T) {
		assert.False(t, anonRequireSigninDrop([]byte(`{not json`), ""))
	})
}

// #2058: pure renote の renote.myReaction を reactionAndUserPairCache から in-memory 算出。
func TestInjectRenoteMyReaction(t *testing.T) {
	base := `{"id":"n1","userId":"author","renoteId":"r1","renote":{"id":"r1","reactions":{"👍":2},"reactionAndUserPairCache":["alice/👍","bob/❤"]}}`

	// alice は reaction 済 → renote.myReaction="👍" がセットされる。
	out := injectRenoteMyReaction([]byte(base), "alice")
	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	renote := m["renote"].(map[string]any)
	assert.Equal(t, "👍", renote["myReaction"], "viewer の reaction が inject される (#2058)")

	// carol は未 reaction → payload 不変 (myReaction 無し)。
	assert.Equal(t, base, string(injectRenoteMyReaction([]byte(base), "carol")), "未 reaction は不変")
	// 未認証 (viewerID="") → 不変。
	assert.Equal(t, base, string(injectRenoteMyReaction([]byte(base), "")), "anon は不変")
	// quote renote (text あり = 非 pure renote) → 不変。
	quote := `{"id":"n2","userId":"a","renoteId":"r1","text":"q","renote":{"reactions":{"👍":1},"reactionAndUserPairCache":["alice/👍"]}}`
	assert.Equal(t, quote, string(injectRenoteMyReaction([]byte(quote), "alice")), "quote は対象外")
	// renote.reactions 空 → 不変。
	noReact := `{"id":"n3","userId":"a","renoteId":"r1","renote":{"reactions":{},"reactionAndUserPairCache":[]}}`
	assert.Equal(t, noReact, string(injectRenoteMyReaction([]byte(noReact), "alice")), "reactions 空は対象外")
	// cache が reaction 種類数より少ない (truncated) → 不変。
	trunc := `{"id":"n4","userId":"a","renoteId":"r1","renote":{"reactions":{"👍":1,"❤":1},"reactionAndUserPairCache":["alice/👍"]}}`
	assert.Equal(t, trunc, string(injectRenoteMyReaction([]byte(trunc), "alice")), "truncated cache は skip")
	// renote でない → 不変。
	plain := `{"id":"n5","userId":"a"}`
	assert.Equal(t, plain, string(injectRenoteMyReaction([]byte(plain), "alice")))
}

// #2058 HIGH-1: truncated 判定は reaction 種類数でなく総数 (sum) で行う。
// #2058 HIGH-2: pair suffix の raw reaction を colon-form/legacy 正規化する。
func TestInjectRenoteMyReaction_SumAndNormalize(t *testing.T) {
	// 単一種類だが総数 3 > cache 長 2 → truncated 扱いで skip (key-count なら誤って算出)。
	trunc := `{"id":"n1","userId":"a","renoteId":"r1","renote":{"reactions":{"👍":3},"reactionAndUserPairCache":["bob/👍","carol/👍"]}}`
	assert.Equal(t, trunc, string(injectRenoteMyReaction([]byte(trunc), "alice")), "総数>cache長は truncated で skip (#2058 HIGH-1)")

	// legacy text reaction "like" は "👍" に正規化される。
	legacy := `{"id":"n2","userId":"a","renoteId":"r1","renote":{"reactions":{"👍":1},"reactionAndUserPairCache":["alice/like"]}}`
	out := injectRenoteMyReaction([]byte(legacy), "alice")
	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, "👍", m["renote"].(map[string]any)["myReaction"], "legacy reaction を正規化 (#2058 HIGH-2)")

	// custom emoji :smile: は :smile@.: に正規化される。
	custom := `{"id":"n3","userId":"a","renoteId":"r1","renote":{"reactions":{":smile@.:":1},"reactionAndUserPairCache":["alice/:smile:"]}}`
	out = injectRenoteMyReaction([]byte(custom), "alice")
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, ":smile@.:", m["renote"].(map[string]any)["myReaction"], "custom emoji を正規化 (#2058 HIGH-2)")
}

func TestRenotedReplyVisibleTo(t *testing.T) {
	pureRenoteWithReply := func(replyUser, vis string) []byte {
		return []byte(`{"userId":"author","renoteId":"r1","renote":{"reply":{"userId":"` +
			replyUser + `","visibility":"` + vis + `"}}}`)
	}
	snap := map[string]bool{"author": false, "followee": false}

	// followers 限定の返信をリノートされた場合、返信先の投稿者を follow して
	// いなければ home に流さない (upstream home-timeline.ts)。
	assert.False(t, replyShouldEmit(pureRenoteWithReply("stranger", "followers"), "viewer", snap, false, replyGateHome))
	// follow していれば流す
	assert.True(t, replyShouldEmit(pureRenoteWithReply("followee", "followers"), "viewer", snap, false, replyGateHome))
	// 自分の投稿への返信なら流す
	assert.True(t, replyShouldEmit(pureRenoteWithReply("viewer", "followers"), "viewer", snap, false, replyGateHome))
	// public な返信のリノートは無条件で流す
	assert.True(t, replyShouldEmit(pureRenoteWithReply("stranger", "public"), "viewer", snap, false, replyGateHome))
	// 引用リノート (text あり = 純粋リノートでない) は対象外
	quote := []byte(`{"userId":"author","renoteId":"r1","text":"hi","renote":{"reply":{"userId":"stranger","visibility":"followers"}}}`)
	assert.True(t, replyShouldEmit(quote, "viewer", snap, false, replyGateHome))
}
