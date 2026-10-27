package paramschema

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the lower bound of the conformance corpus: every keyword of
// every upstream paramDef that can produce errors[0] must have a case the
// corpus rejects at exactly that schemaPath. A case count alone does not do
// this: the fixed inputs the generator makes for every endpoint already reach
// any small count, so a generator that lost all its mutations stayed green.
//
// **件数ではなく、keywordの位置を名指しで要求する(#3673、CLAUDE.md Section 4
// 「ゲートを書くとき」)。** 届かない位置は、構造で決まるものを規則で、それ以外を
// 1件ずつ、理由と一緒に除く。どちらも、使われていない(除く必要が無くなった)
// ものがあれば落とす。

// errorKeywords are the keywords whose schemaPath can be ajv's errors[0].
var errorKeywords = []string{"type", "enum", "required", "maximum", "minimum", "maxLength", "minLength", "pattern", "format", "maxItems", "minItems", "uniqueItems", "anyOf", "oneOf", "if"}

// step is how a schema node was reached from its parent.
type step struct {
	parent map[string]any
	via    string // "properties", "items", "anyOf", "oneOf", "allOf", "if", "then", "else"
	index  int    // branch index under anyOf / oneOf / allOf
}

// keywordPosition is one keyword of one schema node.
type keywordPosition struct {
	ep, path, keyword string
	node              map[string]any
	steps             []step // from the root to node
}

func keywordPositions(ep string, root map[string]any) []keywordPosition {
	var out []keywordPosition
	var walk func(node map[string]any, path string, steps []step)
	walk = func(node map[string]any, path string, steps []step) {
		for _, kw := range errorKeywords {
			if _, ok := node[kw]; ok {
				out = append(out, keywordPosition{ep: ep, path: path + "/" + kw, keyword: kw, node: node, steps: steps})
			}
		}
		child := func(v any, p string, s step) {
			if m, ok := v.(map[string]any); ok {
				walk(m, p, append(append([]step{}, steps...), s))
			}
		}
		if props, ok := node["properties"].(map[string]any); ok {
			for name, v := range props {
				child(v, path+"/properties/"+escapeFragment(name), step{parent: node, via: "properties"})
			}
		}
		child(node["items"], path+"/items", step{parent: node, via: "items"})
		for _, kw := range []string{"anyOf", "oneOf", "allOf"} {
			branches, _ := node[kw].([]any)
			for i, b := range branches {
				child(b, path+"/"+kw+"/"+strconv.Itoa(i), step{parent: node, via: kw, index: i})
			}
		}
		for _, kw := range []string{"if", "then", "else"} {
			child(node[kw], path+"/"+kw, step{parent: node, via: kw})
		}
	}
	walk(root, "#", nil)
	return out
}

// schemaTypes is the type keyword plus null when nullable (nil without type).
func schemaTypes(node map[string]any) []string {
	t, ok := node["type"].(string)
	if !ok {
		return nil
	}
	if nullable, _ := node["nullable"].(bool); nullable && t != "null" {
		return []string{t, "null"}
	}
	return []string{t}
}

// exclusiveOneOf reports whether no value can pass two branches of a oneOf:
// each pair of branches has disjoint types, or both require a property whose
// enums do not overlap.
func exclusiveOneOf(node map[string]any) bool {
	branches, _ := node["oneOf"].([]any)
	for i := range branches {
		for j := i + 1; j < len(branches); j++ {
			a, _ := branches[i].(map[string]any)
			b, _ := branches[j].(map[string]any)
			if !disjointTypes(a, b) && !disjointRequiredEnum(a, b) {
				return false
			}
		}
	}
	return true
}

func disjointTypes(a, b map[string]any) bool {
	ta, tb := schemaTypes(a), schemaTypes(b)
	if ta == nil || tb == nil {
		return false
	}
	for _, x := range ta {
		for _, y := range tb {
			if x == y || x == "number" && y == "integer" || x == "integer" && y == "number" {
				return false
			}
		}
	}
	return true
}

