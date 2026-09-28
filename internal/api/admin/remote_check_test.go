package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/remotecheck"
	"github.com/shiroha-a/mk/internal/core/selfcheck"
)

type stubRemoteChecker struct {
	got      []remotecheck.Request
	deadline bool
}

func (s *stubRemoteChecker) CheckRemoteHost(ctx context.Context, req remotecheck.Request) remotecheck.Report {
	s.got = append(s.got, req)
	_, s.deadline = ctx.Deadline()
	return remotecheck.Report{Host: req.Host, OK: true, Results: []selfcheck.Result{{Name: "nodeinfo", Status: selfcheck.StatusOK}}}
}

// 管理者が指定したホストを正規化して検査に渡す。account はユーザー名だけにする。
func TestFederationCheckHost_NormalizesAndRuns(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	s := &stubRemoteChecker{}
	h.SetRemoteChecker(s, "self.example")

	for _, tc := range []struct {
		body     string
		host     string
		username string
	}{
		{`{"host":"Remote.Example"}`, "remote.example", ""},
		{`{"host":"https://remote.example/@alice"}`, "remote.example", ""},
		{`{"host":"remote.example","account":"alice"}`, "remote.example", "alice"},
		{`{"host":"remote.example","account":"@alice@Remote.Example"}`, "remote.example", "alice"},
		{`{"host":"bücher.example"}`, "xn--bcher-kva.example", ""},
		{`{"host":"remote.example."}`, "remote.example", ""},
		{`{"host":"remote.example.:8443"}`, "remote.example:8443", ""},
		{`{"host":"remote.example","account":"alice@remote.example."}`, "remote.example", "alice"},
	} {
		rec := doPost(h.FederationCheckHost, tc.body, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, tc.body)
		var got remotecheck.Report
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, tc.host, got.Host, tc.body)
		last := s.got[len(s.got)-1]
		assert.Equal(t, remotecheck.Request{Host: tc.host, Username: tc.username}, last, tc.body)
	}
	assert.True(t, s.deadline, "the check runs under a timeout")
}

// 自ホストは断る。self-check (SSRF ガードの無い経路) へは分岐させない。
func TestFederationCheckHost_RefusesSelf(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	s := &stubRemoteChecker{}
	h.SetRemoteChecker(s, "self.example")

	for _, body := range []string{`{"host":"self.example"}`, `{"host":"SELF.example"}`, `{"host":"https://self.example/"}`, `{"host":"self.example."}`, `{"host":"self.example:443"}`} {
		rec := doPost(h.FederationCheckHost, body, adminUser)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), "CANNOT_CHECK_SELF", body)
	}
	assert.Empty(t, s.got, "nothing is fetched for our own host")
}

func TestFederationCheckHost_RejectsBadInput(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	s := &stubRemoteChecker{}
	h.SetRemoteChecker(s, "self.example")

	for _, tc := range []struct{ body, code string }{
		{`{}`, "INVALID_PARAM"},
		{`{"host":"  "}`, "INVALID_PARAM"},
		{`{"host":"remote.example","account":"alice@other.example"}`, "INVALID_ACCOUNT"},
		{`{"host":"remote.example","account":"a/b"}`, "INVALID_ACCOUNT"},
		{`{"host":"remote.example","account":"@"}`, "INVALID_ACCOUNT"},
		{`{"host":"remote.example","account":"al ice"}`, "INVALID_ACCOUNT"},
	} {
		rec := doPost(h.FederationCheckHost, tc.body, adminUser)
		assert.Equal(t, http.StatusBadRequest, rec.Code, tc.body)
		assert.Contains(t, rec.Body.String(), tc.code, tc.body)
	}
	assert.Empty(t, s.got)
}

// 未配線でも 200 で空の結果を返す (self-check と同じ扱い)。
func TestFederationCheckHost_Unwired(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.FederationCheckHost, `{"host":"remote.example"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"host":"remote.example","results":[],"ok":true}`, rec.Body.String())
}
