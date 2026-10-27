package paramschema

import (
	"strconv"
)

// Error is the first validation error, as ajv reports it in errors[0].
type Error struct {
	// SchemaPath is ajv's schemaPath, such as "#/properties/limit/maximum".
	SchemaPath string
	// Message is ajv's message, such as "must be <= 100".
	Message string
}

func (e *Error) Error() string {
	return e.SchemaPath + ": " + e.Message
}

// Validate checks data, a value decoded by encoding/json (with or without
// UseNumber), against the schema. Like ajv with useDefaults, it inserts the
// defaults of absent properties into the objects (maps) of data in place,
// before it checks required. It returns nil when data is valid, or the first
// error otherwise; the defaults inserted before the error stay.
func (s *Schema) Validate(data any) *Error {
	v := &validation{}
	if v.validate(s.root, data) {
		return nil
	}
	return &v.errors[0]
}

// validation collects errors like ajv's vErrors. Inside anyOf / oneOf the
// errors of failed branches stay until a branch passes, so errors[0] can be an
// error from a branch rather than the anyOf / oneOf error itself.
type validation struct {
	errors []Error
}

func (v *validation) fail(n *node, keyword, message string) bool {
	v.errors = append(v.errors, Error{SchemaPath: n.path + "/" + keyword, Message: message})
	return false
}

func (v *validation) typeError(n *node) bool {
	return v.fail(n, "type", "must be "+n.typeName)
}

// validate reports whether data is valid against n. It stops at the first
// failing keyword, as ajv does without allErrors.
//
// **keywordの順はajvの生成するコードに合わせる(#3673)。** ajvはkeywordを、
// 型を問わない組(enum、anyOf、oneOf、allOf、if) → number → string → array →
// objectの組の順に検査し、組の中はajvに登録された順(maximumがminimumより
// 先、maxLengthがminLengthより先、itemsがuniqueItemsより先、requiredが
// propertiesより先)に見る。型の誤りは、型が2つ(nullable)か、型の組の
// keywordがこのschemaに無ければ最初に、そうでなければ組の中で報告する。
func (v *validation) validate(n *node, data any) bool {
	if n.typeUpFront && !isAnyType(n.types, data) {
		return v.typeError(n)
	}
	if !v.untypedGroup(n, data) {
		return false
	}
	for _, g := range groupOrder {
		if !n.usesGroup[g] {
			continue
		}
		// 組に入るかはajvのcheckDataTypeで決まる(numberは有限の数だけ)。
		if !isType(g, data) {
			if n.inGroupType == g {
				return v.typeError(n)
			}
			continue
		}
		var ok bool
		switch g {
		case "number":
			ok = v.numberGroup(n, data)
		case "string":
			ok = v.stringGroup(n, data.(string))
		case "array":
			ok = v.arrayGroup(n, data.([]any))
		default:
			ok = v.objectGroup(n, data.(map[string]any))
		}
		if !ok {
			return false
		}
	}
	return true
}

func (v *validation) untypedGroup(n *node, data any) bool {
	if n.enum != nil {
		found := false
		for _, e := range n.enum {
			if jsEqual(data, e) {
				found = true
				break
			}
		}
		if s, ok := data.(string); ok && !found && n.enumPattern != nil {
			found = n.enumPattern.MatchString(s)
		}
		if !found {
			return v.fail(n, "enum", "must be equal to one of the allowed values")
		}
	}
	if n.anyOf != nil {
		start := len(v.errors)
		passed := false
		for _, b := range n.anyOf {
			if v.validate(b, data) {
				passed = true
				break
			}
		}
		if !passed {
			return v.fail(n, "anyOf", "must match a schema in anyOf")
		}
		v.errors = v.errors[:start]
	}
	if n.oneOf != nil {
		start := len(v.errors)
		passed := false
		for _, b := range n.oneOf {
			if !v.validate(b, data) {
				continue
			}
			if passed {
				// 2つ目が通った時点で、残りのbranchは見ない。
				passed = false
				break
			}
			passed = true
		}
		if !passed {
			return v.fail(n, "oneOf", "must match exactly one schema in oneOf")
		}
		v.errors = v.errors[:start]
	}
	for _, b := range n.allOf {
		if !v.validate(b, data) {
			return false
		}
	}
	if n.ifN != nil {
		// ifの誤りは記録しない(ajvのcreateErrors: false)。
		start := len(v.errors)
		ifOK := v.validate(n.ifN, data)
		v.errors = v.errors[:start]
		clause, keyword := n.elseN, "else"
		if ifOK {
			clause, keyword = n.thenN, "then"
		}
		if clause != nil && !v.validate(clause, data) {
			return v.fail(n, "if", `must match "`+keyword+`" schema`)
		}
	}
	return true
}

