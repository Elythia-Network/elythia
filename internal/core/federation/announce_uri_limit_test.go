package federation_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
)

// 列に入らない id の Announce は renote を作らないこと (#2723)。
//
// `note.uri` は varchar(512)。Announce の id は renote の身元で、**同じ activity の
// 重複検出 (`FindByURI`) の鍵**でもあるので切らずに拒否する。切ると別の行と衝突する。
//
// (Undo(Announce) はこの URI を引かない — `ListRenotesOf` で announcer の renote を
// 探す。)
func TestProcess_Announce_RejectsOversizedURI(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic,
	}
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	before := len(env.noteRepo.Notes)

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/` + strings.Repeat("a", 512) + `",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	err := env.processor.Process(body)
	require.Error(t, err, "列に入らない id の Announce を受理している")
	// error は上位に surface する (トップレベル配送ならその inbox job は retry を
	// 使い切って dead になる)。ここで確かめるのは renote を作らないことまで。
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
	assert.Len(t, env.noteRepo.Notes, before, "renote が作られている")
}

// ちょうど列に収まる id は受理すること (境界を 1 つずらす実装を弾く)。
func TestProcess_Announce_AcceptsURIAtColumnLimit(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic,
	}
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	before := len(env.noteRepo.Notes)

	prefix := "https://remote.example/announces/"
	// 512 は migration の列長そのもの。定数を参照すると両側が一緒に動く。
	id := prefix + strings.Repeat("a", 512-len(prefix))
	require.Len(t, []rune(id), 512)
	body := []byte(`{
		"type": "Announce",
		"id": "` + id + `",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Len(t, env.noteRepo.Notes, before+1, "renote が作られていない")
}

// 列に入らない id の note を対象にした Like は ack すること (#2723)。
//
// **拒否の結末は呼び出し元で違う** (一覧は `ingestNoteWithCreated` の gate の
// コメント)。ここで見るのは Like 経路 — `ResolveNote` の失敗を
// `isPermanentSkipError` に通すので ack する。transient に落ちると、同じ document を
// 取り直す activity が retry のたびに走る。
func TestProcess_Like_AcksOversizedTargetURI(t *testing.T) {
	longURI := "https://remote.example/notes/" + strings.Repeat("n", 512)
	// fetcher は常にこの Note document を返す。actor は先に repo へ入れておく
	// (fetch させると同じ body が返って actor 解決で落ちる)。
	noteDoc := `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + longURI + `",` +
		`"type":"Note","attributedTo":"https://remote.example/users/alice","content":"hi",` +
		`"to":["https://www.w3.org/ns/activitystreams#Public"]}`
	env := newFullProcessor(t, noteDoc)
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	now := time.Now()
	env.userRepo.Users["u_alice"] = &model.User{
		ID: "u_alice", Username: "alice", UsernameLower: "alice",
		URI: &uri, Host: &host, LastFetchedAt: &now,
	}

	body := []byte(`{
		"type": "Like",
		"id": "https://remote.example/likes/1",
		"actor": "` + uri + `",
		"object": "` + longURI + `",
		"content": "👍"
	}`)
	// **ack すること** (error を返すと retry に乗る)。
	require.NoError(t, env.processor.Process(body), "取り込めない note への Like で retry に乗せている")
	assert.Empty(t, env.reactionRepo.Reactions, "reaction を作っている")
}

// fragment に NUL を持つ Announce の id も拒否すること (#2723)。
//
// **この経路は生きている。** `url.Parse` は制御文字を弾くが **fragment だけは例外**
// で、`https://h/a#\x00` は parse を通る。`authorizeActor` の host 検査も
// `Hostname()` を見るだけなので抜ける。素通しすると `FindByURI` の SELECT 自体が
// 22021 で落ちる。
func TestProcess_Announce_RejectsURIWithNULInFragment(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic,
	}
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	before := len(env.noteRepo.Notes)

	// 前提: この URI は url.Parse を通る (通らないなら gate 以前に落ちてしまい、
	// このテストは gate を検査していないことになる)。
	raw := "https://remote.example/announces/1#\x00"
	u, err := url.Parse(raw)
	require.NoError(t, err, "前提: fragment の NUL は parse を通る")
	require.Equal(t, "remote.example", u.Hostname())

	body := []byte(`{
		"type": "Announce",
		"id": ` + quoteJSONStr(raw) + `,
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	err = env.processor.Process(body)
	require.Error(t, err, "NUL を含む id の Announce を受理している")
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
	assert.Len(t, env.noteRepo.Notes, before, "renote が作られている")
}

// quoteJSONStr renders s as a JSON string literal.
func quoteJSONStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
