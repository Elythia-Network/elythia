// Package paramschema validates API parameters against upstream Misskey's
// paramDef the way upstream's ajv does (#3667, #3673).
//
// Upstream validates every endpoint's parameters with ajv 8 configured as
// `new Ajv({ useDefaults: true })` plus the format `misskey:id`
// (packages/backend/src/server/api/endpoint-base.ts), and answers the first
// error as INVALID_PARAM with info {param: schemaPath, reason: message}. This
// package reproduces that verdict, that first error and the inserted defaults
// for the subset of JSON Schema the paramDefs use, without a dependency:
//
//   - keywords: type, nullable, enum (of primitive values), properties,
//     required, additionalProperties (true only), items (one schema),
//     minItems, maxItems, uniqueItems (over items of primitive types),
//     minLength, maxLength (in code points), pattern (a JavaScript u-flag
//     RegExp), format (misskey:id), minimum, maximum, anyOf, oneOf, allOf,
//     if / then / else, default, description
//   - keywords are checked in ajv's order: the type (up front or with its
//     keyword group), then enum and the applicators, then the number, string,
//     array and object groups. Within the object group, defaults are inserted
//     first, then required, then properties in schema order. Validation stops
//     at the first error, like ajv without allErrors.
//
// Compile rejects anything else, including what ajv's strict mode rejects
// (an unknown keyword or format, a default inside anyOf / oneOf / if), so
// that an upstream paramDef this package cannot reproduce fails loudly instead
// of validating differently.
//
// The conformance test replays a corpus that ajv itself judged
// (testdata/ajv_corpus.jsonl, built by tools/paramdefspec) against every
// upstream paramDef (upstream_paramdefs.json).
package paramschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
)

// Schema is a compiled paramDef. It is safe for concurrent use.
type Schema struct {
	root *node
}

type property struct {
	name string
	node *node
}

// defaultValue is the default of the property name.
type defaultValue struct {
	name  string
	value any
}

// node is one compiled (sub)schema.
type node struct {
	// path is ajv's errSchemaPath of this node ("#", "#/properties/limit").
	path string

	// types is the type keyword plus "null" when nullable is true.
	types []string
	// typeName is the type keyword itself, for "must be <type>".
	typeName string
	// typeUpFront: ajv checks the type before every keyword, because there
	// are two types (nullable) or the type has no keyword group of its own
	// in this schema. Otherwise the type error belongs to inGroupType.
	typeUpFront bool
	inGroupType string

	enum []any
	// enumPattern is a string pattern that also passes enum. Only an Elythia
	// overlay sets it (x-elythia-enumPattern, overlay.go).
	enumPattern *regexp.Regexp
	anyOf       []*node
	oneOf       []*node
	allOf       []*node
	ifN         *node
	thenN       *node
	elseN       *node

	maximum, minimum *float64

	maxLength, minLength *int
	pattern              *regexp.Regexp
	patternSource        string
	format               string
	formatFn             func(string) bool

	maxItems, minItems *int
	items              *node
	uniqueItems        bool

	required   []string
	properties []property
	defaults   []defaultValue // in schema order

	// usesGroup records which typed keyword groups have a keyword here
	// (ajv's shouldUseGroup).
	usesGroup map[string]bool
}

// formats are the formats upstream adds with ajv.addFormat.
var formats = map[string]func(string) bool{
	// ajv.addFormat('misskey:id', /^[a-zA-Z0-9]+$/)
	"misskey:id": func(s string) bool {
		if s == "" {
			return false
		}
		for i := 0; i < len(s); i++ {
			c := s[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				return false
			}
		}
		return true
	},
}

// objectPrototypeNames are the names every JavaScript object inherits.
// ajv reads `data[name] === undefined`, so a required property with one of
// these names is always present upstream, and `__proto__` is skipped by
// properties. Compile refuses such names rather than emulate that.
var objectPrototypeNames = map[string]bool{
	"__proto__": true, "constructor": true, "hasOwnProperty": true, "isPrototypeOf": true,
	"propertyIsEnumerable": true, "toLocaleString": true, "toString": true, "valueOf": true,
	"__defineGetter__": true, "__defineSetter__": true, "__lookupGetter__": true, "__lookupSetter__": true,
}

