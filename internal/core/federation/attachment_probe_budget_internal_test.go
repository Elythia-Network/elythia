package federation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nopFetcher never serves anything; these tests never resolve remote objects.
type nopFetcher struct{}

func (nopFetcher) FetchObject(string) ([]byte, error) { return nil, http.ErrServerClosed }

// probe は呼び出し側の ctx に従うこと。ここで `context.Background()` を作って
// いると、ジョブ側がどれだけ打ち切っても外向き GET が出続ける。
func TestProbeAttachment_HonorsCallerContext(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(renderTestPNG(t, 4, 4))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p, ok := probeAttachment(ctx, srv.Client(), srv.URL+"/cat.png")
	assert.False(t, ok, "打ち切られた ctx で probe が成功してはいけない")
	assert.Equal(t, attachmentProbe{}, p)
	assert.Zero(t, hits.Load(), "打ち切られた ctx で外向き GET を出してはいけない")
}

// hangingTransport blocks until the request context is done, simulating a
// media server that accepts the connection and never answers.
type hangingTransport struct{ calls atomic.Int32 }

func (h *hangingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.calls.Add(1)
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// 先頭取得の合計時間を 1 document 分で打ち切ること。
//
// 添付ごとの attachmentFetchTimeout (3s) しか無いと、無応答のメディアサーバーを
// 指した添付を並べるだけで `添付数 × 3s` だけ inbox worker が止まる。ここでは
// 予算を 150ms に縮めて、4 添付でも予算のオーダーで終わることを見る (予算が
// 効いていなければ 4 × attachmentFetchTimeout = 12s 掛かる)。
func TestUpsertAttachments_BoundsTotalProbeTime(t *testing.T) {
	drive := testutil.NewMockDriveFileRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), nopFetcher{}, idGen)
	r.SetDriveFileRepo(drive)
	tr := &hangingTransport{}
	r.SetAttachmentProbeClient(&http.Client{Transport: tr})
	r.probeBudget = 150 * time.Millisecond

	docs := make([]activitypub.Document, 0, 4)
	for i := 0; i < 4; i++ {
		docs = append(docs, activitypub.Document{
			Type: "Document", MediaType: "image/png",
			URL: "https://media.example/files/" + string(rune('a'+i)) + ".png",
		})
	}

	userID, host := "ru", "remote.example"
	start := time.Now()
	ids := r.upsertAttachments(docs, &userID, &host)
	elapsed := time.Since(start)

	// 添付そのものは保存される (probe は best-effort)。
	require.Len(t, ids, 4)
	assert.Less(t, elapsed, 2*time.Second,
		"probe が添付ごとに独立したタイムアウトを使っている (合計の予算が効いていない)")
	assert.GreaterOrEqual(t, tr.calls.Load(), int32(1), "probe 自体は走っていること")
	// 予算切れ後の添付は properties 空のまま (probe 失敗と同じ degrade)。
	for _, fid := range ids {
		assert.Nil(t, drive.Files[fid].Properties)
	}
}

// 予算が未設定 (zero value) の Resolver でも probe は動くこと。0 をそのまま
// context.WithTimeout へ渡すと即時期限切れになり、probe が恒久的に死ぬ。
func TestUpsertAttachments_ZeroProbeBudgetFallsBackToDefault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(renderTestPNG(t, 12, 34))
	}))
	defer srv.Close()

	drive := testutil.NewMockDriveFileRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), nopFetcher{}, idGen)
	r.SetDriveFileRepo(drive)
	r.SetAttachmentProbeClient(srv.Client())
	require.Zero(t, r.probeBudget, "NewResolver は probeBudget を明示設定しない")

	userID, host := "ru", "remote.example"
	ids := r.upsertAttachments([]activitypub.Document{{
		Type: "Document", MediaType: "image/png", URL: srv.URL + "/cat.png",
	}}, &userID, &host)

	require.Len(t, ids, 1)
	assert.Equal(t, int32(1), hits.Load(), "既定予算で probe が走ること")
	require.NotNil(t, drive.Files[ids[0]].Properties)
	assert.JSONEq(t, `{"width":12,"height":34}`, string(drive.Files[ids[0]].Properties))
}

// **既定値そのものを固定する (レビュー L2)。** 他のテストは `probeBudget` を
// 注入しているので、定数を戻しても気付けなかった。塞いだのは「上限いっぱいの
// 添付 x 1 件あたりの timeout」の直列占有なので、その worst case より十分
// 小さいことを要求する。
func TestAttachmentProbeBudgetBoundsWorstCase(t *testing.T) {
	worst := time.Duration(maxRemoteAttachments) * attachmentFetchTimeout
	if attachmentProbeBudget >= worst {
		t.Fatalf("予算 %v が worst case %v を抑えていない (上限だけでは直列占有が残る)",
			attachmentProbeBudget, worst)
	}
	// 桁が変わる変更 (10s -> 10000s) を落とす。
	if attachmentProbeBudget > worst/2 {
		t.Fatalf("予算 %v が worst case %v の半分を超えている", attachmentProbeBudget, worst)
	}
	if attachmentProbeBudget <= 0 {
		t.Fatalf("予算が %v で probe が常に打ち切られる", attachmentProbeBudget)
	}
}
