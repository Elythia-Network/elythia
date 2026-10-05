package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// **MFM の原文を受信側まで運ぶ。** `content` は HTML なので、MFM に戻すと
// 情報が落ちる (装飾 / 色 / `<center>` / 引用の改行 / 数式が消え、絵文字の
// 前後にゼロ幅文字が入る)。note 取り込みは `source` -> `_misskey_content` ->
// `content` の 3 段で拾っており、chat だけ `content` しか見ていなかった。
func TestProcess_CreateChat_CarriesMFMSource(t *testing.T) {
	cases := []struct {
		name    string
		object  string
		wantMFM string
	}{
		{
			name: "source を優先する",
			object: `"content": "<p>$[shake x]</p>",
			"source": {"content": "$[shake ゆれる]", "mediaType": "text/x.misskeymarkdown"},
			"_misskey_content": "こちらは使わない",`,
			wantMFM: "$[shake ゆれる]",
		},
		{
			name: "source が無ければ _misskey_content",
			object: `"content": "<p>x</p>",
			"_misskey_content": "$[fg.color=f00 あか]",`,
			wantMFM: "$[fg.color=f00 あか]",
		},
		{
			name: "mediaType が違う source は使わない",
			object: `"content": "<p>x</p>",
			"source": {"content": "無視される", "mediaType": "text/html"},`,
			wantMFM: "",
		},
		{
			name:    "どちらも無ければ空",
			object:  `"content": "<p>plain</p>",`,
			wantMFM: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, _, _ := newProcessor(t, aliceActor)
			repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
			chatSvc := &stubChatReceiver{}
			p.SetChatService(chatSvc)

			body := []byte(`{
				"id": "https://remote.example/activities/create-chat-src",
				"type": "Create",
				"actor": "https://remote.example/users/alice",
				"object": {
					"id": "https://remote.example/chat-messages/cm-src",
					"type": "Note",
					"attributedTo": "https://remote.example/users/alice",
					` + tc.object + `
					"to": ["https://example.com/users/bob"],
					"_misskey_talk": true
				}
			}`)
			require.NoError(t, p.Process(body))
			require.Equal(t, 1, chatSvc.called)
			assert.Equal(t, tc.wantMFM, chatSvc.lastMFM, "MFM の原文が受信側へ渡っていない")
		})
	}
}

// `Misskey:ChatMessage` (activity 自体が message) の経路も同じ 3 段で拾う。
func TestProcess_ChatMessage_CarriesMFMSource(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)

	body := []byte(`{
		"id": "https://remote.example/chat-messages/direct-src",
		"type": "Misskey:ChatMessage",
		"actor": "https://remote.example/users/alice",
		"attributedTo": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"content": "<p>$[shake x]</p>",
		"source": {"content": "$[shake ゆれる]", "mediaType": "text/x.misskeymarkdown"}
	}`)
	require.NoError(t, p.Process(body))
	require.Equal(t, 1, chatSvc.called)
	assert.Equal(t, "$[shake ゆれる]", chatSvc.lastMFM, "MFM の原文が受信側へ渡っていない")
}
