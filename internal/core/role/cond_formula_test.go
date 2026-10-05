package role

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strptr is a small helper to build *string fields inline.
func strptr(s string) *string { return &s }

func TestCondFormula_UnmarshalNumValue(t *testing.T) {
	// followersMoreThanOrEq: `value` フィールドは整数で来る。Custom unmarshal
	// が NumValue に詰めることを確認する (= 旧来の generic Value any 設計と
	// 比べて型安全になっていることの regression guard)。
	raw := `{"type":"followersMoreThanOrEq","value":42}`
	var f CondFormula
	require.NoError(t, json.Unmarshal([]byte(raw), &f))
	assert.Equal(t, CondTypeFollowersMoreThanOrEq, f.Type)
	assert.Equal(t, int64(42), f.NumValue)
	assert.Nil(t, f.Value)
}

func TestCondFormula_UnmarshalNotNested(t *testing.T) {
	// not は value にネストされた CondFormula を持つ。Custom unmarshal が
	// Value (pointer) に nested を詰めることを確認。
	raw := `{"type":"not","value":{"type":"isBot"}}`
	var f CondFormula
	require.NoError(t, json.Unmarshal([]byte(raw), &f))
	assert.Equal(t, CondTypeNot, f.Type)
	require.NotNil(t, f.Value)
	assert.Equal(t, CondTypeIsBot, f.Value.Type)
}

func TestCondFormula_UnmarshalAndOr(t *testing.T) {
	raw := `{"type":"and","values":[{"type":"isBot"},{"type":"isCat"}]}`
	var f CondFormula
	require.NoError(t, json.Unmarshal([]byte(raw), &f))
	assert.Equal(t, CondTypeAnd, f.Type)
	assert.Len(t, f.Values, 2)
	assert.Equal(t, CondTypeIsBot, f.Values[0].Type)
	assert.Equal(t, CondTypeIsCat, f.Values[1].Type)
}

func TestEvalCond_NilUser(t *testing.T) {
	// 防御的: nil user は全 type で false。upstream は user! で non-null
	// 仮定だが、Go では呼び出し元のミスで nil が漏れるので安全側に倒す。
	assert.False(t, EvalCond(nil, nil, CondFormula{Type: CondTypeIsBot}, nil))
}

func TestEvalCond_BooleanUserFlags(t *testing.T) {
	tests := []struct {
		name   string
		user   *model.User
		ftype  CondFormulaType
		expect bool
	}{
		{"isBot=true", &model.User{IsBot: true}, CondTypeIsBot, true},
		{"isBot=false", &model.User{IsBot: false}, CondTypeIsBot, false},
		{"isCat=true", &model.User{IsCat: true}, CondTypeIsCat, true},
		{"isLocked=true", &model.User{IsLocked: true}, CondTypeIsLocked, true},
		{"isSuspended=true", &model.User{IsSuspended: true}, CondTypeIsSuspended, true},
		{"isExplorable=true", &model.User{IsExplorable: true}, CondTypeIsExplorable, true},
		{"isLocal (host nil)", &model.User{}, CondTypeIsLocal, true},
		{"isLocal (host set) false", &model.User{Host: strptr("remote.example")}, CondTypeIsLocal, false},
		{"isRemote (host set)", &model.User{Host: strptr("remote.example")}, CondTypeIsRemote, true},
		{"isRemote (host nil) false", &model.User{}, CondTypeIsRemote, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expect, EvalCond(tc.user, nil, CondFormula{Type: tc.ftype}, nil))
		})
	}
}

func TestEvalCond_NumericComparators(t *testing.T) {
	user := &model.User{FollowersCount: 10, FollowingCount: 5, NotesCount: 100}

	tests := []struct {
		name   string
		ftype  CondFormulaType
		num    int64
		expect bool
	}{
		{"followersLessThanOrEq 10==10", CondTypeFollowersLessThanOrEq, 10, true},
		{"followersLessThanOrEq 10<5", CondTypeFollowersLessThanOrEq, 5, false},
		{"followersMoreThanOrEq 10>=10", CondTypeFollowersMoreThanOrEq, 10, true},
		{"followersMoreThanOrEq 10>=20", CondTypeFollowersMoreThanOrEq, 20, false},
		{"followingLessThanOrEq 5<=10", CondTypeFollowingLessThanOrEq, 10, true},
		{"followingMoreThanOrEq 5>=1", CondTypeFollowingMoreThanOrEq, 1, true},
		{"notesLessThanOrEq 100<=100", CondTypeNotesLessThanOrEq, 100, true},
		{"notesMoreThanOrEq 100>=200", CondTypeNotesMoreThanOrEq, 200, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := CondFormula{Type: tc.ftype, NumValue: tc.num}
			assert.Equal(t, tc.expect, EvalCond(user, nil, f, nil))
		})
	}
}

func TestEvalCond_RoleAssignedTo(t *testing.T) {
	assigned := []*model.Role{
		{ID: "role-a"},
		{ID: "role-b"},
	}
	user := &model.User{ID: "u1"}

	assert.True(t, EvalCond(user, assigned, CondFormula{Type: CondTypeRoleAssignedTo, RoleID: "role-a"}, nil))
	assert.False(t, EvalCond(user, assigned, CondFormula{Type: CondTypeRoleAssignedTo, RoleID: "role-c"}, nil))
	// nil entry は skip して継続する (= panic しない regression guard)。
	assert.True(t, EvalCond(user, []*model.Role{nil, {ID: "role-x"}},
		CondFormula{Type: CondTypeRoleAssignedTo, RoleID: "role-x"}, nil))
}

