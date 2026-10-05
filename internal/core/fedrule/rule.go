// Package fedrule applies the admin-defined federation rules (#3090) to
// inbound activities and notes.
//
// meta の blockedHosts / silencedHosts などはホストを丸ごと扱うことしかできない。
// ここは「条件と動作の組」をホスト単位の設定に**追加の層として**重ねる
// (置き換えない)。Akkoma / Pleroma の MRF と同じ考え方を、宣言的に書ける範囲に
// 絞って持つ。コードが要る判断はプラグイン (#3071) の担当。
//
// ルールには 2 種類ある:
//   - note: 投稿 (Create / Update と、Announce 先・返信先などを取りに行った投稿) の
//     中身まで見る。拒否のほか、メディアを落とす / CW を付ける / センシティブに
//     する / タイムラインから外す、で書き換えられる
//   - activity: 受信した activity を種別で見る。拒否だけができる
//
// どちらも mode が record のあいだは記録だけを残して何もしない。いきなり効かせると
// 誤爆に気付けないので、まず当たり具合を見てから enforce にする運用を想定している。
package fedrule

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/misc/keyword"
	"github.com/elythia-network/elythia/internal/misc/searchnorm"
	"github.com/elythia-network/elythia/internal/model"
)

// Limits on what a rule may hold. 評価は受信のたびに走るので、ルールの数と
// パターンの大きさに上限を置く (Go の regexp は線形時間なので、壊滅的な
// バックトラックは起きない)。
const (
	MaxRules          = 100
	MaxNameLength     = 128
	MaxListItems      = 100
	MaxPatternLength  = 1024
	MaxTagLength      = 128
	MaxCWLength       = 512
	MaxNewWithinHours = 24 * 365
	MaxPosition       = 1_000_000
)

// ActivityTypes are the activity types an activity rule may name (lowercased
// when matched). processor.go の dispatchActivity が扱う種別と揃えてある。
var ActivityTypes = []string{
	"Follow", "Undo", "Accept", "Reject", "Create", "Update", "Delete",
	"Like", "EmojiReact", "Announce", "Block", "Flag", "Move", "Add", "Remove",
}

// ValidationError explains why a rule was refused.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

func invalid(field, reason string) error { return &ValidationError{Field: field, Reason: reason} }

