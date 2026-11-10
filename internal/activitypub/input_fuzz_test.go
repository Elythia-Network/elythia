package activitypub

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 連合の相手が中身を決める入力を読む関数の fuzz (#4116)。どれも panic しないこと、
// 時間内に終わることを確かめ、性質のはっきりしたものはその性質も確かめる。

// fuzzInputMax caps the inputs the fuzz targets read. Larger ones only slow
// the search down.
const fuzzInputMax = 64 << 10

// fuzzDeadline bounds one call. The seeds also run in the required test
// under -race, so it leaves a wide margin.
const fuzzDeadline = 5 * time.Second

// within fails t when fn does not return before fuzzDeadline.
func within(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(fuzzDeadline):
		t.Fatalf("did not return within %s", fuzzDeadline)
	}
}

// FuzzParseSignatureHeader checks that a parsed Signature header always has
// the parts the verification uses: a key id, a signature and at least one
// header name, each without white space.
func FuzzParseSignatureHeader(f *testing.F) {
	for _, s := range []string{
		`keyId="https://remote.example/users/a#main-key",algorithm="rsa-sha256",headers="(request-target) host date digest",signature="c2ln"`,
		`keyId="k",signature="s"`,
		`keyId="k",headers="",signature="s"`,
		`keyId="k", headers="  date   host ", signature="s"`,
		`keyId="k",signature=""`,
		`keyId="",signature="s"`,
		`keyId=k,signature=s`,
		`keyId="a"keyId="b",signature="s"`,
		`hs2019 keyId="k",created=1,expires=2,signature="s"`,
		``,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > fuzzInputMax {
			t.Skip()
		}
		var parsed *ParsedSignature
		var err error
		within(t, func() { parsed, err = ParseSignatureHeader(in) })
		if err != nil {
			if parsed != nil {
				t.Fatalf("error %v with a result %+v", err, parsed)
			}
			return
		}
		if parsed.KeyID == "" || parsed.Signature == "" {
			t.Fatalf("accepted without a key id or a signature: %+v", parsed)
		}
		if len(parsed.Headers) == 0 {
			t.Fatalf("accepted without header names: %+v", parsed)
		}
		for _, h := range parsed.Headers {
			if h == "" || strings.ContainsAny(h, " \t\r\n") {
				t.Fatalf("header name %q in %+v", h, parsed)
			}
		}
	})
}

// FuzzParsePublicKey checks that a parsed public key has the type it reports
// and survives encoding it again.
func FuzzParsePublicKey(f *testing.F) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.Fatal(err)
	}
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	for _, pub := range []any{&rsaKey.PublicKey, edPub} {
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			f.Fatal(err)
		}
		block := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		f.Add(block)
		f.Add(strings.ReplaceAll(block, "\n", "\r\n"))
		f.Add(block[:len(block)/2])
	}
	f.Add(string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&rsaKey.PublicKey)})))
	f.Add("-----BEGIN PUBLIC KEY-----\n-----END PUBLIC KEY-----\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > fuzzInputMax {
			t.Skip()
		}
		var pub any
		var kt KeyType
		var err error
		within(t, func() { pub, kt, err = ParsePublicKey(in) })
		if err != nil {
			if pub != nil {
				t.Fatalf("error %v with a key %T", err, pub)
			}
			return
		}
		switch k := pub.(type) {
		case *rsa.PublicKey:
			if kt != KeyTypeRSA || k.N == nil {
				t.Fatalf("RSA key reported as %v", kt)
			}
		case ed25519.PublicKey:
			if kt != KeyTypeEd25519 || len(k) != ed25519.PublicKeySize {
				t.Fatalf("Ed25519 key of %d bytes reported as %v", len(k), kt)
			}
		default:
			t.Fatalf("accepted a %T", pub)
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			t.Fatalf("the parsed key does not encode: %v", err)
		}
		again, kt2, err := ParsePublicKey(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
		if err != nil || kt2 != kt || !reflect.DeepEqual(again, pub) {
			t.Fatalf("the key changes when encoded again: %v", err)
		}
	})
}

