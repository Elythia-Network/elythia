package hashtags

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"time"

	corehashtag "github.com/elythia-network/elythia/internal/core/hashtag"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referenceTrend is the straightforward per-bucket set aggregation that
// aggregateTrend replaced (#3579). The test keeps it as the definition of the
// expected result.
func referenceTrend(rows []trendRow, gen id.Generator, now time.Time) []map[string]any {
	bucketUsers := map[string][trendBucketCount]map[string]struct{}{}
	totalUsers := map[string]map[string]struct{}{}
	for _, r := range rows {
		ts, err := gen.ParseTime(r.ID)
		if err != nil {
			continue
		}
		secsAgo := now.Sub(ts).Seconds()
		if secsAgo < 0 || secsAgo >= float64(trendBucketSeconds*trendBucketCount) {
			continue
		}
		idx := int(secsAgo) / trendBucketSeconds
		for _, tag := range r.Tags {
			if tag == "" {
				continue
			}
			b, ok := bucketUsers[tag]
			if !ok {
				for i := range b {
					b[i] = map[string]struct{}{}
				}
				bucketUsers[tag] = b
				totalUsers[tag] = map[string]struct{}{}
			}
			b[idx][r.UserID] = struct{}{}
			totalUsers[tag][r.UserID] = struct{}{}
		}
	}
	tags := make([]string, 0, len(totalUsers))
	for tag := range totalUsers {
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool {
		if len(totalUsers[tags[i]]) != len(totalUsers[tags[j]]) {
			return len(totalUsers[tags[i]]) > len(totalUsers[tags[j]])
		}
		return tags[i] < tags[j]
	})
	if len(tags) > trendTopN {
		tags = tags[:trendTopN]
	}
	result := make([]map[string]any, 0, len(tags))
	for _, tag := range tags {
		chart := make([]int, trendBucketCount)
		maxUsers := 0
		for i := range chart {
			chart[i] = len(bucketUsers[tag][i])
			maxUsers = max(maxUsers, chart[i])
		}
		result = append(result, map[string]any{"tag": tag, "chart": chart, "usersCount": maxUsers})
	}
	return result
}

func noExclusion(string) bool { return false }

// TestAggregateTrend_MatchesReference pins that the bitmask aggregation
// returns exactly what per-bucket user sets return (#3579): same ranking,
// same tie-break, same chart and usersCount.
func TestAggregateTrend_MatchesReference(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	// now にナノ秒の端数を持たせ、ago をミリ秒で振る。整数秒だけだと secsAgo が
	// 常に整数になり、bucket の切り捨てを四捨五入に変えても気付けない。
	now := time.Date(2026, 10, 16, 12, 0, 0, 123456789, time.UTC)

	for seed := uint64(1); seed <= 20; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rnd := rand.New(rand.NewPCG(seed, 3579))
			var rows []trendRow
			for range 2000 {
				// 窓の外 (未来と 200 分より前) も混ぜる。
				ago := time.Duration(rnd.IntN(230*60*1000)-10*60*1000) * time.Millisecond
				var tags []string
				for range 1 + rnd.IntN(3) {
					// 少数のタグに偏らせ、同点の並び替えも踏む。
					tags = append(tags, fmt.Sprintf("tag%d", rnd.IntN(40)*rnd.IntN(40)/40))
				}
				if rnd.IntN(20) == 0 {
					tags = append(tags, "") // 空のタグは数えない
				}
				rows = append(rows, trendRow{
					ID:     gen.Generate(now.Add(-ago)),
					UserID: fmt.Sprintf("u%d", rnd.IntN(60)),
					Tags:   tags,
				})
			}
			rows = append(rows, trendRow{ID: "not-an-aidx", UserID: "u1", Tags: []string{"broken"}})

			want := referenceTrend(rows, gen, now)
			require.Len(t, want, trendTopN, "fixture must fill the top list")
			assert.Equal(t, want, aggregateTrend(rows, gen, now, noExclusion))
		})
	}
}

