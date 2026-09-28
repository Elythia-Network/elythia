package fedrule

import (
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/misc/idnhost"
	"github.com/shiroha-a/mk/internal/misc/keyword"
	"github.com/shiroha-a/mk/internal/model"
)

// Actor is what the rules know about the sender.
type Actor struct {
	// Known は actor の行がこちらにあるか。無い (初めて見る) ときは IsBot を
	// 判定できないので、bot の条件はどちらの値でも当たらない。初観測からの時間は
	// 0 として扱う (= 新規)。
	Known     bool
	IsBot     bool
	FirstSeen time.Time
}

// ActivityInput is an inbound activity, after signature verification.
type ActivityInput struct {
	// Host は actor のホスト (正規化済み)。
	Host  string
	Type  string
	Actor Actor
	// Subject は記録に残す識別子 (activity の id、無ければ actor)。
	Subject string
}

// NoteInput is a remote note about to be stored (or an edit to one).
type NoteInput struct {
	Host  string
	Actor Actor
	Text  string
	CW    string
	// PollChoices は投票の選択肢。パターンは本文・CW・選択肢のどれかに当たれば満たす。
	PollChoices   []string
	Tags          []string
	HasAttachment bool
	// Subject は note の URI。
	Subject string
	// Update は編集の取り込み。
	Update bool
	// At は「初めて見てから N 時間以内」を測る基準の時刻。ゼロなら評価した時刻。
	//
	// **編集は投稿した時刻で測る。** 評価した時刻で測ると、新規のうちに投稿して
	// 時間が経ってから編集するだけで条件から外れ、ルールの CW が外れる (編集は
	// Update の summary を正とするので付け直されない。#3090 の敵対的レビューで実測)。
	At time.Time
}

// Decision is the combined effect of the enforced rules that matched.
//
// **拒否が最優先。** Reject が立っていれば他の動作は意味を持たない。それ以外は
// 当たったルールの動作をすべて合わせる。CW の文言は評価順で最初に当たった
// ルールのものを使う。
type Decision struct {
	Reject     bool
	StripMedia bool
	Sensitive  bool
	Unlist     bool
	CW         *string
}

// Any reports whether the decision changes anything.
func (d Decision) Any() bool {
	return d.Reject || d.StripMedia || d.Sensitive || d.Unlist || d.CW != nil
}

// Hit is one rule matching one activity or note.
type Hit struct {
	RuleID  string    `json:"ruleId"`
	Host    string    `json:"host"`
	Subject string    `json:"subject"`
	Kind    string    `json:"kind"`
	Applied bool      `json:"applied"`
	At      time.Time `json:"at"`
}

// compiledRule is a rule prepared for evaluation.
type compiledRule struct {
	rule     *model.FederationRule
	patterns *keyword.Matcher
	types    map[string]bool
	tags     map[string]bool
}

// ruleSet is an immutable snapshot of the active rules in evaluation order.
type ruleSet struct {
	notes      []*compiledRule
	activities []*compiledRule
}

func compileRules(rules []*model.FederationRule) *ruleSet {
	set := &ruleSet{}
	for _, r := range rules {
		if r.Mode != model.FederationRuleModeRecord && r.Mode != model.FederationRuleModeEnforce {
			continue
		}
		m, err := keyword.Compile(r.Patterns)
		if err != nil {
			// 保存時に検証しているので来ないはずだが、来たら**そのルールを外す**。
			// パターンだけ外して残すと、条件が緩んで想定外の投稿に当たる。
			continue
		}
		c := &compiledRule{rule: r, patterns: m, types: map[string]bool{}, tags: map[string]bool{}}
		for _, t := range r.ActivityTypes {
			c.types[strings.ToLower(t)] = true
		}
		for _, t := range r.Tags {
			c.tags[t] = true
		}
		switch r.Target {
		case model.FederationRuleTargetActivity:
			set.activities = append(set.activities, c)
		case model.FederationRuleTargetNote:
			set.notes = append(set.notes, c)
		}
	}
	return set
}

// canonicalType lowercases an activity type and folds the aliases that
// dispatchActivity treats as the same activity.
func canonicalType(t string) string {
	t = strings.ToLower(t)
	if t == "emojireaction" {
		return "emojireact"
	}
	return t
}

func (c *compiledRule) matchesActor(host string, a Actor, now time.Time) bool {
	r := c.rule
	if len(r.Hosts) > 0 && (host == "" || !idnhost.MatchesBlockList(r.Hosts, host)) {
		return false
	}
	if r.IsBot != nil && (!a.Known || a.IsBot != *r.IsBot) {
		return false
	}
	if r.NewWithinHours != nil && a.Known {
		if a.FirstSeen.IsZero() || now.Sub(a.FirstSeen) >= time.Duration(*r.NewWithinHours)*time.Hour {
			return false
		}
	}
	return true
}

func (c *compiledRule) matchesActivity(in ActivityInput, now time.Time) bool {
	return c.types[canonicalType(in.Type)] && c.matchesActor(in.Host, in.Actor, now)
}

func (c *compiledRule) matchesNote(in NoteInput, now time.Time) bool {
	r := c.rule
	if !c.matchesActor(in.Host, in.Actor, now) {
		return false
	}
	if r.HasAttachment != nil && in.HasAttachment != *r.HasAttachment {
		return false
	}
	if len(c.tags) > 0 {
		found := false
		for _, t := range in.Tags {
			if c.tags[t] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !c.patterns.Empty() && !c.patterns.Match(in.Text) && !c.patterns.Match(in.CW) && !c.matchesAny(in.PollChoices) {
		return false
	}
	return true
}

func (c *compiledRule) matchesAny(texts []string) bool {
	for _, t := range texts {
		if c.patterns.Match(t) {
			return true
		}
	}
	return false
}

func (d *Decision) merge(r *model.FederationRule) {
	d.Reject = d.Reject || r.Reject
	d.StripMedia = d.StripMedia || r.StripMedia
	d.Sensitive = d.Sensitive || r.Sensitive
	d.Unlist = d.Unlist || r.Unlist
	if d.CW == nil && r.CW != nil {
		cw := *r.CW
		d.CW = &cw
	}
}