func disjointRequiredEnum(a, b map[string]any) bool {
	enumOf := func(s map[string]any, name string) ([]any, bool) {
		req, _ := s["required"].([]any)
		found := false
		for _, r := range req {
			found = found || r == name
		}
		props, _ := s["properties"].(map[string]any)
		p, _ := props[name].(map[string]any)
		e, ok := p["enum"].([]any)
		return e, found && ok
	}
	propsA, _ := a["properties"].(map[string]any)
	for name := range propsA {
		ea, okA := enumOf(a, name)
		eb, okB := enumOf(b, name)
		if !okA || !okB {
			continue
		}
		overlap := false
		for _, x := range ea {
			for _, y := range eb {
				overlap = overlap || x == y
			}
		}
		if !overlap {
			return true
		}
	}
	return false
}

// unreachableRules exclude keyword positions that cannot be errors[0] for
// structural reasons.
var unreachableRules = []struct {
	name, reason string
	match        func(keywordPosition) bool
}{
	{
		name:   "if",
		reason: "ajvはifの中の誤りを記録しない(createErrors: false)。thenやelseが落ちると、枝の中の誤りが#/ifの誤りより先に入る",
		match: func(p keywordPosition) bool {
			if p.keyword == "if" {
				return true
			}
			for _, s := range p.steps {
				if s.via == "if" {
					return true
				}
			}
			return false
		},
	},
	{
		name:   "cannot-fail",
		reason: "required: []、minLength: 0、minItems: 0、uniqueItems: falseは、どの値でも誤りにならない",
		match: func(p keywordPosition) bool {
			switch v := p.node[p.keyword].(type) {
			case []any:
				return p.keyword == "required" && len(v) == 0
			case float64:
				return (p.keyword == "minLength" || p.keyword == "minItems") && v == 0
			case bool:
				return p.keyword == "uniqueItems" && !v
			}
			return false
		},
	},
	{
		name:   "required-with-defaults",
		reason: "requiredの名前がすべて同じnodeのpropertiesにdefaultを持つ。defaultはrequiredより先に入る",
		match: func(p keywordPosition) bool {
			req, _ := p.node["required"].([]any)
			props, _ := p.node["properties"].(map[string]any)
			if p.keyword != "required" || len(req) == 0 {
				return false
			}
			for _, r := range req {
				name, _ := r.(string)
				prop, _ := props[name].(map[string]any)
				if _, ok := prop["default"]; !ok {
					return false
				}
			}
			return true
		},
	},
	{
		name:   "anyOf",
		reason: "anyOfが落ちるとき、1つ目の枝の誤りがanyOf自身の誤りと2つ目以降の枝の誤りより先に入る",
		match: func(p keywordPosition) bool {
			if p.keyword == "anyOf" {
				return true
			}
			for _, s := range p.steps {
				if s.via == "anyOf" && s.index > 0 {
					return true
				}
			}
			return false
		},
	},
	{
		name:   "exclusive-oneOf",
		reason: "枝が互いに排他な(型か、必須の項目のenumが重ならない)oneOfは、2つの枝が同時に通らないので、oneOf自身の誤りが出ない",
		match: func(p keywordPosition) bool {
			return p.keyword == "oneOf" && exclusiveOneOf(p.node)
		},
	},
	{
		name:   "oneOf-later-branch",
		reason: "oneOfのi番目(i>=1)の枝の誤りが先頭に来るには、それより前の枝がすべて通り、あとの枝がもう1つ通る必要がある。前の2つが通った時点でajvは止まるので、届くのは枝が3つ以上あるときの2つ目だけ",
		match: func(p keywordPosition) bool {
			for _, s := range p.steps {
				if s.via != "oneOf" || s.index == 0 {
					continue
				}
				branches, _ := s.parent["oneOf"].([]any)
				if s.index >= 2 || len(branches) == 2 {
					return true
				}
			}
			return false
		},
	},
	{
		name:   "type-checked-by-parent",
		reason: "anyOf / oneOf / allOfの枝の型が親の型(nullableを含む)を含み、親が型を最初に見る形(型が2つか、その型の組のkeywordを持たない)である。型の違う値は、枝より先に親の型で落ちる。親が型の組のkeywordを持つと、ajvは親の型を組の中へ後回しにし、枝のtypeが先に出るので除かない",
		match: func(p keywordPosition) bool {
			if p.keyword != "type" || len(p.steps) == 0 {
				return false
			}
			last := p.steps[len(p.steps)-1]
			if last.via != "anyOf" && last.via != "oneOf" && last.via != "allOf" {
				return false
			}
			parent, own := schemaTypes(last.parent), schemaTypes(p.node)
			if parent == nil {
				return false
			}
			if len(parent) == 1 {
				for _, kw := range groupKeywords[parent[0]] {
					if _, ok := last.parent[kw]; ok {
						return false
					}
				}
			}
			for _, t := range parent {
				found := false
				for _, o := range own {
					found = found || t == o
				}
				if !found {
					return false
				}
			}
			return true
		},
	},
}

