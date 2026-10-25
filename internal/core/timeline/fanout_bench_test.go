package timeline

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/event"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
)

// roundTripCounter is a go-redis hook counting the round trips to Redis: one
// per plain command, one per pipeline.
type roundTripCounter struct{ n int64 }

func (c *roundTripCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *roundTripCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.n++
		return next(ctx, cmd)
	}
}

func (c *roundTripCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.n++
		return next(ctx, cmds)
	}
}

// redisCPUAndCalls reads the server CPU seconds (user+sys) and the number of
// calls per command from INFO.
func redisCPUAndCalls(b *testing.B) (float64, map[string]int64) {
	b.Helper()
	ctx := context.Background()
	info, err := testRedis.Client.Info(ctx, "cpu", "commandstats").Result()
	if err != nil {
		b.Fatalf("info: %v", err)
	}
	var cpu float64
	calls := map[string]int64{}
	for _, line := range strings.Split(info, "\r\n") {
		switch {
		case strings.HasPrefix(line, "used_cpu_user:"), strings.HasPrefix(line, "used_cpu_sys:"):
			v, _ := strconv.ParseFloat(line[strings.IndexByte(line, ':')+1:], 64)
			cpu += v
		case strings.HasPrefix(line, "cmdstat_"):
			name := line[len("cmdstat_"):strings.IndexByte(line, ':')]
			rest := line[strings.IndexByte(line, ':')+1:]
			for _, kv := range strings.Split(rest, ",") {
				if strings.HasPrefix(kv, "calls=") {
					n, _ := strconv.ParseInt(kv[len("calls="):], 10, 64)
					calls[name] = n
				}
			}
		}
	}
	return cpu, calls
}

// redisMaxOutputBuffer reads client_recent_max_output_buffer: the biggest
// output buffer any client had in the last few seconds.
func redisMaxOutputBuffer(b *testing.B) int64 {
	b.Helper()
	info, err := testRedis.Client.Info(context.Background(), "clients").Result()
	if err != nil {
		b.Fatalf("info clients: %v", err)
	}
	for _, line := range strings.Split(info, "\r\n") {
		if v, ok := strings.CutPrefix(line, "client_recent_max_output_buffer:"); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return -1
}

// processCPU returns the user+sys CPU time this process has used.
func processCPU() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkFanoutHook_Followers measures one OnNoteCreated for a local author
// with N local followers, through the real NotePublisher and PubSubService.
// Reports the Redis commands and the Redis server CPU per note as well.
func BenchmarkFanoutHook_Followers(b *testing.B) {
	type benchCase struct {
		followers  int
		subscribed bool
	}
	for _, c := range []benchCase{{22, false}, {300, false}, {300, true}} {
		n := c.followers
		name := fmt.Sprintf("followers=%d", n)
		if c.subscribed {
			name += "/subscribed"
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			testRedis.FlushAll(ctx)
			client := redis.NewClient(testRedis.Client.Options())
			defer client.Close()
			rt := &roundTripCounter{}
			client.AddHook(rt)
			fanout := NewFanoutTimelineService(client, idGen, "")
			following := testutil.NewMockFollowingRepository()
			for i := 0; i < n; i++ {
				fid := fmt.Sprintf("f%04d", i)
				following.Followings[fid] = &model.Following{ID: fid, FollowerID: "follower" + fid, FolloweeID: "author", WithReplies: true}
			}
			h := NewFanoutHook(fanout, following)
			ps := event.NewPubSubService(client, "bench:")
			defer ps.Close()
			h.SetStreamingPublisher(stream.NewNotePublisher(ps, idGen, nil))
			if c.subscribed {
				// 本番と同じく、自プロセスが <prefix>* を PSUBSCRIBE している状態に
				// する。フォロワー宛ての PUBLISH はすべてこの接続へ流れ、購読者の
				// いない宛先の分は enqueue で捨てられる。
				unsub := ps.Subscribe(ctx, "bench-sink", func([]byte) {})
				defer unsub()
				time.Sleep(200 * time.Millisecond)
			}
			author := &model.User{ID: "author", Username: "author"}
			text := strings.Repeat("hello ", 40)

			testRedis.Client.ConfigResetStat(ctx)
			cpu0, _ := redisCPUAndCalls(b)
			go0 := processCPU()
			rt.n = 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				note := &model.Note{ID: idGen.Generate(time.Now()), UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic}
				h.OnNoteCreated(note, author)
			}
			b.StopTimer()
			if c.subscribed {
				// 受信 goroutine が流れ込んだ分を読み終えるのを待ってから CPU を読む。
				time.Sleep(300 * time.Millisecond)
			}
			goCPU := processCPU() - go0
			b.ReportMetric(float64(goCPU.Microseconds())/float64(b.N), "go-cpu-us/op")
			cpu1, calls := redisCPUAndCalls(b)
			b.ReportMetric((cpu1-cpu0)*1e6/float64(b.N), "redis-cpu-us/op")
			var total int64
			for name, c := range calls {
				if name == "info" || name == "config|resetstat" {
					continue
				}
				total += c
			}
			b.ReportMetric(float64(total)/float64(b.N), "redis-cmds/op")
			b.ReportMetric(float64(rt.n)/float64(b.N), "round-trips/op")
			b.ReportMetric(float64(calls["lpush"])/float64(b.N), "lpush/op")
			b.ReportMetric(float64(calls["publish"])/float64(b.N), "publish/op")
			b.ReportMetric(float64(calls["ltrim"])/float64(b.N), "ltrim/op")
			b.ReportMetric(float64(redisMaxOutputBuffer(b)), "redis-max-outbuf-bytes")
		})
	}
}
