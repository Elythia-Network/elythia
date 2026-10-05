// Package status lets each user set a short status message that shows on their
// profile and expires on its own.
//
// mk-go のプラグイン機構 (#2476) のサンプル。**外部サービスに依存しない**ので、
// そのまま組み込んで動かしながら読める。
//
// 公開面のほとんどをひととおり使う。どれも「例のために足した」ものではなく、
// この機能に必要だから登場する:
//
//   - Storage / Migrations … ステータスの保存先
//   - Jobs                 … 期限切れの掃除 (期限を持つなら必ず要る)
//   - Config               … 最大文字数を運営者が決められるようにする
//   - API                  … 凍結・削除済み利用者の分を表示しない
//   - Router / Request     … 保存と取得
//   - StatusError          … 文字数超過を利用者に伝える
package status

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elythia-network/elythia/plugin"
)

// Plugin is the entry point the build-time generator references.
//
// プラグインは `Plugin` という名前のパッケージ変数を公開する。生成される
// 登録コードがこれを参照する。
var Plugin = plugin.Definition{
	Name:       "status",
	Version:    "1.0.0",
	APIVersion: plugin.APIVersion,
	Migrations: migrations,
	Routes:     routes,
	Jobs:       jobs,
}

// migrations は Definition で宣言する。
//
// Routes / Jobs の中で Storage().Migrate() を呼ぶと、ロールを分割したときに
// 片方でしか走らない (queue 専用プロセスがテーブルの無い schema でジョブを
// 回すことになる)。宣言しておけば mk-go がどちらのロールでも必ず適用する。
var migrations = []plugin.Migration{
	{Version: 1, SQL: `
		CREATE TABLE statuses (
			user_id    text PRIMARY KEY,
			text       text NOT NULL,
			expires_at timestamptz,
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE INDEX statuses_expires_at_idx ON statuses (expires_at)
			WHERE expires_at IS NOT NULL;
	`},
}

// config mirrors the `plugins.status` section of the instance config.
type config struct {
	// MaxLength bounds the message. 運営者が伸ばせるようにしている。
	MaxLength int `json:"maxLength"`
}

func loadConfig(ctx plugin.Context) (config, error) {
	// 既定値を入れてから渡す。設定が書かれていなければそのまま残る。
	c := config{MaxLength: 30}
	if err := ctx.Config().Unmarshal(&c); err != nil {
		return c, err
	}
	if c.MaxLength <= 0 || c.MaxLength > 200 {
		return c, fmt.Errorf("maxLength は 1〜200 で指定してください (%d)", c.MaxLength)
	}
	return c, nil
}

// durations are the display periods the UI offers.
//
// 任意の秒数を受けると、極端に長い値でテーブルが掃除されなくなる。選択肢を
// 決めておく方が扱いが単純になる。
var durations = map[string]time.Duration{
	"1h": time.Hour,
	"1d": 24 * time.Hour,
	"1w": 7 * 24 * time.Hour,
	"":   0, // 無期限
}

