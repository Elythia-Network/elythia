package entitycompat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/notification"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
)

// --- unit tests: union parser + runtime validator ---------------------------

func TestParseUnion(t *testing.T) {
	lines := []string{
		"        N: {",
		"            id: string;",
		"            type: 'a';",
		"            onlyA: string;",
		"        } | {",
		"            id: string;",
		"            type: 'b';",
		"            onlyB: number | null;",
		"        };",
	}
	got := ParseUnion(lines, "N")
	if len(got) != 2 {
		t.Fatalf("want 2 variants, got %d: %v", len(got), got)
	}
	if _, ok := got["a"]["onlyA"]; !ok {
		t.Error("variant a missing onlyA")
	}
	if f, ok := got["b"]["onlyB"]; !ok || !f.Nullable {
		t.Errorf("variant b onlyB: %+v ok=%v (want nullable)", f, ok)
	}
	if ParseUnion(lines, "Absent") != nil {
		t.Error("absent union should return nil")
	}
}

func TestLiteralOf(t *testing.T) {
	cases := map[string]string{"'follow';": "follow", "'a' | 'b'": "a", "string;": "", "": ""}
	for in, want := range cases {
		if got := literalOf(in); got != want {
			t.Errorf("literalOf(%q)=%q want %q", in, got, want)
		}
	}
}

func TestValidateValue(t *testing.T) {
	schema := Schema{
		"req":      {Type: "string"},
		"nullable": {Type: "string", Nullable: true},
		"nonnull":  {Type: "string"},
		"opt":      {Type: "string", Optional: true},
		"num":      {Type: "number"},
	}
	actual := map[string]any{
		"req":      "ok",
		"nullable": nil,   // allowed (nullable)
		"nonnull":  nil,   // VIOLATION: null on non-null
		"num":      "str", // VIOLATION: type mismatch
		"extra":    1,     // INFO
		// "opt" absent -> allowed
	}
	got := map[string]Severity{}
	for _, f := range ValidateValue("F", "L", "G", actual, schema) {
		got[f.Field+"/"+f.Kind] = f.Sev
	}
	if got["nonnull/nullable"] != SevHigh {
		t.Error("expected nonnull null violation HIGH")
	}
	if got["num/type"] != SevMed {
		t.Error("expected num type mismatch MED")
	}
	if got["extra/extra"] != SevInfo {
		t.Error("expected extra INFO")
	}
	if _, ok := got["nullable/nullable"]; ok {
		t.Error("nullable=null must not be flagged")
	}
	if _, ok := got["opt/missing"]; ok {
		t.Error("absent optional must not be flagged")
	}

	// missing required
	got2 := map[string]Severity{}
	for _, f := range ValidateValue("F", "L", "G", map[string]any{"nullable": "x", "nonnull": "x", "num": 1}, schema) {
		got2[f.Field+"/"+f.Kind] = f.Sev
	}
	if got2["req/missing"] != SevHigh {
		t.Error("expected req missing HIGH")
	}
}

// TestValidateValue_ArrayElem covers the Layer 2 array element-type check
// (#1298): string[] vs object[] etc. that the coarse "array" Type alone cannot
// distinguish.
func TestValidateValue_ArrayElem(t *testing.T) {
	schema := Schema{
		"roles":   {Type: "array", Elem: "object"}, // golden {id,name}[]
		"tags":    {Type: "array", Elem: "string"}, // golden string[]
		"raw":     {Type: "array", Elem: "object"}, // raw JSON column (json.RawMessage)
		"refs":    {Type: "array", Elem: "other"},  // unclassifiable element -> skipped
		"emptied": {Type: "array", Elem: "object"}, // empty -> cannot determine -> skipped
	}
	actual := map[string]any{
		// VIOLATION: golden wants object elements but runtime is string[].
		"roles": []any{"role1", "role2"},
		// OK: string elements match.
		"tags": []any{"a", "b"},
		// OK: raw JSON decoded to object elements.
		"raw": json.RawMessage(`[{"id":"r1","name":"n1"}]`),
		// skipped: golden elem "other".
		"refs": []any{"whatever"},
		// skipped: empty array.
		"emptied": []any{},
	}
	got := map[string]Severity{}
	for _, f := range ValidateValue("F", "L", "G", actual, schema) {
		got[f.Field+"/"+f.Kind] = f.Sev
	}
	if got["roles/elem"] != SevMed {
		t.Error("expected roles element mismatch (string vs object) MED")
	}
	if _, ok := got["tags/elem"]; ok {
		t.Error("matching string[] must not be flagged")
	}
	if _, ok := got["raw/elem"]; ok {
		t.Error("raw JSON object[] must decode and match (no finding)")
	}
	if _, ok := got["refs/elem"]; ok {
		t.Error("golden elem 'other' must be skipped")
	}
	if _, ok := got["emptied/elem"]; ok {
		t.Error("empty array must be skipped (element type undeterminable)")
	}
}

