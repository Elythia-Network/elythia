package processors_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/require"
)

// countingExtraStore is an in-memory federation.PublickeyExtraStore that counts
// FindByUserAndKeyID round trips.
type countingExtraStore struct {
	rows  map[string]*model.UserPublickeyExtra // userID + "\x00" + keyID
	finds atomic.Int64
}

func newCountingExtraStore() *countingExtraStore {
	return &countingExtraStore{rows: map[string]*model.UserPublickeyExtra{}}
}

func (s *countingExtraStore) Upsert(pk *model.UserPublickeyExtra) error {
	s.rows[pk.UserID+"\x00"+pk.KeyID] = pk
	return nil
}

func (s *countingExtraStore) FindByUserAndKeyID(userID, keyID string) (*model.UserPublickeyExtra, error) {
	s.finds.Add(1)
	if row, ok := s.rows[userID+"\x00"+keyID]; ok {
		return row, nil
	}
	return nil, testutil.ErrNotFound
}

func (s *countingExtraStore) ListByUserID(userID string) ([]model.UserPublickeyExtra, error) {
	var out []model.UserPublickeyExtra
	for _, r := range s.rows {
		if r.UserID == userID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *countingExtraStore) DeleteByKeyID(userID, keyID string) error {
	delete(s.rows, userID+"\x00"+keyID)
	return nil
}

// countingPublickeyStore counts user_publickey reads.
type countingPublickeyStore struct {
	*testutil.MockUserPublickeyRepository
	finds atomic.Int64
}

func (s *countingPublickeyStore) FindByUserID(userID string) (*model.UserPublickey, error) {
	s.finds.Add(1)
	return s.MockUserPublickeyRepository.FindByUserID(userID)
}

const benchActorURI = "https://remote.example/users/alice"

// inboxPipeline wires the production InboxProcessor + federation.Resolver (as
// the signature verifier) + federation.Processor over in-memory repositories,
// so the whole Handle path runs except the activity handler itself: the actor
// is suspended, so process() drops the activity right after its moderation
// check (= the last step that parses the body).
type inboxPipeline struct {
	inbox       *processors.InboxProcessor
	userFinds   *atomic.Int64
	extra       *countingExtraStore
	pubkeys     *countingPublickeyStore
	key         *activitypub.PrivateKey
	actorDBRows *testutil.MockUserRepository
}

func newInboxPipeline(tb testing.TB) *inboxPipeline {
	tb.Helper()
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(tb, err)
	key, err := activitypub.NewPrivateKey(benchActorURI+"#main-key", priv)
	require.NoError(tb, err)

	host := "remote.example"
	now := time.Now()
	actor := &model.User{
		ID: "alice_remote", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: uptr(benchActorURI), LastFetchedAt: &now,
		IsSuspended: true,
	}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[actor.ID] = actor
	var userFinds atomic.Int64
	userRepo.FindByURIHook = func(string) error {
		userFinds.Add(1)
		return nil
	}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(tb, err)
	resolver := federation.NewResolver(userRepo, testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), ldFwdNoFetch{}, idGen)
	pubkeys := &countingPublickeyStore{MockUserPublickeyRepository: testutil.NewMockUserPublickeyRepository()}
	pubkeys.Keys[actor.ID] = &model.UserPublickey{UserID: actor.ID, KeyID: benchActorURI + "#main-key", KeyPEM: pub}
	resolver.SetPublickeyRepo(pubkeys)
	extra := newCountingExtraStore()
	resolver.SetPublickeyExtraRepo(extra)

	fed := federation.NewProcessor(resolver, nil, nil, nil, userRepo, testutil.NewMockNoteRepository(), "", nil, nil)
	inbox := processors.NewInboxProcessor(fed, resolver, nil, nil, nil, nil)
	inbox.SetLDSignatureVerifier(federation.NewLDSignatureVerifier(pubkeys.MockUserPublickeyRepository))
	return &inboxPipeline{inbox: inbox, userFinds: &userFinds, extra: extra, pubkeys: pubkeys, key: key, actorDBRows: userRepo}
}

func (p *inboxPipeline) resetCounters() {
	p.userFinds.Store(0)
	p.extra.finds.Store(0)
	p.pubkeys.finds.Store(0)
}

func (p *inboxPipeline) dbRoundTrips() int64 {
	return p.userFinds.Load() + p.extra.finds.Load() + p.pubkeys.finds.Load()
}

// misskeyCreateBody builds a Misskey-shaped Create(Note) with the full
// Misskey @context. pad lengthens the note text to reach larger bodies.
func misskeyCreateBody(tb testing.TB, n int, pad int) []byte {
	tb.Helper()
	noteURI := "https://remote.example/notes/" + strings.Repeat("a", 4) + string(rune('a'+n%26))
	text := "hello from alice " + strings.Repeat("x", pad)
	act := map[string]any{
		"@context":  []any{activitypub.ContextURL, activitypub.SecurityContextURL, activitypub.MisskeyContext},
		"id":        noteURI + "/activity",
		"actor":     benchActorURI,
		"type":      "Create",
		"published": "2026-10-15T00:00:00.000Z",
		"object": map[string]any{
			"id":                noteURI,
			"type":              "Note",
			"attributedTo":      benchActorURI,
			"content":           "<p>" + text + "</p>",
			"_misskey_content":  text,
			"source":            map[string]any{"content": text, "mediaType": "text/x.misskeymarkdown"},
			"published":         "2026-10-15T00:00:00.000Z",
			"to":                []any{"https://www.w3.org/ns/activitystreams#Public"},
			"cc":                []any{benchActorURI + "/followers"},
			"inReplyTo":         nil,
			"attachment":        []any{},
			"sensitive":         false,
			"tag":               []any{},
			"_misskey_quote":    nil,
			"quoteUrl":          nil,
			"_misskey_reaction": nil,
		},
		"to": []any{"https://www.w3.org/ns/activitystreams#Public"},
		"cc": []any{benchActorURI + "/followers"},
	}
	b, err := json.Marshal(act)
	require.NoError(tb, err)
	return b
}

func benchmarkInboxHandle(b *testing.B, pad int) {
	// 破棄のログ (凍結済み actor) が測定を支配しないよう、捨てる。
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(prev) })
	p := newInboxPipeline(b)
	body := misskeyCreateBody(b, 0, pad)
	task := driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(b, signedInboxPayload(b, p.key, body))}
	// 1 回目で鍵のキャッシュを温める (backoff の記録もここで入る)。
	require.NoError(b, p.inbox.Handle(context.Background(), task))
	p.resetCounters()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.inbox.Handle(context.Background(), task); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(p.dbRoundTrips())/float64(b.N), "db/op")
	b.ReportMetric(float64(len(body)), "body-bytes")
}

// BenchmarkInboxHandle_Small measures one signed Misskey Create through the
// whole worker path (signature verify, actor gate, process prelude).
func BenchmarkInboxHandle_Small(b *testing.B) { benchmarkInboxHandle(b, 0) }

// BenchmarkInboxHandle_Large is the same with a ~62KB body (the text appears
// three times: content / _misskey_content / source).
func BenchmarkInboxHandle_Large(b *testing.B) { benchmarkInboxHandle(b, 20000) }
