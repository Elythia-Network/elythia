package openapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// object is a JSON object that keeps the order of its keys.
//
// 本家の/api.jsonはJSのオブジェクトの順で項目を並べる。Scalarは引数や応答の
// 項目をその順で表示するので、Goのmapで受けて名前順に並べ替えると、本家の
// 画面と並びが変わる。goldenの順をそのまま持ち回るために、自前で順を持つ。
type object struct {
	keys []string
	vals map[string]any
}

func newObject() *object {
	return &object{vals: map[string]any{}}
}

// get returns the value of key and whether it is present.
func (o *object) get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// set assigns key like a JS property assignment: an existing key keeps its
// position, a new one goes last.
func (o *object) set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// del removes key like the JS delete operator.
func (o *object) del(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// MarshalJSON writes the members in key order.
func (o *object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalValue(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalValue(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalValue encodes v without escaping <, > and &, like JSON.stringify.
func marshalValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// clone deep-copies a decoded value (deepClone of upstream's misc/clone.ts).
func clone(v any) any {
	switch t := v.(type) {
	case *object:
		c := &object{keys: append([]string(nil), t.keys...), vals: make(map[string]any, len(t.vals))}
		for k, x := range t.vals {
			c.vals[k] = clone(x)
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, x := range t {
			c[i] = clone(x)
		}
		return c
	default:
		return v
	}
}

// decode parses JSON into *object / []any / string / json.Number / bool / nil,
// keeping the key order of every object.
func decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("openapi: trailing data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("openapi: decode: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("openapi: decode: %w", err)
				}
				// Decoderはオブジェクトの中では、キーの位置に文字列しか返さない。
				k := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				// JSON.parseと同じく、重複したキーは後の値で上書きし、位置は最初のまま。
				o.set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, fmt.Errorf("openapi: decode: %w", err)
			}
			return o, nil
		default: // '['
			a := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, fmt.Errorf("openapi: decode: %w", err)
			}
			return a, nil
		}
	default:
		return t, nil
	}
}