func TestValidateUnionValue(t *testing.T) {
	variants := map[string]Schema{
		"a": {"id": {Type: "string"}, "type": {Type: "string"}},
	}
	// matching variant, clean
	if f := ValidateUnionValue("N", map[string]any{"type": "a", "id": "x"}, variants); len(f) != 0 {
		t.Errorf("clean variant should have no findings, got %v", f)
	}
	// unknown type literal
	f := ValidateUnionValue("N", map[string]any{"type": "zzz"}, variants)
	if len(f) != 1 || f[0].Sev != SevHigh {
		t.Errorf("unknown type should be 1 HIGH finding, got %v", f)
	}
}

func TestJSONTypeAndScalar(t *testing.T) {
	s := "x"
	cases := []struct {
		v    any
		want string
	}{
		{"x", "string"}, {true, "boolean"}, {1, "number"}, {1.5, "number"},
		{[]int{1}, "array"}, {map[string]int{}, "object"}, {struct{}{}, "object"},
		{nil, "null"}, {&s, "string"}, {(*string)(nil), "null"},
	}
	for _, c := range cases {
		if got := jsonType(c.v); got != c.want {
			t.Errorf("jsonType(%#v)=%q want %q", c.v, got, c.want)
		}
	}
	if !scalarMismatch("string", "number") {
		t.Error("string vs number should mismatch")
	}
	if scalarMismatch("object", "array") {
		t.Error("object/array buckets must be skipped")
	}
	if scalarMismatch("string", "string") {
		t.Error("same type must not mismatch")
	}
}

// --- L2 integration: validate real PackNotification output ------------------

func loadUnions(t *testing.T) map[string]map[string]Schema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden_unions.json"))
	if err != nil {
		t.Fatalf("read golden_unions: %v", err)
	}
	var u map[string]map[string]Schema
	if err := json.Unmarshal(b, &u); err != nil {
		t.Fatalf("decode golden_unions: %v", err)
	}
	return u
}

// notifFixture builds a packed notification map for a given type using the
// same construction pattern as the entity package's own packer tests.
func notifFixture(typ notification.Type, withUser, withNote bool, extra map[string]any, opts ...entity.NotificationOption) map[string]any {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID:         "n1",
		CreatedAt:  time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		Type:       typ,
		NotifierID: "u_actor",
		Extra:      extra,
	}
	var user *model.User
	var note *model.Note
	if withUser {
		user = &model.User{ID: "u_actor", Username: "actor", UsernameLower: "actor"}
	}
	if withNote {
		s := "hi"
		n.NoteID = "note1"
		note = &model.Note{ID: "note1", UserID: "u_me", Text: &s}
	}
	if typ == notification.TypeReaction {
		n.Reaction = ":smile:"
	}
	// roleAssigned は notifier を持たない (RoleService.assign は system/moderator
	// 起点で notifier 概念が無い)。golden union の variant も userId を含まない。
	if typ == notification.TypeRoleAssigned {
		n.NotifierID = ""
	}
	return entity.PackNotification(n, user, note, idGen, nil, nil, opts...)
}