func TestEvalCond_AndOrNot(t *testing.T) {
	user := &model.User{IsBot: true, IsCat: false}

	// and: isBot && isCat → false
	andF := CondFormula{Type: CondTypeAnd, Values: []CondFormula{
		{Type: CondTypeIsBot}, {Type: CondTypeIsCat},
	}}
	assert.False(t, EvalCond(user, nil, andF, nil))

	// or: isBot || isCat → true
	orF := CondFormula{Type: CondTypeOr, Values: []CondFormula{
		{Type: CondTypeIsBot}, {Type: CondTypeIsCat},
	}}
	assert.True(t, EvalCond(user, nil, orF, nil))

	// not: !isCat → true
	notF := CondFormula{Type: CondTypeNot, Value: &CondFormula{Type: CondTypeIsCat}}
	assert.True(t, EvalCond(user, nil, notF, nil))

	// not with nil operand → false (defensive)
	assert.False(t, EvalCond(user, nil, CondFormula{Type: CondTypeNot}, nil))

	// empty and (=true), empty or (=false): JS / TS 同等
	assert.True(t, EvalCond(user, nil, CondFormula{Type: CondTypeAnd}, nil))
	assert.False(t, EvalCond(user, nil, CondFormula{Type: CondTypeOr}, nil))
}

func TestEvalCond_CreatedComparators(t *testing.T) {
	g, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	// "now" を固定して、その 1 時間前 / 1 時間後の id を生成して評価する。
	now := time.Now()
	pastID := g.Generate(now.Add(-1 * time.Hour))
	user := &model.User{ID: pastID}

	// createdLessThan sec=7200 (= 2h) → user は 1h 前なので「2h 以内」=true
	assert.True(t, evalCondAt(user, nil,
		CondFormula{Type: CondTypeCreatedLessThan, Sec: 7200}, g, now))
	// createdLessThan sec=600 (= 10min) → user は 1h 前なので「10min 以内」=false
	assert.False(t, evalCondAt(user, nil,
		CondFormula{Type: CondTypeCreatedLessThan, Sec: 600}, g, now))
	// createdMoreThan sec=600 → 「10min より前」=true
	assert.True(t, evalCondAt(user, nil,
		CondFormula{Type: CondTypeCreatedMoreThan, Sec: 600}, g, now))
	// createdMoreThan sec=7200 → 「2h より前」=false
	assert.False(t, evalCondAt(user, nil,
		CondFormula{Type: CondTypeCreatedMoreThan, Sec: 7200}, g, now))
}

func TestEvalCond_CreatedComparators_NilGenOrBadID(t *testing.T) {
	user := &model.User{ID: "not-a-valid-id"}
	// idGen 未配線 → false (fail-closed)
	assert.False(t, EvalCond(user, nil, CondFormula{Type: CondTypeCreatedLessThan, Sec: 60}, nil))
	assert.False(t, EvalCond(user, nil, CondFormula{Type: CondTypeCreatedMoreThan, Sec: 60}, nil))

	// idGen 配線済でも parse error → false
	g, _ := id.NewGenerator("aidx")
	assert.False(t, EvalCond(user, nil, CondFormula{Type: CondTypeCreatedLessThan, Sec: 60}, g))
	assert.False(t, EvalCond(user, nil, CondFormula{Type: CondTypeCreatedMoreThan, Sec: 60}, g))
}

func TestEvalCond_UnknownType(t *testing.T) {
	// 未知 type は false に倒す (fail-closed)。upstream で新 type が追加されたとき
	// silently true にして over-permissive にしないための regression guard。
	assert.False(t, EvalCond(&model.User{}, nil, CondFormula{Type: "definitelyNotAType"}, nil))
}

// coerceToBaseType / maxNumber / aggregateChatAvailability は internal
// helper だが、JSON 値 (float64) → base 型 (int / int64 / float64) の
// 変換ロジックが consumer 側 type assert の安全性を担保するので、direct
// unit test で 100% に近い coverage を狙う。
func TestCoerceToBaseType(t *testing.T) {
	// base が int の場合: int / float64 / 型不一致 (string) は素通し。
	assert.Equal(t, 42, coerceToBaseType(0, 42))
	assert.Equal(t, 42, coerceToBaseType(0, float64(42)))
	assert.Equal(t, "x", coerceToBaseType(0, "x"))

	// base が int64 の場合: int64 / int / float64 / それ以外は素通し。
	assert.Equal(t, int64(7), coerceToBaseType(int64(0), int64(7)))
	assert.Equal(t, int64(7), coerceToBaseType(int64(0), 7))
	assert.Equal(t, int64(7), coerceToBaseType(int64(0), float64(7)))
	assert.Equal(t, true, coerceToBaseType(int64(0), true))

	// base が float64 の場合: float64 / int / それ以外は素通し。
	assert.Equal(t, float64(3.5), coerceToBaseType(float64(0), float64(3.5)))
	assert.Equal(t, float64(3), coerceToBaseType(float64(0), 3))
	assert.Equal(t, "x", coerceToBaseType(float64(0), "x"))

	// base が bool / string は素通し。
	assert.Equal(t, true, coerceToBaseType(false, true))
	assert.Equal(t, "value", coerceToBaseType("base", "value"))
}

