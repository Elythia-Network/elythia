package server

import (
	"context"
	"runtime"
	"sync"

	"github.com/elythia-network/elythia/internal/bgwork"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/model"
)

// 配送を要求の外で積む (#4216)。
//
// リアクションとノートの削除の配送のhookは、フォロワーのinbox(公開ノートの削除は
// 全リモートのinbox。リモート30万人のfixtureで106〜123ms、#4214の調査)を引いて、
// inboxの数だけジョブを積む。要求の中で呼ぶと、応答がそれを待つ。本家もawaitせずに
// 応答を返す(ReactionServiceのtrackPromise、NoteDeleteServiceのdeliverToConcerned)。
//
// 世代の停止で待ちが尽きると、まだ走り始めていない配送は捨てる(件数とログに残す)。
// 走り始めて止めきれなかった配送は件数に入らず、bgworkの「止まらなかった」ログに
// 出るだけで、DBを閉じた後に途中で失敗する。ノートの作成の配送と同じ扱いで、本家も
// プロセスが止まれば積む前の配送を失う。

// asyncHookRunner runs delivery hooks in goroutines of the server generation,
// at most cap(slots) at once, and runs the hooks of one key in the order they
// were called.
//
// **同じ鍵のhookは、呼ばれた順に1本ずつ走らせる。** リアクションの鍵は「人と
// ノート」。リアクションの付け替えは、1回のreactions/createでUndo(旧)とLike(新)を
// 続けて呼ぶ。別々のgoroutineにすると逆の順で積まれうる。受け取った側(本家も
// Elythiaも)はUndoの中のリアクションの種類を見ずにその人のリアクションを丸ごと
// 消すので、Likeが先に処理されると、相手のサーバーではリアクションが消えたまま
// 戻らない。守れるのは積む順までで、届く順はworkerが並行に送るので保証しない。
//
// **同時に走る数を絞る。** 要求の中で走らせていたときは、同時に走る数がクライアントの
// 要求の数で頭打ちだった。応答を先に返すと押し戻す力が無くなるので、走る数を枠で
// 絞る。待つgoroutineの数は絞らない(呼ぶたびに1本起こし、要求の数とレート制限で
// 頭打ちになる。ノートの作成のhookと同じ)。枠の大きさはノートの作成のhookと同じ
// 設定(noteHookConcurrency)に従うが、枠そのものは別に持つ。リアクションと削除も別のrunnerにして、全リモートへ積む削除が
// 続いてもリアクションの配送が待たされないようにする。
type asyncHookRunner struct {
	background *bgwork.Group
	slots      chan struct{}

	mu sync.Mutex
	// tails holds, per key, the done channel of the hook last called with it
	// (it may still be waiting for the one before).
	tails map[string]chan struct{}
}

// newAsyncHookRunner returns a runner of background with n slots. n <= 0
// selects the default of the note hooks (GOMAXPROCS * 2).
func newAsyncHookRunner(background *bgwork.Group, n int) *asyncHookRunner {
	if n <= 0 {
		n = runtime.GOMAXPROCS(0) * 2
	}
	return &asyncHookRunner{
		background: background,
		slots:      make(chan struct{}, max(1, n)),
		tails:      map[string]chan struct{}{},
	}
}

func (r *asyncHookRunner) run(what, key string, fn func()) {
	done := make(chan struct{})
	r.mu.Lock()
	prev := r.tails[key]
	r.tails[key] = done
	r.mu.Unlock()
	finish := func() {
		close(done)
		r.mu.Lock()
		if r.tails[key] == done {
			delete(r.tails, key)
		}
		r.mu.Unlock()
	}
	started := r.background.GoAs(what, func(ctx context.Context) {
		defer finish()
		// 前のhookを待ってから枠を取る。枠を握ったまま待つと、待つだけのhookが枠を
		// 埋めて、待たれている側が走れなくなる。
		if prev != nil {
			select {
			case <-prev:
			case <-ctx.Done():
			}
		}
		// 停止が始まったら走らせない。前のhookが停止で捨てられてdoneを閉じたとき、
		// その前のhookはまだ走っていることがあり、ここで走ると同じ鍵の2本が並ぶ。
		if ctx.Err() != nil {
			r.background.RecordDropped(what, "the generation stop gave up before the previous hook of the same key finished")
			return
		}
		select {
		case r.slots <- struct{}{}:
		case <-ctx.Done():
			r.background.RecordDropped(what, "no slot before the generation stop gave up")
			return
		}
		defer func() { <-r.slots }()
		fn()
	})
	if !started {
		// 走らせなかったhookを待つ後続が、止まったままにならないように閉じる。
		finish()
	}
}

// asyncReactionFederationHook runs the reaction delivery hook outside the
// request (#4216).
type asyncReactionFederationHook struct {
	inner  corereaction.FederationHook
	runner *asyncHookRunner
}

// delivers mirrors the early return of the inner hook (shouldDeliver), so
// received reactions and local-only notes do not start a goroutine.
func (h asyncReactionFederationHook) delivers(reactor *model.User, target *model.Note) bool {
	return reactor != nil && target != nil && reactor.IsLocal() && !target.LocalOnly
}

func (h asyncReactionFederationHook) OnReactionAdded(reactor *model.User, target *model.Note, reaction string) {
	if !h.delivers(reactor, target) {
		return
	}
	h.runner.run("reaction delivery", reactor.ID+"/"+target.ID, func() { h.inner.OnReactionAdded(reactor, target, reaction) })
}

func (h asyncReactionFederationHook) OnReactionRemoved(reactor *model.User, target *model.Note, reaction string) {
	if !h.delivers(reactor, target) {
		return
	}
	h.runner.run("reaction delivery", reactor.ID+"/"+target.ID, func() { h.inner.OnReactionRemoved(reactor, target, reaction) })
}

// asyncNoteDeleteFederationHook runs the note delete delivery hook outside the
// request (#4216).
type asyncNoteDeleteFederationHook struct {
	inner  corenote.DeleteFederationHook
	runner *asyncHookRunner
}

func (h asyncNoteDeleteFederationHook) OnNoteDeleted(author *model.User, note *model.Note) {
	// 受信したDeleteとローカル限定のノートは、内側のhookが何もせずに戻る。
	if author == nil || note == nil || !author.IsLocal() || note.LocalOnly {
		return
	}
	h.runner.run("note delete delivery", note.ID, func() { h.inner.OnNoteDeleted(author, note) })
}
