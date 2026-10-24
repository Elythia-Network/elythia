package openapi

import "encoding/json"

// schemaKind selects how convertSchema treats a Misskey schema, like the type
// argument of upstream's convertSchemaToOpenApiSchema.
type schemaKind int

const (
	kindParam schemaKind = iota
	kindRes
)

// convertSchema ports convertSchemaToOpenApiSchema of upstream's
// server/api/openapi/schemas.ts with includeSelfRef = false, which is how
// upstream serves /api.json. It turns a Misskey schema (with optional /
// nullable / ref) into an OpenAPI schema and never modifies its input.
//
// 本家の処理を1行ずつ写している。項目の順も本家の/api.jsonと同じになるよう、
// 代入と削除をJSと同じ順で行う(object.setは既存の位置を保ち、新しい項目は末尾)。
func convertSchema(schema any, kind schemaKind) any {
	s, ok := schema.(*object)
	if !ok {
		// 本家はオブジェクトでないschemaを想定していない。deepCloneはそのまま返す。
		return clone(schema)
	}
	res := clone(s).(*object)
	for _, k := range []string{"optional", "nullable", "ref", "selfRef"} {
		res.del(k)
	}

	typ, _ := s.get("type")
	if typ == "object" {
		if props, ok := s.get("properties"); ok && truthy(props) {
			if p, ok := props.(*object); ok {
				resProps, _ := res.get("properties")
				rp := resProps.(*object)
				if kind == kindRes {
					var required []any
					for _, k := range p.keys {
						if !truthy(field(p.vals[k], "optional")) {
							required = append(required, k)
						}
					}
					// 空の配列はOpenAPIで許されないので、本家も付けない。
					if len(required) > 0 {
						res.set("required", required)
					}
				}
				for _, k := range p.keys {
					rp.set(k, convertSchema(p.vals[k], kind))
				}
			}
		}
	}

	if typ == "array" {
		if items, ok := s.get("items"); ok && truthy(items) {
			res.set("items", convertSchema(items, kind))
		}
	}

	for _, o := range []string{"anyOf", "oneOf", "allOf"} {
		if v, ok := s.get(o); ok {
			list, _ := v.([]any)
			out := make([]any, len(list))
			for i, x := range list {
				out[i] = convertSchema(x, kind)
			}
			res.set(o, out)
		}
	}

	ref, hasRef := s.get("ref")
	nullable := truthy(field(s, "nullable"))
	// includeSelfRef = falseなので、selfRefの付いた参照は$refにしない(本家の
	// /api.jsonが循環しないようにしている)。
	if kind == kindRes && hasRef && truthy(ref) && !truthy(field(s, "selfRef")) {
		r := "#/components/schemas/" + toString(ref)
		if nullable {
			ro := newObject()
			ro.set("$ref", r)
			null := newObject()
			null.set("type", "null")
			res.set("oneOf", []any{ro, null})
		} else {
			res.set("$ref", r)
		}
		res.del("type")
	} else if nullable {
		switch t := typ.(type) {
		case []any:
			if !containsString(t, "null") {
				rt, _ := res.get("type")
				res.set("type", append(rt.([]any), "null"))
			}
		case string:
			res.set("type", []any{t, "null"})
		}
	}

	return res
}

// field returns v[key] when v is an object, nil otherwise (JS `v.key` on an
// object, without the TypeError on null).
func field(v any, key string) any {
	o, ok := v.(*object)
	if !ok {
		return nil
	}
	x, _ := o.get(key)
	return x
}

// truthy is JS truthiness for decoded JSON values.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	default:
		return true
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := marshalValue(v)
	return string(b)
}

func containsString(list []any, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