func routes(ctx plugin.Context, r plugin.Router) error {
	cfg, err := loadConfig(ctx)
	if err != nil {
		// ここで返したエラーは起動を止める。設定の書き間違いに気付かないまま
		// 動くより、起動時に落ちた方がよい。
		return err
	}
	db := ctx.Storage().DB()

	// frontend から呼ぶものは POST にする。host.api (= misskeyApi) が POST
	// 固定で、Misskey 本体の API も POST 基本なのでそれに倣う。

	r.POST("/me", func(req plugin.Request) (any, error) {
		me := req.UserID()
		if me == "" {
			return nil, plugin.Errorf(http.StatusUnauthorized, "ログインが必要です")
		}
		s, err := load(req.Context(), db, me)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return map[string]any{"text": nil, "maxLength": cfg.MaxLength}, nil
		}
		return map[string]any{
			"text": s.Text, "expiresAt": s.ExpiresAt, "maxLength": cfg.MaxLength,
		}, nil
	})

	r.POST("/me/set", func(req plugin.Request) (any, error) {
		me := req.UserID()
		if me == "" {
			return nil, plugin.Errorf(http.StatusUnauthorized, "ログインが必要です")
		}

		var body struct {
			Text     string `json:"text"`
			Duration string `json:"duration"`
		}
		if err := req.Bind(&body); err != nil {
			return nil, plugin.Errorf(http.StatusBadRequest, "リクエストを読めません")
		}

		text := strings.TrimSpace(body.Text)
		// 空文字は削除として扱う。UI から消せないと不便。
		if text == "" {
			if _, err := db.ExecContext(req.Context(),
				`DELETE FROM statuses WHERE user_id = $1`, me); err != nil {
				return nil, err
			}
			return map[string]any{"text": nil}, nil
		}

		// **文字数は rune で数える。** len() だと日本語が 3 倍に数えられ、
		// 「30文字まで」と言いながら 10 文字で弾くことになる。
		if utf8.RuneCountInString(text) > cfg.MaxLength {
			return nil, plugin.Errorf(http.StatusBadRequest,
				"%d 文字以内にしてください", cfg.MaxLength)
		}

		d, ok := durations[body.Duration]
		if !ok {
			return nil, plugin.Errorf(http.StatusBadRequest, "表示期間の指定が不正です")
		}
		var expires *time.Time
		if d > 0 {
			t := time.Now().Add(d)
			expires = &t
		}

		if _, err := db.ExecContext(req.Context(), `
			INSERT INTO statuses (user_id, text, expires_at, updated_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (user_id) DO UPDATE SET
				text = EXCLUDED.text, expires_at = EXCLUDED.expires_at, updated_at = now()
		`, me, text, expires); err != nil {
			return nil, err
		}
		return map[string]any{"text": text, "expiresAt": expires}, nil
	})

	r.POST("/show", func(req plugin.Request) (any, error) {
		var body struct {
			UserID string `json:"userId"`
		}
		if err := req.Bind(&body); err != nil || body.UserID == "" {
			return nil, plugin.Errorf(http.StatusBadRequest, "userId が必要です")
		}

		s, err := load(req.Context(), db, body.UserID)
		if err != nil {
			return nil, err
		}
		// 未設定は「無い」であってエラーではない。表示側はこれを見て何も描かない。
		if s == nil {
			return map[string]any{"set": false}, nil
		}

		// **凍結・削除済みの利用者の分は出さない。**
		//
		// mk-go の API を呼ぶと、可視性やモデレーション状態の判断を自前で
		// 実装せずに済む (エンドポイントが同じ経路を通る)。ここを DB の直読みで
		// 済ませると、凍結したはずの利用者の文言が残り続ける。
		if !visible(req.Context(), ctx, body.UserID) {
			return map[string]any{"set": false}, nil
		}

		return map[string]any{
			"set": true, "text": s.Text, "expiresAt": s.ExpiresAt, "updatedAt": s.UpdatedAt,
		}, nil
	})

	// 一覧。プラグインの独自ページ (/plugin/status) が使う。
	//
	// 表示名は mk-go の API から引く。**プラグインは本体のテーブルを読まない**
	// ので、user の情報はここでしか取れない (可視性やモデレーション状態の
	// 判断も API 側が持っている)。
	r.POST("/recent", func(req plugin.Request) (any, error) {
		rows, err := db.QueryContext(req.Context(), `
			SELECT user_id, text, updated_at FROM statuses
			WHERE expires_at IS NULL OR expires_at > now()
			ORDER BY updated_at DESC LIMIT 20
		`)
		if err != nil {
			return nil, err
		}
		defer rows.Close() //nolint:errcheck // 読み取りのみ

		type entry struct {
			UserID    string    `json:"userId"`
			Text      string    `json:"text"`
			UpdatedAt time.Time `json:"updatedAt"`
			Username  string    `json:"username"`
		}
		var out []entry
		var ids []string
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.UserID, &e.Text, &e.UpdatedAt); err != nil {
				return nil, err
			}
			out = append(out, e)
			ids = append(ids, e.UserID)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(out) == 0 {
			return map[string]any{"items": []entry{}}, nil
		}

		// **1 回でまとめて引く。** 1 件ずつ呼ぶと利用者数だけリクエストが増え、
		// レート制限にも掛かる。
		raw, err := ctx.API().Anonymous().Call(req.Context(), "users/show",
			map[string]any{"userIds": ids})
		if err != nil {
			// 表示名が引けなくても一覧自体は返す。
			ctx.Logger().Warn("利用者の取得に失敗しました", "err", err)
			return map[string]any{"items": out}, nil
		}
		var users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		}
		if err := json.Unmarshal(raw, &users); err == nil {
			byID := map[string]string{}
			for _, u := range users {
				byID[u.ID] = u.Username
			}
			for i := range out {
				out[i].Username = byID[out[i].UserID]
			}
			// API が返さなかった利用者 (凍結・削除済み) は伏せる。
			filtered := out[:0]
			for _, e := range out {
				if e.Username != "" {
					filtered = append(filtered, e)
				}
			}
			out = filtered
		}
		return map[string]any{"items": out}, nil
	})

	// 管理用。**画面を出すだけでは守れない** — Router は認証の有無しか見ないので、
	// ハンドラ側で必ず権限を確認する。
	r.POST("/admin/stats", func(req plugin.Request) (any, error) {
		if !req.IsModerator() {
			return nil, plugin.Errorf(http.StatusForbidden, "権限がありません")
		}
		var total, expiring int
		if err := db.QueryRowContext(req.Context(), `
			SELECT count(*), count(*) FILTER (WHERE expires_at IS NOT NULL)
			FROM statuses
		`).Scan(&total, &expiring); err != nil {
			return nil, err
		}
		return map[string]any{"total": total, "expiring": expiring}, nil
	})

	return nil
}