// #1036 review: base が []string (uploadableFileTypes 等) のときの coerce。
// JSON unmarshal は []any で来るので []string に丸める。これで後段の
// aggregatePolicyValues の case []string 分岐が meta.policies override 経由
// でも fire するようになり、set union 集約が一貫して動く。
func TestCoerceToBaseType_StringSlice(t *testing.T) {
	base := []string{"image/*"}

	// []any (JSON unmarshal 由来) → []string に正規化
	got := coerceToBaseType(base, []any{"image/*", " video/* "})
	assert.Equal(t, []string{"image/*", "video/*"}, got)

	// []string はそのまま返す
	assert.Equal(t, []string{"a"}, coerceToBaseType(base, []string{"a"}))

	// 空文字 entry は normalize で skip される
	got = coerceToBaseType(base, []any{"image/*", "", "  "})
	assert.Equal(t, []string{"image/*"}, got)

	// 完全に空 (normalize 後 0 件) は base にフォールバック
	got = coerceToBaseType(base, []any{"", "  "})
	assert.Equal(t, base, got)

	// 型不一致 (string / object 等) は base にフォールバック
	assert.Equal(t, base, coerceToBaseType(base, "not a slice"))
	assert.Equal(t, base, coerceToBaseType(base, 42))
}

// coerce は NaN / +-Inf / int レンジ超過の float64 を silently truncate せず
// base に倒す (admin UI で異常値が入ってきた場合の fail-soft)。
func TestCoerceToBaseType_RejectsInvalidFloat(t *testing.T) {
	// base int の場合
	assert.Equal(t, 99, coerceToBaseType(99, math.NaN()))
	assert.Equal(t, 99, coerceToBaseType(99, math.Inf(1)))
	assert.Equal(t, 99, coerceToBaseType(99, math.Inf(-1)))
	// base int64 の場合
	assert.Equal(t, int64(99), coerceToBaseType(int64(99), math.NaN()))
	assert.Equal(t, int64(99), coerceToBaseType(int64(99), math.Inf(1)))
	// base float64 の場合: NaN / Inf も base に倒す (DefaultPolicies に
	// float64 policy は現状無いが将来追加された時に consumer に漏らさない)。
	assert.Equal(t, float64(0.5), coerceToBaseType(float64(0.5), math.NaN()))
	assert.Equal(t, float64(0.5), coerceToBaseType(float64(0.5), math.Inf(1)))
	assert.Equal(t, float64(0.5), coerceToBaseType(float64(0.5), math.Inf(-1)))
}

// `float64(math.MaxInt64)` は IEEE 754 で表現できず `2^63` に丸まるため、
// strict less-than guard でこの境界値も reject する (= int64(2^63) が
// implementation-defined になる落とし穴を踏まない)。
func TestCoerceToBaseType_Rejects2Pow63Boundary(t *testing.T) {
	overflow := float64(math.MaxInt64) // 実体は 2^63、math.MaxInt64+1 相当
	assert.Equal(t, int64(99), coerceToBaseType(int64(99), overflow))
	// 同じ値は int base でも (64-bit platform 想定で) reject される。
	assert.Equal(t, 99, coerceToBaseType(99, overflow))
}

// isFiniteAndInRange の境界条件を直接 cover する。caller の coerceToBaseType
// / maxNumber 経由 test だけでなく helper 単体としての挙動も保証する。
func TestIsFiniteAndInRange(t *testing.T) {
	// finite + 範囲内 → true (下限は等号許容、上限は厳密 less-than)。
	assert.True(t, isFiniteAndInRange(0, -100, 100))
	assert.True(t, isFiniteAndInRange(-100, -100, 100), "lo 境界は inclusive")
	assert.False(t, isFiniteAndInRange(100, -100, 100), "hi 境界は exclusive")
	assert.False(t, isFiniteAndInRange(101, -100, 100))
	assert.False(t, isFiniteAndInRange(-101, -100, 100))
	// NaN / +-Inf は常に false。
	assert.False(t, isFiniteAndInRange(math.NaN(), -100, 100))
	assert.False(t, isFiniteAndInRange(math.Inf(1), -100, 100))
	assert.False(t, isFiniteAndInRange(math.Inf(-1), -100, 100))
}

func TestMaxNumberAsInt_NoUsableValues(t *testing.T) {
	// 全 entry が int / float64 以外 → base を返す (found=false path)。
	got := maxNumber([]any{"not a number", true, nil}, 99)
	assert.Equal(t, 99, got)
}

func TestMaxNumberAsInt_SkipsInvalidFloat(t *testing.T) {
	// NaN / Inf は silently skip。usable entry が無ければ base に倒す。
	got := maxNumber([]any{math.NaN(), math.Inf(1), math.Inf(-1)}, 99)
	assert.Equal(t, 99, got)
	// NaN/Inf 混在でも valid な float entry があれば、それを int に丸めて返す。
	got = maxNumber([]any{math.NaN(), float64(42), math.Inf(1)}, 99)
	assert.Equal(t, 42, got)
}

