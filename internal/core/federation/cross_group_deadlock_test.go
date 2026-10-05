package federation

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
)

// gateFetcher serves canned documents but blocks the first fetch of one URI
// until released, so a test can pin one worker inside a known call.
type gateFetcher struct {
	docs    map[string]string
	gate    string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *gateFetcher) FetchObject(uri string) ([]byte, error) {
	if uri == f.gate {
		f.once.Do(func() { close(f.reached) })
		<-f.release
	}
	if b, ok := f.docs[uri]; ok {
		return []byte(b), nil
	}
	return nil, errors.New("no fixture for " + uri)
}

// **actor の in-flight もグラフに載っていること。** 載せていないと、actor の
// 鍵を待っているチェーンが「走っている」と見え、そこを通る循環を見逃す
// (#2685 review HIGH-1)。
//
// featured の取り込みは待たなくなった (resolveNoteBestEffort) ので、
// note→actor→note の循環はもう作れない。それでも actor 側の登録は要る:
// `processRemoteMove` が移行先の actor を待つので、**互いを movedTo に指す
// 2 つの actor** を 2 worker が同時に取得すると actor どうしで循環する。
//
// ここでは実際の解決を 1 本走らせ、(1) note の著者解決が actor の鍵で待つときに
// その待ちがグラフに現れること、(2) featured がその note を要求しても双方が
// 返ることを見る。(1) が崩れると、待ちが見えないまま循環を許すようになる。
func TestResolveActor_NoteAuthorWaitIsTrackedOnTheGraph(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/quinn"
		featured = "https://remote.example/users/quinn/collections/featured"
		noteN    = "https://remote.example/notes/n1"
	)
	f := &gateFetcher{
		docs: map[string]string{
			actorURI: featuredActorDoc("remote.example", "quinn", featured),
			featured: `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + featured +
				`","type":"OrderedCollection","orderedItems":["` + noteN + `"]}`,
			noteN: plainNoteDoc(noteN, actorURI),
		},
		gate:    actorURI,
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(), urls, f, idGen)
	r.SetPinningRepo(testutil.NewMockUserNotePiningRepository(), idGen)

	var once sync.Once
	release := func() { once.Do(func() { close(f.release) }) }
	t.Cleanup(release)

	w2 := make(chan error, 1)
	go func() {
		_, e := r.ResolveActor(actorURI)
		w2 <- e
	}()
	select {
	case <-f.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("W2 never reached the gated actor fetch")
	}

	w1 := make(chan error, 1)
	go func() {
		_, e := r.ResolveNote(noteN)
		w1 <- e
	}()
	// **W1 が actor の in-flight に実際にぶら下がるまで待つ。** 固定の sleep に
	// すると、遅い環境で W1 がまだ note を fetch している間に gate を開けてしまい、
	// 競合が起きないまま緑になる (回帰を検出しないテストになる)。
	waitUntilSomeoneBlockedOnActor(t, r)
	release()

	for i := 0; i < 2; i++ {
		select {
		case e := <-w1:
			t.Logf("W1 (ResolveNote) returned err=%v", e)
		case e := <-w2:
			t.Logf("W2 (ResolveActor) returned err=%v", e)
		case <-time.After(15 * time.Second):
			t.Fatalf("DEADLOCK: %d/2 returned; note と actor をまたぐ待ちが解けていない", i)
		}
	}
}

// waitUntilSomeoneBlockedOnActor blocks until some chain is waiting on an
// actor key.
func waitUntilSomeoneBlockedOnActor(t *testing.T, r *Resolver) {
	t.Helper()
	require.Eventually(t, func() bool {
		r.waits.mu.Lock()
		defer r.waits.mu.Unlock()
		for _, st := range r.waits.chains {
			if strings.HasPrefix(st.waitingOn, waitKeyActor) {
				return true
			}
		}
		return false
	}, 10*time.Second, time.Millisecond,
		"actor の鍵を待っているチェーンがグラフに現れない (actor 側が未登録)")
}