// group keywords, in ajv's rule order within each group.
var groupKeywords = map[string][]string{
	"number": {"maximum", "minimum", "format"},
	"string": {"maxLength", "minLength", "pattern", "format"},
	"array":  {"maxItems", "minItems", "items", "uniqueItems"},
	"object": {"required", "additionalProperties", "properties"},
}

var groupOrder = []string{"number", "string", "array", "object"}

var knownTypes = map[string]bool{"null": true, "boolean": true, "object": true, "array": true, "number": true, "integer": true, "string": true}

var supportedKeywords = map[string]bool{
	"type": true, "nullable": true, "enum": true, "anyOf": true, "oneOf": true, "allOf": true,
	"if": true, "then": true, "else": true, "maximum": true, "minimum": true,
	"maxLength": true, "minLength": true, "pattern": true, "format": true,
	"maxItems": true, "minItems": true, "items": true, "uniqueItems": true,
	"required": true, "properties": true, "additionalProperties": true,
	"default": true, "description": true,
}

// Compile compiles a paramDef given as JSON. The order of the properties in
// the JSON is the order ajv checks them in, so it must be the order of the
// upstream object literal (as tools/paramdefspec writes it).
func Compile(paramDef []byte) (*Schema, error) {
	dec := json.NewDecoder(bytes.NewReader(paramDef))
	v, err := decodeOrdered(dec)
	if err != nil {
		return nil, fmt.Errorf("paramschema: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("paramschema: trailing data after the schema")
	}
	obj, ok := v.(*orderedObject)
	if !ok {
		return nil, errors.New("paramschema: schema must be an object")
	}
	// ajvのstrict modeは最上位のdefaultを誤りにする(checkNoDefault)。
	if _, ok := obj.get("default"); ok {
		return nil, errors.New("paramschema: #: default is ignored in the schema root")
	}
	root, err := compileNode(obj, "#", false)
	if err != nil {
		return nil, fmt.Errorf("paramschema: %w", err)
	}
	return &Schema{root: root}, nil
}

// compileNode compiles obj found at path. composite is true inside anyOf,
// oneOf and if, where ajv's strict mode rejects defaults.
func compileNode(obj *orderedObject, path string, composite bool) (*node, error) {
	n := &node{path: path, usesGroup: map[string]bool{}}
	for _, k := range obj.keys {
		if !supportedKeywords[k] {
			return nil, fmt.Errorf("%s: unsupported keyword %q", path, k)
		}
	}
	for _, g := range groupOrder {
		for _, k := range groupKeywords[g] {
			if _, ok := obj.get(k); ok {
				n.usesGroup[g] = true
			}
		}
	}
	steps := []func(*node, *orderedObject, bool) error{
		compileType, compileEnum, compileApplicators, compileNumberKeywords,
		compileStringKeywords, compileArrayKeywords, compileObjectKeywords, compileMetadata,
	}
	for _, step := range steps {
		if err := step(n, obj, composite); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func compileType(n *node, obj *orderedObject, _ bool) error {
	tv, hasType := obj.get("type")
	nv, hasNullable := obj.get("nullable")
	nullable := false
	if hasNullable {
		b, ok := nv.(bool)
		if !ok {
			return fmt.Errorf("%s: nullable must be a boolean", n.path)
		}
		nullable = b
	}
	if !hasType {
		if hasNullable {
			return fmt.Errorf(`%s: "nullable" cannot be used without "type"`, n.path)
		}
		return nil
	}
	t, ok := tv.(string)
	if !ok || !knownTypes[t] {
		// 型の配列(`type: [..]`)は使われていない。
		return fmt.Errorf("%s: unsupported type %v", n.path, tv)
	}
	n.typeName = t
	n.types = []string{t}
	if t == "null" {
		if hasNullable && !nullable {
			return fmt.Errorf("%s: type: null contradicts nullable: false", n.path)
		}
	} else if nullable {
		n.types = append(n.types, "null")
	}
	// ajv (coerceAndCheckDataType)は、型が1つで、その型のkeywordの組がこの
	// schemaにあるときだけ、型の誤りを組の中で報告する(enumやanyOfより後)。
	// integer / boolean / nullは組を持たないので、いつも先に見る。
	if len(n.types) == 1 && n.usesGroup[t] {
		n.inGroupType = t
	} else {
		n.typeUpFront = true
	}
	return nil
}

func compileEnum(n *node, obj *orderedObject, _ bool) error {
	ev, ok := obj.get("enum")
	if !ok {
		return nil
	}
	arr, ok := ev.([]any)
	if !ok || len(arr) == 0 {
		return fmt.Errorf("%s: enum must be a non-empty array", n.path)
	}
	for _, v := range arr {
		switch v.(type) {
		case *orderedObject, []any:
			// fast-deep-equalで比べる値。jsEqualのコメントを参照。
			return fmt.Errorf("%s: enum with an object or array value is unsupported", n.path)
		}
		n.enum = append(n.enum, v)
	}
	return nil
}

func compileApplicators(n *node, obj *orderedObject, composite bool) error {
	for _, kw := range []string{"anyOf", "oneOf", "allOf"} {
		v, ok := obj.get(kw)
		if !ok {
			continue
		}
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 {
			return fmt.Errorf("%s: %s must be a non-empty array", n.path, kw)
		}
		var subs []*node
		for i, x := range arr {
			o, ok := x.(*orderedObject)
			if !ok {
				return fmt.Errorf("%s/%s/%d: schema must be an object", n.path, kw, i)
			}
			// anyOfとoneOfの中はajvのcompositeRuleで、defaultを入れない
			// (strict modeではcompileの誤り)。allOfの中は入れる。
			sub, err := compileNode(o, fmt.Sprintf("%s/%s/%d", n.path, kw, i), composite || kw != "allOf")
			if err != nil {
				return err
			}
			subs = append(subs, sub)
		}
		switch kw {
		case "anyOf":
			n.anyOf = subs
		case "oneOf":
			n.oneOf = subs
		default:
			n.allOf = subs
		}
	}
	_, hasIf := obj.get("if")
	_, hasThen := obj.get("then")
	_, hasElse := obj.get("else")
	if hasIf != (hasThen || hasElse) {
		// ajvのstrict modeは、片方だけのif / then / elseを誤りにする。
		return fmt.Errorf("%s: if needs then or else (and they need if)", n.path)
	}
	for _, kw := range []string{"if", "then", "else"} {
		v, ok := obj.get(kw)
		if !ok {
			continue
		}
		o, ok := v.(*orderedObject)
		if !ok {
			return fmt.Errorf("%s/%s: schema must be an object", n.path, kw)
		}
		sub, err := compileNode(o, n.path+"/"+kw, composite || kw == "if")
		if err != nil {
			return err
		}
		switch kw {
		case "if":
			n.ifN = sub
		case "then":
			n.thenN = sub
		default:
			n.elseN = sub
		}
	}
	return nil
}

func compileNumberKeywords(n *node, obj *orderedObject, _ bool) error {
	for _, kw := range []string{"maximum", "minimum"} {
		v, ok := obj.get(kw)
		if !ok {
			continue
		}
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("%s: %s must be a number", n.path, kw)
		}
		if kw == "maximum" {
			n.maximum = &f
		} else {
			n.minimum = &f
		}
	}
	return nil
}

func nonNegativeInt(n *node, obj *orderedObject, kw string) (*int, error) {
	v, ok := obj.get(kw)
	if !ok {
		return nil, nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
		return nil, fmt.Errorf("%s: %s must be a non-negative integer", n.path, kw)
	}
	i := int(f)
	return &i, nil
}

func compileStringKeywords(n *node, obj *orderedObject, _ bool) error {
	var err error
	if n.maxLength, err = nonNegativeInt(n, obj, "maxLength"); err != nil {
		return err
	}
	if n.minLength, err = nonNegativeInt(n, obj, "minLength"); err != nil {
		return err
	}
	if v, ok := obj.get("pattern"); ok {
		src, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: pattern must be a string", n.path)
		}
		re, err := compileJSRegexp(src)
		if err != nil {
			return fmt.Errorf("%s: %w", n.path, err)
		}
		n.pattern, n.patternSource = re, src
	}
	if v, ok := obj.get("format"); ok {
		name, _ := v.(string)
		fn, ok := formats[name]
		if !ok {
			return fmt.Errorf("%s: unknown format %v", n.path, v)
		}
		n.format, n.formatFn = name, fn
	}
	return nil
}

func compileArrayKeywords(n *node, obj *orderedObject, composite bool) error {
	var err error
	if n.maxItems, err = nonNegativeInt(n, obj, "maxItems"); err != nil {
		return err
	}
	if n.minItems, err = nonNegativeInt(n, obj, "minItems"); err != nil {
		return err
	}
	if v, ok := obj.get("uniqueItems"); ok {
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("%s: uniqueItems must be a boolean", n.path)
		}
		n.uniqueItems = b
	}
	if v, ok := obj.get("items"); ok {
		o, ok := v.(*orderedObject)
		if !ok {
			// tuple (itemsの配列)は使われていない。
			return fmt.Errorf("%s: items must be a schema object", n.path)
		}
		if n.items, err = compileNode(o, n.path+"/items", composite); err != nil {
			return err
		}
	}
	if n.uniqueItems {
		// itemsの型がprimitiveだけでないと、ajvはfast-deep-equalで比べる。
		// jsEqualのコメントを参照。
		if n.items == nil || len(n.items.types) == 0 {
			return fmt.Errorf("%s: uniqueItems needs items with a type", n.path)
		}
		for _, t := range n.items.types {
			if t == "object" || t == "array" {
				return fmt.Errorf("%s: uniqueItems over %s items is unsupported", n.path, t)
			}
		}
	}
	return nil
}

