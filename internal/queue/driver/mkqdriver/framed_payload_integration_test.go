package mkqdriver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/mkq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// receiveOne starts the server with a handler for taskType and returns the
// payload of the first job it receives.
func receiveOne(t *testing.T, srv driver.Server, taskType string) func() []byte {
	t.Helper()
	var (
		mu  sync.Mutex
		got []byte
		wg  sync.WaitGroup
	)
	wg.Add(1)
	srv.Handle(taskType, func(_ context.Context, task driver.Task) error {
		mu.Lock()
		defer mu.Unlock()
		if got == nil {
			got = append([]byte{}, task.Payload()...)
			wg.Done()
		}
		return nil
	})
	require.NoError(t, srv.Start())
	t.Cleanup(srv.Shutdown)
	return func() []byte {
		if !waitGroupTimeout(&wg, 10*time.Second) {
			t.Fatal("handler not invoked within timeout")
		}
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// jobData returns the BullMQ `data` field of the only job in queue.
func jobData(t *testing.T, queueName string) string {
	t.Helper()
	ctx := context.Background()
	all, err := testRedis.Client.Keys(ctx, "bull:"+queueName+":[0-9]*").Result()
	require.NoError(t, err)
	// worker が取り出すと `bull:<queue>:<id>:lock` も並ぶので、job の HASH だけ残す。
	var keys []string
	for _, k := range all {
		if strings.Count(k, ":") == 2 {
			keys = append(keys, k)
		}
	}
	require.Len(t, keys, 1, "%v", all)
	data, err := testRedis.Client.HGet(ctx, keys[0], "data").Result()
	require.NoError(t, err)
	return data
}

// **受信の本文は Redis を通っても 1 バイトも変わらないこと (#3614)。** 受信の
// 本文は受付で Digest と照合したバイト列で、worker はそれをそのまま処理する。
// Misskey の本文は < をエスケープせず、日本語を含む。空白を含む本文 (詰まって
// いない JSON) も届く。
//
// 2 つの書き方を通す。"current" はこの版の Enqueue (書く側を有効にするまでは
// 以前の形)。"compact" は書く側を有効にしたときの形を mkq で直接積んだもの
// (読む側を先に出す 2 段の、2 段目の job)。compact では、本文が二重の base64 に
// ならず、data がほぼ本文の大きさに収まることも見る。
func TestFramedPayload_InboxBodySurvivesRedisByteForByte(t *testing.T) {
	type compactFramed struct {
		Type     string          `json:"type"`
		BodyJSON json.RawMessage `json:"bodyJson"`
	}
	for name, body := range map[string]string{
		"misskey":    `{"type":"Create","object":{"content":"<p>今日はいい天気でした &amp; 散歩</p>","_misskey_content":"今日はいい天気でした"}}`,
		"compact":    `{"type":"Create","object":{"content":"hello"}}`,
		"whitespace": "{\n  \"type\": \"Create\",\n  \"object\": { \"content\": \"<b>x</b>\" }\n}\n",
	} {
		for _, writer := range []string{"current", "compact"} {
			t.Run(name+"/"+writer, func(t *testing.T) {
				d := newDriver(t)
				wait := receiveOne(t, d.Server(), queue.TaskTypeInbox)
				// 大きさの比較が本文に引きずられるよう、本文を伸ばす。
				long := strings.Replace(body, "Create", "Create"+strings.Repeat("x", 2000), 1)
				ip := queue.InboxPayload{Body: []byte(long), Method: "POST", Path: "/inbox", Headers: map[string]string{"Digest": "SHA-256=x"}}
				var payload []byte
				var err error
				if writer == "current" {
					payload, err = json.Marshal(ip)
					require.NoError(t, err)
					require.NoError(t, d.Client().Enqueue(context.Background(), queue.TaskTypeInbox, payload, driver.WithQueue("inbox")))
					data := jobData(t, "inbox")
					if driver.CompactJobPayloads {
						assert.Contains(t, data, `"bodyJson":{`, "書く側を有効にしたら新しい形で積む")
					} else {
						assert.NotContains(t, data, `"bodyJson"`, "この版はまだ以前の形で積む")
						assert.Contains(t, data, `"body":"`)
					}
				} else {
					payload, err = ip.MarshalCompactJSON()
					require.NoError(t, err)
					c, err := mkq.NewClient(context.Background(), mkq.Config{Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}}})
					require.NoError(t, err)
					t.Cleanup(func() { _ = c.Close() })
					_, err = mkq.Define[compactFramed](c, "inbox").Add(context.Background(), compactFramed{Type: queue.TaskTypeInbox, BodyJSON: payload}, mkq.WithJobName(queue.TaskTypeInbox))
					require.NoError(t, err)
					data := jobData(t, "inbox")
					assert.Contains(t, data, `"bodyJson":{`, "payload を base64 にしない")
					assert.Less(t, len(data), len(long)*12/10, "data は本文の 1.2 倍に収まる: %d / %d", len(data), len(long))
				}

				got := wait()
				assert.Equal(t, string(payload), string(got))
				decoded, err := queue.DecodeInboxPayload(got)
				require.NoError(t, err)
				assert.True(t, bytes.Equal([]byte(long), decoded.Body), "本文が変わった:\n got %q\nwant %q", decoded.Body, long)
			})
		}
	}
}