// FuzzNormalize checks that the JSON-LD normalization is deterministic, gives
// JSON, and is idempotent. The inbox normalizes the same bytes more than once
// (the gate, the dispatch, the activity id), and an earlier map-order bug made
// them disagree (ErrConflictingKeys).
func FuzzNormalize(f *testing.F) {
	for _, s := range []string{
		`{"@context":"https://www.w3.org/ns/activitystreams","type":"Create","actor":"https://remote.example/users/a","object":{"type":"Note","content":"hi"}}`,
		`{"actor":"https://a.example/u","as:actor":"https://a.example/u"}`,
		`{"actor":"https://a.example/u","as:actor":"https://b.example/u"}`,
		`{"https://www.w3.org/ns/activitystreams#actor":{"@id":"https://a.example/u"},"@type":["https://www.w3.org/ns/activitystreams#Follow"]}`,
		`{"content":{"@value":"x","@language":"ja"},"to":[{"@id":"https://www.w3.org/ns/activitystreams#Public"}]}`,
		`[{"type":"Like"},{"type":"Undo"}]`,
		`{"object":{"object":{"object":{"@id":"x"}}}}`,
		`"text"`, `123`, `null`, `{}`, `[]`, `{"a":`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > fuzzInputMax {
			t.Skip()
		}
		var out, again []byte
		var err, err2 error
		within(t, func() {
			out, err = Normalize(in)
			again, err2 = Normalize(in)
		})
		if (err == nil) != (err2 == nil) || !bytes.Equal(out, again) {
			t.Fatalf("not deterministic: %q / %q (%v / %v)", out, again, err, err2)
		}
		if err != nil || len(in) == 0 {
			return
		}
		if !json.Valid(out) {
			t.Fatalf("not JSON: %q", out)
		}
		twice, err := Normalize(out)
		if err != nil || !bytes.Equal(twice, out) {
			t.Fatalf("not idempotent: %q -> %q (%v)", out, twice, err)
		}
	})
}

// FuzzUnmarshalObjects decodes the objects a remote server sends into the
// types the federation code reads, and checks that decoding never panics and
// gives the same value each time. The lenient fields (APLenient*) have no
// MarshalJSON, so the value is not compared with an encoded copy.
func FuzzUnmarshalObjects(f *testing.F) {
	for _, s := range []string{
		`{"id":"https://remote.example/notes/1","type":"Note","attributedTo":"https://remote.example/users/a","content":"<p>hi</p>","published":"2026-01-01T00:00:00Z","to":["https://www.w3.org/ns/activitystreams#Public"],"tag":[{"type":"Mention","href":"https://e.example/u"}],"attachment":[{"type":"Document","url":"https://e.example/a.png"}]}`,
		`{"type":"Note","published":["2024-01-01T00:00:00Z"],"sensitive":"true","source":"raw","replies":{"totalItems":"3"}}`,
		`{"type":"Note","published":1700000000000,"content":null,"tag":{"type":"Hashtag","name":"#x"}}`,
		`{"id":"https://remote.example/users/a","type":["Person","Service"],"preferredUsername":"a","publicKey":{"id":"#k","publicKeyPem":"-"},"icon":[{"url":"https://e.example/i.png"}],"endpoints":"https://e.example/inbox","assertionMethod":{"id":"#m"}}`,
		`{"type":"Question","oneOf":[{"name":"a","replies":{"totalItems":1}}],"endTime":0}`,
		`{"type":"Create","actor":{"id":"https://e.example/u"},"object":"https://e.example/n"}`,
		`{"type":["Image"],"url":[{"href":"https://e.example/x"}],"mediaType":7}`,
		`[]`, `null`, `"x"`, `{"type":`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > fuzzInputMax {
			t.Skip()
		}
		for _, newValue := range []func() any{
			func() any { return new(Note) },
			func() any { return new(Person) },
			func() any { return new(Activity) },
			func() any { return new(Image) },
		} {
			a, b := newValue(), newValue()
			var errA, errB error
			within(t, func() {
				errA = json.Unmarshal(in, a)
				errB = json.Unmarshal(in, b)
			})
			if (errA == nil) != (errB == nil) || !reflect.DeepEqual(a, b) {
				t.Fatalf("%T is not deterministic (%v / %v)", a, errA, errB)
			}
		}
	})
}