// unreachableEntries are the remaining positions that cannot be errors[0],
// keyed by "<endpoint> <schemaPath>".
var unreachableEntries = map[string]string{}

func init() {
	// users/showなどは、allOfの1つ目がanyOfで、その枝はすべてtype: objectを持つ。
	// objectでない値はallOfの1つ目で落ちるので、2つ目のtype: objectには届かない。
	for _, ep := range []string{"admin/emoji/update", "notes/search-by-tag", "users/followers", "users/following", "users/search-by-username-and-host", "users/show"} {
		unreachableEntries[ep+" #/allOf/1/type"] = "allOfの1つ目のanyOfの全部の枝がtype: objectで、objectでない値はそこで落ちる"
	}
}

func TestCorpusRejectsAtEveryReachableKeyword(t *testing.T) {
	g := readGolden(t)
	_, _, cases := readCorpus(t)
	// Elythiaの宣言(#3869)も、本家のものと同じく全部の位置で拒否されること
	// を求める。名前は本家と重ならない(addElythiaEndpointsが拒否する)。
	_, _, elythiaCases := readCorpusFile(t, elythiaCorpusFile)
	cases = append(cases, elythiaCases...)
	paramDefs := map[string]json.RawMessage{}
	for name, ep := range g.Endpoints {
		paramDefs[name] = ep.ParamDef
	}
	elythia := readElythiaParamDefs(t)
	for name, pd := range elythia {
		paramDefs[name] = pd
	}
	rejected := map[string]bool{}
	for _, c := range cases {
		if !*c.OK {
			rejected[c.ep+" "+c.Path] = true
		}
	}
	ruleUsed := map[string]bool{}
	entryUsed := map[string]bool{}
	var missing []string
	positions := 0
	elythiaPositions := 0
	for name, pd := range paramDefs {
		var root map[string]any
		if err := json.Unmarshal(pd, &root); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, p := range keywordPositions(name, root) {
			positions++
			if _, ok := elythia[name]; ok {
				elythiaPositions++
			}
			key := p.ep + " " + p.path
			matched := false
			for _, r := range unreachableRules {
				if !r.match(p) {
					continue
				}
				if rejected[key] {
					// ajvがそこで拒否しているので、規則が広すぎる。
					t.Errorf("rule %q excludes %s, but the corpus rejects there", r.name, key)
				}
				ruleUsed[r.name] = true
				matched = true
			}
			if rejected[key] {
				continue
			}
			if _, ok := unreachableEntries[key]; ok {
				if matched {
					t.Errorf("%s is excluded by a rule and by an entry; drop the entry", key)
				}
				entryUsed[key] = true
				matched = true
			}
			if !matched {
				missing = append(missing, key)
			}
		}
	}
	// 位置を1つも数えられないなら、走査が壊れている。
	if positions < len(g.Endpoints) || elythiaPositions < len(elythia) {
		t.Fatalf("found only %d keyword positions (%d of Elythia's)", positions, elythiaPositions)
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("no case in the corpus is rejected at %s (fix tools/paramdefspec/corpus.mjs, or exclude it with a reason)", m)
	}
	for _, r := range unreachableRules {
		if !ruleUsed[r.name] {
			t.Errorf("unreachable rule %q excludes nothing; remove it", r.name)
		}
	}
	for key := range unreachableEntries {
		if !entryUsed[key] {
			t.Errorf("unreachable entry %q is not needed (the corpus rejects there, or a rule covers it); remove it", key)
		}
	}
}

