package queue_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// **配送・受信の Enqueue の入口で積む payload は、driver.CompactJobPayloads に
// 従ったバイト列であること** (#3614。2.1.0 のレビュー 2 周目の M-1)。有効なら
// MarshalCompactJSON と同じ、false なら #3614 より前の版と同じ。読み戻して比べる
// テストは、読む側がどちらの形も読むので形が変わっても通ってしまう。バイト列で
// 比べる。
// Inbox / Path / Headers に & < > を入れてあるのは、HTML のエスケープの有無も
// バイト列に出るようにするため (定数が false のときは本文が base64 になるので
// 出ない。2.1.0 のレビュー 3 周目の L-2)。
func TestClient_EnqueueWritesTheShapeSelectedByCompactJobPayloads(t *testing.T) {
	for name, body := range map[string]string{
		// mk-go の json.Marshal が描く配送の本文 (< はエスケープ済み)。
		"marshalled": `{"type":"Create","object":{"content":"\u003cp\u003ehi\u003c/p\u003e"}}`,
		// Misskey の本文 (< が生)。
		"misskey": `{"type":"Create","object":{"content":"<p>今日は` + strings.Repeat("晴れ", 40) + `</p>"}}`,
		"pretty":  "{\n  \"type\": \"Create\"\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, rc := newRecordingClient(nil)
			dp := queue.DeliverPayload{Inbox: "https://remote.example/inbox?a=1&b=<x>", Body: []byte(body), KeyID: "k", SignerUserID: "u1"}
			ip := queue.InboxPayload{Body: []byte(body), Method: "POST", Path: "/inbox?a=1&b=<x>", Headers: map[string]string{"Digest": "SHA-256=x", "Signature": `keyId="https://remote.example/u#k&<>"`}}
			require.NoError(t, c.EnqueueDeliver(dp))
			require.NoError(t, c.EnqueueInbox(context.Background(), ip))
			require.Len(t, rc.enqueued, 2)

			wantDeliver, wantInbox := queue.Pre3614DeliverJSON(t, dp), queue.Pre3614InboxJSON(t, ip)
			if driver.CompactJobPayloads {
				var err error
				wantDeliver, err = dp.MarshalCompactJSON()
				require.NoError(t, err)
				wantInbox, err = ip.MarshalCompactJSON()
				require.NoError(t, err)
			}
			assert.Equal(t, queue.TaskTypeDeliver, rc.enqueued[0].taskType)
			assert.Equal(t, string(wantDeliver), string(rc.enqueued[0].payload))
			assert.Equal(t, queue.TaskTypeInbox, rc.enqueued[1].taskType)
			assert.Equal(t, string(wantInbox), string(rc.enqueued[1].payload))
		})
	}
}

// **配送・受信の payload には、driver の検査を省かせる印を付ける** (#3614)。
// 1 つの投稿を inbox の数だけ積む配送で、同じ検査を宛先ごとに繰り返さないため。
// 付けてよいのは、MarshalJSON がどちらの形でも encoding/json と同じ書き方の
// object を返すからで、それは FuzzActivityBodyRoundTrip (mkqdriver) が確かめる。
func TestClient_DeliverAndInboxMarkTheirPayloadsVerbatim(t *testing.T) {
	c, rc := newRecordingClient(nil)
	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{Inbox: "https://remote.example/inbox", Body: []byte(`{"type":"Create"}`), KeyID: "k", SignerUserID: "u1"}))
	require.NoError(t, c.EnqueueInbox(context.Background(), queue.InboxPayload{Body: []byte(`{"type":"Create"}`)}))
	require.Len(t, rc.enqueued, 2)
	assert.True(t, rc.enqueued[0].opts.VerbatimJSONPayload, "deliver")
	assert.True(t, rc.enqueued[1].opts.VerbatimJSONPayload, "inbox")
}
