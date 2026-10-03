package federation

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/model"
)

// Stats handles POST /api/federation/stats.
//
// 連合統計: followers / following がもっとも多いリモートインスタンス上位
// N 件と、そのほかのインスタンスをまとめた残り総計を返す。
func (h *Handler) Stats(c echo.Context) error {
	// admin/overview は GET で叩いてくるので `query` タグも必要
	// (#421 Devin review: charts.Request と同じ pattern)。
	var req struct {
		Limit *int `json:"limit" query:"limit"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}

	empty := map[string]any{
		"topSubInstances":     []any{},
		"otherFollowersCount": 0,
		"topPubInstances":     []any{},
		"otherFollowingCount": 0,
	}
	if h.svc == nil {
		return c.JSON(http.StatusOK, empty)
	}

	// "+" 接頭辞が DESC (= 上位順) なので、followers / following の多い順を
	// 取るには +followers / +following を渡す (repository の sort の向きを本家
	// TS の federation/instances に合わせたため)。upstream stats.ts は
	// followersCount>0 / followingCount>0 で絞るので Subscribing / Publishing を
	// 立てる (= instance.go の followersCount>0 / followingCount>0 フィルタ、#1544)。
	yes := true
	subs, err := h.svc.List(model.InstanceListFilter{Subscribing: &yes, SortBy: "+followers", Limit: limit})
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	pubs, err := h.svc.List(model.InstanceListFilter{Publishing: &yes, SortBy: "+following", Limit: limit})
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// upstream stats.ts は following テーブルを count する (allSubCount =
	// followeeHost not null, allPubCount = followerHost not null)。旧実装は
	// instance テーブルの followersCount/followingCount 全行合算で semantics が
	// 異なっていた (#1544)。followingRepo 未配線時は 0 (= other は 0 クランプ)。
	allSubCount, allPubCount, err := h.remoteFollowCounts()
	if err != nil {
		return apierr.JSONInternalError(c)
	}

	// isBlocked / isSilenced / isMediaSilenced 用に meta の host 一覧を 1 度
	// だけ取得し、両リストの pack で使い回す。
	hosts, err := h.svc.FederationHostLists()
	if err != nil {
		return apierr.JSONInternalError(c)
	}

	// federation/stats は公開エンドポイントなので moderationNote は出さない。
	// upstream は me を packMany に渡し moderator には見せるが、frontend の stats
	// consumer は host/count しか使わないため、mk-go は安全側に倒して常に隠す
	// (緩い→厳しい方向の意図的な divergence、#parity review F1-fed)。
	//
	// signatureCapability (#2393) は nil 固定で渡す (= 常に null)。本エンドポイントは
	// 無認証の公開 API で、consumer は host/count しか読まない。表示されない情報の
	// ために追加クエリを撃たない。必要になったら instances 側と同じ bulk lookup を足す。
	topSubFollowers := 0
	topSub := make([]map[string]any, 0, len(subs))
	for _, inst := range subs {
		topSubFollowers += inst.FollowersCount
		topSub = append(topSub, instanceToMap(inst, hosts, false, nil))
	}
	topPubFollowing := 0
	topPub := make([]map[string]any, 0, len(pubs))
	for _, inst := range pubs {
		topPubFollowing += inst.FollowingCount
		topPub = append(topPub, instanceToMap(inst, hosts, false, nil))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"topSubInstances": topSub,
		// upstream: Math.max(0, allSubCount - gotSubCount)。負値クランプを揃える。
		"otherFollowersCount": maxInt(0, int(allSubCount)-topSubFollowers),
		"topPubInstances":     topPub,
		"otherFollowingCount": maxInt(0, int(allPubCount)-topPubFollowing),
	})
}

// remoteFollowCounts returns (allSubCount, allPubCount) from the following
// table: allSubCount = rows with followeeHost not null, allPubCount = rows with
// followerHost not null. followingRepo 未配線時は (0, 0)。
func (h *Handler) remoteFollowCounts() (int64, int64, error) {
	if h.followingRepo == nil {
		return 0, 0, nil
	}
	sub, err := h.followingRepo.CountRemoteFollowees()
	if err != nil {
		return 0, 0, err
	}
	pub, err := h.followingRepo.CountRemoteFollowers()
	if err != nil {
		return 0, 0, err
	}
	return sub, pub, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