func TestKeywordPositionsWalkEverySubschema(t *testing.T) {
	var root map[string]any
	src := `{"type":"object","properties":{"a/b":{"type":"array","items":{"enum":[1]}}},"anyOf":[{"required":["x"]}],"oneOf":[{"type":"string"}],"allOf":[{"minimum":1}],"if":{"type":"null"},"then":{"maxLength":1},"else":{"pattern":"a"}}`
	if err := json.Unmarshal([]byte(src), &root); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range keywordPositions("ep", root) {
		got = append(got, p.path)
	}
	sort.Strings(got)
	want := []string{"#/allOf/0/minimum", "#/anyOf", "#/anyOf/0/required", "#/else/pattern", "#/if", "#/if/type", "#/oneOf", "#/oneOf/0/type", "#/properties/a~1b/items/enum", "#/properties/a~1b/type", "#/then/maxLength", "#/type"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestExclusiveOneOf(t *testing.T) {
	tests := []struct {
		schema string
		want   bool
	}{
		{`{"oneOf":[{"type":"string"},{"type":"array"}]}`, true},
		{`{"oneOf":[{"type":"string"},{"type":"string","nullable":true}]}`, false},
		{`{"oneOf":[{"type":"integer"},{"type":"number"}]}`, false},
		{`{"oneOf":[{"type":"string"},{}]}`, false},
		{`{"oneOf":[{"type":"object","properties":{"k":{"enum":["a"]}},"required":["k"]},{"type":"object","properties":{"k":{"enum":["b"]}},"required":["k"]}]}`, true},
		{`{"oneOf":[{"type":"object","properties":{"k":{"enum":["a"]}},"required":["k"]},{"type":"object","properties":{"k":{"enum":["a","b"]}},"required":["k"]}]}`, false},
		{`{"oneOf":[{"type":"object","properties":{"k":{"enum":["a"]}}},{"type":"object","properties":{"k":{"enum":["b"]}},"required":["k"]}]}`, false},
	}
	for _, tt := range tests {
		var node map[string]any
		if err := json.Unmarshal([]byte(tt.schema), &node); err != nil {
			t.Fatal(err)
		}
		if got := exclusiveOneOf(node); got != tt.want {
			t.Errorf("exclusiveOneOf(%s) = %v, want %v", tt.schema, got, tt.want)
		}
	}
	if strings.Join(schemaTypes(map[string]any{"type": "null", "nullable": true}), ",") != "null" {
		t.Error("schemaTypes of nullable null")
	}
}

// TestTypeCheckedByParentRule pins when a branch's type is excluded: only when
// the parent checks its type before the branches. With a keyword of its type's
// group, ajv defers the parent's type error into that group, after anyOf /
// oneOf / allOf, so the branch's type is errors[0] (checked against ajv 8.20.0).
func TestTypeCheckedByParentRule(t *testing.T) {
	tests := []struct {
		schema, path string
		want         bool
	}{
		{`{"type":"object","oneOf":[{"type":"object","required":["a"]}]}`, "#/oneOf/0/type", true},
		{`{"type":"object","nullable":true,"properties":{"a":{}},"oneOf":[{"type":"object","nullable":true}]}`, "#/oneOf/0/type", true},
		{`{"type":"object","properties":{"a":{"type":"string"}},"oneOf":[{"type":"object","required":["a"]}]}`, "#/oneOf/0/type", false},
		{`{"type":"string","maxLength":5,"anyOf":[{"type":"string","pattern":"^a"}]}`, "#/anyOf/0/type", false},
		{`{"type":"array","items":{"type":"string"},"allOf":[{"type":"array","maxItems":3}]}`, "#/allOf/0/type", false},
		{`{"type":"integer","maximum":3,"anyOf":[{"type":"integer"}]}`, "#/anyOf/0/type", true},
		{`{"type":"object","oneOf":[{"type":"string"}]}`, "#/oneOf/0/type", false},
		{`{"oneOf":[{"type":"string"}]}`, "#/oneOf/0/type", false},
		{`{"type":"object","properties":{"a":{"type":"object"}}}`, "#/properties/a/type", false},
	}
	var rule func(keywordPosition) bool
	for _, r := range unreachableRules {
		if r.name == "type-checked-by-parent" {
			rule = r.match
		}
	}
	for _, tt := range tests {
		var root map[string]any
		if err := json.Unmarshal([]byte(tt.schema), &root); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range keywordPositions("ep", root) {
			if p.path == tt.path {
				found = true
				if got := rule(p); got != tt.want {
					t.Errorf("%s %s: got %v, want %v", tt.schema, tt.path, got, tt.want)
				}
			}
		}
		if !found {
			t.Errorf("%s has no %s", tt.schema, tt.path)
		}
	}
}
