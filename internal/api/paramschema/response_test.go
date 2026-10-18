package paramschema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/upstreamsrc"
)

func TestResponseIsUpstreamInvalidParam(t *testing.T) {
	s, err := Compile([]byte(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":100,"default":10}}}`))
	if err != nil {
		t.Fatal(err)
	}
	e := s.Validate(map[string]any{"limit": 0.0})
	if e == nil {
		t.Fatal("limit 0 was accepted")
	}
	b, err := json.Marshal(e.Response())
	if err != nil {
		t.Fatal(err)
	}
	// endpoint-base.tsが組むApiErrorをApiCallServiceが返すbody。infoは、ajv
	// 8.20.0が{"limit":0}に返すerrors[0] (corpusのlimitの行と同じ)。
	const want = `{"error":{"message":"Invalid param.","code":"INVALID_PARAM","id":"3d81ceae-475f-4600-b2a8-2bc116157532","kind":"client","info":{"param":"#/properties/limit/minimum","reason":"must be >= 1"}}}`
	var got, w any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		t.Fatalf("Response() = %s\nwant %s", b, want)
	}
}

// TestResponseMatchesUpstreamEndpointBase reads upstream's endpoint-base.ts
// and checks that it still builds the error the way Response does. It runs
// in `make upstream-check` (MK_UPSTREAM_REQUIRE=1) and skips without the
// upstream checkout.
func TestResponseMatchesUpstreamEndpointBase(t *testing.T) {
	dir, err := upstreamsrc.Locate(filepath.Join("..", "..", ".."))
	if err != nil {
		if upstreamsrc.Required() {
			t.Fatalf("%s が立っているのに本家を読めない: %v", upstreamsrc.EnvRequire, err)
		}
		t.Skipf("upstream Misskey not available: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "packages/backend/src/server/api/endpoint-base.ts"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"message: 'Invalid param.'",
		"code: 'INVALID_PARAM'",
		"id: '3d81ceae-475f-4600-b2a8-2bc116157532'",
		"param: errors[0].schemaPath",
		"reason: errors[0].message",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("endpoint-base.ts no longer contains %q; review Response", want)
		}
	}
}