func TestMaxNumber_ExactMixedNumericOrdering(t *testing.T) {
	assert.Equal(t, 3, maxNumber([]any{int64(3), 2.5}, 0))
	assert.Equal(t, 1.5, maxNumber([]any{1, 1.5}, 0))
	assert.Equal(t, 1.5, maxNumber([]any{1.5, 1}, 0))
	assert.Equal(t, -1, maxNumber([]any{-1.5, -1}, 0))
	assert.Equal(t, -1, maxNumber([]any{-1, -1.5}, 0))
	assert.Equal(t, 2.5, maxNumber([]any{1.5, 2.5}, 0))
}

func TestPolicyUnlimitedOrAboveCap(t *testing.T) {
	assert.True(t, policyUnlimitedOrAboveCap(-1, 10))
	assert.True(t, policyUnlimitedOrAboveCap(11, 10))
	assert.False(t, policyUnlimitedOrAboveCap(5, 10))
	assert.True(t, policyUnlimitedOrAboveCap(int64(-1), 10))
	assert.True(t, policyUnlimitedOrAboveCap(int64(11), 10))
	assert.False(t, policyUnlimitedOrAboveCap(int64(5), 10))
	assert.True(t, policyUnlimitedOrAboveCap(-0.5, 10))
	assert.True(t, policyUnlimitedOrAboveCap(10.5, 10))
	assert.False(t, policyUnlimitedOrAboveCap(0.5, 10))
	assert.False(t, policyUnlimitedOrAboveCap("invalid", 10))
}

func TestAggregateChatAvailability_AvailableShortCircuit(t *testing.T) {
	// "available" を含む場合は readonly / unavailable をスキップして即返す。
	got := aggregateChatAvailability([]any{"readonly", "unavailable", "available"})
	assert.Equal(t, "available", got)
}

// #1034: slice 型 base (uploadableFileTypes 等) は set union で集約される。
// 旧 KeepsBase 挙動からの差分: 単一 override は set union で抽出され、
// override に書かれた pattern が結果に現れる。
func TestAggregatePolicyValues_SliceBaseSetUnion(t *testing.T) {
	base := []string{"image/*"}
	got := aggregatePolicyValues("uploadableFileTypes", base, []any{[]string{"video/*"}})
	// upstream RoleService.calc(...set union) と等価。base は集約対象外で
	// override 値のみから set を作るので base "image/*" は結果に含まれない。
	assert.Equal(t, []string{"video/*"}, got)
}

// values が空なら型に依らず base を返す (= 旧来挙動の維持)。
func TestAggregatePolicyValues_EmptyValuesReturnsBase(t *testing.T) {
	assert.Equal(t, 42, aggregatePolicyValues("anything", 42, nil))
}

// set union の deterministic 出力 + entry trim + 空文字 skip を直接 cover。
func TestAggregatePolicyValues_SliceSetUnionDeterministic(t *testing.T) {
	base := []string{"image/*"}
	// 複数 role 由来の override を flatten + dedup + trim + sort
	got := aggregatePolicyValues("uploadableFileTypes", base, []any{
		[]string{"image/*", "  text/* "}, // trim される
		[]string{"video/*", "image/*"},   // dedup される
		[]string{"audio/*", ""},          // 空文字は skip
	})
	// sort 後の deterministic 順
	assert.Equal(t, []string{"audio/*", "image/*", "text/*", "video/*"}, got)
}

// JSON unmarshal 由来の []any も []string と同じく accept する。
func TestAggregatePolicyValues_SliceSetUnionFromJSONAny(t *testing.T) {
	base := []string{"image/*"}
	got := aggregatePolicyValues("uploadableFileTypes", base, []any{
		[]any{"image/*", "video/*"},
	})
	assert.Equal(t, []string{"image/*", "video/*"}, got)
}

// 全候補から取り出せた entry が 0 個なら base を返す (= fail-soft)。
func TestAggregatePolicyValues_SliceSetUnionEmptyFallsBackToBase(t *testing.T) {
	base := []string{"image/*"}
	// values は空 slice の override / 空文字のみの override / 型不一致の混在。
	got := aggregatePolicyValues("uploadableFileTypes", base, []any{
		[]string{},   // 空 slice
		[]string{""}, // 空文字のみ
		42,           // 型不一致
	})
	// 結果 set が空なので base が返る。
	assert.Equal(t, base, got)
}

// normalizeStringSlice の direct unit test (型 coercion 全 path)。
func TestNormalizeStringSlice(t *testing.T) {
	assert.Nil(t, normalizeStringSlice(nil))
	assert.Equal(t, []string{"a", "b"}, normalizeStringSlice([]string{"a", " b "}))
	assert.Equal(t, []string{}, normalizeStringSlice([]string{"", "  "}))
	assert.Equal(t, []string{"x"}, normalizeStringSlice([]any{42, "x", true, "  "}))
	// 型不一致 (string / slice ではない) は nil
	assert.Nil(t, normalizeStringSlice("not a slice"))
	assert.Nil(t, normalizeStringSlice(42))
}

