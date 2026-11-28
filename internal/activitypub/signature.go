package activitypub

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// PrivateKey wraps a parsed private key (RSA or Ed25519) and its keyId URI
// used as the `keyId` parameter in outgoing Signature headers.
type PrivateKey struct {
	KeyID      string
	PrivatePEM string
	signer     crypto.Signer
	keyType    KeyType
}

// NewPrivateKey wraps a PEM string into a PrivateKey, parsing it once.
// 既存互換のためRSAを期待する。Ed25519鍵はNewEd25519PrivateKeyを使うこと。
func NewPrivateKey(keyID, privatePEM string) (*PrivateKey, error) {
	parsed, err := ParseRSAPrivateKey(privatePEM)
	if err != nil {
		return nil, err
	}
	return &PrivateKey{
		KeyID:      keyID,
		PrivatePEM: privatePEM,
		signer:     parsed,
		keyType:    KeyTypeRSA,
	}, nil
}

// NewEd25519PrivateKey wraps an Ed25519 PKCS8 PEM into a PrivateKey.
func NewEd25519PrivateKey(keyID, privatePEM string) (*PrivateKey, error) {
	parsed, err := ParseEd25519PrivateKey(privatePEM)
	if err != nil {
		return nil, err
	}
	return &PrivateKey{
		KeyID:      keyID,
		PrivatePEM: privatePEM,
		signer:     parsed,
		keyType:    KeyTypeEd25519,
	}, nil
}

// SHA256Digest returns the "SHA-256=<base64>" digest header value for body.
func SHA256Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
}

// algorithmForKeyType returns the Signature header `algorithm` value emitted
// by SignRequest for the given key type.
func algorithmForKeyType(kt KeyType) string {
	if kt == KeyTypeEd25519 {
		return "ed25519"
	}
	return "rsa-sha256"
}

// SignRequest mutates req to include the Date, Host, Digest (when bodyDigest!="")
// and Signature headers required by the Cavage HTTP Signatures draft v12.
//
// includeHeaders はヘッダ名の小文字リスト。"(request-target)" を含めると
// "(request-target): <method> <path>" が署名対象に追加される。
//
// algorithmは鍵種別から自動決定される (RSA→rsa-sha256, Ed25519→ed25519)。
func SignRequest(req *http.Request, key *PrivateKey, bodyDigest string, includeHeaders []string) error {
	if key == nil || key.signer == nil {
		return errors.New("private key required")
	}

	if req.Header.Get("Date") == "" {
		req.Header.Set("Date", nowFunc().UTC().Format(http.TimeFormat))
	}
	if req.Header.Get("Host") == "" {
		req.Header.Set("Host", req.URL.Host)
	}
	if bodyDigest != "" {
		req.Header.Set("Digest", bodyDigest)
	}

	signingString, err := buildSigningString(req, includeHeaders)
	if err != nil {
		return err
	}

	sig, err := signWithKey(key, []byte(signingString))
	if err != nil {
		return err
	}

	header := fmt.Sprintf(
		`keyId="%s",algorithm="%s",headers="%s",signature="%s"`,
		key.KeyID,
		algorithmForKeyType(key.keyType),
		strings.Join(includeHeaders, " "),
		base64.StdEncoding.EncodeToString(sig),
	)
	req.Header.Set("Signature", header)
	// Hostヘッダはnet/httpがリクエストラインから自動付与するため取り除く。
	req.Header.Del("Host")
	return nil
}

