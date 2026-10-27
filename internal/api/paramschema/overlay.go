package paramschema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// elythiaOverlays is elythia_overlays.json, the values Elythia accepts on top
// of upstream's paramDef (#3865).
//
//go:embed elythia_overlays.json
var elythiaOverlays []byte

var effectiveOnce = sync.OnceValues(func() (map[string]Endpoint, error) {
	return parseEffective(upstreamGolden, elythiaOverlays)
})

// Effective returns the upstream endpoints of Upstream with the values
// Elythia accepts on top of upstream's paramDef overlaid (#3865). The request
// validation uses it; Upstream stays upstream's paramDef as is, for the
// conformance with ajv. The map is shared and must not be modified.
//
// 重ねる宣言はelythia_overlays.jsonに、本家のparamDefと同じ形(properties・
// items・enum)で書く。足すことしかできない: 本家のenumに値を足す
// (enum、x-elythia-enumPattern)、本家の形式に別の値を許す
// (x-elythia-formatAlso)、本家に無い項目を足す(x-elythia-properties、#3867)
// だけで、本家が拒否する入力の最初のエラーは、足した値に当たるもの以外は
// 変わらない。本家が受理する入力は、足した項目に宣言から外れる値を持つもの
// (本家は知らない項目として通していた)だけが拒否に変わる。
func Effective() (map[string]Endpoint, error) {
	return effectiveOnce()
}

// ElythiaOverlays returns the embedded elythia_overlays.json as is, for the
// tests that check the declared values against the implementation. The slice
// is shared and must not be modified.
func ElythiaOverlays() []byte {
	return elythiaOverlays
}

// parseEffective compiles golden afresh, so that the nodes Upstream shares
// stay untouched, and applies overlays to it.
func parseEffective(golden, overlays []byte) (map[string]Endpoint, error) {
	eps, err := parseUpstream(golden)
	if err != nil {
		return nil, err
	}
	if err := applyOverlays(eps, overlays); err != nil {
		return nil, err
	}
	return eps, nil
}