// **更新の前に積まれた job (外側も本文も base64) を、新しい worker が処理できる
// こと。** 再送を待つ job は Redis に約 32 時間 (ブレーカーで待たされる job は
// もっと長く) 残る。以前の版と同じ形の data を mkq で直接積んで確かめる。
func TestFramedPayload_ProcessesJobsEnqueuedBefore3614(t *testing.T) {
	d := newDriver(t)
	wait := receiveOne(t, d.Server(), queue.TaskTypeDeliver)

	// mk-go の json.Marshal が書く本文 (< はエスケープ済み)。
	activity, err := json.Marshal(map[string]any{"type": "Create", "object": map[string]any{"content": "<p>hi</p>"}})
	require.NoError(t, err)
	type pre3614Deliver struct {
		Inbox        string `json:"inbox"`
		Body         []byte `json:"body"`
		KeyID        string `json:"keyId"`
		SignerUserID string `json:"signerUserId,omitempty"`
	}
	type pre3614Framed struct {
		Type string `json:"type"`
		Body []byte `json:"body,omitempty"`
	}
	inner, err := json.Marshal(pre3614Deliver{Inbox: "https://remote.example/inbox", Body: activity, KeyID: "k", SignerUserID: "u1"})
	require.NoError(t, err)
	c, err := mkq.NewClient(context.Background(), mkq.Config{Redis: redis.UniversalOptions{Addrs: []string{testRedis.Addr}}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = mkq.Define[pre3614Framed](c, "deliver").Add(context.Background(), pre3614Framed{Type: queue.TaskTypeDeliver, Body: inner}, mkq.WithJobName(queue.TaskTypeDeliver))
	require.NoError(t, err)
	assert.Contains(t, jobData(t, "deliver"), `"body":"`, "以前の形 (base64) で積めていること")

	got := wait()
	assert.Equal(t, string(inner), string(got))
	p, err := queue.DecodeDeliverPayload(got)
	require.NoError(t, err)
	assert.Equal(t, string(activity), string(p.Body))
	assert.Equal(t, "https://remote.example/inbox", p.Inbox)
	assert.Equal(t, "u1", p.SignerUserID)
}

// 構造の形だけ整った不正な JSON (`{"a":}`) は、書く側を有効にすると埋め込みの
// 候補になるが mkq の json.Marshal で失敗する。以前の形 (base64) で積み直し、
// そのまま届くこと (積み直しそのものは TestAddFramed_FallsBackOnInvalidEmbeddedJSON
// が書く側を有効にした形で見る)。定期実行の登録も同じ。
func TestFramedPayload_InvalidJSONFallsBackToBase64(t *testing.T) {
	d := newDriver(t)
	wait := receiveOne(t, d.Server(), "test:raw")
	bad := []byte(`{"a":}`)
	require.NoError(t, d.Client().Enqueue(context.Background(), "test:raw", bad, driver.WithQueue("deliver")))
	assert.Contains(t, jobData(t, "deliver"), `"body":"eyJhIjp9"`)
	assert.Equal(t, string(bad), string(wait()))

	// 定期実行の登録も同じく積み直す。
	sched := d.Scheduler()
	require.NoError(t, sched.Register("0 0 * * *", "task:bad", bad, driver.WithQueue("maintenance")))
	require.NoError(t, sched.Register("0 0 * * *", "task:json", []byte(`{"a":1}`), driver.WithQueue("maintenance")))
}

// scheduleData returns the job data strings mkq stored for cron registrations
// of taskType (the repeat template), found by scanning every hash in Redis.
func scheduleData(t *testing.T, taskType string) []string {
	t.Helper()
	ctx := context.Background()
	keys, err := testRedis.Client.Keys(ctx, "*").Result()
	require.NoError(t, err)
	var out []string
	for _, k := range keys {
		if typ, err := testRedis.Client.Type(ctx, k).Result(); err != nil || typ != "hash" {
			continue
		}
		fields, err := testRedis.Client.HGetAll(ctx, k).Result()
		require.NoError(t, err)
		for _, v := range fields {
			if strings.Contains(v, `"type":"`+taskType+`"`) {
				out = append(out, v)
			}
		}
	}
	return out
}

// **定期実行の登録も、書く側を有効にするまでは以前の形で積むこと** (#3614 の
// 1 段目。レビュー 2 周目の L-1)。payload を持つ定期実行はプラグインの cron で、
// 登録した template から作られる次の 1 回が、戻した後の古い版にも渡る。
func TestScheduler_RegisterWritesTheShapeSelectedByCompactJobPayloads(t *testing.T) {
	d := newDriver(t)
	require.NoError(t, d.Scheduler().Register("0 0 * * *", "task:json3614", []byte(`{"a":"x"}`), driver.WithQueue("maintenance")))
	data := scheduleData(t, "task:json3614")
	require.NotEmpty(t, data, "登録した template が Redis に見つからない")
	for _, v := range data {
		if driver.CompactJobPayloads {
			assert.Contains(t, v, `"bodyJson":{"a":"x"}`)
		} else {
			assert.NotContains(t, v, `"bodyJson"`, "この版はまだ以前の形で積む")
			assert.Contains(t, v, `"body":"eyJhIjoieCJ9"`)
		}
	}
}