// signWithKey produces a signature byte slice for the given signing input.
// 鍵種別に応じてRSA-SHA256 / Ed25519を切り替える。
func signWithKey(key *PrivateKey, signingInput []byte) ([]byte, error) {
	switch key.keyType {
	case KeyTypeRSA:
		hashed := sha256.Sum256(signingInput)
		// rsa.SignPKCS1v15はPKCS1v15ではrandを使わないため失敗しない。
		return rsa.SignPKCS1v15(randReader, key.signer.(*rsa.PrivateKey), crypto.SHA256, hashed[:])
	case KeyTypeEd25519:
		// Ed25519は内部でSHA-512を使うため事前ハッシュ不要。Sign()の
		// optsは必ずcrypto.Hash(0)を渡す (ed25519仕様)。
		return key.signer.Sign(randReader, signingInput, crypto.Hash(0))
	default:
		return nil, fmt.Errorf("unsupported key type: %d", key.keyType)
	}
}

// buildSigningString concatenates the canonical signing string for the
// requested headers.
func buildSigningString(req *http.Request, includeHeaders []string) (string, error) {
	parts := make([]string, 0, len(includeHeaders))
	for _, raw := range includeHeaders {
		key := strings.ToLower(raw)
		if key == "(request-target)" {
			parts = append(parts, fmt.Sprintf("(request-target): %s %s", strings.ToLower(req.Method), req.URL.RequestURI()))
			continue
		}
		var value string
		if key == "host" {
			// SignRequest が事前に Host を埋めているが、Verify 経路では
			// 受信側 net/http が r.Host にしかセットしないことがある。
			value = req.Header.Get("Host")
			if value == "" {
				value = req.Host
			}
		} else {
			value = req.Header.Get(key)
		}
		if value == "" {
			return "", fmt.Errorf("missing required header %q for signature", key)
		}
		parts = append(parts, fmt.Sprintf("%s: %s", key, value))
	}
	return strings.Join(parts, "\n"), nil
}

// ParsedSignature is the structured form of a Signature header.
type ParsedSignature struct {
	KeyID     string
	Algorithm string
	Headers   []string
	Signature string
}

// ParseSignatureHeader parses an HTTP Signature header into its components.
//
// Parameters are read as name="value" pairs from left to right; a repeated
// parameter keeps its last value. Parameters other than keyId, algorithm,
// headers and signature, unquoted values and empty values are ignored.
func ParseSignatureHeader(header string) (*ParsedSignature, error) {
	if header == "" {
		return nil, errors.New("missing signature header")
	}
	out := &ParsedSignature{}
	// 受付では署名を確かめる前に誰の要求でも通るので、正規表現(1回約6µs)をやめて
	// 手で走査する(#4265)。結果は以前の`(\w+)="([^"]+)"`のFindAllStringSubmatchと
	// 同じで、signature_parse_test.goがその正規表現と突き合わせている
	for rest := header; ; {
		name, value, next, ok := nextSignatureParam(rest)
		if !ok {
			break
		}
		rest = next
		switch name {
		case "keyId":
			out.KeyID = value
		case "algorithm":
			out.Algorithm = value
		case "headers":
			out.Headers = strings.Fields(value)
		case "signature":
			out.Signature = value
		}
	}
	if out.KeyID == "" || out.Signature == "" {
		return nil, errors.New("invalid signature header: keyId and signature are required")
	}
	if len(out.Headers) == 0 {
		// RFC default = ["date"]
		out.Headers = []string{"date"}
	}
	return out, nil
}

// nextSignatureParam finds the leftmost name="value" pair in s, where name is
// a run of ASCII letters, digits and '_' and value is one or more bytes other
// than '"'. It returns the pair and the rest of s after the closing quote, or
// ok=false when s has no such pair.
func nextSignatureParam(s string) (name, value, rest string, ok bool) {
	for i := 0; i < len(s); {
		if !isSignatureNameByte(s[i]) {
			i++
			continue
		}
		start := i
		for i < len(s) && isSignatureNameByte(s[i]) {
			i++
		}
		// 名前は語の文字の並びを最後まで取る(途中で切ると次が語の文字で、=に当たらない)
		if i+1 >= len(s) || s[i] != '=' || s[i+1] != '"' {
			continue
		}
		n := strings.IndexByte(s[i+2:], '"')
		if n < 0 {
			// 閉じる"がこの先に無いなら、この先のどの名前も組にならない(組になるには
			// ="の"がこの先に要り、それ自体がs[i+2:]の中の"になる)。なので打ち切っても
			// 結果は変わらない。continueでも計算量は線形のまま(この先に="は無く、
			// IndexByteが末尾まで読むのはこの1回だけ)で、残りを1バイトずつ飛ばす手間を省く
			return "", "", "", false
		}
		if n == 0 {
			// 値が空(="")のものは組にしない。=の次から走査を続ける
			continue
		}
		valueStart := i + 2
		return s[start:i], s[valueStart : valueStart+n], s[valueStart+n+1:], true
	}
	return "", "", "", false
}