func TestCondFormula_UnmarshalErrors(t *testing.T) {
	// invalid outer JSON は error を伝搬する。
	var f CondFormula
	require.Error(t, json.Unmarshal([]byte(`not json`), &f))

	// not の operand に malformed JSON が来た場合も error を伝搬する。
	// raw.Value = []byte("not json") は json.RawMessage の constraint は
	// pass するが、nested の json.Unmarshal が失敗する。
	require.Error(t, json.Unmarshal([]byte(`{"type":"not","value":"not-an-object"}`), &f))

	// numeric comparator に文字列が来た場合も error。
	require.Error(t, json.Unmarshal([]byte(`{"type":"followersMoreThanOrEq","value":"abc"}`), &f))
}

// **本人が満たせる条件で管理者を配れないこと (#3037)。**
//
// 管理画面は条件を並べるだけなので、`isCat` にチェックを入れた管理者ロールを
// 作るのは操作としては簡単だが、そのロールは**「猫と名乗る」だけで誰でも
// 取れる**。作った側は「条件を満たす人に配る」つもりで、「誰でも自分で
// 満たせる条件」だとは気付きにくい。
func TestCondDependsOnUserControlledValue(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		// 本人が切り替えられる / 積み上げられる。
		{"isCat", `{"type":"isCat"}`, true},
		{"isBot", `{"type":"isBot"}`, true},
		{"isLocked", `{"type":"isLocked"}`, true},
		{"isExplorable", `{"type":"isExplorable"}`, true},
		{"notesMoreThanOrEq", `{"type":"notesMoreThanOrEq","value":10}`, true},
		{"followersMoreThanOrEq", `{"type":"followersMoreThanOrEq","value":10}`, true},
		{"followingLessThanOrEq", `{"type":"followingLessThanOrEq","value":10}`, true},
		// **登録するだけで満たせる (#3037 レビュー 2 周目)。**
		// 「本人が値を変えられるか」で考えると取りこぼしていた。
		{"isLocal", `{"type":"isLocal"}`, true},
		{"createdLessThan", `{"type":"createdLessThan","sec":86400}`, true},
		// 攻撃者がアカウントを用意できない。
		{"isRemote", `{"type":"isRemote"}`, false},
		{"isSuspended", `{"type":"isSuspended"}`, false},
		// **アカウントの年齢は barrier にならない (#3045)。** 攻撃者は
		// いくらでも待てるので、どれだけ長い `sec` を置いても「登録して
		// 待って取る」が成立する。#3044 は 30 日を境に通していたが、閾値は
		// 境界ではなく取り違えの検出器でしかなかった。
		{"createdMoreThan 1時間", `{"type":"createdMoreThan","sec":3600}`, true},
		{"createdMoreThan 1日 (管理画面の既定)", `{"type":"createdMoreThan","sec":86400}`, true},
		{"createdMoreThan 0秒", `{"type":"createdMoreThan","sec":0}`, true},
		{"createdMoreThan 30日", `{"type":"createdMoreThan","sec":2592000}`, true},
		{"createdMoreThan 1年", `{"type":"createdMoreThan","sec":31536000}`, true},
		// **`sec=0` の `createdLessThan` だけは恒偽。** `t.After(now)` なので
		// 誰にも一致しない = 特権が誰にも渡らない。これは閾値ではない。
		{"createdLessThan 0秒", `{"type":"createdLessThan","sec":0}`, false},
		{"createdLessThan 負数", `{"type":"createdLessThan","sec":-1}`, false},
		{"createdLessThan sec 省略", `{"type":"createdLessThan"}`, false},
		{"roleAssignedTo", `{"type":"roleAssignedTo","roleId":"r1"}`, false},
		// **否定は向きが入れ替わる。** 新規アカウントは「1 年以上前に
		// 作られて**いない**」「凍結されて**いない**」「そのロールを持って
		// **いない**」をどれもそのまま満たす。
		{"not createdMoreThan", `{"type":"not","value":{"type":"createdMoreThan","sec":31536000}}`, true},
		// **`not(createdLessThan)` は「`sec` 以上前に作られた」(#3045)。**
		// #3044 は `createdLessThan` の否定側を「満たせない」に固定しており、
		// `not(createdLessThan 1秒)` = 1 秒より前に作られた全アカウント、が
		// そのまま通っていた。極性ごとに当てないと片側が残る。
		{"not createdLessThan 1秒", `{"type":"not","value":{"type":"createdLessThan","sec":1}}`, true},
		{"not createdLessThan 1年", `{"type":"not","value":{"type":"createdLessThan","sec":31536000}}`, true},
		// `sec=0` の `createdLessThan` は恒偽なので、その否定は恒真。
		{"not createdLessThan 0秒 (恒真)", `{"type":"not","value":{"type":"createdLessThan","sec":0}}`, true},
		{"not isSuspended", `{"type":"not","value":{"type":"isSuspended"}}`, true},
		{"not roleAssignedTo", `{"type":"not","value":{"type":"roleAssignedTo","roleId":"r1"}}`, true},
		{"not isLocal", `{"type":"not","value":{"type":"isLocal"}}`, false},
		// **恒真式。** 条件を書いたつもりで全員に一致する。
		{"空の and", `{"type":"and","values":[]}`, true},
		{"空の or を否定", `{"type":"not","value":{"type":"or","values":[]}}`, true},
		{"未知の type を否定", `{"type":"not","value":{"type":"somethingNew"}}`, true},
		// **中身の無い `not` は恒偽 (レビュー 3 周目で訂正)。**
		// `evalCondAt` は nil の operand に false を返すので `not(nil)` は
		// 定数 false = 誰にも一致しない。2 周目は恒真だと決め打って極性を
		// 逆に持っており、**二重否定で符号が戻って `not(not())` が
		// 素通り**していた。
		{"中身の無い not", `{"type":"not"}`, false},
		{"中身の無い not の否定 (恒真)", `{"type":"not","value":{"type":"not"}}`, true},
		{"and(not) の否定 (恒真)", `{"type":"not","value":{"type":"and","values":[{"type":"not"}]}}`, true},
		{"or(not) の否定 (恒真)", `{"type":"not","value":{"type":"or","values":[{"type":"not"}]}}`, true},
		// 空の `or` そのものは誰にも一致しない。
		{"空の or", `{"type":"or","values":[]}`, false},
		// **入れ子も見る。** and / or / not のどこかにあれば同じこと。
		{"and の中", `{"type":"and","values":[{"type":"isLocal"},{"type":"isCat"}]}`, true},
		{"or の中", `{"type":"or","values":[{"type":"isCat"}]}`, true},
		{"not の中", `{"type":"not","value":{"type":"isCat"}}`, true},
		{"深い入れ子", `{"type":"and","values":[{"type":"or","values":[{"type":"not","value":{"type":"isBot"}}]}]}`, true},
		// **`and` は全部を満たす必要がある。** `isLocal` は登録すれば満たせるが
		// `roleAssignedTo` は誰かが配る必要があるので、この組み合わせは安全。
		// 「どこかに危ない葉があるか」で見るとこの正当な設定を弾く。
		{"入れ子だが全部安全", `{"type":"and","values":[{"type":"isLocal"},{"type":"roleAssignedTo","roleId":"staff"}]}`, false},
		{"and に用意できない葉 (isRemote)", `{"type":"and","values":[{"type":"isCat"},{"type":"isRemote"}]}`, false},
		// **年齢と組んでも安全にはならない (#3045)。** 「登録して N 年経った
		// ローカル利用者は全員」で、待てば満たせる。
		{"and に年齢条件 (1年)", `{"type":"and","values":[{"type":"isLocal"},{"type":"createdMoreThan","sec":31536000}]}`, true},
		{"and に年齢条件 (1秒)", `{"type":"and","values":[{"type":"isLocal"},{"type":"createdMoreThan","sec":1}]}`, true},
		{"and に not(createdLessThan)", `{"type":"and","values":[{"type":"isLocal"},{"type":"not","value":{"type":"createdLessThan","sec":1}}]}`, true},
		// **`or` はどれか 1 つで足りる。**
		{"or に危ない枝が 1 つ", `{"type":"or","values":[{"type":"roleAssignedTo","roleId":"staff"},{"type":"isCat"}]}`, true},
		{"or が全部安全", `{"type":"or","values":[{"type":"roleAssignedTo","roleId":"staff"},{"type":"isRemote"}]}`, false},
		// **知らない型は判定できないので拒否側に倒す。** 評価は false に
		// 倒れるが、`not` で包まれると恒真式になる。
		{"未知の type", `{"type":"somethingNew"}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var f CondFormula
			require.NoError(t, json.Unmarshal([]byte(tt.raw), &f))
			assert.Equal(t, tt.want, CondDependsOnUserControlledValue(f))
		})
	}
}

// **参照の収集は `not` と入れ子の中まで見る (#3037 レビュー 3 周目)。**
//
// `RoleGrantsPrivilegeIndirectly` がこの結果で「そのロールを配ると特権が付くか」
// を判定する。`not(roleAssignedTo staff)` を持つ特権ロールでは、`staff` を
// **外した**ときにメンバーが特権を得るので、unassign 側の保護にこの枝が要る。
// 2 周目はこの再帰を書いたのにテストを置いておらず、**丸ごと消しても
// `internal/core/role` と `internal/api/admin` が緑のまま**だった。
func TestCondReferencedRoleIDs(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want []string
	}{
		{"単体", `{"type":"roleAssignedTo","roleId":"r1"}`, []string{"r1"}},
		{"not の中", `{"type":"not","value":{"type":"roleAssignedTo","roleId":"r1"}}`, []string{"r1"}},
		{"and の中", `{"type":"and","values":[{"type":"isCat"},{"type":"roleAssignedTo","roleId":"r1"}]}`, []string{"r1"}},
		{
			"深い入れ子で複数",
			`{"type":"or","values":[{"type":"not","value":{"type":"roleAssignedTo","roleId":"a"}},{"type":"and","values":[{"type":"roleAssignedTo","roleId":"b"}]}]}`,
			[]string{"a", "b"},
		},
		{"roleId が空なら拾わない", `{"type":"roleAssignedTo"}`, nil},
		{"参照が無い", `{"type":"isCat"}`, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var f CondFormula
			require.NoError(t, json.Unmarshal([]byte(tt.raw), &f))
			assert.Equal(t, tt.want, CondReferencedRoleIDs(f))
		})
	}
}

// attackerAccounts builds the accounts an attacker can actually get on this
// instance: local, not suspended, holding no manually assigned role. Everything
// the attacker does control is varied — the profile toggles, the three counters
// (throwaway accounts can move them), and **the account's age, because the
// attacker can wait** (#3045).
func attackerAccounts(t *testing.T, g id.Generator, now time.Time) []*model.User {
	t.Helper()
	ages := []time.Duration{
		0, time.Second, time.Hour, 24 * time.Hour,
		30 * 24 * time.Hour, 365 * 24 * time.Hour, 5 * 365 * 24 * time.Hour,
	}
	counts := []int{0, 1, 10, 100}
	var users []*model.User
	for _, age := range ages {
		for _, bot := range []bool{false, true} {
			for _, cat := range []bool{false, true} {
				for _, locked := range []bool{false, true} {
					for _, explorable := range []bool{false, true} {
						for _, followers := range counts {
							for _, following := range counts {
								for _, notes := range counts {
									users = append(users, &model.User{
										ID:             g.Generate(now.Add(-age)),
										Host:           nil,
										IsSuspended:    false,
										IsBot:          bot,
										IsCat:          cat,
										IsLocked:       locked,
										IsExplorable:   explorable,
										FollowersCount: followers,
										FollowingCount: following,
										NotesCount:     notes,
									})
								}
							}
						}
					}
				}
			}
		}
	}
	require.NotEmpty(t, users)
	return users
}

// randomFormula builds one deterministic pseudo-random formula, including the
// degenerate shapes (empty operand lists, `not` without an operand, unknown
// types, non-positive `sec`) that the guard has to answer.
func randomFormula(r *rand.Rand, depth int) CondFormula {
	leaves := []CondFormulaType{
		CondTypeIsLocal, CondTypeIsRemote, CondTypeIsSuspended, CondTypeIsLocked,
		CondTypeIsBot, CondTypeIsCat, CondTypeIsExplorable,
		CondTypeCreatedLessThan, CondTypeCreatedMoreThan,
		CondTypeFollowersLessThanOrEq, CondTypeFollowersMoreThanOrEq,
		CondTypeFollowingLessThanOrEq, CondTypeFollowingMoreThanOrEq,
		CondTypeNotesLessThanOrEq, CondTypeNotesMoreThanOrEq,
		CondTypeRoleAssignedTo, "somethingNew",
	}
	if depth > 0 && r.Intn(3) == 0 {
		switch r.Intn(3) {
		case 0, 1:
			typ := CondTypeAnd
			if r.Intn(2) == 0 {
				typ = CondTypeOr
			}
			n := r.Intn(3) // 0 operand (= 恒真 / 恒偽) も作る
			values := make([]CondFormula, 0, n)
			for i := 0; i < n; i++ {
				values = append(values, randomFormula(r, depth-1))
			}
			return CondFormula{Type: typ, Values: values}
		default:
			if r.Intn(8) == 0 {
				return CondFormula{Type: CondTypeNot} // operand 無し
			}
			nested := randomFormula(r, depth-1)
			return CondFormula{Type: CondTypeNot, Value: &nested}
		}
	}
	leaf := CondFormula{Type: leaves[r.Intn(len(leaves))]}
	leaf.Sec = []int64{-31536000, 0, 1, 86400, 2592000, 31536000}[r.Intn(6)]
	leaf.NumValue = []int64{-1, 0, 10, 100}[r.Intn(4)]
	if r.Intn(2) == 0 {
		leaf.RoleID = "staff"
	}
	return leaf
}

// **「通す」と答えた式に、攻撃者が用意できるアカウントが 1 つでも一致しては
// ならない (#3045)。** 判定 (`condSatisfiable`) と評価 (`evalCondAt`) は別々の
// 再帰なので、葉 1 つの極性を取り違えるだけで両者がずれる — #3044 の
// `createdLessThan` の否定側がまさにそれで、テーブル駆動のケースは
// **極性ごとに 1 つずつ書かないと片側が無検証で残る**。
//
// 見るのは**片方向だけ** — 「拒否したが実は誰も満たせない」(偽陽性) は
// 安全側なので許す。グリッドに居ないだけの witness もあるため、そちらを
// 検査にすると正当な実装が落ちる。
//
// **だからテーブル駆動のケースと両輪で、どちらも要る。** 判定表 15 キー x
// 2 極性 + `condLeafAgeBased` の 3 形 = 33 形を 1 つずつ反転した実測では、
// ここだけだと 5 形 (`isLocal.negative` / `isRemote.positive` /
// `isSuspended.positive` / `roleAssignedTo.positive` /
// `ageBased.positive -> true` = どれも「通しすぎ」ではなく**拒否しすぎ**の
// 方向。`ageBased.positive -> false` のほうはここが検出する) が緑のまま残り、
// `TestCondDependsOnUserControlledValue` だけだと 12 形が残る。両方で 0。
// **片方を「もう一方に包含される」と思って整理しないこと** — 拒否しすぎ側が
// 無検証になると、`and(isLocal, roleAssignedTo staff)` のような正当な設定を
// 弾く回帰が緑で通る。
func TestGuardIsSoundAgainstAttackerReachableAccounts(t *testing.T) {
	g, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	now := time.Now()
	users := attackerAccounts(t, g, now)

	// seed は固定する。ランダムだと失敗を手元で再現できず、required check が
	// 不定期に赤くなる (#2795 と同じ理由)。
	r := rand.New(rand.NewSource(3045))
	const formulas = 3000
	checked := 0
	seenLeaves := map[CondFormulaType]bool{}
	for i := 0; i < formulas; i++ {
		f := randomFormula(r, 3)
		collectLeafTypes(f, seenLeaves)
		if CondDependsOnUserControlledValue(f) {
			continue // 拒否側。ここでは何も主張しない
		}
		checked++
		for _, u := range users {
			if evalCondAt(u, nil, f, g, now) {
				raw, _ := json.Marshal(f)
				t.Fatalf("通すと判定した式に一致するアカウントがある: %s (user id=%s bot=%v cat=%v locked=%v explorable=%v followers=%d following=%d notes=%d)",
					raw, u.ID, u.IsBot, u.IsCat, u.IsLocked, u.IsExplorable,
					u.FollowersCount, u.FollowingCount, u.NotesCount)
			}
		}
	}
	// **「通す」と答えた式が 1 つも無いと、この検査は何も見ていない。**
	// 生成器を壊したときに「違反 0 件」と区別が付かなくなるので下限を置く。
	require.Greater(t, checked, 100, "生成した式のうち guard が通したもの")
	// **件数だけでは足りない (実測)。** 葉を全部 `somethingNew` に潰しても、
	// 空の `or` のような恒偽の合成が「通す」側に入るので件数は埋まる。
	// 判定表を素通りしたまま緑になるので、**葉の型が出そろっているか**も見る。
	leafTypes := 0
	for _, typ := range declaredCondFormulaTypes(t) {
		switch typ {
		case CondTypeAnd, CondTypeOr, CondTypeNot:
			continue
		}
		leafTypes++
		assert.True(t, seenLeaves[typ], "生成した式に %s が 1 度も出ていない", typ)
	}
	// 抽出が空振りするとループが 0 回になり、**この 2 本目が無言で消える**。
	// upstream の葉は 16 種。
	require.GreaterOrEqual(t, leafTypes, 16, "cond_formula.go から拾った葉の型")
}

// collectLeafTypes records every non-composite type reachable in the formula.
func collectLeafTypes(f CondFormula, into map[CondFormulaType]bool) {
	switch f.Type {
	case CondTypeAnd, CondTypeOr:
		for _, v := range f.Values {
			collectLeafTypes(v, into)
		}
	case CondTypeNot:
		if f.Value != nil {
			collectLeafTypes(*f.Value, into)
		}
	default:
		into[f.Type] = true
	}
}

// **宣言した type は全部、明示的に判定を決めておく (#3045)。**
//
// 決めていない type は `condSatisfiable` の fail-closed な既定 (「知らない型は
// 拒否側」) に落ちる。拒否側なので危険側には倒れないが、**判定表からエントリが
// 消えても外から見える挙動が変わらない**ので、テーブルを壊す変更が黙って通る。
// #3045 で `createdMoreThan` を `condLeafWithThreshold` から表へ移したときに
// 実際にそうなった (エントリを消しても他のテストは全て緑のままだった)。
//
// あわせて、合成 (`and` / `or` / `not`) は `condSatisfiable` 自身が畳むので
// 表には**置かない**ことも固定する。置くと operand を畳まずに葉として答える。
func TestEveryCondTypeHasAnExplicitDecision(t *testing.T) {
	composite := map[CondFormulaType]bool{
		CondTypeAnd: true, CondTypeOr: true, CondTypeNot: true,
	}
	types := declaredCondFormulaTypes(t)
	// 抽出が空振りすると「検査していないのに緑」になる。upstream の
	// `RoleService.evalCond` は 19 variants (葉 16 + and / or / not)。
	require.GreaterOrEqual(t, len(types), 19, "cond_formula.go から拾った型")
	for _, typ := range types {
		t.Run(string(typ), func(t *testing.T) {
			_, inTable := condLeafSatisfiability[typ]
			_, inAgeBased := condLeafAgeBased(CondFormula{Type: typ})
			if composite[typ] {
				assert.False(t, inTable, "合成は condSatisfiable が畳むので表に置かない")
				assert.False(t, inAgeBased, "合成は condSatisfiable が畳むので年齢側にも置かない")
				return
			}
			assert.True(t, inTable != inAgeBased,
				"葉はちょうど 1 つの経路で判定すること (表=%v / 年齢=%v)", inTable, inAgeBased)
		})
	}
}

// declaredCondFormulaTypes reads the CondFormulaType constants out of the
// source so a type added upstream joins the check without anyone remembering
// to update a second list.
func declaredCondFormulaTypes(t *testing.T) []CondFormulaType {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "cond_formula.go", nil, 0)
	require.NoError(t, err)
	var out []CondFormulaType
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := vs.Type.(*ast.Ident); !ok || ident.Name != "CondFormulaType" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				out = append(out, CondFormulaType(unquoted))
			}
		}
	}
	return out
}
