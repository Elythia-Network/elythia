package bubblegame

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/core/bubbleversus"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// SetRecords wires the match records and the block checker used to decide who
// may see them (#3232).
func (h *VersusHandler) SetRecords(records repository.BubbleVersusRepository, blocks bubbleversus.BlockChecker) {
	h.records = records
	h.blocks = blocks
}

// canView reports whether viewer may see rec.
//
// 参加者は常に見られる。それ以外は、**両者が公開にした**対局だけを、閲覧者と
// 参加者のどちらの向きにもブロックが無いときに見せる (#3232)。相手の盤面と
// 得点も一緒に出るので、片方の意思だけでは公開しない。
func (h *VersusHandler) canView(viewerID string, rec *model.BubbleGameVersusRecord) (bool, error) {
	if viewerID == rec.User1ID || viewerID == rec.User2ID {
		return true, nil
	}
	if !rec.User1Public || !rec.User2Public {
		return false, nil
	}
	// 退会の処理中 (isDeleted は立ったが行はまだある) と凍結された利用者の対局は、
	// 参加者以外に見せない。行の削除 (CASCADE) は退会のジョブの最後なので、
	// それまでの間に公開のまま出続ける。
	for _, u := range []*model.User{rec.User1, rec.User2} {
		if u == nil || u.IsDeleted || u.IsSuspended {
			return false, nil
		}
	}
	// ブロックを判定できない (配線されていない) なら見せない。
	if h.blocks == nil {
		return false, nil
	}
	for _, p := range []string{rec.User1ID, rec.User2ID} {
		for _, pair := range [][2]string{{viewerID, p}, {p, viewerID}} {
			blocked, err := h.blocks.IsBlocked(pair[0], pair[1])
			if err != nil {
				// 判定できないまま見せない (招待の checkBlocks と同じ fail-closed)。
				return false, fmt.Errorf("bubble-game/versus: block check: %w", err)
			}
			if blocked {
				return false, nil
			}
		}
	}
	return true, nil
}

func timeISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// packRecord renders a stored match. withLogs adds both players' logs, which
// can be large, so only the detail endpoint sets it.
func packRecord(rec *model.BubbleGameVersusRecord, withLogs bool) map[string]any {
	out := map[string]any{
		"id":        rec.ID,
		"gameMode":  rec.GameMode,
		"startedAt": timeISO(rec.StartedAt),
		"endedAt":   nil,
		"winnerId":  rec.WinnerID,
		"reason":    rec.Reason,
		"isPublic":  rec.User1Public && rec.User2Public,
	}
	if rec.EndedAt != nil {
		out["endedAt"] = timeISO(*rec.EndedAt)
	}
	type side struct {
		name    string
		userID  string
		user    *model.User
		score   *int
		frame   *int
		reason  *string
		version *int
		logs    []byte
		public  bool
	}
	for _, s := range []side{
		{"user1", rec.User1ID, rec.User1, rec.User1Score, rec.User1Frame, rec.User1Reason, rec.User1GameVersion, rec.User1Logs, rec.User1Public},
		{"user2", rec.User2ID, rec.User2, rec.User2Score, rec.User2Frame, rec.User2Reason, rec.User2GameVersion, rec.User2Logs, rec.User2Public},
	} {
		out[s.name+"Id"] = s.userID
		if s.user != nil {
			out[s.name] = entity.PackUserLite(s.user)
		} else {
			out[s.name] = nil
		}
		out[s.name+"Public"] = s.public
		var result any
		if s.score != nil {
			result = map[string]any{"score": *s.score, "frame": *s.frame, "reason": *s.reason, "gameVersion": s.version}
		}
		out[s.name+"Result"] = result
		if withLogs {
			// 届いていない側 (切断した側など) は null。リプレイはその盤面を出さない。
			var logs any
			if len(s.logs) > 0 {
				logs = json.RawMessage(s.logs)
			}
			out[s.name+"Logs"] = logs
		}
	}
	if withLogs {
		// シードはリプレイに要る。一覧には要らないので詳細でだけ返す。
		out["seed"] = rec.Seed
	}
	return out
}

