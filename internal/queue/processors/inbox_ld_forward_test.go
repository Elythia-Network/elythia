package processors_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/activitypub/ld"
	"github.com/shiroha-a/mk/internal/core/federation"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/processors"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 転送された LD-Signature 付き activity の、署名が覆っていないキーを handler に
// 渡さないことを、本物の LDSignatureVerifier と federation.Processor を通して
// 確かめる。
//
// 攻撃の形: Mastodon の文書は `_misskey_content` を context で定義していないので、
// AS2 の `@vocab: "_:"` で blank node IRI になり URDNA2015 の結果に現れない。
// 転送者 (mallory) はそれを被害者 (bob) の署名済み Create / Update に足し、自分の
// HTTP 署名で送る。LD-Signature はそれでも通るので、生 body を処理すると bob 名義の
// ノート本文を mallory が決められた。

const (
	ldFwdVictimURI   = "https://m.example/users/bob"
	ldFwdVictimKeyID = "https://m.example/users/bob#main-key"
	ldFwdAttackerURI = "https://evil.example/users/mallory"
)

// ldFwdNoFetch fails every fetch: the test must not depend on remote lookups.
type ldFwdNoFetch struct{}

func (ldFwdNoFetch) FetchObject(uri string) ([]byte, error) {
	return nil, errors.New("fetch disabled in test: " + uri)
}

type ldForwardEnv struct {
	inbox       *processors.InboxProcessor
	noteRepo    *testutil.MockNoteRepository
	victimID    string
	victimPriv  string
	attackerKey *activitypub.PrivateKey
}

func newLDForwardEnv(t *testing.T) *ldForwardEnv {
	t.Helper()
	attackerPriv, attackerPub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	attackerKey, err := activitypub.NewPrivateKey(ldFwdAttackerURI+"#main-key", attackerPriv)
	require.NoError(t, err)
	victimPriv, victimPub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)

	victimHost := "m.example"
	attackerHost := "evil.example"
	victim := &model.User{ID: "bob_remote", Username: "bob", Host: &victimHost, URI: uptr(ldFwdVictimURI)}
	attacker := &model.User{ID: "mallory_remote", Username: "mallory", Host: &attackerHost, URI: uptr(ldFwdAttackerURI)}

	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[victim.ID] = victim
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	resolver := federation.NewResolver(userRepo, noteRepo, activitypub.NewURLBuilder("https://example.com"), ldFwdNoFetch{}, idGen)
	followingSvc := corefollowing.NewService(userRepo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	fed := federation.NewProcessor(resolver, followingSvc, nil, nil, userRepo, noteRepo)

	pubkeys := testutil.NewMockUserPublickeyRepository()
	pubkeys.Keys[victim.ID] = &model.UserPublickey{UserID: victim.ID, KeyID: ldFwdVictimKeyID, KeyPEM: victimPub}

	inbox := processors.NewInboxProcessor(fed)
	inbox.SetSignatureVerifier(&multiActorVerifier{
		pubKey: attackerPub,
		byURI: map[string]*model.User{
			ldFwdAttackerURI: attacker,
			ldFwdVictimURI:   victim,
		},
	})
	inbox.SetLDSignatureVerifier(federation.NewLDSignatureVerifier(pubkeys))
	return &ldForwardEnv{inbox: inbox, noteRepo: noteRepo, victimID: victim.ID, victimPriv: victimPriv, attackerKey: attackerKey}
}

// mastodonContext is the @context Mastodon puts on a Create: AS2 + security
// plus inline toot / ostatus terms, and **no** Misskey extension terms.
func mastodonContext() []any {
	return []any{
		"https://www.w3.org/ns/activitystreams",
		"https://w3id.org/security/v1",
		map[string]any{
			"ostatus":   "http://ostatus.org#",
			"atomUri":   "ostatus:atomUri",
			"sensitive": "as:sensitive",
			"toot":      "http://joinmastodon.org/ns#",
			"Hashtag":   "as:Hashtag",
			"Emoji":     "toot:Emoji",
		},
	}
}