func TestEmbeddedGolden(t *testing.T) {
	g := EmbeddedGolden()
	if len(g) == 0 {
		t.Fatal("embedded golden snapshot is empty")
	}
	if _, ok := g["UserLite"]; !ok {
		t.Error("embedded golden missing UserLite")
	}
}

func TestValidateResponse(t *testing.T) {
	// 未知 schema は誤用を可視化するため単一 HIGH finding を返す。
	f := ValidateResponse("NoSuchSchema", map[string]any{})
	if len(f) != 1 || f[0].Sev != SevHigh {
		t.Errorf("unknown schema: want 1 HIGH finding, got %v", f)
	}
	// 既知 schema (UserLite) に必須欄を欠く response -> gated drift。
	if got := ValidateResponse("UserLite", map[string]any{"username": "x"}); len(got) == 0 {
		t.Error("expected gated drift for incomplete UserLite response")
	}
	// gated は HIGH/MED のみ (INFO の extra は含まない)。
	for _, x := range ValidateResponse("UserLite", map[string]any{"username": "x", "extraKey": 1}) {
		if x.Sev != SevHigh && x.Sev != SevMed {
			t.Errorf("ValidateResponse returned non-gated severity %s", x.Sev)
		}
	}
}

// TestValidateResponse_Union covers the discriminated-union dispatch path
// (#1326): a union schema name (Notification) routes to ValidateUnionValue via
// the value's `type` discriminator.
func TestValidateResponse_Union(t *testing.T) {
	if _, ok := EmbeddedUnions()["Notification"]; !ok {
		t.Fatal("embedded unions missing Notification")
	}
	// follow variant の必須欄 (createdAt/id/type/user/userId) を満たす -> clean。
	ok := map[string]any{
		"id": "n1", "createdAt": "2026-05-27T00:00:00.000Z", "type": "follow",
		"userId": "u1", "user": map[string]any{"id": "u1"},
	}
	if f := ValidateResponse("Notification", ok); len(f) != 0 {
		t.Errorf("complete follow notification should pass, got %v", f)
	}
	// 未知 type literal -> union dispatch 不能で HIGH。
	if f := ValidateResponse("Notification", map[string]any{"type": "zzz"}); len(f) != 1 || f[0].Sev != SevHigh {
		t.Errorf("unknown notification type: want 1 HIGH, got %v", f)
	}
	// 必須 user 欠落 -> gated drift。
	if f := ValidateResponse("Notification", map[string]any{
		"id": "n1", "createdAt": "x", "type": "follow", "userId": "u1",
	}); len(f) == 0 {
		t.Error("expected gated drift for follow notification missing user")
	}
}

// requireL2Schema loads a flat golden schema for an L2 map-based packer test,
// failing if the snapshot is missing/empty (regenerate-miss guard).
func requireL2Schema(t *testing.T, name string) Schema {
	t.Helper()
	golden, err := LoadGoldenSnapshot(filepath.Join("testdata", "golden_schemas.json"))
	if err != nil {
		t.Fatalf("load golden: %v", err)
	}
	schema, ok := golden[name]
	if !ok || len(schema) == 0 {
		t.Fatalf("%s schema missing/empty in golden snapshot; run `go run ./tools/shapediff`", name)
	}
	return schema
}

// assertNoGatedDrift fails on any HIGH/MED finding from validating a packer's
// runtime map output against its golden schema.
func assertNoGatedDrift(t *testing.T, name string, actual map[string]any, schema Schema) {
	t.Helper()
	for _, f := range ValidateValue(name, name, name, actual, schema) {
		if f.Sev == SevHigh || f.Sev == SevMed {
			t.Errorf("%s shape drift: %s [%s]: %s", name, f.Field, f.Kind, f.Detail)
		}
	}
}

