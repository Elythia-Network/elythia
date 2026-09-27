package ld

// InboxCompactContext returns the JSON-LD context an LD-signed inbound
// activity is compacted into before it is verified and handed to the
// activity handlers. It mirrors upstream Misskey TS `CONTEXT`
// (`core/activitypub/misc/contexts.ts`: AS 2.0 + W3C Security v1 + the
// extension term definitions), so the compacted document has the same shape
// upstream's InboxProcessorService passes to ApInboxService.
//
// A fresh value is returned on every call because json-gold may keep
// references into the context while processing; sharing one instance across
// requests would let one compaction observe another's state.
func InboxCompactContext() []any {
	// **upstream の CONTEXT と同じ語彙にしておくことが要点。** compact 後の
	// 文書に現れるキーは「署名された RDF の述語のうち、この context で短縮
	// できたもの」だけになる。ここに無い語は完全 IRI (例: `toot:blurhash`) で
	// 残って handler からは見えなくなり、逆に AS2 の `@vocab: "_:"` で blank
	// node IRI になった述語 (= 署名対象外) は `_:<name>` のまま残るので、
	// `_misskey_content` のような生キーとして読まれることはない。
	//
	// mk-go の送信用 context (activitypub.MisskeyContext) とは 2 点違う:
	// `quoteUrl` を upstream は `as:quoteUrl`、mk-go は `misskey:quoteUrl` に
	// 割り当てており、`schema` の IRI も `http://schema.org#` と
	// `http://schema.org/` で違う。受信側は upstream と同じ形を handler に
	// 渡すのが目的なので upstream に合わせる (mk-go 由来の転送活動では
	// `quoteUrl` が `misskey:quoteUrl` のまま残るが、引用は `_misskey_quote`
	// でも届く)。
	return []any{
		"https://www.w3.org/ns/activitystreams",
		"https://w3id.org/security/v1",
		map[string]any{
			"Key": "sec:Key",
			// as non-standards
			"manuallyApprovesFollowers": "as:manuallyApprovesFollowers",
			"sensitive":                 "as:sensitive",
			"Hashtag":                   "as:Hashtag",
			"quoteUrl":                  "as:quoteUrl",
			// Mastodon
			"toot":         "http://joinmastodon.org/ns#",
			"Emoji":        "toot:Emoji",
			"featured":     "toot:featured",
			"discoverable": "toot:discoverable",
			// schema
			"schema":        "http://schema.org#",
			"PropertyValue": "schema:PropertyValue",
			"value":         "schema:value",
			// Misskey
			"misskey":                               "https://misskey-hub.net/ns#",
			"_misskey_content":                      "misskey:_misskey_content",
			"_misskey_quote":                        "misskey:_misskey_quote",
			"_misskey_reaction":                     "misskey:_misskey_reaction",
			"_misskey_votes":                        "misskey:_misskey_votes",
			"_misskey_summary":                      "misskey:_misskey_summary",
			"_misskey_followedMessage":              "misskey:_misskey_followedMessage",
			"_misskey_requireSigninToViewContents":  "misskey:_misskey_requireSigninToViewContents",
			"_misskey_makeNotesFollowersOnlyBefore": "misskey:_misskey_makeNotesFollowersOnlyBefore",
			"_misskey_makeNotesHiddenBefore":        "misskey:_misskey_makeNotesHiddenBefore",
			"_misskey_license":                      "misskey:_misskey_license",
			"freeText": map[string]any{
				"@id":   "misskey:freeText",
				"@type": "schema:text",
			},
			"isCat": "misskey:isCat",
			// vcard
			"vcard": "http://www.w3.org/2006/vcard/ns#",
		},
	}
}