func compileObjectKeywords(n *node, obj *orderedObject, composite bool) error {
	if v, ok := obj.get("additionalProperties"); ok {
		if b, ok := v.(bool); !ok || !b {
			// 使われているのはtrue (何もしない)だけ。
			return fmt.Errorf("%s: only additionalProperties: true is supported", n.path)
		}
	}
	if v, ok := obj.get("required"); ok {
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: required must be an array", n.path)
		}
		seen := map[string]bool{}
		for _, x := range arr {
			s, ok := x.(string)
			if !ok || seen[s] {
				return fmt.Errorf("%s: required must hold distinct strings", n.path)
			}
			if objectPrototypeNames[s] {
				return fmt.Errorf("%s: required property %q is inherited by every JavaScript object", n.path, s)
			}
			seen[s] = true
			n.required = append(n.required, s)
		}
	}
	if v, ok := obj.get("properties"); ok {
		props, ok := v.(*orderedObject)
		if !ok {
			return fmt.Errorf("%s: properties must be an object", n.path)
		}
		for _, name := range props.keys {
			if objectPrototypeNames[name] {
				return fmt.Errorf("%s: property %q is inherited by every JavaScript object", n.path, name)
			}
			pv, _ := props.get(name)
			po, ok := pv.(*orderedObject)
			if !ok {
				return fmt.Errorf("%s/properties/%s: schema must be an object", n.path, escapeFragment(name))
			}
			child, err := compileNode(po, n.path+"/properties/"+escapeFragment(name), composite)
			if err != nil {
				return err
			}
			n.properties = append(n.properties, property{name: name, node: child})
			if d, ok := po.get("default"); ok {
				if composite {
					return fmt.Errorf("%s/properties/%s: default is ignored inside anyOf / oneOf / if", n.path, escapeFragment(name))
				}
				n.defaults = append(n.defaults, defaultValue{name: name, value: toPlain(d)})
			}
		}
	}
	return nil
}

func compileMetadata(n *node, obj *orderedObject, _ bool) error {
	if v, ok := obj.get("description"); ok {
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: description must be a string", n.path)
		}
	}
	return nil
}

// orderedObject is a JSON object that remembers the order of its keys.
type orderedObject struct {
	keys []string
	vals map[string]any
}

func (o *orderedObject) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// decodeOrdered decodes the next JSON value, keeping object key order.
// Numbers are float64; duplicate keys keep the last value (like JSON.parse)
// at the position of the first.
func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &orderedObject{vals: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := o.vals[k]; !dup {
					o.keys = append(o.keys, k)
				}
				o.vals[k] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		default:
			// 閉じ括弧が先に来るJSONはTokenが誤りにするので、ここは'['。
			arr := []any{}
			for dec.More() {
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
	}
	return tok, nil
}

// toPlain converts decoded values to what encoding/json would produce
// (map[string]any for objects).
func toPlain(v any) any {
	switch v := v.(type) {
	case *orderedObject:
		m := make(map[string]any, len(v.keys))
		for _, k := range v.keys {
			m[k] = toPlain(v.vals[k])
		}
		return m
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = toPlain(x)
		}
		return out
	}
	return v
}