// TestAnnouncementShapeL2 validates the actual PackAnnouncement map output
// against the golden Announcement schema. Announcement is packed as
// map[string]any (not a reflectable struct), so this is the L2 runtime check
// for that map-based packer (#1224 history: createdAt non-null cast crash).
func TestAnnouncementShapeL2(t *testing.T) {
	schema := requireL2Schema(t, "Announcement")
	idGen, _ := id.NewGenerator("aidx")
	img := "https://example.com/a.png"
	updated := time.Date(2026, 5, 26, 1, 0, 0, 0, time.UTC)
	uid := "u_author"
	cases := map[string]*model.Announcement{
		// nullable 欄 (imageUrl/updatedAt) 未設定 -> null path。
		"minimal": {
			ID: idGen.Generate(time.Now()), Title: "maintenance", Text: "down",
			Icon: "info", Display: "normal",
		},
		// nullable 欄に値あり -> 非null string pass-through path (scalarMismatch を
		// 実際に効かせる) + forYou=true (UserID 設定)。
		"populated": {
			ID: idGen.Generate(time.Now()), Title: "t", Text: "x", Icon: "info",
			Display: "banner", ImageURL: &img, UpdatedAt: &updated, UserID: &uid,
			NeedConfirmationToRead: true, Silence: true,
		},
	}
	for name, a := range cases {
		assertNoGatedDrift(t, "Announcement["+name+"]", entity.PackAnnouncement(a, idGen, true, ""), schema)
	}
}

// TestClipShapeL2 validates PackClip's map output against golden Clip. Clip is
// map-based; with idGen + owner it must emit all required golden fields
// (createdAt / user 含む)。
func TestClipShapeL2(t *testing.T) {
	schema := requireL2Schema(t, "Clip")
	idGen, _ := id.NewGenerator("aidx")
	owner := &model.User{ID: "u_owner", Username: "owner"}
	desc := "my clip"
	cl := &model.Clip{ID: idGen.Generate(time.Now()), UserID: "u_owner", Name: "clip", Description: &desc, IsPublic: true}
	out := entity.PackClip(cl, idGen, owner, entity.ClipExtras{})
	assertNoGatedDrift(t, "Clip", out, schema)
}

// TestSigninShapeL2 validates PackSignin's map output against golden Signin.
func TestSigninShapeL2(t *testing.T) {
	schema := requireL2Schema(t, "Signin")
	idGen, _ := id.NewGenerator("aidx")
	s := &model.Signin{ID: idGen.Generate(time.Now()), IP: "203.0.113.7", Success: true}
	out := entity.PackSignin(s, idGen)
	assertNoGatedDrift(t, "Signin", out, schema)
}

// TestPageShapeL2 validates PackPageWithContext's map output against golden
// Page. Tested with the common "no eye-catching image" case: golden requires
// `eyeCatchingImage` present (DriveFile | null), so it must be emitted as null
// rather than omitted.
func TestPageShapeL2(t *testing.T) {
	schema := requireL2Schema(t, "Page")
	idGen, _ := id.NewGenerator("aidx")
	owner := &model.User{ID: "u_owner", Username: "owner"}
	liked := false
	p := &model.Page{
		ID:        idGen.Generate(time.Now()),
		UserID:    "u_owner",
		UpdatedAt: time.Now(),
		Title:     "title",
		Name:      "name",
		Font:      "sans-serif",
		Content:   datatypes.JSON([]byte("[]")),
		Variables: datatypes.JSON([]byte("[]")),
	}
	out := entity.PackPageWithContext(p, entity.PackPageContext{IDGen: idGen, Owner: owner, IsLiked: &liked})
	assertNoGatedDrift(t, "Page", out, schema)
}

