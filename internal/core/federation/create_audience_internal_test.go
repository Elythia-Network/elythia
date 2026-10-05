package federation

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeMergedNote unmarshals the result of mergeCreateAudience into the same
// activitypub.Note shape IngestNoteWithCreated parses, so assertions read the
// exact fields downstream visibility derivation consumes.
func decodeMergedNote(t *testing.T, raw json.RawMessage) activitypub.Note {
	t.Helper()
	var n activitypub.Note
	require.NoError(t, json.Unmarshal(raw, &n))
	return n
}

// TestMergeCreateAudience is a white-box test for the #1560 audience union.
// Black-box Process() tests verify the end-to-end visibility outcome, but the
// helper is exercised here directly so each branch (union order, dedup,
// attributedTo fill, degrade paths) is sealed independent of the ingest plumbing.
func TestMergeCreateAudience(t *testing.T) {
	const actor = "https://remote.example/users/alice"
	const followers = "https://remote.example/users/alice/followers"

	t.Run("activity_to_cc_unioned_onto_note_object", func(t *testing.T) {
		// Note object 自身は to/cc を持たず、audience は Create 側のみ。
		// 本家挙動どおり union 後の object には followers + Public が乗る。
		raw := []byte(`{
			"type": "Create",
			"actor": "` + actor + `",
			"to": ["` + followers + `"],
			"cc": ["` + activitypub.Public + `"],
			"object": {
				"type": "Note",
				"id": "https://remote.example/notes/1",
				"attributedTo": "` + actor + `",
				"content": "hi"
			}
		}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		merged := mergeCreateAudience(act)
		note := decodeMergedNote(t, merged)
		assert.Equal(t, []string{followers}, []string(note.To))
		assert.Equal(t, []string{activitypub.Public}, []string(note.CC))
		// union された to/cc から home が導出される (followers in to + Public in cc)。
		assert.Equal(t, "home", string(deriveVisibility(remoteActor(actor), note.To, note.CC)))
	})

	t.Run("union_dedups_and_preserves_activity_then_object_order", func(t *testing.T) {
		raw := []byte(`{
			"type": "Create",
			"actor": "` + actor + `",
			"to": ["` + activitypub.Public + `", "https://x/a"],
			"object": {
				"type": "Note",
				"id": "https://remote.example/notes/2",
				"attributedTo": "` + actor + `",
				"to": ["https://x/a", "https://x/b"]
			}
		}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		note := decodeMergedNote(t, mergeCreateAudience(act))
		// activity 側を先、object 側を後ろに連結し重複 (https://x/a) は 1 回のみ。
		assert.Equal(t, []string{activitypub.Public, "https://x/a", "https://x/b"}, []string(note.To))
	})

	t.Run("audience_as_single_string_is_accepted", func(t *testing.T) {
		// to が array でなく単一 string でも APIDList 経由で拾う。
		raw := []byte(`{
			"type": "Create",
			"actor": "` + actor + `",
			"to": "` + activitypub.Public + `",
			"object": {
				"type": "Note",
				"id": "https://remote.example/notes/3",
				"attributedTo": "` + actor + `"
			}
		}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		note := decodeMergedNote(t, mergeCreateAudience(act))
		assert.Equal(t, []string{activitypub.Public}, []string(note.To))
		assert.Equal(t, "public", string(deriveVisibility(remoteActor(actor), note.To, note.CC)))
	})

	// upstream の parseAudience は getApIds -> getApId なので `{"id": ...}` 形式の
	// audience も id として読む。`[]string` 決め打ちだと object 要素を落として
	// **public のはずの note が followers になる** (#2662)。
	t.Run("audience_as_id_object_is_accepted", func(t *testing.T) {
		raw := []byte(`{
			"type": "Create",
			"actor": "` + actor + `",
			"to": {"id": "` + activitypub.Public + `"},
			"object": {
				"type": "Note",
				"id": "https://remote.example/notes/9",
				"attributedTo": "` + actor + `",
				"to": ["` + followers + `"]
			}
		}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		note := decodeMergedNote(t, mergeCreateAudience(act))
		assert.Equal(t, []string{activitypub.Public, followers}, []string(note.To))
		assert.Equal(t, "public", string(deriveVisibility(remoteActor(actor), note.To, note.CC)))
	})

	// 読めない要素は落とす。upstream は getApId が throw して note ごと reject
	// するが、mk-go は残りを活かす (可視性は狭い側に倒れる)。
	t.Run("audience_with_unreadable_element_drops_it", func(t *testing.T) {
		raw := []byte(`{
			"type": "Create",
			"actor": "` + actor + `",
			"to": ["` + activitypub.Public + `", 42, {"type": "Note"}],
			"object": {
				"type": "Note",
				"id": "https://remote.example/notes/10",
				"attributedTo": "` + actor + `"
			}
		}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		note := decodeMergedNote(t, mergeCreateAudience(act))
		assert.Equal(t, []string{activitypub.Public}, []string(note.To))
	})

	t.Run("missing_attributedTo_filled_from_actor", func(t *testing.T) {
		raw := []byte(`{
			"type": "Create",
			"actor": "` + actor + `",
			"object": {
				"type": "Note",
				"id": "https://remote.example/notes/4",
				"content": "no attributedTo"
			}
		}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		note := decodeMergedNote(t, mergeCreateAudience(act))
		assert.Equal(t, actor, note.AttributedTo.String())
	})

	t.Run("present_attributedTo_not_overwritten", func(t *testing.T) {
		raw := []byte(`{
			"type": "Create",
			"actor": "` + actor + `",
			"object": {
				"type": "Note",
				"id": "https://remote.example/notes/5",
				"attributedTo": "https://remote.example/users/bob",
				"to": ["` + activitypub.Public + `"]
			}
		}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		note := decodeMergedNote(t, mergeCreateAudience(act))
		assert.Equal(t, "https://remote.example/users/bob", note.AttributedTo.String())
	})

	t.Run("no_audience_no_change_returns_original_object", func(t *testing.T) {
		// activity / object 双方に to/cc が無く attributedTo もある場合は
		// 元の object をそのまま素通しする (= byte 同一)。
		obj := `{"type":"Note","id":"https://remote.example/notes/6","attributedTo":"` + actor + `"}`
		raw := []byte(`{"type":"Create","actor":"` + actor + `","object":` + obj + `}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		merged := mergeCreateAudience(act)
		assert.JSONEq(t, obj, string(merged))
		// object に audience 追加が無いので元 RawMessage と byte 一致する。
		assert.Equal(t, []byte(obj), []byte(merged))
	})

	t.Run("object_audience_only_is_preserved_without_rewrite", func(t *testing.T) {
		// activity 側に audience が無く object 側だけにある純粋な一般 note は
		// 書き戻し不要なので元の object を素通しする。
		obj := `{"type":"Note","id":"https://remote.example/notes/7","attributedTo":"` + actor + `","to":["` + activitypub.Public + `"]}`
		raw := []byte(`{"type":"Create","actor":"` + actor + `","object":` + obj + `}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)

		merged := mergeCreateAudience(act)
		assert.Equal(t, []byte(obj), []byte(merged))
		note := decodeMergedNote(t, merged)
		assert.Equal(t, "public", string(deriveVisibility(remoteActor(actor), note.To, note.CC)))
	})

	t.Run("empty_object_returns_input", func(t *testing.T) {
		var act genericActivity
		assert.Nil(t, mergeCreateAudience(act))
	})

	t.Run("non_object_string_iri_returns_input", func(t *testing.T) {
		// object が embedded JSON object でなく string IRI のケースは degrade。
		raw := []byte(`{"type":"Create","actor":"` + actor + `","object":"https://remote.example/notes/8"}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)
		merged := mergeCreateAudience(act)
		assert.Equal(t, `"https://remote.example/notes/8"`, string(merged))
	})

	t.Run("json_null_object_returns_input", func(t *testing.T) {
		// object: null は map unmarshal で nil になるので元の object を返す。
		raw := []byte(`{"type":"Create","actor":"` + actor + `","object":null}`)
		var act genericActivity
		_ = json.Unmarshal(raw, &act)
		merged := mergeCreateAudience(act)
		// act.Object は "null" literal、または空。どちらでも degrade で OK。
		assert.True(t, len(merged) == 0 || string(merged) == "null")
	})
}

// TestDecodeAudience covers the string/array/null parsing of an AP audience field.
func TestDecodeAudience(t *testing.T) {
	t.Run("nil_input", func(t *testing.T) {
		assert.Nil(t, decodeAudience(nil))
	})
	t.Run("null_literal", func(t *testing.T) {
		assert.Nil(t, decodeAudience(json.RawMessage(`null`)))
	})
	t.Run("single_string", func(t *testing.T) {
		assert.Equal(t, []string{"https://x/a"}, decodeAudience(json.RawMessage(`"https://x/a"`)))
	})
	t.Run("array", func(t *testing.T) {
		assert.Equal(t, []string{"a", "b"}, decodeAudience(json.RawMessage(`["a","b"]`)))
	})
	t.Run("invalid_json_yields_nil", func(t *testing.T) {
		assert.Nil(t, decodeAudience(json.RawMessage(`{`)))
	})
}

// TestUnionStrings covers the order-preserving dedup semantics.
func TestUnionStrings(t *testing.T) {
	t.Run("both_empty", func(t *testing.T) {
		assert.Nil(t, unionStrings(nil, nil))
	})
	t.Run("a_only", func(t *testing.T) {
		assert.Equal(t, []string{"a", "b"}, unionStrings([]string{"a", "b"}, nil))
	})
	t.Run("b_only", func(t *testing.T) {
		assert.Equal(t, []string{"c"}, unionStrings(nil, []string{"c"}))
	})
	t.Run("dedup_across_and_within", func(t *testing.T) {
		assert.Equal(t, []string{"a", "b", "c"}, unionStrings([]string{"a", "a", "b"}, []string{"b", "c"}))
	})
}

// TestEncodeAudienceIfChanged covers the no-write and rewrite branches.
func TestEncodeAudienceIfChanged(t *testing.T) {
	t.Run("empty_merged_no_write", func(t *testing.T) {
		out, ok := encodeAudienceIfChanged(nil, nil)
		assert.False(t, ok)
		assert.Nil(t, out)
	})
	t.Run("identical_array_no_write", func(t *testing.T) {
		_, ok := encodeAudienceIfChanged(json.RawMessage(`["a","b"]`), []string{"a", "b"})
		assert.False(t, ok)
	})
	t.Run("changed_writes_array", func(t *testing.T) {
		out, ok := encodeAudienceIfChanged(json.RawMessage(`"a"`), []string{"a", "b"})
		require.True(t, ok)
		assert.JSONEq(t, `["a","b"]`, string(out))
	})
	t.Run("missing_original_writes_array", func(t *testing.T) {
		out, ok := encodeAudienceIfChanged(nil, []string{"a"})
		require.True(t, ok)
		assert.JSONEq(t, `["a"]`, string(out))
	})
}

// remoteActor returns a remote user whose actor URI is uri and whose
// followersUri is unknown.
func remoteActor(uri string) *model.User {
	host := "remote.example"
	return &model.User{ID: "remote-" + uri, URI: &uri, Host: &host}
}

// TestDeriveVisibility seals the parseAudience-faithful mapping (#1864):
// to-Public→public, cc-Public→home (no followers-in-to requirement),
// the author's own followers collection in to OR cc→followers, else
// specified. Also covers the as:Public / bare Public aliases upstream
// isPublic accepts.
func TestDeriveVisibility(t *testing.T) {
	// Mastodon 形式: actor が /users/alice、followers は uri + "/followers"。
	const mastodonActor = "https://remote.example/users/alice"
	const mastodonFollowers = mastodonActor + "/followers"
	// Misskey 形式: actor が /users/<id>、followers も uri + "/followers"。
	const misskeyActor = "https://misskey.example/users/9abcdefghi"
	const misskeyFollowers = misskeyActor + "/followers"
	// followersUri が uri + "/followers" と異なる実装。
	const customActor = "https://blog.example/author/carol"
	const customFollowers = "https://blog.example/wp-json/activitypub/1.0/actors/1/followers"
	custom := remoteActor(customActor)
	cf := customFollowers
	custom.FollowersURI = &cf
	// 他人の followers collection。本家 isFollowers は一致しないので other に入れる。
	const otherFollowers = "https://remote.example/users/bob/followers"

	mastodon := remoteActor(mastodonActor)
	cases := []struct {
		name  string
		actor *model.User
		to    []string
		cc    []string
		want  string
	}{
		{"to has Public", mastodon, []string{activitypub.Public}, []string{mastodonFollowers}, "public"},
		{"unlisted: followers in to, Public in cc", mastodon, []string{mastodonFollowers}, []string{activitypub.Public}, "home"},
		{"cc-only Public, no followers in to", mastodon, nil, []string{activitypub.Public}, "home"},
		{"cc-only Public with mention in to", mastodon, []string{"https://x/u1"}, []string{activitypub.Public}, "home"},
		{"mastodon followers in to only", mastodon, []string{mastodonFollowers}, nil, "followers"},
		{"mastodon followers in cc only", mastodon, nil, []string{mastodonFollowers}, "followers"},
		{"misskey followers in to", remoteActor(misskeyActor), []string{misskeyFollowers}, nil, "followers"},
		{"stored followersUri in to", custom, []string{customFollowers}, nil, "followers"},
		{"uri-derived followers ignored when followersUri is stored", custom, []string{customActor + "/followers"}, nil, "specified"},
		{"another actor's followers collection is not followers", mastodon, []string{otherFollowers}, nil, "specified"},
		{"another actor's followers in cc is not followers", mastodon, []string{"https://x/u1"}, []string{otherFollowers}, "specified"},
		{"followers with trailing slash is not followers", mastodon, []string{mastodonFollowers + "/"}, nil, "specified"},
		{"nil actor never matches followers", nil, []string{mastodonFollowers}, nil, "specified"},
		{"actor without uri never matches followers", &model.User{}, []string{"/followers"}, nil, "specified"},
		{"specified: only mentions", mastodon, []string{"https://x/u1"}, nil, "specified"},
		{"empty audience", mastodon, nil, nil, "specified"},
		{"as:Public alias in to", mastodon, []string{"as:Public"}, nil, "public"},
		{"bare Public alias in cc", mastodon, nil, []string{"Public"}, "home"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, string(deriveVisibility(tc.actor, tc.to, tc.cc)))
		})
	}
}