func (v *validation) numberGroup(n *node, data any) bool {
	f, _ := number(data)
	if n.maximum != nil && f > *n.maximum {
		return v.fail(n, "maximum", "must be <= "+jsNumberString(*n.maximum))
	}
	if n.minimum != nil && f < *n.minimum {
		return v.fail(n, "minimum", "must be >= "+jsNumberString(*n.minimum))
	}
	// formatはnumberの組にも入るが、misskey:idはstringのformatなので何もしない。
	return true
}

func (v *validation) stringGroup(n *node, s string) bool {
	if n.maxLength != nil && ucs2length(s) > *n.maxLength {
		return v.fail(n, "maxLength", "must NOT have more than "+strconv.Itoa(*n.maxLength)+" characters")
	}
	if n.minLength != nil && ucs2length(s) < *n.minLength {
		return v.fail(n, "minLength", "must NOT have fewer than "+strconv.Itoa(*n.minLength)+" characters")
	}
	if n.pattern != nil && !n.pattern.MatchString(s) {
		return v.fail(n, "pattern", `must match pattern "`+n.patternSource+`"`)
	}
	if n.formatFn != nil && !n.formatFn(s) {
		return v.fail(n, "format", `must match format "`+n.format+`"`)
	}
	return true
}

func (v *validation) arrayGroup(n *node, arr []any) bool {
	if n.maxItems != nil && len(arr) > *n.maxItems {
		return v.fail(n, "maxItems", "must NOT have more than "+strconv.Itoa(*n.maxItems)+" items")
	}
	if n.minItems != nil && len(arr) < *n.minItems {
		return v.fail(n, "minItems", "must NOT have fewer than "+strconv.Itoa(*n.minItems)+" items")
	}
	if n.items != nil {
		for _, el := range arr {
			if !v.validate(n.items, el) {
				return false
			}
		}
	}
	if n.uniqueItems {
		if i, j, dup := duplicateItems(n, arr); dup {
			return v.fail(n, "uniqueItems", "must NOT have duplicate items (items ## "+strconv.Itoa(j)+" and "+strconv.Itoa(i)+" are identical)")
		}
	}
	return true
}

// duplicateItems finds the pair ajv's uniqueItems reports, returning ajv's
// (i, j) for the message "items ## j and i are identical".
//
// **ajvは、itemsの型がprimitiveだけのとき、末尾から値をobjectのキーにして
// 登録していき、既に登録された値に当たったら(i=今の位置、j=登録済みの後ろの
// 位置)を報告する(#3673)。** それ以外(型の無いitems、object・配列のitems)の
// 総当たりはfast-deep-equalで比べるので、Compileで拒んでいる(jsEqualを参照)。
func duplicateItems(n *node, arr []any) (i, j int, dup bool) {
	itemTypes := n.items.types
	// ajvはここでitemsの型に合わない要素を飛ばすが、itemsはuniqueItemsより
	// 先に検査され、合わない要素があればそこで止まるので、ここには来ない。
	indices := map[string]int{}
	for i := len(arr) - 1; i >= 0; i-- {
		key := uniqueKey(arr[i], len(itemTypes) > 1)
		// `indices["__proto__"]`はObject.prototypeを返し、数の代入は無視される。
		// そのためajvはこの値の重複を見つけない。
		if key == "__proto__" {
			continue
		}
		if j, ok := indices[key]; ok {
			return i, j, true
		}
		indices[key] = i
	}
	return 0, 0, false
}

// uniqueKey is the property key ajv's uniqueItems uses for a primitive item.
// With more than one item type, strings get a "_" suffix so that "1" and 1
// (or "null" and null) differ.
func uniqueKey(item any, multi bool) string {
	switch kindOf(item) {
	case kindString:
		s := item.(string)
		if multi {
			s += "_"
		}
		return s
	case kindBool:
		return strconv.FormatBool(item.(bool))
	case kindNull:
		return "null"
	}
	f, _ := number(item)
	return jsNumberString(f)
}

func (v *validation) objectGroup(n *node, obj map[string]any) bool {
	// defaultはrequiredより先に入る(ajvのiterateKeywords → assignDefaults)。
	for _, d := range n.defaults {
		if _, ok := obj[d.name]; !ok {
			obj[d.name] = deepCopy(d.value)
		}
	}
	for _, name := range n.required {
		if _, ok := obj[name]; !ok {
			return v.fail(n, "required", "must have required property '"+name+"'")
		}
	}
	for _, p := range n.properties {
		pv, ok := obj[p.name]
		if !ok {
			continue
		}
		if !v.validate(p.node, pv) {
			return false
		}
	}
	return true
}
