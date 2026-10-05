package abuse_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/abuse"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stubs -------------------------------------------------------------------

type stubReportStore struct {
	report   *model.AbuseUserReport
	findErr  error
	updated  map[string]any
	updateEr error
}

func (s *stubReportStore) FindByID(_ string) (*model.AbuseUserReport, error) {
	return s.report, s.findErr
}
func (s *stubReportStore) UpdateFields(_ string, fields map[string]any) error {
	if s.updateEr != nil {
		return s.updateEr
	}
	s.updated = fields
	return nil
}

type stubSystemActor struct {
	actor *model.User
	err   error
}

func (s *stubSystemActor) Fetch(_ string) (*model.User, error) {
	return s.actor, s.err
}

type stubRenderer struct {
	lastActor *model.User
	lastURI   string
	lastBody  string
}

func (s *stubRenderer) RenderFlag(actor *model.User, targetURI, content string) *activitypub.Flag {
	s.lastActor = actor
	s.lastURI = targetURI
	s.lastBody = content
	return &activitypub.Flag{
		Activity: activitypub.Activity{
			Object: activitypub.Object{Type: "Flag", Context: "https://www.w3.org/ns/activitystreams"},
			Actor:  "https://local.example/users/" + actor.ID,
		},
		Object:  targetURI,
		Content: content,
	}
}

type spyDeliverer struct {
	signerID string
	body     []byte
	inboxes  []string
	err      error
	called   int
}

func (s *spyDeliverer) DeliverActivity(signerID string, body []byte, inboxes []string) error {
	s.called++
	s.signerID = signerID
	s.body = body
	s.inboxes = inboxes
	return s.err
}

// --- helpers -----------------------------------------------------------------

func remoteTarget() *model.User {
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	inbox := "https://remote.example/users/alice/inbox"
	return &model.User{ID: "u-remote", Host: &host, URI: &uri, Inbox: &inbox}
}

func localTarget() *model.User {
	return &model.User{ID: "u-local"}
}

// --- tests -------------------------------------------------------------------

func TestForwardReport_RemoteTargetEnqueuesFlag(t *testing.T) {
	report := &model.AbuseUserReport{ID: "r1", Comment: "spam!", TargetUser: remoteTarget()}
	store := &stubReportStore{report: report}
	sys := &stubSystemActor{actor: &model.User{ID: "instance"}}
	render := &stubRenderer{}
	deliver := &spyDeliverer{}

	f := abuse.NewForwarder(store, sys, render, deliver)
	require.NoError(t, f.ForwardReport("r1"))

	assert.Equal(t, map[string]any{"forwarded": true}, store.updated)
	assert.Equal(t, 1, deliver.called)
	assert.Equal(t, "instance", deliver.signerID)
	assert.Equal(t, []string{"https://remote.example/users/alice/inbox"}, deliver.inboxes)

	// body は Flag JSON-LD
	var payload map[string]any
	require.NoError(t, json.Unmarshal(deliver.body, &payload))
	assert.Equal(t, "Flag", payload["type"])
	assert.Equal(t, "https://remote.example/users/alice", payload["object"])
	assert.Equal(t, "spam!", payload["content"])

	// renderer 引数確認
	assert.Equal(t, "instance", render.lastActor.ID)
	assert.Equal(t, "https://remote.example/users/alice", render.lastURI)
	assert.Equal(t, "spam!", render.lastBody)
}

// TestForwardReport_RealRendererIncludesID は #1951 の回帰テスト。stub ではなく実
// activitypub.Renderer を通し、配信される Flag body に非空の id が乗ることを検証する。
// id を持たない activity は受信側 InboxProcessor に silent drop されるため。
func TestForwardReport_RealRendererIncludesID(t *testing.T) {
	report := &model.AbuseUserReport{ID: "r1", Comment: "spam!", TargetUser: remoteTarget()}
	store := &stubReportStore{report: report}
	sys := &stubSystemActor{actor: &model.User{ID: "instance"}}
	render := activitypub.NewRenderer(activitypub.NewURLBuilder("https://local.example"))
	deliver := &spyDeliverer{}

	f := abuse.NewForwarder(store, sys, render, deliver)
	require.NoError(t, f.ForwardReport("r1"))

	require.Equal(t, 1, deliver.called)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(deliver.body, &payload))
	id, ok := payload["id"].(string)
	require.True(t, ok, "delivered Flag must carry a string id")
	assert.NotEmpty(t, id)
	assert.True(t, strings.HasPrefix(id, "https://local.example/"))
}

func TestForwardReport_LocalTargetMarksForwardedOnly(t *testing.T) {
	report := &model.AbuseUserReport{ID: "r2", Comment: "x", TargetUser: localTarget()}
	store := &stubReportStore{report: report}
	deliver := &spyDeliverer{}

	f := abuse.NewForwarder(store, &stubSystemActor{}, &stubRenderer{}, deliver)
	require.NoError(t, f.ForwardReport("r2"))

	assert.Equal(t, map[string]any{"forwarded": true}, store.updated)
	assert.Equal(t, 0, deliver.called, "ローカル通報は配送スキップ")
}

