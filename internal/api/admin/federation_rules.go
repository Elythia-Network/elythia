package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/fedrule"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/model"
)

// FederationRuleManager edits the federation rules (#3090).
// 実装は fedrule.Manager。
type FederationRuleManager interface {
	List() ([]*model.FederationRule, error)
	Counts(ctx context.Context) (map[string]int64, error)
	Samples(ctx context.Context, ruleID string, limit int) ([]fedrule.Hit, error)
	Create(rule *model.FederationRule) (*model.FederationRule, error)
	Update(rule *model.FederationRule) (before, after *model.FederationRule, err error)
	Delete(ruleID string) (*model.FederationRule, error)
}

// SetFederationRuleManager wires the federation rules.
func (h *Handler) SetFederationRuleManager(m FederationRuleManager) {
	h.fedRules = m
}

// federationRuleBody is the editable part of a rule.
type federationRuleBody struct {
	Name           string   `json:"name"`
	Mode           string   `json:"mode"`
	Target         string   `json:"target"`
	Position       int      `json:"position"`
	Hosts          []string `json:"hosts"`
	ActivityTypes  []string `json:"activityTypes"`
	IsBot          *bool    `json:"isBot"`
	NewWithinHours *int     `json:"newWithinHours"`
	Patterns       []string `json:"patterns"`
	HasAttachment  *bool    `json:"hasAttachment"`
	Tags           []string `json:"tags"`
	Reject         bool     `json:"reject"`
	StripMedia     bool     `json:"stripMedia"`
	Sensitive      bool     `json:"sensitive"`
	Unlist         bool     `json:"unlist"`
	CW             *string  `json:"cw"`
}

func (b federationRuleBody) model(id string) *model.FederationRule {
	return &model.FederationRule{
		ID: id, Name: b.Name, Mode: b.Mode, Target: b.Target, Position: b.Position,
		Hosts: model.StringArray(b.Hosts), ActivityTypes: model.StringArray(b.ActivityTypes),
		IsBot: b.IsBot, NewWithinHours: b.NewWithinHours, Patterns: model.StringArray(b.Patterns),
		HasAttachment: b.HasAttachment, Tags: model.StringArray(b.Tags),
		Reject: b.Reject, StripMedia: b.StripMedia, Sensitive: b.Sensitive, Unlist: b.Unlist, CW: b.CW,
	}
}

type federationRuleResponse struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	federationRuleBody
	// Hits は直近 24 時間に当たった件数 (fedrule.HitWindow)。
	Hits int64 `json:"hits"`
}

func packFederationRule(r *model.FederationRule, hits int64) federationRuleResponse {
	nonNil := func(s model.StringArray) []string {
		if s == nil {
			return []string{}
		}
		return []string(s)
	}
	return federationRuleResponse{
		ID: r.ID, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Hits: hits,
		federationRuleBody: federationRuleBody{
			Name: r.Name, Mode: r.Mode, Target: r.Target, Position: r.Position,
			Hosts: nonNil(r.Hosts), ActivityTypes: nonNil(r.ActivityTypes), IsBot: r.IsBot,
			NewWithinHours: r.NewWithinHours, Patterns: nonNil(r.Patterns), HasAttachment: r.HasAttachment,
			Tags: nonNil(r.Tags), Reject: r.Reject, StripMedia: r.StripMedia, Sensitive: r.Sensitive,
			Unlist: r.Unlist, CW: r.CW,
		},
	}
}

func noSuchFederationRule(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_RULE", "No such federation rule.", "3706c21c-8123-4a06-94ba-56115cae8cd9"))
}

// federationRuleError maps the manager's errors to responses.
func federationRuleError(c echo.Context, err error) error {
	var v *fedrule.ValidationError
	switch {
	case errors.As(err, &v):
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid rule: "+v.Error(), "8efb928f-23d5-4af2-8459-e0b2e6c72e3c"))
	case errors.Is(err, fedrule.ErrNotFound):
		return noSuchFederationRule(c)
	case errors.Is(err, fedrule.ErrTooManyRules):
		return c.JSON(http.StatusBadRequest, apierr.Error("TOO_MANY_RULES", "You cannot create federation rules any more.", "a6f9bbb2-0f18-4464-a640-c8fb0d6b8e96"))
	}
	slog.Error("admin: federation rule operation failed", "err", err)
	return apierr.JSONInternalError(c)
}

func federationRuleLogInfo(r *model.FederationRule) map[string]any {
	return map[string]any{"ruleId": r.ID, "ruleName": r.Name, "rule": packFederationRule(r, 0).federationRuleBody}
}

