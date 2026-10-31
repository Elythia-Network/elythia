package activitypub

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// sig builds a ParsedSignature carrying the given signed header set.
func sig(headers ...string) *ParsedSignature {
	return &ParsedSignature{KeyID: "k", Signature: "s", Headers: headers}
}

func TestVerifyInboxAdmission(t *testing.T) {
	body := []byte(`{"type":"Follow"}`)
	goodDigest := SHA256Digest(body) // "sha-256=<base64>"
	const host = "example.com"

	// dateHeader を空にした行は skew 検査を通らない (upstream も Date が無ければ
	// 検査しない)。既存ケースはそのまま digest / host の検査だけを見る。
	tests := []struct {
		name         string
		parsed       *ParsedSignature
		hostHeader   string
		expectedHost string
		dateHeader   string
		digestHeader string
		body         []byte
		wantErr      error
	}{
		{
			name:         "valid with host and digest signed",
			parsed:       sig("(request-target)", "date", "host", "digest"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: goodDigest,
			body:         body,
			wantErr:      nil,
		},
		{
			name:         "tampered body fails digest value comparison",
			parsed:       sig("(request-target)", "date", "host", "digest"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: goodDigest,
			body:         []byte(`{"type":"Follow","tampered":true}`),
			wantErr:      ErrInboxDigestMismatch,
		},
		{
			name:         "digest not in signed headers",
			parsed:       sig("(request-target)", "date", "host"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: goodDigest,
			body:         body,
			wantErr:      ErrInboxDigestUnsigned,
		},
		{
			name:         "digest header missing",
			parsed:       sig("(request-target)", "date", "host", "digest"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: "",
			body:         body,
			wantErr:      ErrInboxDigestMissing,
		},
		{
			name:         "digest header malformed",
			parsed:       sig("(request-target)", "date", "host", "digest"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: "not-a-digest",
			body:         body,
			wantErr:      ErrInboxDigestMalformed,
		},
		{
			name:         "unsupported digest algorithm",
			parsed:       sig("(request-target)", "date", "host", "digest"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: "SHA-512=abc",
			body:         body,
			wantErr:      ErrInboxDigestAlgo,
		},
		{
			// #2087: (request-target) 未署名は upstream parseRequest 同様 reject。
			name:         "request-target not signed",
			parsed:       sig("date", "host", "digest"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: goodDigest,
			body:         body,
			wantErr:      ErrInboxRequestTargetUnsigned,
		},
		{
			// #2087: date 未署名も reject (expectedHost 未設定でも必須)。
			name:         "date not signed (even when expectedHost empty)",
			parsed:       sig("(request-target)", "host", "digest"),
			hostHeader:   host,
			expectedHost: "",
			digestHeader: goodDigest,
			body:         body,
			wantErr:      ErrInboxDateUnsigned,
		},
		{
			name:         "host not signed when expectedHost set",
			parsed:       sig("(request-target)", "date", "digest"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: goodDigest,
			body:         body,
			wantErr:      ErrInboxHostUnsigned,
		},
		{
			name:         "host mismatch when expectedHost set",
			parsed:       sig("(request-target)", "date", "host", "digest"),
			hostHeader:   "evil.example",
			expectedHost: host,
			digestHeader: goodDigest,
			body:         body,
			wantErr:      ErrInboxHostMismatch,
		},
		{
			name:         "host check skipped when expectedHost empty",
			parsed:       sig("(request-target)", "date", "digest"),
			hostHeader:   "anything",
			expectedHost: "",
			digestHeader: goodDigest,
			body:         body,
			wantErr:      nil,
		},
		{
			name:         "host comparison is case-insensitive",
			parsed:       sig("(request-target)", "date", "host", "digest"),
			hostHeader:   "Example.COM",
			expectedHost: host,
			digestHeader: goodDigest,
			body:         body,
			wantErr:      nil,
		},
		{
			name:         "digest algo is case-insensitive",
			parsed:       sig("(request-target)", "date", "host", "digest"),
			hostHeader:   host,
			expectedHost: host,
			digestHeader: "sha-256=" + goodDigest[len("sha-256="):],
			body:         body,
			wantErr:      nil,
		},
		{
			name:         "nil parsed signature",
			parsed:       nil,
			hostHeader:   host,
			expectedHost: host,
			digestHeader: goodDigest,
			body:         body,
			wantErr:      errNilParsed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyInboxAdmission(tt.parsed, tt.hostHeader, tt.expectedHost, tt.dateHeader, tt.digestHeader, tt.body)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if tt.wantErr == errNilParsed {
				// nil parsed returns a generic error, not a sentinel.
				if err == nil {
					t.Fatalf("expected error for nil parsed, got nil")
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

// errNilParsed is a test marker; VerifyInboxAdmission returns a generic (non-sentinel)
// error for a nil parsed signature, so the table uses this to branch.
var errNilParsed = errors.New("nil parsed marker")

// 署名済みリクエストを永久に再投函できないこと。**window が無いと、捕まえた
// 署名が期限なしで使える。**
func TestVerifyInboxAdmission_DateSkew(t *testing.T) {
	body := []byte(`{"type":"Follow"}`)
	digest := SHA256Digest(body)
	const host = "example.com"
	// 固定時刻に対して相対で見る。実時刻に依存させると境界のテストが不安定になる。
	base := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	restore := nowFuncForAdmission
	nowFuncForAdmission = func() time.Time { return base }
	t.Cleanup(func() { nowFuncForAdmission = restore })

	admit := func(date string) error {
		return VerifyInboxAdmission(sig("(request-target)", "date", "host", "digest"),
			host, host, date, digest, body)
	}
	at := func(d time.Duration) string { return base.Add(d).UTC().Format(http.TimeFormat) }

	t.Run("窓の中は通る", func(t *testing.T) {
		for name, d := range map[string]time.Duration{
			"現在":     0,
			"過去ぎりぎり": -InboxDateSkew,
			"未来ぎりぎり": InboxDateSkew,
			"少し過去":   -InboxDateSkew / 2,
		} {
			t.Run(name, func(t *testing.T) {
				if err := admit(at(d)); err != nil {
					t.Fatalf("通るはずが %v", err)
				}
			})
		}
	})

	t.Run("窓の外は弾く", func(t *testing.T) {
		for name, d := range map[string]time.Duration{
			"わずかに過去":  -InboxDateSkew - time.Second,
			"わずかに未来":  InboxDateSkew + time.Second,
			"1 日前":    -24 * time.Hour,
			"1 年前の再送": -365 * 24 * time.Hour,
		} {
			t.Run(name, func(t *testing.T) {
				if err := admit(at(d)); !errors.Is(err, ErrInboxDateSkew) {
					t.Fatalf("ErrInboxDateSkew のはずが %v", err)
				}
			})
		}
	})

	// **upstream に合わせて緩くしてある部分。** ここを厳しくすると、JS では
	// 読めて Go では読めない書式を送る peer からの配送を落とす。値を差し替えれば
	// 署名が壊れるので、緩くても再投函の穴にはならない。
	t.Run("読めない値と空は通す", func(t *testing.T) {
		for _, date := range []string{"", "   ", "not a date", "0"} {
			if err := admit(date); err != nil {
				t.Errorf("date=%q は通るはずが %v", date, err)
			}
		}
	})

	// RFC1123 以外の HTTP-date も読めること (http.ParseTime が受ける 3 形式)。
	t.Run("RFC850 形式も読む", func(t *testing.T) {
		old := base.Add(-24 * time.Hour).UTC().Format(time.RFC850)
		if err := admit(old); !errors.Is(err, ErrInboxDateSkew) {
			t.Fatalf("ErrInboxDateSkew のはずが %v", err)
		}
	})
}

// **X-Date は署名されているときだけ Date より優先される (#3037)。**
//
// 無条件に優先すると、捕まえたリクエストに新しい `X-Date` を足すだけで
// clockSkew 検査を迂回できる (署名は元の `Date` に対して作られているので
// そのまま通る) = 一度盗聴できた配送を永久に再投函できる。
func TestInboxDateHeader(t *testing.T) {
	tests := []struct {
		name   string
		date   string
		xDate  string
		signed []string
		want   string
	}{
		{name: "Date のみ", date: "d", signed: []string{"date"}, want: "d"},
		{name: "どちらも無い", signed: []string{"date"}, want: ""},
		// 署名されていない X-Date は無視する (= 再投函の穴を塞ぐ)。
		{name: "X-Date が未署名なら Date", date: "d", xDate: "x", signed: []string{"date"}, want: "d"},
		{name: "X-Date が未署名で Date が無い", xDate: "x", signed: []string{"date"}, want: ""},
		// 署名している peer には従来どおり。
		{name: "X-Date が署名済みなら優先", date: "d", xDate: "x", signed: []string{"date", "x-date"}, want: "x"},
		{name: "署名済み X-Date のみ", xDate: "x", signed: []string{"x-date"}, want: "x"},
		// 署名ヘッダ名の大小は揃っていない peer がいる。
		{name: "大文字の署名ヘッダ名", date: "d", xDate: "x", signed: []string{"Date", "X-Date"}, want: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.date != "" {
				h.Set("Date", tt.date)
			}
			if tt.xDate != "" {
				h.Set("X-Date", tt.xDate)
			}
			if got := InboxDateHeader(h, tt.signed); got != tt.want {
				t.Fatalf("InboxDateHeader() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplitDigestHeader(t *testing.T) {
	tests := []struct {
		in    string
		algo  string
		value string
		ok    bool
	}{
		{"SHA-256=abc123", "SHA-256", "abc123", true},
		{"SHA-256=ab+c/12==", "SHA-256", "ab+c/12==", true}, // base64 padding survives
		{"  SHA-256=x  ", "SHA-256", "x", true},             // trimmed
		{"=value", "", "", false},                           // empty algo
		{"algo=", "", "", false},                            // empty value
		{"noequals", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		algo, value, ok := splitDigestHeader(tt.in)
		if ok != tt.ok || algo != tt.algo || value != tt.value {
			t.Errorf("splitDigestHeader(%q) = (%q,%q,%v), want (%q,%q,%v)", tt.in, algo, value, ok, tt.algo, tt.value, tt.ok)
		}
	}
}

// **JS が読めて Go が読めない書式で skew 検査が丸ごと飛んでいた。**
// `http.ParseTime` が扱うのは RFC1123 (GMT) / RFC850 / ANSIC だけで、
// RFC1123Z と ISO8601 は失敗する。読めない値は upstream に合わせて通す設計
// なので、そういう Date を出す peer は**30 日前のリクエストでも通っていた** —
// replay guard の TTL が切れた後は同じ activity を無期限に再投函できる。
func TestVerifyInboxAdmission_DateFormatsJSAccepts(t *testing.T) {
	body := []byte(`{"type":"Follow"}`)
	digest := SHA256Digest(body)
	const host = "example.com"
	base := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	restore := nowFuncForAdmission
	nowFuncForAdmission = func() time.Time { return base }
	t.Cleanup(func() { nowFuncForAdmission = restore })

	admit := func(date string) error {
		return VerifyInboxAdmission(sig("(request-target)", "date", "host", "digest"),
			host, host, date, digest, body)
	}
	stale := base.Add(-30 * 24 * time.Hour).UTC()

	t.Run("窓の外は書式によらず弾く", func(t *testing.T) {
		for name, layout := range map[string]string{
			"RFC1123Z":    time.RFC1123Z,
			"RFC3339":     time.RFC3339,
			"RFC3339Nano": time.RFC3339Nano,
		} {
			t.Run(name, func(t *testing.T) {
				if err := admit(stale.Format(layout)); !errors.Is(err, ErrInboxDateSkew) {
					t.Fatalf("30 日前の Date が通った (%s): %v", layout, err)
				}
			})
		}
		// HTTP-date の GMT 形は元から `http.ParseTime` が読む。
		t.Run("HTTP-date", func(t *testing.T) {
			if err := admit(stale.Format(http.TimeFormat)); !errors.Is(err, ErrInboxDateSkew) {
				t.Fatalf("30 日前の Date が通った: %v", err)
			}
		})
		// **`UTC` の綴りも読む (2 周目レビュー H1)。** Go の
		// `t.UTC().Format(time.RFC1123)` や Python の `%Z` はこれを出す。
		// 読めないと、その peer からの署名付き POST を無期限に再投函できる。
		t.Run("RFC1123 UTC", func(t *testing.T) {
			if err := admit(stale.Format(time.RFC1123)); !errors.Is(err, ErrInboxDateSkew) {
				t.Fatalf("30 日前の Date が通った (UTC 綴り): %v", err)
			}
		})
	})

	t.Run("窓の中は書式によらず通る", func(t *testing.T) {
		for name, layout := range map[string]string{
			"RFC1123Z":    time.RFC1123Z,
			"RFC3339":     time.RFC3339,
			"RFC3339Nano": time.RFC3339Nano,
			"RFC1123 UTC": time.RFC1123,
		} {
			t.Run(name, func(t *testing.T) {
				if err := admit(base.Format(layout)); err != nil {
					t.Fatalf("通るはずが %v", err)
				}
			})
		}
	})

	// 本当に解釈できない値は従来どおり通す (upstream も Invalid Date は素通し)。
	t.Run("解釈できない値は通す", func(t *testing.T) {
		if err := admit("not a date at all"); err != nil {
			t.Fatalf("通るはずが %v", err)
		}
	})

	// **ゾーン略称は読まない (レビュー M2)。** Go は未知の略称をオフセット 0 の
	// 捏造ゾーンとして受けるので、絶対時刻がサーバーの TZ 設定に依存してずれる。
	// ずれた瞬間に skew の窓から外れて 401 になり、**これまで検査を skip して
	// 通っていた peer を落とす**方向の退行になる。読めない値として扱う。
	t.Run("ゾーン略称は読まない", func(t *testing.T) {
		for _, raw := range []string{
			"Mon, 14 Sep 2026 09:00:00 JST",
			"Sun, 13 Sep 2026 20:00:00 EST",
			"Mon, 14 Sep 2026 09:00:00 MST",
		} {
			if err := admit(raw); err != nil {
				t.Fatalf("ゾーン略称の Date を解釈して弾いている (%s): %v", raw, err)
			}
		}
	})
}

// 署名付きの GET を、見せない投稿を返してよい相手として扱う前の検査 (#3914)。
// 受信より厳しく、自ホストの未設定と読めない Date も落とす。
func TestVerifyFetchAdmission(t *testing.T) {
	const host = "example.com"
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	restore := nowFuncForAdmission
	nowFuncForAdmission = func() time.Time { return base }
	t.Cleanup(func() { nowFuncForAdmission = restore })
	now := base.Format(http.TimeFormat)
	full := sig("(request-target)", "host", "date", "accept")

	tests := []struct {
		name         string
		parsed       *ParsedSignature
		hostHeader   string
		expectedHost string
		dateHeader   string
		wantErr      error
		wantAnyErr   bool
	}{
		{name: "valid", parsed: full, hostHeader: host, expectedHost: host, dateHeader: now},
		{name: "host compared case-insensitively", parsed: full, hostHeader: "Example.COM", expectedHost: host, dateHeader: now},
		{name: "nil signature", parsed: nil, hostHeader: host, expectedHost: host, dateHeader: now, wantAnyErr: true},
		{name: "expected host not configured", parsed: full, hostHeader: host, expectedHost: "", dateHeader: now, wantErr: ErrFetchHostNotConfigured},
		{name: "request-target unsigned", parsed: sig("host", "date"), hostHeader: host, expectedHost: host, dateHeader: now, wantErr: ErrInboxRequestTargetUnsigned},
		{name: "date unsigned", parsed: sig("(request-target)", "host"), hostHeader: host, expectedHost: host, dateHeader: now, wantErr: ErrInboxDateUnsigned},
		{name: "host unsigned", parsed: sig("(request-target)", "date"), hostHeader: host, expectedHost: host, dateHeader: now, wantErr: ErrInboxHostUnsigned},
		{name: "signed for another host", parsed: full, hostHeader: "other.example", expectedHost: host, dateHeader: now, wantErr: ErrInboxHostMismatch},
		{name: "date outside skew", parsed: full, hostHeader: host, expectedHost: host, dateHeader: base.Add(-InboxDateSkew - time.Second).Format(http.TimeFormat), wantErr: ErrInboxDateSkew},
		{name: "empty date", parsed: full, hostHeader: host, expectedHost: host, dateHeader: "", wantErr: ErrFetchDateUnreadable},
		{name: "unreadable date", parsed: full, hostHeader: host, expectedHost: host, dateHeader: "not a date", wantErr: ErrFetchDateUnreadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyFetchAdmission(tt.parsed, tt.hostHeader, tt.expectedHost, tt.dateHeader)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("want %v, got %v", tt.wantErr, err)
				}
			case tt.wantAnyErr:
				if err == nil {
					t.Fatal("want an error, got nil")
				}
			default:
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
			}
		})
	}
}