func TestForwardReport_RemoteWithoutInboxIsNoop(t *testing.T) {
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	report := &model.AbuseUserReport{ID: "r3", TargetUser: &model.User{ID: "u", Host: &host, URI: &uri}}
	deliver := &spyDeliverer{}

	f := abuse.NewForwarder(&stubReportStore{report: report}, &stubSystemActor{}, &stubRenderer{}, deliver)
	require.NoError(t, f.ForwardReport("r3"))
	assert.Equal(t, 0, deliver.called)
}

func TestForwardReport_FindErrorPropagates(t *testing.T) {
	store := &stubReportStore{findErr: errors.New("db boom")}
	f := abuse.NewForwarder(store, &stubSystemActor{}, &stubRenderer{}, &spyDeliverer{})
	err := f.ForwardReport("nope")
	assert.Error(t, err)
}

func TestForwardReport_SystemActorErrorPropagates(t *testing.T) {
	report := &model.AbuseUserReport{ID: "r", TargetUser: remoteTarget()}
	store := &stubReportStore{report: report}
	sys := &stubSystemActor{err: errors.New("no actor")}
	f := abuse.NewForwarder(store, sys, &stubRenderer{}, &spyDeliverer{})
	assert.Error(t, f.ForwardReport("r"))
}

func TestForwardReport_DelivererErrorPropagates(t *testing.T) {
	report := &model.AbuseUserReport{ID: "r", Comment: "c", TargetUser: remoteTarget()}
	store := &stubReportStore{report: report}
	sys := &stubSystemActor{actor: &model.User{ID: "instance"}}
	deliver := &spyDeliverer{err: errors.New("redis down")}
	f := abuse.NewForwarder(store, sys, &stubRenderer{}, deliver)
	assert.Error(t, f.ForwardReport("r"))
}

func TestForwardReport_UpdateErrorPropagates(t *testing.T) {
	report := &model.AbuseUserReport{ID: "r", TargetUser: remoteTarget()}
	store := &stubReportStore{report: report, updateEr: errors.New("db locked")}
	f := abuse.NewForwarder(store, &stubSystemActor{}, &stubRenderer{}, &spyDeliverer{})
	assert.Error(t, f.ForwardReport("r"))
}

func TestForwardReport_NilTargetIsNoop(t *testing.T) {
	report := &model.AbuseUserReport{ID: "r", TargetUser: nil}
	deliver := &spyDeliverer{}
	f := abuse.NewForwarder(&stubReportStore{report: report}, &stubSystemActor{}, &stubRenderer{}, deliver)
	require.NoError(t, f.ForwardReport("r"))
	assert.Equal(t, 0, deliver.called)
}

func TestForwardReport_RemoteWithoutURIIsNoop(t *testing.T) {
	host := "remote.example"
	report := &model.AbuseUserReport{ID: "r", TargetUser: &model.User{ID: "u", Host: &host}}
	deliver := &spyDeliverer{}
	f := abuse.NewForwarder(&stubReportStore{report: report}, &stubSystemActor{}, &stubRenderer{}, deliver)
	require.NoError(t, f.ForwardReport("r"))
	assert.Equal(t, 0, deliver.called)
}

// systemSvc が nil actor + nil err を返す異常系のガード。
func TestForwardReport_NilSystemActorReturnsError(t *testing.T) {
	report := &model.AbuseUserReport{ID: "r", TargetUser: remoteTarget()}
	f := abuse.NewForwarder(&stubReportStore{report: report}, &stubSystemActor{actor: nil}, &stubRenderer{}, &spyDeliverer{})
	err := f.ForwardReport("r")
	require.Error(t, err)
}

// Inbox 不在なら SharedInbox へ倒す (preferredInbox のフォールバック)。
func TestForwardReport_UsesSharedInboxWhenInboxNil(t *testing.T) {
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	shared := "https://remote.example/inbox"
	target := &model.User{ID: "u-r", Host: &host, URI: &uri, SharedInbox: &shared}
	report := &model.AbuseUserReport{ID: "r", Comment: "c", TargetUser: target}
	deliver := &spyDeliverer{}

	f := abuse.NewForwarder(&stubReportStore{report: report}, &stubSystemActor{actor: &model.User{ID: "instance"}}, &stubRenderer{}, deliver)
	require.NoError(t, f.ForwardReport("r"))
	assert.Equal(t, 1, deliver.called)
	assert.Equal(t, []string{shared}, deliver.inboxes)
}

// **個別 inbox が優先される** (upstream の direct recipe と同じ。理由は
// core/federation の preferredInbox の GoDoc)。
func TestForwardReport_PrefersIndividualInbox(t *testing.T) {
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	inbox := "https://remote.example/users/alice/inbox"
	shared := "https://remote.example/inbox"
	target := &model.User{ID: "u-r", Host: &host, URI: &uri, Inbox: &inbox, SharedInbox: &shared}
	report := &model.AbuseUserReport{ID: "r", Comment: "c", TargetUser: target}
	deliver := &spyDeliverer{}

	f := abuse.NewForwarder(&stubReportStore{report: report}, &stubSystemActor{actor: &model.User{ID: "instance"}}, &stubRenderer{}, deliver)
	require.NoError(t, f.ForwardReport("r"))
	assert.Equal(t, []string{inbox}, deliver.inboxes,
		"個別 inbox が sharedInbox より優先されるべき")
}
