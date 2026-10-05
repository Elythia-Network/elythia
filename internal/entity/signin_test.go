package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestPackSignin_Basic(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	sid := idGen.Generate(time.Now())
	s := &model.Signin{
		ID:      sid,
		UserID:  "u1",
		IP:      "1.2.3.4",
		Headers: datatypes.JSON([]byte(`{"user-agent":"test"}`)),
		Success: true,
	}
	out := PackSignin(s, idGen)
	assert.Equal(t, sid, out["id"])
	assert.Equal(t, "1.2.3.4", out["ip"])
	assert.Equal(t, true, out["success"])
	// headers は RawMessage として埋まる (JSON encoder が生バイトを出す)
	raw, ok := out["headers"].(json.RawMessage)
	require.True(t, ok)
	assert.JSONEq(t, `{"user-agent":"test"}`, string(raw))
	// createdAt が aidx-ID から復元されていること
	_, ok = out["createdAt"]
	assert.True(t, ok)
}

func TestPackSignin_NilInput(t *testing.T) {
	assert.Nil(t, PackSignin(nil))
}

func TestPackSignin_WithoutIDGen_OmitsCreatedAt(t *testing.T) {
	s := &model.Signin{ID: "x", IP: "::1", Success: false}
	out := PackSignin(s)
	_, ok := out["createdAt"]
	assert.False(t, ok)
	// headers 空でも default map が入ること
	_, ok = out["headers"].(map[string]any)
	assert.True(t, ok)
}

func TestPackSignin_InvalidIDOmitsCreatedAt(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	s := &model.Signin{ID: "not-aidx", Success: true}
	out := PackSignin(s, idGen)
	_, ok := out["createdAt"]
	assert.False(t, ok)
}