// isSignatureNameByte reports whether c is in the \w class of Go regular
// expressions ([0-9A-Za-z_]).
func isSignatureNameByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// VerifyRequest verifies an incoming HTTP request signature against the
// supplied PEM public key. RSA / Ed25519 public keys are both supported,
// dispatched on the parsed Signature `algorithm` parameter and the public
// key type.
//
// 公開鍵 PEM を毎回パースする経路。inbound hot path で同一鍵を繰り返し検証する
// 場合は PublicKeyCache.VerifyRequestCached で x509 パースをメモ化すること
// (#1426)。
func VerifyRequest(req *http.Request, publicKeyPEM string) error {
	// signature header の parse と algorithm guard は PEM パースより前に行う。
	// 未対応 algorithm のリクエストに無駄な x509 パースを走らせず、空 PEM でも
	// algorithm error を返す既存挙動を維持する。
	parsed, err := parseSignatureForVerify(req)
	if err != nil {
		return err
	}
	pub, kt, err := ParsePublicKey(publicKeyPEM)
	if err != nil {
		return err
	}
	return verifyParsed(req, parsed, pub, kt)
}

// VerifyRequestWithKey verifies an incoming HTTP request signature against an
// already-parsed public key. This is the parse-free entry of VerifyRequest: the
// inbound hot path (#1426) supplies a memoized (pub, kt) so the per-request
// pem.Decode + x509.ParsePKIXPublicKey cost is paid once per (keyId, PEM)
// instead of on every verify.
//
// pub / kt は ParsePublicKey 由来であること (RSA は *rsa.PublicKey、Ed25519 は
// ed25519.PublicKey)。algorithm と鍵種別の不一致は verifyAlgorithm が拒否する。
func VerifyRequestWithKey(req *http.Request, pub crypto.PublicKey, kt KeyType) error {
	parsed, err := parseSignatureForVerify(req)
	if err != nil {
		return err
	}
	return verifyParsed(req, parsed, pub, kt)
}

// parseSignatureForVerify parses the Signature header and rejects unsupported
// algorithm names. これを verify 経路 (VerifyRequest / VerifyRequestWithKey /
// PublicKeyCache.VerifyRequestCached) で共有することで、header parse を 1 経路
// につき 1 回に統一し、algorithm guard を必ず公開鍵パースより前に効かせる
// (= 3 経路でエラー優先順位を揃える、#1426 review)。
func parseSignatureForVerify(req *http.Request) (*ParsedSignature, error) {
	parsed, err := ParseSignatureHeader(req.Header.Get("Signature"))
	if err != nil {
		return nil, err
	}
	if !isKnownAlgorithm(parsed.Algorithm) {
		return nil, fmt.Errorf("unsupported algorithm %q", parsed.Algorithm)
	}
	return parsed, nil
}