// IsValidationError reports whether err is a ValidationError.
func IsValidationError(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

// Normalize validates rule in place and brings its values into the stored
// form: hosts are trimmed and lowercased, tags lose a leading '#' and are
// normalized like note.tags, activity types take their canonical spelling,
// and empty entries are dropped.
func Normalize(rule *model.FederationRule) error {
	rule.Name = strings.TrimSpace(rule.Name)
	if utf8.RuneCountInString(rule.Name) > MaxNameLength {
		return invalid("name", fmt.Sprintf("must be at most %d characters", MaxNameLength))
	}
	if !storableText(rule.Name) {
		return invalid("name", "contains characters that cannot be stored")
	}
	if rule.Position < 0 || rule.Position > MaxPosition {
		// 列は integer。範囲外は保存で DB エラー (500) になる。
		return invalid("position", fmt.Sprintf("must be between 0 and %d", MaxPosition))
	}
	switch rule.Mode {
	case model.FederationRuleModeDisabled, model.FederationRuleModeRecord, model.FederationRuleModeEnforce:
	default:
		return invalid("mode", "must be disabled, record or enforce")
	}

	var err error
	if rule.Hosts, err = normalizeList("hosts", rule.Hosts, 128, normalizeHostPattern); err != nil {
		return err
	}
	if rule.Patterns, err = normalizeList("patterns", rule.Patterns, MaxPatternLength, func(s string) string {
		// パターンの前後の空白は意味を持たない (単語は空白区切り)。regex の中の
		// 空白は `/.../` の内側なので trim しても変わらない。
		return strings.TrimSpace(s)
	}); err != nil {
		return err
	}
	if _, err := keyword.Compile(rule.Patterns); err != nil {
		return invalid("patterns", err.Error())
	}
	if rule.Tags, err = normalizeList("tags", rule.Tags, MaxTagLength, func(s string) string {
		return searchnorm.Normalize(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	}); err != nil {
		return err
	}
	if rule.ActivityTypes, err = normalizeList("activityTypes", rule.ActivityTypes, 64, strings.TrimSpace); err != nil {
		return err
	}
	for i, t := range rule.ActivityTypes {
		canonical, ok := canonicalActivityType(t)
		if !ok {
			return invalid("activityTypes", fmt.Sprintf("unknown activity type %q", t))
		}
		rule.ActivityTypes[i] = canonical
	}
	if rule.NewWithinHours != nil && (*rule.NewWithinHours < 1 || *rule.NewWithinHours > MaxNewWithinHours) {
		return invalid("newWithinHours", fmt.Sprintf("must be between 1 and %d", MaxNewWithinHours))
	}
	if rule.CW != nil {
		cw := strings.TrimSpace(*rule.CW)
		switch {
		case cw == "":
			rule.CW = nil
		case utf8.RuneCountInString(cw) > MaxCWLength:
			return invalid("cw", fmt.Sprintf("must be at most %d characters", MaxCWLength))
		case !storableText(cw):
			return invalid("cw", "contains characters that cannot be stored")
		default:
			rule.CW = &cw
		}
	}

	switch rule.Target {
	case model.FederationRuleTargetActivity:
		if len(rule.ActivityTypes) == 0 {
			return invalid("activityTypes", "an activity rule needs at least one activity type")
		}
		// activity の段階では投稿の中身がまだ分からないので、中身の条件と
		// 書き換えは持てない。
		if len(rule.Patterns) > 0 || len(rule.Tags) > 0 || rule.HasAttachment != nil {
			return invalid("target", "an activity rule cannot look at note contents")
		}
		if !rule.Reject || rule.StripMedia || rule.Sensitive || rule.Unlist || rule.CW != nil {
			return invalid("target", "an activity rule can only reject")
		}
	case model.FederationRuleTargetNote:
		if len(rule.ActivityTypes) > 0 {
			return invalid("activityTypes", "a note rule cannot name activity types")
		}
		// **条件の無いルールは作らせない。** 全投稿に当たるルールは、ほぼ確実に
		// 書きかけの事故で、enforce で拒否にすると連合が丸ごと止まる。
		if len(rule.Hosts) == 0 && rule.IsBot == nil && rule.NewWithinHours == nil &&
			len(rule.Patterns) == 0 && len(rule.Tags) == 0 && rule.HasAttachment == nil {
			return invalid("target", "a rule needs at least one condition")
		}
		if !rule.Reject && !rule.StripMedia && !rule.Sensitive && !rule.Unlist && rule.CW == nil {
			return invalid("target", "a rule needs at least one action")
		}
	default:
		return invalid("target", "must be note or activity")
	}
	return nil
}

func normalizeList(field string, in model.StringArray, maxLen int, norm func(string) string) (model.StringArray, error) {
	out := model.StringArray{}
	seen := map[string]bool{}
	for _, v := range in {
		v = norm(v)
		if v == "" || seen[v] {
			continue
		}
		if utf8.RuneCountInString(v) > maxLen {
			return nil, invalid(field, fmt.Sprintf("each entry must be at most %d characters", maxLen))
		}
		if !storableText(v) {
			return nil, invalid(field, "contains characters that cannot be stored")
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) > MaxListItems {
		return nil, invalid(field, fmt.Sprintf("must have at most %d entries", MaxListItems))
	}
	return out, nil
}

func canonicalActivityType(t string) (string, bool) {
	for _, known := range ActivityTypes {
		if strings.EqualFold(known, t) {
			return known, true
		}
	}
	return "", false
}

// storableText reports whether s can be written to a text column (NUL と不正な
// UTF-8 は PostgreSQL がクエリごと落とす)。
func storableText(s string) bool {
	return !strings.ContainsRune(s, 0) && utf8.ValidString(s)
}

// normalizeHostPattern trims and lowercases a host pattern. URL の形で書かれて
// いたらホストだけを取り出す (そのままだと一致する相手が無く、効いていない
// ことに気付けない)。
func normalizeHostPattern(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	return s
}
