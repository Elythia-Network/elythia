package trustlevel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/elythia-network/elythia/plugin"
)

// reconciler walks local users and grants the role to those that qualify.
//
// **イベントではなく走査で作る (#2586)。** 実績の集計をイベントの加算で持つと、
// 重複・欠落・順序逆転がそのまま結果の誤差になる。毎回 API から現在値を読み直せば
// 結果は冪等で、後からイベントを足しても意味が変わらない。
type reconciler struct {
	ctx plugin.Context
	cfg config
	db  *sql.DB
}

// run performs one full pass and records what happened.
func (r *reconciler) run(ctx context.Context) error {
	started := time.Now()
	var scanned, granted, failed int
	var runErr error

	caller := r.ctx.API().AsUser(r.cfg.ActorID)
	for offset := 0; ; offset += r.cfg.PageSize {
		users, err := r.page(ctx, caller, offset)
		if err != nil {
			runErr = err
			break
		}
		if len(users) == 0 {
			break
		}
		scanned += len(users)
		for _, u := range users {
			ok, err := r.evaluate(ctx, caller, u)
			switch {
			case err != nil:
				failed++
			case ok:
				granted++
			}
		}
		if len(users) < r.cfg.PageSize {
			break
		}
	}

	r.record(ctx, started, scanned, granted, failed, runErr)
	if failed > 0 {
		r.ctx.Logger().Warn("付与に失敗した利用者があります",
			"failed", failed, "管理画面", "/admin/plugin/trustlevel")
	}
	// **個々の付与失敗でジョブを失敗にしない。** ジョブがエラーを返すと queue が
	// 1 周まるごと再試行する。主体の管理者が使えないなら全員が失敗するので、
	// 再試行しても同じところで止まるだけで、キューを埋めるだけになる。原因は
	// subjects.last_error と runs に残り、管理画面の failing 件数で見える。
	//
	// 一覧そのものが引けなかった場合 (runErr) は別で、一時的な失敗なら再試行に
	// 意味がある。
	//
	// **1 周の記録を残してから返す。** 途中で落ちた回こそ、あとで原因を追いたくなる。
	return runErr
}

// user is the slice of admin/show-users each pass needs.
//
// 生の JSON から必要な項目だけ取り出す。レスポンス全体を構造体にすると、
// upstream の項目増減がそのままプラグインの破綻になる。
type user struct {
	ID          string `json:"id"`
	Host        *string
	CreatedAt   time.Time
	NotesCount  int
	IsSuspended bool
}