// verifyParsed is the shared verification core invoked once the signature
// header is parsed and the public key is available. signing string の再構築 →
// 署名 base64 decode → 鍵種別ごとの verify → Digest フォーマットチェックを行う。
func verifyParsed(req *http.Request, parsed *ParsedSignature, pub crypto.PublicKey, kt KeyType) error {
	signingString, err := buildSigningString(req, parsed.Headers)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(parsed.Signature)
	if err != nil {
		return err
	}
	if err := verifyAlgorithm(parsed.Algorithm, kt, pub, []byte(signingString), sig); err != nil {
		return err
	}
	// Digest check (for POST bodies). 形式 (sha-256= prefix) のみを見る軽い
	// guard で、authoritative な値検証 (sha256(body) との一致) は inbox handler の
	// admission (VerifyInboxAdmission) 側で行う (#1949)。ここで body を読むと
	// legacy 経路では既に drain 済みのため値比較できない。
	if expected := req.Header.Get("Digest"); expected != "" && req.Body != nil {
		if !strings.HasPrefix(strings.ToLower(expected), "sha-256=") {
			return errors.New("unsupported digest algorithm")
		}
	}
	return nil
}

// isKnownAlgorithm reports whether the algorithm name is one we accept.
// Empty string is allowed (Cavage default behaviour: derive from key type).
func isKnownAlgorithm(alg string) bool {
	switch strings.ToLower(alg) {
	case "", "hs2019", "rsa-sha256", "rsa-sha512", "ed25519", "ed25519-sha512":
		return true
	}
	return false
}

// verifyAlgorithm dispatches the actual signature verification based on the
// Signature header's `algorithm` parameter and the public key type.
//
// 対応マトリクス:
//   - "" / "hs2019": 鍵種別から判定 (RSA→SHA-256, Ed25519→ed25519)
//   - "rsa-sha256": RSA + SHA-256
//   - "rsa-sha512": RSA + SHA-512
//   - "ed25519" / "ed25519-sha512": Ed25519
//
// algorithmと鍵種別が合わないケース (RSA鍵でalgorithm=ed25519等) は
// 明示的に拒否する。
func verifyAlgorithm(alg string, kt KeyType, pub crypto.PublicKey, signingInput, sig []byte) error {
	algLower := strings.ToLower(alg)
	switch algLower {
	case "", "hs2019":
		// 鍵種別ベースで dispatch
		return verifyByKeyType(kt, pub, signingInput, sig)
	case "rsa-sha256":
		if kt != KeyTypeRSA {
			return fmt.Errorf("algorithm %q requires RSA key", alg)
		}
		hashed := sha256.Sum256(signingInput)
		return rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, hashed[:], sig)
	case "rsa-sha512":
		if kt != KeyTypeRSA {
			return fmt.Errorf("algorithm %q requires RSA key", alg)
		}
		hashed := sha512.Sum512(signingInput)
		return rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA512, hashed[:], sig)
	case "ed25519", "ed25519-sha512":
		if kt != KeyTypeEd25519 {
			return fmt.Errorf("algorithm %q requires Ed25519 key", alg)
		}
		if !ed25519.Verify(pub.(ed25519.PublicKey), signingInput, sig) {
			return errors.New("ed25519 verification failed")
		}
		return nil
	default:
		return fmt.Errorf("unsupported algorithm %q", alg)
	}
}

// verifyByKeyType is the key-type-driven fallback used for hs2019 and
// algorithm-omitted signatures.
func verifyByKeyType(kt KeyType, pub crypto.PublicKey, signingInput, sig []byte) error {
	switch kt {
	case KeyTypeRSA:
		hashed := sha256.Sum256(signingInput)
		return rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, hashed[:], sig)
	case KeyTypeEd25519:
		if !ed25519.Verify(pub.(ed25519.PublicKey), signingInput, sig) {
			return errors.New("ed25519 verification failed")
		}
		return nil
	default:
		return fmt.Errorf("unsupported key type: %d", kt)
	}
}

// ResolveKeyURL extracts the actor URI from a key fragment URI by stripping
// the trailing #fragment.
func ResolveKeyURL(keyID string) string {
	u, err := url.Parse(keyID)
	if err != nil {
		return keyID
	}
	u.Fragment = ""
	return u.String()
}