func jobs(ctx plugin.Context, j plugin.Jobs) error {
	db := ctx.Storage().DB()

	// 期限切れの掃除。**期限を持つなら必ず要る** — 読み取り側で弾くだけだと、
	// 消えたはずの文言が DB に残り続ける。
	j.Handle("prune", func(c context.Context, _ json.RawMessage) error {
		res, err := db.ExecContext(c,
			`DELETE FROM statuses WHERE expires_at IS NOT NULL AND expires_at <= now()`)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			ctx.Logger().Info("期限切れのステータスを削除しました", "count", n)
		}
		return nil
	})
	j.Schedule("0 * * * *", "prune", nil)
	return nil
}

// record is one stored status.
type record struct {
	Text      string
	ExpiresAt *time.Time
	UpdatedAt time.Time
}

// load reads a user's status, treating an expired one as absent.
//
// 掃除ジョブは 1 時間ごとなので、**読み取り側でも期限を見る**。そうしないと
// 掃除までの最大 1 時間、切れたはずのものが表示される。
func load(ctx context.Context, db *sql.DB, userID string) (*record, error) {
	var r record
	err := db.QueryRowContext(ctx, `
		SELECT text, expires_at, updated_at FROM statuses
		WHERE user_id = $1 AND (expires_at IS NULL OR expires_at > now())
	`, userID).Scan(&r.Text, &r.ExpiresAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// visible reports whether the user should have their status shown.
//
// 判断は mk-go の API に任せる。存在しない / 凍結された利用者は users/show が
// エラーを返すので、こちらでモデレーション状態を持つ必要がない。
func visible(ctx context.Context, pctx plugin.Context, userID string) bool {
	raw, err := pctx.API().Anonymous().Call(ctx, "users/show", map[string]any{"userId": userID})
	if err != nil {
		return false
	}
	var u struct {
		IsSuspended bool `json:"isSuspended"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		// 応答の形が変わった場合は「出さない」に倒す。判断できないまま
		// 表示するより安全側。
		return false
	}
	return !u.IsSuspended
}