// TestNotificationShapeL2 validates actual PackNotification map output against
// the golden Notification union — the map-based, discriminated-union surface
// that the Layer 0 reflection gate cannot reach.
func TestNotificationShapeL2(t *testing.T) {
	unions := loadUnions(t)
	notif := unions["Notification"]
	if len(notif) == 0 {
		t.Fatal("no Notification union variants in snapshot")
	}
	// 各 variant が非空か (書式変更でパーサが空振りした兆候の検出)。
	for typ, v := range notif {
		if len(v) == 0 {
			t.Errorf("Notification variant %q parsed to 0 fields; snapshot likely broken", typ)
		}
	}

	idGenRole, _ := id.NewGenerator("aidx")
	roleLookup := entity.WithRoleLookup(func(string) (map[string]any, bool) {
		return entity.PackRole(&model.Role{ID: "r1", Name: "VIP", IsPublic: true}, 0, idGenRole, corerole.DefaultPolicies()), true
	})
	// #2868: abuseReport は read 時に通報の状態を引く。**渡さないと通知ごと
	// drop される** (fail-closed) ので、shape を検査するには必須。
	abuseLookup := entity.WithAbuseReportLookup(func(string) (entity.AbuseReportStatus, bool) {
		return entity.AbuseReportStatus{Resolved: true, ResolvedAs: "accept", AssigneeID: "mod1"}, true
	})
	chatInvLookup := entity.WithChatInvitationLookup(func(invID, _ string) (map[string]any, bool) {
		return map[string]any{
			"id": invID, "createdAt": "2026-05-25T00:00:00.000Z",
			"userId": "u_invitee", "roomId": "room1",
		}, true
	})
	cases := []struct {
		typ      notification.Type
		withUser bool
		withNote bool
		extra    map[string]any
		opts     []entity.NotificationOption
	}{
		{notification.TypeFollow, true, false, nil, nil},
		{notification.TypeReaction, true, true, nil, nil},
		{notification.TypeMention, true, true, nil, nil},
		{notification.TypeReply, true, true, nil, nil},
		{notification.TypeRenote, true, true, nil, nil},
		{notification.TypeQuote, true, true, nil, nil},
		{notification.TypeReceiveFollowReq, true, false, nil, nil},
		{notification.TypePollVote, true, true, nil, nil},
		{notification.TypeImportCompleted, false, false, map[string]any{"fileId": "df_1"}, nil},
		// #1559: note は notifier(user) + note を伴う。login/createToken/test は
		// notifier も extra も持たない bare shape (id/createdAt/type)。
		{notification.TypeNote, true, true, nil, nil},
		{notification.TypeLogin, false, false, nil, nil},
		{notification.TypeCreateToken, false, false, nil, nil},
		{notification.TypeTest, false, false, nil, nil},
		// #1559: roleAssigned は Extra["roleId"] を read 時に packed role へ解決する。
		{notification.TypeRoleAssigned, false, false, map[string]any{"roleId": "r1"}, []entity.NotificationOption{roleLookup}},
		// #1559: chatRoomInvitationReceived は Extra["invitationId"] を read 時に
		// packed invitation へ解決する。notifier(招待者) を持つので userId も出る。
		{notification.TypeChatRoomInvitationReceived, true, false, map[string]any{"invitationId": "inv1"}, []entity.NotificationOption{chatInvLookup}},
		// #2868: abuseReport は mk-go 固有。golden の union に無いので
		// allowlist_l2.json に登録してある。**実際に produce される初めての
		// union 外 type** なので、ここに載せないと誰も検査しない。
		{notification.TypeAbuseReport, true, false, map[string]any{"reportId": "rep1", "targetUserId": "u2"}, []entity.NotificationOption{abuseLookup}},
	}

	allow, err := LoadAllowlist(filepath.Join("testdata", "allowlist_l2.json"))
	if err != nil {
		t.Fatalf("load L2 allowlist: %v", err)
	}

	var gated []Finding
	for _, c := range cases {
		out := notifFixture(c.typ, c.withUser, c.withNote, c.extra, c.opts...)
		for _, f := range ValidateUnionValue("Notification", out, notif) {
			if f.Sev == SevHigh || f.Sev == SevMed {
				gated = append(gated, f)
			}
		}
	}

	violations, stale := RunGate(gated, allow)
	for _, v := range violations {
		t.Errorf("NEW notification shape drift: %s [%s]: %s\n  -> fix the packer, or add to testdata/allowlist_l2.json with a reason",
			v.Field, v.Kind, v.Detail)
	}
	for _, s := range stale {
		t.Errorf("stale L2 allowlist entry: %s [%s] no longer drifts; remove it from testdata/allowlist_l2.json", s.Field, s.Kind)
	}
}