// History handles bubble-game/versus/history: finished matches of userId
// (default: the caller), newest first. Other users' history only contains
// matches the caller may see.
func (h *VersusHandler) History(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		UserID    string `json:"userId"`
		Limit     *int   `json:"limit"`
		UntilID   string `json:"untilId"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errVersusInvalidP)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return c.JSON(http.StatusBadRequest, errVersusInvalidP)
	}
	_, untilID, cursorOK := id.NormalizeCursor("", req.UntilID, nil, req.UntilDate)
	if !cursorOK {
		return c.JSON(http.StatusBadRequest, errVersusInvalidP)
	}
	target := me.ID
	if req.UserID != "" && req.UserID != me.ID {
		u, err := h.users.FindByID(req.UserID)
		if repository.IsNotFound(err) || (err == nil && u == nil) {
			return c.JSON(http.StatusNotFound, errNoSuchUser)
		}
		if err != nil {
			return h.fail(c, err)
		}
		target = u.ID
	}
	// 確保量に利用者の値 (limit) を使わない。ResolveLimit で 100 までに収めているが、
	// 静的解析 (CodeQL) はその上限を追えない。
	out := []map[string]any{}
	cursor := untilID
	// 見せない対局 (ブロック・退会の処理中・凍結) を飛ばした分は引き直して、
	// limit 件に達するか記録が尽きるまで集める。飛ばしたまま短いページを返すと、
	// 1 ページ分すべてが見せない対局だったときに空のページになり、クライアントは
	// そこで終わりだと判断して、それより古い対局に二度と届かない。
	// 引き直しの回数には上限を置く (1 回の要求で記録を読み続けないため)。
	for round := 0; round < historyMaxRounds && len(out) < limit; round++ {
		recs, err := h.records.ListByUser(target, target != me.ID, cursor, limit)
		if err != nil {
			return h.fail(c, err)
		}
		for _, rec := range recs {
			ok, err := h.canView(me.ID, rec)
			if err != nil {
				return h.fail(c, err)
			}
			if ok {
				out = append(out, packRecord(rec, false))
				if len(out) == limit {
					break
				}
			}
		}
		if len(recs) < limit {
			break
		}
		cursor = recs[len(recs)-1].ID
	}
	return c.JSON(http.StatusOK, out)
}

// historyMaxRounds bounds how many pages History reads to fill one response.
const historyMaxRounds = 10

// ShowRecord handles bubble-game/versus/record: one stored match with both
// players' logs.
func (h *VersusHandler) ShowRecord(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		MatchID string `json:"matchId"`
	}
	if ok, err := bindMatchID(c, &req, &req.MatchID); !ok {
		return err
	}
	rec, err := h.records.FindByID(req.MatchID)
	if repository.IsNotFound(err) {
		return c.JSON(http.StatusNotFound, errNoSuchMatch)
	}
	if err != nil {
		return h.fail(c, err)
	}
	// 終局していない対局は記録として見せない (履歴にも出ない)。
	if rec.EndedAt == nil {
		return c.JSON(http.StatusNotFound, errNoSuchMatch)
	}
	ok, err := h.canView(me.ID, rec)
	if err != nil {
		return h.fail(c, err)
	}
	if !ok {
		// 見られない対局は、あることも知らせない。
		return c.JSON(http.StatusNotFound, errNoSuchMatch)
	}
	return c.JSON(http.StatusOK, packRecord(rec, true))
}

// SetPublic handles bubble-game/versus/set-public: the caller's own consent
// to show the match to others.
func (h *VersusHandler) SetPublic(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		MatchID  string `json:"matchId"`
		IsPublic *bool  `json:"isPublic"`
	}
	if ok, err := bindMatchID(c, &req, &req.MatchID); !ok {
		return err
	}
	if req.IsPublic == nil {
		return c.JSON(http.StatusBadRequest, errVersusInvalidP)
	}
	found, err := h.records.SetPublic(req.MatchID, me.ID, *req.IsPublic)
	if err != nil {
		return h.fail(c, err)
	}
	if !found {
		return c.JSON(http.StatusNotFound, errNoSuchMatch)
	}
	return c.NoContent(http.StatusNoContent)
}
