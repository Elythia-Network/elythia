package paramschema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// elythiaParamDefs is elythia_paramdefs.json, the paramDef and meta of the
// endpoints Elythia has and upstream does not (#3869).
//
//go:embed elythia_paramdefs.json
var elythiaParamDefs []byte

// ElythiaParamDefs returns the embedded elythia_paramdefs.json as is, for the
// tests that check the declarations against the routes, the ajv corpus and
// elythia-js. The slice is shared and must not be modified.
func ElythiaParamDefs() []byte {
	return elythiaParamDefs
}

// addElythiaEndpoints compiles the paramDefs of b, which has the shape of the
// endpoints of upstream_paramdefs.json, and adds them to eps.
//
// 本家と同じ名前の宣言は拒否する: 本家のendpointの引数を変えるのは重ねる宣言
// (elythia_overlays.json)の役目で、そちらは足すことしかできない。defaultも
// 書けない: 検証は写しにしか入れず、handlerが既定値を持つので、書いた既定値は
// handlerに届かず、食い違っても気付けない。
func addElythiaEndpoints(eps map[string]Endpoint, b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var g struct {
		Endpoints map[string]struct {
			ParamDef json.RawMessage `json:"paramDef"`
			Meta     struct {
				AllowGet    bool `json:"allowGet"`
				RequireFile bool `json:"requireFile"`
				// FileField is the name of the multipart file part of a
				// requireFile endpoint, when it is not upstream's "file"
				// (drive/files/create-chunked/append sends "chunk").
				FileField string `json:"fileField"`
			} `json:"meta"`
		} `json:"endpoints"`
	}
	if err := dec.Decode(&g); err != nil {
		return fmt.Errorf("paramschema: elythia paramDefs: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("paramschema: elythia paramDefs: trailing data")
	}
	if len(g.Endpoints) == 0 {
		return errors.New("paramschema: elythia paramDefs: no endpoints")
	}
	for name, ep := range g.Endpoints {
		if _, ok := eps[name]; ok {
			return fmt.Errorf("paramschema: elythia paramDef for %s: an upstream endpoint", name)
		}
		v, err := decodeOrdered(json.NewDecoder(bytes.NewReader(ep.ParamDef)))
		if err != nil {
			return fmt.Errorf("paramschema: elythia paramDef for %s: %w", name, err)
		}
		o, ok := v.(*orderedObject)
		if !ok {
			return fmt.Errorf("paramschema: elythia paramDef for %s must be an object", name)
		}
		if err := checkNoDefault(o, "#"); err != nil {
			return fmt.Errorf("paramschema: elythia paramDef for %s: %w", name, err)
		}
		// ファイルの欄の名前は、ファイルを受けるendpointにしか意味が無い。引数と
		// 同じ名前にすると、/api.jsonの本文で引数の型がファイルに上書きされる。
		if ff := ep.Meta.FileField; ff != "" {
			if !ep.Meta.RequireFile {
				return fmt.Errorf("paramschema: elythia paramDef for %s: meta.fileField without meta.requireFile", name)
			}
			// anyOf の分岐ごとの項目には足していないので、elythia-js の型の検査
			// (elythiajs) が分岐の項目と必須を食い違えて作る
			if _, ok := o.get("anyOf"); ok {
				return fmt.Errorf("paramschema: elythia paramDef for %s: meta.fileField with anyOf", name)
			}
			if props, ok := o.get("properties"); ok {
				if po, ok := props.(*orderedObject); ok {
					if _, dup := po.get(ff); dup {
						return fmt.Errorf("paramschema: elythia paramDef for %s: meta.fileField %q is also a property", name, ff)
					}
				}
			}
		}
		s, err := Compile(ep.ParamDef)
		if err != nil {
			return fmt.Errorf("paramschema: elythia paramDef for %s: %w", name, err)
		}
		eps[name] = Endpoint{Schema: s, AllowGet: ep.Meta.AllowGet, RequireFile: ep.Meta.RequireFile, Elythia: true}
	}
	return nil
}
