package role

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// policyInputs は「ロールごとの override だけを持つ」test 入力を rolePolicyInput へ
// 包む。roleID は空のままでよい — この file の test は置換を扱わない（置換は
// plugin_policy_test.go 側）。
func policyInputs(overrides ...map[string]rolePolicyOverride) []rolePolicyInput {
	out := make([]rolePolicyInput, 0, len(overrides))
	for _, m := range overrides {
		out = append(out, rolePolicyInput{overrides: m})
	}
	return out
}

// TestOptOutNotificationTypesAggregatesByIntersection pins that the mk-go
// specific opt-out policy is merged the opposite way from uploadableFileTypes.
//
// **union だと複数ロールに属するほど通知が減る (#2898)。** upstream の policy は
// 緩い方に倒す (bool は OR、数値は max) ので、opt-out は全ロールが切っている型
// だけを切る。
func TestOptOutNotificationTypesAggregatesByIntersection(t *testing.T) {
	base := []string{}

	tests := []struct {
		name   string
		values []any
		want   any
	}{
		{
			name:   "single role opts out of two types",
			values: []any{[]string{"abuseReport", "note"}},
			want:   []string{"abuseReport", "note"},
		},
		{
			name: "two roles: only the shared type is cut",
			// 片方は abuseReport と note、もう片方は abuseReport だけを切る。
			// note を切っていないロールに属している以上 note は届く。
			values: []any{[]string{"abuseReport", "note"}, []string{"abuseReport"}},
			want:   []string{"abuseReport"},
		},
		{
			name:   "a role that cuts nothing keeps everything",
			values: []any{[]string{"abuseReport"}, []string{}},
			want:   []string{},
		},
		{
			name:   "disjoint roles cut nothing",
			values: []any{[]string{"abuseReport"}, []string{"note"}},
			want:   []string{},
		},
		{
			name: "invalid candidates are skipped, not treated as empty",
			// 型不一致を intersection に参加させると結果が必ず空になり、
			// 不正な値の role が 1 つあるだけで他 role の設定が消える。
			values: []any{[]string{"abuseReport"}, "not-a-slice"},
			want:   []string{"abuseReport"},
		},
		{
			name:   "all candidates invalid falls back to base",
			values: []any{"not-a-slice", 42},
			want:   base,
		},
		{
			name:   "json decoded []any is accepted",
			values: []any{[]any{"abuseReport", "note"}, []any{"abuseReport"}},
			want:   []string{"abuseReport"},
		},
		{
			name:   "whitespace-only entries are dropped before intersecting",
			values: []any{[]string{"abuseReport", "  "}, []string{"abuseReport"}},
			want:   []string{"abuseReport"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregatePolicyValues(PolicyOptOutNotificationTypes, base, tt.values)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestUploadableFileTypesStillAggregatesByUnion guards that adding the
// intersection branch did not change the existing []string policy.
func TestUploadableFileTypesStillAggregatesByUnion(t *testing.T) {
	got := aggregatePolicyValues("uploadableFileTypes", []string{"image/*"},
		[]any{[]string{"image/*"}, []string{"video/*"}})
	require.Equal(t, []string{"image/*", "video/*"}, got,
		"uploadableFileTypes must stay set union")
}

// TestOptOutNotificationTypes_UnsetRolesDoNotCancel pins the fix for the
// regression the first version of the intersection had (#2898).
//
// **未設定のロールを intersection に参加させると必ず空になる。** computePolicy は
// override を持たないロールに base (= 空の一覧) を積むので、素直に intersection を
// 取ると 1 つでも未設定のロールがあれば設定が消える。GetUserRoles は
// conditional role も含むので、管理者が割り当てていない自動マッチのロール 1 つで
// 効かなくなっていた。
func TestOptOutNotificationTypes_UnsetRolesDoNotCancel(t *testing.T) {
	base := []string{}

	tests := []struct {
		name      string
		overrides []map[string]rolePolicyOverride
		want      any
	}{
		{
			name: "設定した 1 つと、設定していない 1 つ",
			overrides: []map[string]rolePolicyOverride{
				{PolicyOptOutNotificationTypes: {Priority: 0, Value: []string{"abuseReport"}}},
				{},
			},
			want: []string{"abuseReport"},
		},
		{
			name: "設定した 1 つと、policies を持たないロール",
			overrides: []map[string]rolePolicyOverride{
				{PolicyOptOutNotificationTypes: {Priority: 0, Value: []string{"abuseReport"}}},
				nil,
			},
			want: []string{"abuseReport"},
		},
		{
			name: "useDefault のロールは「設定していない」と同じ",
			overrides: []map[string]rolePolicyOverride{
				{PolicyOptOutNotificationTypes: {Priority: 0, Value: []string{"abuseReport"}}},
				{PolicyOptOutNotificationTypes: {Priority: 0, UseDefault: true}},
			},
			want: []string{"abuseReport"},
		},
		{
			name: "設定した 2 つは intersection を取る",
			overrides: []map[string]rolePolicyOverride{
				{PolicyOptOutNotificationTypes: {Priority: 0, Value: []string{"abuseReport", "note"}}},
				{PolicyOptOutNotificationTypes: {Priority: 0, Value: []string{"abuseReport"}}},
			},
			want: []string{"abuseReport"},
		},
		{
			name: "明示的に空を設定したロールは intersection に参加する",
			// 「このロールでは何も切らない」の明示なので、他ロールの設定を打ち消す。
			overrides: []map[string]rolePolicyOverride{
				{PolicyOptOutNotificationTypes: {Priority: 0, Value: []string{"abuseReport"}}},
				{PolicyOptOutNotificationTypes: {Priority: 0, Value: []string{}}},
			},
			want: []string{},
		},
		{
			name: "誰も設定していなければ base",
			overrides: []map[string]rolePolicyOverride{
				{}, nil,
			},
			want: base,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computePolicy(PolicyOptOutNotificationTypes, base, policyInputs(tt.overrides...), nil)
			require.Equal(t, tt.want, got)
		})
	}
}

// 他の []string policy は未設定ロールの base 参加を保つ (union の挙動を変えない)。
func TestUploadableFileTypes_UnsetRolesStillParticipate(t *testing.T) {
	base := []string{"image/*"}
	got := computePolicy("uploadableFileTypes", base, policyInputs(
		map[string]rolePolicyOverride{"uploadableFileTypes": {Priority: 0, Value: []string{"video/*"}}},
		map[string]rolePolicyOverride{},
	), nil)
	require.Equal(t, []string{"image/*", "video/*"}, got,
		"union の policy では未設定ロールの base も集約に参加する")
}

// TestOptOutNotificationTypes_PriorityCascadeMatchesOtherPolicies pins that the
// intersection branch does not break the upstream priority cascade (#2898).
//
// **絞った結果でグループの有無を判定すると cascade が崩れる。**
// `useDefault=true, priority=2` (= このロールは高優先度でベース値を使う) だけの
// 層で group が空になり、priority 0 まで滑り落ちて下位ロールの設定が効いてしまう。
// intersection の policy でだけ挙動が変わるのは、他の policy と揃わない。
func TestOptOutNotificationTypes_PriorityCascadeMatchesOtherPolicies(t *testing.T) {
	optOutBase := []string{}
	uploadBase := []string{"image/*"}

	// priority 2 の useDefault ロール + priority 0 の明示ロール。
	overrides := func(key string, low []string) []map[string]rolePolicyOverride {
		return []map[string]rolePolicyOverride{
			{key: {Priority: 2, UseDefault: true}},
			{key: {Priority: 0, Value: low}},
		}
	}

	gotOptOut := computePolicy(PolicyOptOutNotificationTypes, optOutBase,
		policyInputs(overrides(PolicyOptOutNotificationTypes, []string{"abuseReport"})...), nil)
	gotUpload := computePolicy("uploadableFileTypes", uploadBase,
		policyInputs(overrides("uploadableFileTypes", []string{"video/*"})...), nil)

	require.Equal(t, optOutBase, gotOptOut,
		"priority 2 が useDefault でも、その層で決まる (priority 0 へ滑り落ちない)")
	require.Equal(t, uploadBase, gotUpload,
		"比較対象: union の policy も同じ形で base に落ち着く")
}

// priority 2 に明示値があればそれで決まる (cascade 自体は生きている)。
func TestOptOutNotificationTypes_HighPriorityExplicitWins(t *testing.T) {
	got := computePolicy(PolicyOptOutNotificationTypes, []string{},
		policyInputs(
			map[string]rolePolicyOverride{PolicyOptOutNotificationTypes: {Priority: 2, Value: []string{"abuseReport"}}},
			map[string]rolePolicyOverride{PolicyOptOutNotificationTypes: {Priority: 0, Value: []string{"note"}}},
		), nil)
	require.Equal(t, []string{"abuseReport"}, got)
}

// TestMergeMetaPolicies_EmptySliceMarshalsAsArray pins the half of the nil fix
// that actually caused the production symptom (#2898).
//
// **`/api/meta` が通る経路は Defaults() ではなく cloneMutablePolicyValue。**
// 本番で `meta.policies.optOutNotificationTypes` が `null` で配信されていたのは
// こちら側で、effectivepolicy.Defaults() のテストだけでは
// cloneMutablePolicyValue を元に戻しても緑のまま通る (実測)。
func TestMergeMetaPolicies_EmptySliceMarshalsAsArray(t *testing.T) {
	for _, name := range []string{"MergeMetaPolicies(nil)", "DefaultPoliciesClone"} {
		t.Run(name, func(t *testing.T) {
			var got map[string]any
			if name == "DefaultPoliciesClone" {
				got = DefaultPoliciesClone()
			} else {
				got = MergeMetaPolicies(nil)
			}
			v, ok := got[PolicyOptOutNotificationTypes]
			require.True(t, ok, "%s が %s を持たない", name, PolicyOptOutNotificationTypes)

			b, err := json.Marshal(v)
			require.NoError(t, err)
			require.Equal(t, "[]", string(b),
				"%s の空 policy が JSON で null になっている (配列を期待する frontend が壊れる)", name)
		})
	}
}

// 空でない []string policy はコピーされ、元を共有しない。
func TestDefaultPoliciesClone_DoesNotShareSlices(t *testing.T) {
	a := DefaultPoliciesClone()["uploadableFileTypes"].([]string)
	b := DefaultPoliciesClone()["uploadableFileTypes"].([]string)
	require.NotEmpty(t, a)
	a[0] = "mutated"
	require.NotEqual(t, "mutated", b[0])
}