// FederationRules handles POST /api/admin/federation/rules/list.
//
// **mk-go 独自 endpoint** (#3090)。連合のルールを評価順に、直近 24 時間に
// 当たった件数を付けて返す。upstream に対応物は無い。
func (h *Handler) FederationRules(c echo.Context) error {
	out := []federationRuleResponse{}
	if h.fedRules == nil {
		return c.JSON(http.StatusOK, out)
	}
	rules, err := h.fedRules.List()
	if err != nil {
		return federationRuleError(c, err)
	}
	counts, err := h.fedRules.Counts(c.Request().Context())
	if err != nil {
		// 件数が読めなくてもルールの一覧は出す (Redis の障害で編集できなくしない)。
		slog.Warn("admin: cannot read federation rule hits", "err", err)
		counts = map[string]int64{}
	}
	for _, r := range rules {
		out = append(out, packFederationRule(r, counts[r.ID]))
	}
	return c.JSON(http.StatusOK, out)
}

// FederationRuleHits handles POST /api/admin/federation/rules/hits.
//
// **mk-go 独自 endpoint** (#3090)。ルールに当たった直近の投稿 / activity を
// 新しい順に返す。record のルールで誤爆していないかを確かめるためのもの。
func (h *Handler) FederationRuleHits(c echo.Context) error {
	var req struct {
		RuleID string `json:"ruleId"`
		Limit  int    `json:"limit"`
	}
	_ = c.Bind(&req)
	if req.RuleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "ruleId is required.", "c5b1c129-8d49-42bd-b946-0c2456dfbf39"))
	}
	if h.fedRules == nil {
		return noSuchFederationRule(c)
	}
	hits, err := h.fedRules.Samples(c.Request().Context(), req.RuleID, req.Limit)
	if err != nil {
		return federationRuleError(c, err)
	}
	return c.JSON(http.StatusOK, hits)
}

// FederationRuleCreate handles POST /api/admin/federation/rules/create.
//
// **mk-go 独自 endpoint** (#3090)。
func (h *Handler) FederationRuleCreate(c echo.Context) error {
	var req federationRuleBody
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid rule.", "8efb928f-23d5-4af2-8459-e0b2e6c72e3c"))
	}
	if h.fedRules == nil {
		return apierr.JSONInternalError(c)
	}
	rule, err := h.fedRules.Create(req.model(""))
	if err != nil {
		return federationRuleError(c, err)
	}
	h.logModeration(c, moderationlog.LogCreateFederationRule, federationRuleLogInfo(rule))
	return c.JSON(http.StatusOK, packFederationRule(rule, 0))
}

// FederationRuleUpdate handles POST /api/admin/federation/rules/update.
//
// **mk-go 独自 endpoint** (#3090)。ルール全体を置き換える (部分更新はしない)。
// 条件を変えると、それまでに当たった記録は捨てる。
func (h *Handler) FederationRuleUpdate(c echo.Context) error {
	var req struct {
		RuleID string `json:"ruleId"`
		federationRuleBody
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid rule.", "8efb928f-23d5-4af2-8459-e0b2e6c72e3c"))
	}
	if req.RuleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "ruleId is required.", "c5b1c129-8d49-42bd-b946-0c2456dfbf39"))
	}
	if h.fedRules == nil {
		return noSuchFederationRule(c)
	}
	before, after, err := h.fedRules.Update(req.federationRuleBody.model(req.RuleID))
	if err != nil {
		return federationRuleError(c, err)
	}
	h.logModeration(c, moderationlog.LogUpdateFederationRule, map[string]any{
		"ruleId": after.ID, "ruleName": after.Name,
		"before": packFederationRule(before, 0).federationRuleBody,
		"after":  packFederationRule(after, 0).federationRuleBody,
	})
	return c.JSON(http.StatusOK, packFederationRule(after, 0))
}

// FederationRuleDelete handles POST /api/admin/federation/rules/delete.
//
// **mk-go 独自 endpoint** (#3090)。
func (h *Handler) FederationRuleDelete(c echo.Context) error {
	var req struct {
		RuleID string `json:"ruleId"`
	}
	_ = c.Bind(&req)
	if req.RuleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "ruleId is required.", "c5b1c129-8d49-42bd-b946-0c2456dfbf39"))
	}
	if h.fedRules == nil {
		return noSuchFederationRule(c)
	}
	rule, err := h.fedRules.Delete(req.RuleID)
	if err != nil {
		return federationRuleError(c, err)
	}
	h.logModeration(c, moderationlog.LogDeleteFederationRule, federationRuleLogInfo(rule))
	return c.NoContent(http.StatusNoContent)
}