// applyOverlays applies overlays, a JSON object from endpoint names to
// overlays, to the compiled paramDefs of eps.
func applyOverlays(eps map[string]Endpoint, overlays []byte) error {
	dec := json.NewDecoder(bytes.NewReader(overlays))
	v, err := decodeOrdered(dec)
	if err != nil {
		return fmt.Errorf("paramschema: overlays: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("paramschema: overlays: trailing data")
	}
	root, ok := v.(*orderedObject)
	if !ok {
		return errors.New("paramschema: overlays must be an object")
	}
	names := append([]string(nil), root.keys...)
	sort.Strings(names)
	for _, name := range names {
		ep, ok := eps[name]
		if !ok {
			return fmt.Errorf("paramschema: overlay for %s: not an upstream endpoint", name)
		}
		if ep.CompileErr != nil {
			return fmt.Errorf("paramschema: overlay for %s: %w", name, ep.CompileErr)
		}
		o, ok := root.vals[name].(*orderedObject)
		if !ok {
			return fmt.Errorf("paramschema: overlay for %s must be an object", name)
		}
		if err := applyOverlay(ep.Schema.root, o); err != nil {
			return fmt.Errorf("paramschema: overlay for %s: %w", name, err)
		}
	}
	return nil
}

// applyOverlay applies the overlay o to n. o has the shape of the paramDef
// (sub)schema of n, with only the keys below.
func applyOverlay(n *node, o *orderedObject) error {
	if len(o.keys) == 0 {
		return fmt.Errorf("%s: empty overlay", n.path)
	}
	for _, k := range o.keys {
		v := o.vals[k]
		var err error
		switch k {
		case "description":
			if _, ok := v.(string); !ok {
				err = fmt.Errorf("%s: description must be a string", n.path)
			}
		case "properties":
			err = overlayProperties(n, v)
		case "x-elythia-properties":
			err = overlayAddProperties(n, v)
		case "items":
			err = overlayItems(n, v)
		case "enum":
			err = overlayEnum(n, v)
		case "x-elythia-enumPattern":
			err = overlayEnumPattern(n, v)
		case "x-elythia-formatAlso":
			err = overlayFormatAlso(n, v)
		default:
			// 必須を外すなどの操作は、要る段で足す(#3667の(4))。
			err = fmt.Errorf("%s: unsupported overlay keyword %q", n.path, k)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func overlayProperties(n *node, v any) error {
	props, ok := v.(*orderedObject)
	if !ok {
		return fmt.Errorf("%s: properties must be an object", n.path)
	}
	for _, name := range props.keys {
		var child *node
		for _, p := range n.properties {
			if p.name == name {
				child = p.node
				break
			}
		}
		if child == nil {
			return fmt.Errorf("%s: upstream has no property %q", n.path, name)
		}
		o, ok := props.vals[name].(*orderedObject)
		if !ok {
			return fmt.Errorf("%s/properties/%s: overlay must be an object", n.path, escapeFragment(name))
		}
		if err := applyOverlay(child, o); err != nil {
			return err
		}
	}
	return nil
}

// overlayAddProperties adds the properties upstream's paramDef does not have,
// each a full paramDef schema (#3867). They are checked after upstream's own
// properties, so an input upstream rejects keeps its first error.
//
// 本家にある項目はここで書けない(propertiesで重ねる)。同じキーで両方を
// 受けると、本家の項目の名前を打ち間違えた宣言が、黙って新しい項目になる。
// defaultも書けない: 検証器が写しに入れる値が変わり、本家が受理する入力の
// defaultを入れた後の値が本家と食い違う。
func overlayAddProperties(n *node, v any) error {
	props, ok := v.(*orderedObject)
	if !ok || len(props.keys) == 0 {
		return fmt.Errorf("%s: x-elythia-properties must be a non-empty object", n.path)
	}
	// objectでない値の項目は検証されないので、足しても何も変わらない。
	if n.typeName != "" && n.typeName != "object" {
		return fmt.Errorf("%s: x-elythia-properties on a %s", n.path, n.typeName)
	}
	// 本家の項目は、anyOf・oneOf・allOf・if / then / elseの中にもある
	// (users/showなど)。そこにある名前を足すと、本家が受理する値を黙って
	// 拒否しうる。
	upstream := map[string]bool{}
	collectPropertyNames(n, upstream)
	for _, name := range props.keys {
		path := n.path + "/properties/" + escapeFragment(name)
		if objectPrototypeNames[name] {
			return fmt.Errorf("%s: property %q is inherited by every JavaScript object", n.path, name)
		}
		if upstream[name] {
			return fmt.Errorf("%s: upstream has the property %q; overlay it with properties", n.path, name)
		}
		o, ok := props.vals[name].(*orderedObject)
		if !ok {
			return fmt.Errorf("%s: schema must be an object", path)
		}
		if err := checkNoDefault(o, path); err != nil {
			return err
		}
		child, err := compileNode(o, path, false)
		if err != nil {
			return err
		}
		n.properties = append(n.properties, property{name: name, node: child})
		// 本家のparamDefにobjectの組のkeywordが無いと、検証はpropertiesを見ない。
		n.usesGroup["object"] = true
	}
	return nil
}

// collectPropertyNames adds the names of the properties and required of n
// and of the schemas its applicators hold to names.
func collectPropertyNames(n *node, names map[string]bool) {
	if n == nil {
		return
	}
	for _, p := range n.properties {
		names[p.name] = true
	}
	// requiredだけに挙げた名前も、本家は型を問わずに受けている。
	for _, name := range n.required {
		names[name] = true
	}
	for _, subs := range [][]*node{n.anyOf, n.oneOf, n.allOf, {n.ifN, n.thenN, n.elseN}} {
		for _, sub := range subs {
			collectPropertyNames(sub, names)
		}
	}
}

// checkNoDefault refuses a default anywhere in the added schema o. The
// values of properties are schemas by name: a property named "default" is
// not the keyword.
func checkNoDefault(o *orderedObject, path string) error {
	if _, ok := o.get("default"); ok {
		return fmt.Errorf("%s: an added property cannot have a default", path)
	}
	for _, k := range o.keys {
		switch v := o.vals[k].(type) {
		case *orderedObject:
			if k == "properties" {
				for _, name := range v.keys {
					if sub, ok := v.vals[name].(*orderedObject); ok {
						if err := checkNoDefault(sub, path+"/properties/"+escapeFragment(name)); err != nil {
							return err
						}
					}
				}
				continue
			}
			if err := checkNoDefault(v, path+"/"+k); err != nil {
				return err
			}
		case []any:
			for i, x := range v {
				if sub, ok := x.(*orderedObject); ok {
					if err := checkNoDefault(sub, fmt.Sprintf("%s/%s/%d", path, k, i)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func overlayItems(n *node, v any) error {
	if n.items == nil {
		return fmt.Errorf("%s: upstream has no items", n.path)
	}
	o, ok := v.(*orderedObject)
	if !ok {
		return fmt.Errorf("%s/items: overlay must be an object", n.path)
	}
	return applyOverlay(n.items, o)
}

// overlayEnum adds values to the enum of n. Each value must be new, and of a
// type of n, or it would not change what n accepts.
func overlayEnum(n *node, v any) error {
	if n.enum == nil {
		return fmt.Errorf("%s: upstream has no enum to add to", n.path)
	}
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return fmt.Errorf("%s: enum must be a non-empty array", n.path)
	}
	for _, x := range arr {
		switch x.(type) {
		case *orderedObject, []any:
			return fmt.Errorf("%s: enum with an object or array value is unsupported", n.path)
		}
		if len(n.types) > 0 && !isAnyType(n.types, x) {
			return fmt.Errorf("%s: enum value %v is not of the type %v", n.path, x, n.types)
		}
		for _, e := range n.enum {
			if jsEqual(x, e) {
				return fmt.Errorf("%s: enum value %v is already allowed", n.path, x)
			}
		}
		// x-elythia-enumPatternを先に書いた宣言で、patternが既に通す値を足しても
		// 何も変わらない。
		if s, ok := x.(string); ok && n.enumPattern != nil && n.enumPattern.MatchString(s) {
			return fmt.Errorf("%s: enum value %v is already allowed by x-elythia-enumPattern", n.path, x)
		}
		n.enum = append(n.enum, x)
	}
	return nil
}

// overlayEnumPattern lets the strings that match a pattern also pass the enum
// of n (the queues of plugins, `plugin:<name>`).
func overlayEnumPattern(n *node, v any) error {
	if n.enum == nil {
		return fmt.Errorf("%s: upstream has no enum to add to", n.path)
	}
	if n.enumPattern != nil {
		return fmt.Errorf("%s: x-elythia-enumPattern is set twice", n.path)
	}
	src, ok := v.(string)
	if !ok {
		return fmt.Errorf("%s: x-elythia-enumPattern must be a string", n.path)
	}
	re, err := compileJSRegexp(src)
	if err != nil {
		return fmt.Errorf("%s: %w", n.path, err)
	}
	for _, e := range n.enum {
		if s, ok := e.(string); ok && re.MatchString(s) {
			return fmt.Errorf("%s: x-elythia-enumPattern matches the allowed value %q", n.path, s)
		}
	}
	n.enumPattern = re
	return nil
}

// overlayFormatAlso lets values (an exact list, a pattern or both) also pass
// the format of n. The format keeps its name, so a value that matches neither
// still gets upstream's error (must match format "misskey:id").
func overlayFormatAlso(n *node, v any) error {
	if n.formatFn == nil {
		return fmt.Errorf("%s: upstream has no format to add to", n.path)
	}
	o, ok := v.(*orderedObject)
	if !ok || len(o.keys) == 0 {
		return fmt.Errorf("%s: x-elythia-formatAlso must be a non-empty object", n.path)
	}
	orig := n.formatFn
	values := map[string]bool{}
	var also []func(string) bool
	for _, k := range o.keys {
		switch k {
		case "values":
			arr, ok := o.vals[k].([]any)
			if !ok || len(arr) == 0 {
				return fmt.Errorf("%s: x-elythia-formatAlso.values must be a non-empty array", n.path)
			}
			for _, x := range arr {
				s, ok := x.(string)
				if !ok || values[s] {
					return fmt.Errorf("%s: x-elythia-formatAlso.values must hold distinct strings", n.path)
				}
				if orig(s) {
					return fmt.Errorf("%s: %q already matches the format %q", n.path, s, n.format)
				}
				values[s] = true
			}
			also = append(also, func(s string) bool { return values[s] })
		case "pattern":
			src, ok := o.vals[k].(string)
			if !ok {
				return fmt.Errorf("%s: x-elythia-formatAlso.pattern must be a string", n.path)
			}
			re, err := compileJSRegexp(src)
			if err != nil {
				return fmt.Errorf("%s: %w", n.path, err)
			}
			also = append(also, re.MatchString)
		default:
			return fmt.Errorf("%s: unsupported x-elythia-formatAlso key %q", n.path, k)
		}
	}
	n.formatFn = func(s string) bool {
		if orig(s) {
			return true
		}
		for _, f := range also {
			if f(s) {
				return true
			}
		}
		return false
	}
	return nil
}