// page fetches one batch of local users.
func (r *reconciler) page(ctx context.Context, caller plugin.Caller, offset int) ([]user, error) {
	raw, err := caller.Call(ctx, "admin/show-users", map[string]any{
		"limit":  r.cfg.PageSize,
		"offset": offset,
		"sort":   "+createdAt",
		"state":  "all",
		"origin": "local",
	})
	if err != nil {
		return nil, fmt.Errorf("admin/show-users: %w", err)
	}
	var rows []struct {
		ID          string  `json:"id"`
		Host        *string `json:"host"`
		CreatedAt   string  `json:"createdAt"`
		NotesCount  int     `json:"notesCount"`
		IsSuspended bool    `json:"isSuspended"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("admin/show-users の応答を読めません: %w", err)
	}
	out := make([]user, 0, len(rows))
	for _, row := range rows {
		u := user{
			ID: row.ID, Host: row.Host,
			NotesCount: row.NotesCount, IsSuspended: row.IsSuspended,
		}
		// createdAt が読めない行は「作成日時が分からない」= 昇格させない。
		// **不明を有利に倒さない。**
		if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
			u.CreatedAt = t
		}
		out = append(out, u)
	}
	return out, nil
}

// evaluate decides one user and grants the role when they qualify.
// 戻り値の bool は「この回で新たに付与したか」。
func (r *reconciler) evaluate(ctx context.Context, caller plugin.Caller, u user) (bool, error) {
	state, err := r.load(ctx, u.ID)
	if err != nil {
		return false, err
	}
	// 既に付与済み / 保留中は何もしない。**保留は運営者の判断なので、
	// 条件を満たしても覆さない。**
	if state.held {
		return false, nil
	}
	if state.granted {
		return false, nil
	}

	if reason, ok := r.qualifies(u); !ok {
		r.save(ctx, u.ID, false, reason, "")
		return false, nil
	}

	if err := r.grant(ctx, caller, u.ID); err != nil {
		// **主体の管理者が使えないときはここに出る。** 保存しておかないと
		// 「なぜか昇格が止まっている」を外から追えない。
		r.save(ctx, u.ID, false, "付与に失敗", err.Error())
		return false, err
	}
	r.save(ctx, u.ID, true, "条件を満たしました", "")
	return true, nil
}

// qualifies reports whether u meets the criteria, with the reason it did not.
func (r *reconciler) qualifies(u user) (string, bool) {
	// リモートは対象外。ロールはローカル利用者の権限なので、そもそも意味が無い。
	if u.Host != nil && *u.Host != "" {
		return "リモートユーザー", false
	}
	if u.IsSuspended {
		return "凍結されています", false
	}
	if u.CreatedAt.IsZero() {
		return "作成日時が読めません", false
	}
	age := time.Since(u.CreatedAt)
	if want := time.Duration(r.cfg.MinAccountAgeDays) * 24 * time.Hour; age < want {
		return fmt.Sprintf("作成から %d 日未満", r.cfg.MinAccountAgeDays), false
	}
	if u.NotesCount < r.cfg.MinNotes {
		return fmt.Sprintf("ノートが %d 件未満 (%d)", r.cfg.MinNotes, u.NotesCount), false
	}
	return "", true
}

// grant assigns the role, treating "already assigned" as success.
func (r *reconciler) grant(ctx context.Context, caller plugin.Caller, userID string) error {
	_, err := caller.Call(ctx, "admin/roles/assign", map[string]any{
		"userId": userID,
		"roleId": r.cfg.RoleID,
		// **expiresAt は渡さない。** 期限付きだと TS へ切り替えた後に失効し、
		// プラグインが居ないので再付与されない。保留はこちらの状態で持つ。
	})
	if err == nil {
		return nil
	}
	// **409 は目的の状態。** 冪等に回すので、既に付いていることを異常系に
	// すると reconcile のたびに失敗が積み上がる。
	var apiErr *plugin.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return nil
	}
	return err
}

// state is what the plugin remembers about one user.
type state struct {
	granted bool
	held    bool
}

func (r *reconciler) load(ctx context.Context, userID string) (state, error) {
	var s state
	err := r.db.QueryRowContext(ctx,
		`SELECT granted, held FROM subjects WHERE user_id = $1`, userID).
		Scan(&s.granted, &s.held)
	if errors.Is(err, sql.ErrNoRows) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	return s, nil
}

// save upserts the evaluation outcome. 失敗はログに留める — 記録が書けなくても
// 判定そのものは進めたい。
func (r *reconciler) save(ctx context.Context, userID string, granted bool, reason, lastErr string) {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO subjects (user_id, granted, reason, last_error, evaluated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id) DO UPDATE
		SET granted = EXCLUDED.granted,
		    reason = EXCLUDED.reason,
		    last_error = EXCLUDED.last_error,
		    evaluated_at = EXCLUDED.evaluated_at
	`, userID, granted, reason, truncate(lastErr, 500))
	if err != nil {
		r.ctx.Logger().Warn("判定結果を保存できませんでした", "userId", userID, "err", err)
	}
}

func (r *reconciler) record(ctx context.Context, started time.Time, scanned, granted, failed int, runErr error) {
	msg := ""
	if runErr != nil {
		msg = truncate(runErr.Error(), 500)
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO runs (started_at, finished_at, scanned, granted, failed, error)
		VALUES ($1, now(), $2, $3, $4, $5)
	`, started, scanned, granted, failed, msg); err != nil {
		r.ctx.Logger().Warn("実行記録を保存できませんでした", "err", err)
	}
	r.ctx.Logger().Info("reconcile を実行しました",
		"scanned", scanned, "granted", granted, "failed", failed,
		"elapsed", time.Since(started).String())
}

// truncate bounds a stored message. **エラー本文をそのまま入れない** — API の
// 応答が丸ごと入ると行が肥大化する。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
