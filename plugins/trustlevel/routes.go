package trustlevel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/plugin"
)

func jobs(ctx plugin.Context, j plugin.Jobs) error {
	cfg, err := loadConfig(ctx)
	if errors.Is(err, errUnconfigured) {
		// **起動は止めない。** 何も登録せず、設定が要ることだけ伝える。
		ctx.Logger().Info("設定されていないので何もしません (roleId と actorId を設定してください)")
		return nil
	}
	if err != nil {
		return err
	}
	r := &reconciler{ctx: ctx, cfg: cfg, db: ctx.Storage().DB()}

	j.Handle("reconcile", func(c context.Context, _ json.RawMessage) error {
		return r.run(c)
	})
	j.Schedule(cfg.Cron, "reconcile", nil)
	return nil
}

func routes(ctx plugin.Context, router plugin.Router) error {
	cfg, err := loadConfig(ctx)
	if errors.Is(err, errUnconfigured) {
		ctx.Logger().Info("設定されていないので何もしません (roleId と actorId を設定してください)")
		return nil
	}
	if err != nil {
		return err
	}
	db := ctx.Storage().DB()

	// **管理用のルートは自分で守る。** Router は認証の有無しか見ないので、
	// 管理画面を出しただけでは API は誰でも叩ける。
	router.POST("/admin/overview", func(req plugin.Request) (any, error) {
		if !req.IsModerator() {
			return nil, plugin.Errorf(http.StatusForbidden, "権限がありません")
		}

		var counts struct {
			Total, Granted, Held, Failing int
		}
		err := db.QueryRowContext(req.Context(), `
			SELECT count(*),
			       count(*) FILTER (WHERE granted),
			       count(*) FILTER (WHERE held),
			       count(*) FILTER (WHERE last_error <> '')
			FROM subjects
		`).Scan(&counts.Total, &counts.Granted, &counts.Held, &counts.Failing)
		if err != nil {
			return nil, err
		}

		runs, err := recentRuns(req.Context(), db, 10)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"config": map[string]any{
				"roleId":            cfg.RoleID,
				"actorId":           cfg.ActorID,
				"minAccountAgeDays": cfg.MinAccountAgeDays,
				"minNotes":          cfg.MinNotes,
				"cron":              cfg.Cron,
			},
			"counts": map[string]any{
				"total": counts.Total, "granted": counts.Granted,
				"held": counts.Held, "failing": counts.Failing,
			},
			"runs": runs,
		}, nil
	})

	router.POST("/admin/subjects", func(req plugin.Request) (any, error) {
		if !req.IsModerator() {
			return nil, plugin.Errorf(http.StatusForbidden, "権限がありません")
		}
		var body struct {
			Filter string `json:"filter"`
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
		}
		_ = req.Bind(&body)
		if body.Limit <= 0 || body.Limit > 100 {
			body.Limit = 50
		}
		if body.Offset < 0 {
			body.Offset = 0
		}

		where := ""
		switch body.Filter {
		case "granted":
			where = "WHERE granted"
		case "held":
			where = "WHERE held"
		case "failing":
			where = "WHERE last_error <> ''"
		}
		rows, err := db.QueryContext(req.Context(), `
			SELECT user_id, granted, held, reason, last_error, evaluated_at
			FROM subjects `+where+`
			ORDER BY evaluated_at DESC NULLS LAST
			LIMIT $1 OFFSET $2
		`, body.Limit, body.Offset)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		out := []map[string]any{}
		for rows.Next() {
			var (
				userID, reason, lastErr string
				granted, held           bool
				evaluatedAt             *time.Time
			)
			if err := rows.Scan(&userID, &granted, &held, &reason, &lastErr, &evaluatedAt); err != nil {
				return nil, err
			}
			out = append(out, map[string]any{
				"userId": userID, "granted": granted, "held": held,
				"reason": reason, "lastError": lastErr, "evaluatedAt": evaluatedAt,
			})
		}
		return map[string]any{"subjects": out}, rows.Err()
	})

	// 保留の切り替え。**条件を満たしても付与しない**運営者の判断を保持する。
	router.POST("/admin/hold", func(req plugin.Request) (any, error) {
		if !req.IsModerator() {
			return nil, plugin.Errorf(http.StatusForbidden, "権限がありません")
		}
		var body struct {
			UserID string `json:"userId"`
			Held   bool   `json:"held"`
		}
		if err := req.Bind(&body); err != nil || body.UserID == "" {
			return nil, plugin.Errorf(http.StatusBadRequest, "userId が要ります")
		}
		if _, err := db.ExecContext(req.Context(), `
			INSERT INTO subjects (user_id, held, reason, evaluated_at)
			VALUES ($1, $2, '手動で設定', now())
			ON CONFLICT (user_id) DO UPDATE SET held = EXCLUDED.held
		`, body.UserID, body.Held); err != nil {
			return nil, err
		}
		return map[string]any{"userId": body.UserID, "held": body.Held}, nil
	})

	return nil
}

// runEntry is one recorded reconcile pass.
type runEntry struct {
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	ElapsedMS  int64     `json:"elapsedMs"`
	Scanned    int       `json:"scanned"`
	Granted    int       `json:"granted"`
	Failed     int       `json:"failed"`
	Error      string    `json:"error"`
}

// recentRuns returns the last passes, newest first.
//
// **サブ issue を切る根拠にする (#2586)。** 「遅い気がする」ではなく、走査件数と
// 所要時間の実測を管理画面から見せる。
func recentRuns(ctx context.Context, db *sql.DB, limit int) ([]runEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT started_at, finished_at, scanned, granted, failed, error
		FROM runs ORDER BY started_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []runEntry{}
	for rows.Next() {
		var e runEntry
		if err := rows.Scan(&e.StartedAt, &e.FinishedAt,
			&e.Scanned, &e.Granted, &e.Failed, &e.Error); err != nil {
			return nil, err
		}
		e.ElapsedMS = e.FinishedAt.Sub(e.StartedAt).Milliseconds()
		out = append(out, e)
	}
	return out, rows.Err()
}