// signAsVictim LD-signs activity with the victim key, then applies tamper to
// the signed document (the forwarder's modifications) and returns the body.
func (e *ldForwardEnv) signAsVictim(t *testing.T, activity map[string]any, created time.Time, tamper func(map[string]any)) []byte {
	t.Helper()
	signed, err := ld.NewProcessor().SignRsaSignature2017(activity, e.victimPriv, ldFwdVictimKeyID, created)
	require.NoError(t, err)
	if tamper != nil {
		tamper(signed)
	}
	body, err := json.Marshal(signed)
	require.NoError(t, err)
	return body
}

// forward delivers body signed with the attacker's HTTP key (signer != actor).
func (e *ldForwardEnv) forward(t *testing.T, body []byte) {
	t.Helper()
	payload := signedInboxPayload(t, e.attackerKey, body)
	require.NoError(t, e.inbox.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
}

func (e *ldForwardEnv) noteByURI(t *testing.T, uri string) *model.Note {
	t.Helper()
	for _, n := range e.noteRepo.Notes {
		if n.URI != nil && *n.URI == uri {
			return n
		}
	}
	return nil
}

func mastodonCreate(noteURI string) map[string]any {
	return map[string]any{
		"@context":  mastodonContext(),
		"id":        noteURI + "/activity",
		"type":      "Create",
		"actor":     ldFwdVictimURI,
		"published": "2026-09-26T00:00:00Z",
		"to":        []any{"https://www.w3.org/ns/activitystreams#Public"},
		"cc":        []any{ldFwdVictimURI + "/followers"},
		"object": map[string]any{
			"id":           noteURI,
			"type":         "Note",
			"summary":      nil,
			"inReplyTo":    nil,
			"published":    "2026-09-26T00:00:00Z",
			"url":          "https://m.example/@bob/1",
			"attributedTo": ldFwdVictimURI,
			"to":           []any{"https://www.w3.org/ns/activitystreams#Public"},
			"cc":           []any{ldFwdVictimURI + "/followers"},
			"sensitive":    false,
			"atomUri":      noteURI,
			"content":      "<p>hello from bob</p>",
			"contentMap":   map[string]any{"en": "<p>hello from bob</p>"},
			"tag":          []any{map[string]any{"type": "Hashtag", "href": "https://m.example/tags/fedi", "name": "#fedi"}},
		},
	}
}

func TestInboxProcessor_ForwardedLDSignedCreate_UnsignedMisskeyContentIgnored(t *testing.T) {
	env := newLDForwardEnv(t)
	noteURI := "https://m.example/users/bob/statuses/1"
	body := env.signAsVictim(t, mastodonCreate(noteURI), time.Now(), func(doc map[string]any) {
		obj := doc["object"].(map[string]any)
		obj["_misskey_content"] = "FORGED BY MALLORY"
		obj["_misskey_summary"] = "FORGED CW"
	})
	env.forward(t, body)

	note := env.noteByURI(t, noteURI)
	require.NotNil(t, note, "正しく LD 署名された転送 Create は受理されること")
	require.NotNil(t, note.Text)
	assert.NotContains(t, *note.Text, "FORGED", "署名外の _misskey_content が本文に使われている")
	assert.Contains(t, *note.Text, "hello from bob", "本文は署名された content から作られること")
	assert.Nil(t, note.CW, "署名外の _misskey_summary が CW に使われている")
	assert.Equal(t, env.victimID, note.UserID)
	// compact 後の形 (`to: "as:Public"` の単値 / 単一要素の `tag` がオブジェクト) を
	// 既存の型が読めていること。
	assert.Equal(t, model.NoteVisibilityPublic, note.Visibility, "as:Public (compact 形) を public と読めていない")
	assert.Contains(t, note.Tags, "fedi", "単一要素に畳まれた tag を読めていない")
}

func TestInboxProcessor_ForwardedLDSignedUpdate_UnsignedMisskeyContentIgnored(t *testing.T) {
	env := newLDForwardEnv(t)
	noteURI := "https://m.example/users/bob/statuses/2"
	host := "m.example"
	original := "original"
	env.noteRepo.Notes["n2"] = &model.Note{ID: "n2", URI: &noteURI, UserID: env.victimID, UserHost: &host, Text: &original}

	update := map[string]any{
		"@context": mastodonContext(),
		"id":       noteURI + "#updates/1",
		"type":     "Update",
		"actor":    ldFwdVictimURI,
		"to":       []any{"https://www.w3.org/ns/activitystreams#Public"},
		"object": map[string]any{
			"id":           noteURI,
			"type":         "Note",
			"attributedTo": ldFwdVictimURI,
			"to":           []any{"https://www.w3.org/ns/activitystreams#Public"},
			"content":      "<p>edited by bob</p>",
			"updated":      "2026-09-26T01:00:00Z",
		},
	}
	body := env.signAsVictim(t, update, time.Now(), func(doc map[string]any) {
		doc["object"].(map[string]any)["_misskey_content"] = "FORGED BY MALLORY"
	})
	env.forward(t, body)

	got := env.noteRepo.Notes["n2"]
	require.NotNil(t, got.Text)
	assert.NotContains(t, *got.Text, "FORGED", "署名外の _misskey_content で本文を書き換えられている")
	assert.Contains(t, *got.Text, "edited by bob", "署名された content で更新されること")
}

// **削りすぎていないこと。** Misskey は `_misskey_content` を自分の context で
// 定義して送るので署名に含まれる。転送経路でもそれは本文として使われる。
func TestInboxProcessor_ForwardedLDSignedCreate_SignedMisskeyContentKept(t *testing.T) {
	env := newLDForwardEnv(t)
	noteURI := "https://m.example/notes/abc"
	create := map[string]any{
		"@context": []any{
			"https://www.w3.org/ns/activitystreams",
			"https://w3id.org/security/v1",
			map[string]any{
				"misskey":          "https://misskey-hub.net/ns#",
				"_misskey_content": "misskey:_misskey_content",
			},
		},
		"id":    noteURI + "/activity",
		"type":  "Create",
		"actor": ldFwdVictimURI,
		"to":    []any{"https://www.w3.org/ns/activitystreams#Public"},
		"object": map[string]any{
			"id":               noteURI,
			"type":             "Note",
			"attributedTo":     ldFwdVictimURI,
			"to":               []any{"https://www.w3.org/ns/activitystreams#Public"},
			"content":          "<p>html rendering</p>",
			"_misskey_content": "signed **mfm**",
		},
	}
	env.forward(t, env.signAsVictim(t, create, time.Now(), nil))

	note := env.noteByURI(t, noteURI)
	require.NotNil(t, note)
	require.NotNil(t, note.Text)
	assert.Equal(t, "signed **mfm**", *note.Text)
}

// 署名時刻が古すぎる転送 activity は、署名が正しくても処理しない (replay 対策)。
func TestInboxProcessor_ForwardedLDSignedCreate_StaleSignatureDropped(t *testing.T) {
	env := newLDForwardEnv(t)
	noteURI := "https://m.example/users/bob/statuses/3"
	body := env.signAsVictim(t, mastodonCreate(noteURI), time.Now().Add(-30*24*time.Hour), nil)
	env.forward(t, body)
	assert.Nil(t, env.noteByURI(t, noteURI), "created が窓の外の LD-Signature で転送 activity が処理された")
}

// ldFwdMoveCreated moves `signature.created` to another key that expands to
// the same RDF (dc:created typed xsd:dateTime).
func ldFwdMoveCreated(key, typ string) func(map[string]any) {
	return func(doc map[string]any) {
		sig := doc["signature"].(map[string]any)
		created := sig["created"].(string)
		delete(sig, "created")
		sig[key] = map[string]any{"@value": created, "@type": typ}
	}
}

// **created を別名のキーへ移しても窓は外れない。** options は identity/v1 で
// 正規化されるので、`created` を消して `dc:created` (型付き) や完全 IRI で同じ値を
// 書いても RDF は変わらず署名は通る。キー名 `created` だけを見る判定だと
// 「created 欠落」として窓の検査を素通りし、古い署名をいつまでも再送できた。
func TestInboxProcessor_ForwardedLDSignedCreate_StaleCreatedUnderAliasDropped(t *testing.T) {
	tests := []struct {
		name   string
		noteID string
		tamper func(map[string]any)
	}{
		{name: "compact IRI dc:created", noteID: "4", tamper: ldFwdMoveCreated("dc:created", "xsd:dateTime")},
		{
			name:   "absolute IRI",
			noteID: "5",
			tamper: ldFwdMoveCreated("http://purl.org/dc/terms/created", "http://www.w3.org/2001/XMLSchema#dateTime"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newLDForwardEnv(t)
			noteURI := "https://m.example/users/bob/statuses/" + tc.noteID
			body := env.signAsVictim(t, mastodonCreate(noteURI), time.Now().Add(-400*24*time.Hour), tc.tamper)
			env.forward(t, body)
			assert.Nil(t, env.noteByURI(t, noteURI), "別名キーへ移した古い created で窓を外せている")
		})
	}
}

// 上の別名は RDF として同じなので、**窓の内側なら受理する**。判定がキー名では
// なく正規化後の値を見ていることの裏付け (キー名で弾くだけの実装なら落ちる)。
func TestInboxProcessor_ForwardedLDSignedCreate_FreshCreatedUnderAliasAccepted(t *testing.T) {
	env := newLDForwardEnv(t)
	noteURI := "https://m.example/users/bob/statuses/6"
	body := env.signAsVictim(t, mastodonCreate(noteURI), time.Now(), ldFwdMoveCreated("dc:created", "xsd:dateTime"))
	env.forward(t, body)
	assert.NotNil(t, env.noteByURI(t, noteURI), "RDF として同じ署名を落としている")
}

// authorizeActor は compact 後の文書で actor / id を見直し、handler へ渡すのも
// その文書にする。生 body と compact 後で actor / id が食い違う形は落とす。
func TestInboxProcessor_ForwardedLDSig_RechecksCompactedFields(t *testing.T) {
	raw := []byte(`{"id":"https://origin.example/creates/1","type":"Create","actor":"https://origin.example/users/alice","signature":{"type":"RsaSignature2017","creator":"https://origin.example/users/alice#main-key"}}`)
	tests := []struct {
		name      string
		compacted string
		wantCalls int
	}{
		{
			name:      "compacted body is dispatched",
			compacted: `{"id":"https://origin.example/creates/1","type":"Create","actor":"https://origin.example/users/alice","object":"compacted"}`,
			wantCalls: 1,
		},
		{
			name:      "compacted actor differs",
			compacted: `{"id":"https://origin.example/creates/1","type":"Create","actor":"https://origin.example/users/carol"}`,
		},
		{
			name:      "compacted id host differs",
			compacted: `{"id":"https://other.example/creates/1","type":"Create","actor":"https://origin.example/users/alice"}`,
		},
		{
			name:      "compacted id missing",
			compacted: `{"type":"Create","actor":"https://origin.example/users/alice"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			priv, pub, err := activitypub.GenerateRSAKeypair()
			require.NoError(t, err)
			key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
			require.NoError(t, err)
			relayHost := "relay.example"
			originHost := "origin.example"
			stub := &stubFedProcessor{}
			p := processors.NewInboxProcessor(stub)
			p.SetSignatureVerifier(&multiActorVerifier{
				pubKey: pub,
				byURI: map[string]*model.User{
					"https://relay.example/actor":        {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
					"https://origin.example/users/alice": {ID: "alice", Host: &originHost, URI: uptr("https://origin.example/users/alice")},
				},
			})
			p.SetLDSignatureVerifier(&stubLDVerifier{
				present:       true,
				creator:       "https://origin.example/users/alice#main-key",
				compactedBody: []byte(tc.compacted),
			})
			require.NoError(t, p.Handle(context.Background(), driver.RawTask{
				TypeName: queue.TaskTypeInbox,
				Body:     mustEncode(t, signedInboxPayload(t, key, raw)),
			}))
			require.Len(t, stub.calls, tc.wantCalls)
			if tc.wantCalls > 0 {
				assert.JSONEq(t, tc.compacted, string(stub.calls[0]), "生 body ではなく compact 後の文書を渡すこと")
			}
		})
	}
}
