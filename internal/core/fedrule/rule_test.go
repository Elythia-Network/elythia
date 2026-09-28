package fedrule

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
)

func ptr[T any](v T) *T { return &v }

func noteRule() *model.FederationRule {
	return &model.FederationRule{Mode: model.FederationRuleModeRecord, Target: model.FederationRuleTargetNote,
		Hosts: model.StringArray{"spam.example"}, Reject: true}
}

func TestNormalize_CleansValues(t *testing.T) {
	r := &model.FederationRule{
		Name: "  ads  ", Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetNote,
		Hosts:    model.StringArray{" Spam.Example ", "", "spam.example", "https://Other.Example:8443/users/x?y#z", "https://user@Third.Example/"},
		Patterns: model.StringArray{"  buy now ", "/fo+/i"},
		Tags:     model.StringArray{"#AD", "ad", " ＡＤ2 "},
		CW:       ptr("  広告  "),
	}
	require.NoError(t, Normalize(r))
	assert.Equal(t, "ads", r.Name)
	assert.Equal(t, model.StringArray{"spam.example", "other.example:8443", "third.example"}, r.Hosts, "trimmed, lowercased, deduplicated, host taken from a URL")
	assert.Equal(t, model.StringArray{"buy now", "/fo+/i"}, r.Patterns)
	assert.Equal(t, model.StringArray{"ad", "ad2"}, r.Tags, "normalized like note.tags")
	assert.Equal(t, "広告", *r.CW)
	assert.Empty(t, r.ActivityTypes)

	r = noteRule()
	r.CW = ptr("   ")
	require.NoError(t, Normalize(r))
	assert.Nil(t, r.CW, "a blank CW means no CW")

	a := &model.FederationRule{Mode: model.FederationRuleModeRecord, Target: model.FederationRuleTargetActivity,
		ActivityTypes: model.StringArray{"follow", "EMOJIREACT"}, Reject: true}
	require.NoError(t, Normalize(a))
	assert.Equal(t, model.StringArray{"Follow", "EmojiReact"}, a.ActivityTypes)
}

func TestNormalize_Refuses(t *testing.T) {
	long := strings.Repeat("a", MaxPatternLength+1)
	many := make(model.StringArray, MaxListItems+1)
	for i := range many {
		many[i] = strings.Repeat("h", i+1) + ".example"
	}
	for name, mut := range map[string]func(r *model.FederationRule){
		"mode":                  func(r *model.FederationRule) { r.Mode = "on" },
		"target":                func(r *model.FederationRule) { r.Target = "user" },
		"name too long":         func(r *model.FederationRule) { r.Name = strings.Repeat("n", MaxNameLength+1) },
		"name with NUL":         func(r *model.FederationRule) { r.Name = "a\x00" },
		"bad regex":             func(r *model.FederationRule) { r.Patterns = model.StringArray{"/(x/"} },
		"pattern too long":      func(r *model.FederationRule) { r.Patterns = model.StringArray{long} },
		"too many hosts":        func(r *model.FederationRule) { r.Hosts = many },
		"host with NUL":         func(r *model.FederationRule) { r.Hosts = model.StringArray{"a\x00.example"} },
		"new within zero hours": func(r *model.FederationRule) { r.NewWithinHours = ptr(0) },
		"new within too long":   func(r *model.FederationRule) { r.NewWithinHours = ptr(MaxNewWithinHours + 1) },
		"cw too long":           func(r *model.FederationRule) { r.CW = ptr(strings.Repeat("c", MaxCWLength+1)) },
		"cw with NUL":           func(r *model.FederationRule) { r.CW = ptr("a\x00") },
		"note rule with types":  func(r *model.FederationRule) { r.ActivityTypes = model.StringArray{"Follow"} },
		"no condition": func(r *model.FederationRule) {
			r.Hosts = nil
		},
		"no action":         func(r *model.FederationRule) { r.Reject = false },
		"negative position": func(r *model.FederationRule) { r.Position = -1 },
		"position too big":  func(r *model.FederationRule) { r.Position = MaxPosition + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			r := noteRule()
			mut(r)
			err := Normalize(r)
			require.Error(t, err)
			assert.True(t, IsValidationError(err))
		})
	}

	for name, mut := range map[string]func(r *model.FederationRule){
		"no types":          func(r *model.FederationRule) { r.ActivityTypes = nil },
		"unknown type":      func(r *model.FederationRule) { r.ActivityTypes = model.StringArray{"Poke"} },
		"content":           func(r *model.FederationRule) { r.Patterns = model.StringArray{"x"} },
		"tags":              func(r *model.FederationRule) { r.Tags = model.StringArray{"x"} },
		"attachment":        func(r *model.FederationRule) { r.HasAttachment = ptr(true) },
		"rewrite":           func(r *model.FederationRule) { r.CW = ptr("x") },
		"strip":             func(r *model.FederationRule) { r.StripMedia = true },
		"sensitive":         func(r *model.FederationRule) { r.Sensitive = true },
		"unlist":            func(r *model.FederationRule) { r.Unlist = true },
		"without rejecting": func(r *model.FederationRule) { r.Reject = false },
	} {
		t.Run("activity "+name, func(t *testing.T) {
			r := &model.FederationRule{Mode: model.FederationRuleModeRecord, Target: model.FederationRuleTargetActivity,
				ActivityTypes: model.StringArray{"Follow"}, Reject: true}
			mut(r)
			assert.Error(t, Normalize(r))
		})
	}
	assert.False(t, IsValidationError(assert.AnError))
}

// 各条件だけのルールは通る (どれか 1 つあれば条件ありとみなす)。
func TestNormalize_AnySingleConditionIsEnough(t *testing.T) {
	for name, mut := range map[string]func(r *model.FederationRule){
		"isBot":         func(r *model.FederationRule) { r.IsBot = ptr(true) },
		"new":           func(r *model.FederationRule) { r.NewWithinHours = ptr(24) },
		"patterns":      func(r *model.FederationRule) { r.Patterns = model.StringArray{"x"} },
		"tags":          func(r *model.FederationRule) { r.Tags = model.StringArray{"x"} },
		"hasAttachment": func(r *model.FederationRule) { r.HasAttachment = ptr(false) },
	} {
		r := noteRule()
		r.Hosts = nil
		mut(r)
		assert.NoError(t, Normalize(r), name)
	}
	for name, mut := range map[string]func(r *model.FederationRule){
		"strip":     func(r *model.FederationRule) { r.StripMedia = true },
		"sensitive": func(r *model.FederationRule) { r.Sensitive = true },
		"unlist":    func(r *model.FederationRule) { r.Unlist = true },
		"cw":        func(r *model.FederationRule) { r.CW = ptr("x") },
	} {
		r := noteRule()
		r.Reject = false
		mut(r)
		assert.NoError(t, Normalize(r), name)
	}
}