// Bucket boundaries: a note exactly 10 minutes old belongs to bucket 1, one
// at 199m59s to the last bucket, and one at 200 minutes is out of the window.
func TestAggregateTrend_BucketEdges(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rows := []trendRow{
		{ID: gen.Generate(now.Add(-10 * time.Minute)), UserID: "a", Tags: []string{"edge"}},
		{ID: gen.Generate(now.Add(-(200*time.Minute - time.Second))), UserID: "b", Tags: []string{"edge"}},
		{ID: gen.Generate(now.Add(-200 * time.Minute)), UserID: "c", Tags: []string{"edge"}},
		// 同じ user が同じ bucket に 2 回書いても 1 と数える。
		{ID: gen.Generate(now.Add(-11 * time.Minute)), UserID: "a", Tags: []string{"edge", "edge"}},
		// edge と同じ 2 人。窓の外の c を数えてしまうと edge が先に来る。
		{ID: gen.Generate(now.Add(-time.Minute)), UserID: "x", Tags: []string{"a_rival"}},
		{ID: gen.Generate(now.Add(-time.Minute)), UserID: "y", Tags: []string{"a_rival"}},
	}
	got := aggregateTrend(rows, gen, now, noExclusion)
	require.Len(t, got, 2)
	assert.Equal(t, "a_rival", got[0]["tag"], "ties are ordered by name; the note at 200 minutes must not count")
	got = got[1:]
	chart := got[0]["chart"].([]int)
	want := make([]int, trendBucketCount)
	want[1], want[19] = 1, 1
	assert.Equal(t, want, chart)
	assert.Equal(t, 1, got[0]["usersCount"])
	assert.Equal(t, want, referenceTrend(rows, gen, now)[1]["chart"])
}

// TestAggregateTrend_ExcludedTagsLeaveTheRanking pins that tags matching
// meta.hiddenTags / meta.sensitiveWords are left out of the ranking (#3628),
// and that the rest is exactly what the reference returns for the notes
// without those tags: the next tags move up and their charts do not change.
func TestAggregateTrend_ExcludedTagsLeaveTheRanking(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	now := time.Date(2026, 10, 16, 12, 0, 0, 123456789, time.UTC)

	for seed := uint64(1); seed <= 10; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rnd := rand.New(rand.NewPCG(seed, 3628))
			var rows []trendRow
			for range 2000 {
				ago := time.Duration(rnd.IntN(200*60*1000)) * time.Millisecond
				var tags []string
				for range 1 + rnd.IntN(3) {
					tags = append(tags, fmt.Sprintf("tag%d", rnd.IntN(40)*rnd.IntN(40)/40))
				}
				rows = append(rows, trendRow{
					ID:     gen.Generate(now.Add(-ago)),
					UserID: fmt.Sprintf("u%d", rnd.IntN(60)),
					Tags:   tags,
				})
			}
			full := referenceTrend(rows, gen, now)
			require.Len(t, full, trendTopN)
			// 1 位を hiddenTags (設定は大文字で書く)、3 位を sensitiveWords の
			// regex で外す。
			hiddenTag := full[0]["tag"].(string)
			sensitiveTag := full[2]["tag"].(string)
			filter := corehashtag.NewRankingFilter(
				[]string{strings.ToUpper(hiddenTag)},
				[]string{"/^" + sensitiveTag + "$/"},
			)

			// 期待値: 外すタグをノートから取り除いて数え直したもの。
			var kept []trendRow
			for _, r := range rows {
				var tags []string
				for _, tag := range r.Tags {
					if tag != hiddenTag && tag != sensitiveTag {
						tags = append(tags, tag)
					}
				}
				kept = append(kept, trendRow{ID: r.ID, UserID: r.UserID, Tags: tags})
			}
			want := referenceTrend(kept, gen, now)
			require.Len(t, want, trendTopN, "fixture must still fill the top list")

			got := aggregateTrend(rows, gen, now, filter.Excludes)
			assert.Equal(t, want, got)
			for _, e := range got {
				assert.NotEqual(t, hiddenTag, e["tag"])
				assert.NotEqual(t, sensitiveTag, e["tag"])
			}
		})
	}
}

// trendFlightKey must give different settings different keys, including
// settings that would collide if the lists were simply joined (#3628).
func TestTrendFlightKey(t *testing.T) {
	keys := map[string]string{
		"none":            trendFlightKey(nil, nil),
		"hidden a":        trendFlightKey([]string{"a"}, nil),
		"sensitive a":     trendFlightKey(nil, []string{"a"}),
		"hidden a,b":      trendFlightKey([]string{"a", "b"}, nil),
		"hidden a sens b": trendFlightKey([]string{"a"}, []string{"b"}),
		"hidden ab":       trendFlightKey([]string{"ab"}, nil),
		"hidden a,empty":  trendFlightKey([]string{"a", ""}, nil),
		"hidden ab,c":     trendFlightKey([]string{"ab", "c"}, nil),
		"hidden a,bc":     trendFlightKey([]string{"a", "bc"}, nil),
	}
	seen := map[string]string{}
	for name, k := range keys {
		if other, ok := seen[k]; ok {
			t.Fatalf("%q and %q share the key %s", name, other, k)
		}
		seen[k] = name
	}
	assert.Equal(t, trendFlightKey([]string{"a"}, []string{"b"}), trendFlightKey([]string{"a"}, []string{"b"}), "same settings share a key")
	assert.Equal(t, trendFlightKey(nil, nil), trendFlightKey([]string{}, []string{}), "nil and empty settings are the same")
}
