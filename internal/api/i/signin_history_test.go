package i

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestSigninHistory(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusOK, postExtra(h.SigninHistory, `{}`, stubUser).Code)
}

func TestSigninHistory_WithData(t *testing.T) {
	h, _ := newExtraHandler(t)
	signinRepo := testutil.NewMockSigninRepository()
	h.SetSigninRepo(signinRepo)

	idGen, _ := id.NewGenerator("aidx")
	now := time.Now()
	signinRepo.Signins = append(signinRepo.Signins, &model.Signin{
		ID:      idGen.Generate(now),
		UserID:  "u1",
		IP:      "192.168.1.1",
		Headers: datatypes.JSON(`{"User-Agent":["test"]}`),
		Success: true,
	})

	rec := postExtra(h.SigninHistory, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	assert.Equal(t, "192.168.1.1", resp[0]["ip"])
	assert.Equal(t, true, resp[0]["success"])
	assert.NotEmpty(t, resp[0]["createdAt"])
	shapetest.Assert(t, "Signin", resp[0]) // L3 (#1322)
}

func TestSigninHistory_Empty(t *testing.T) {
	h, _ := newExtraHandler(t)
	signinRepo := testutil.NewMockSigninRepository()
	h.SetSigninRepo(signinRepo)

	rec := postExtra(h.SigninHistory, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 0)
}

func TestSigninHistory_WithLimit(t *testing.T) {
	h, _ := newExtraHandler(t)
	signinRepo := testutil.NewMockSigninRepository()
	h.SetSigninRepo(signinRepo)

	idGen, _ := id.NewGenerator("aidx")
	for i := 0; i < 5; i++ {
		signinRepo.Signins = append(signinRepo.Signins, &model.Signin{
			ID:      idGen.Generate(time.Now().Add(time.Duration(i) * time.Millisecond)),
			UserID:  "u1",
			IP:      "1.2.3.4",
			Headers: datatypes.JSON(`{}`),
			Success: true,
		})
	}

	rec := postExtra(h.SigninHistory, `{"limit":2}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
}

func TestSigninHistory_NoRepo(t *testing.T) {
	h, _ := newExtraHandler(t)
	// signinRepoが未設定の場合は空配列を返す
	rec := postExtra(h.SigninHistory, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
}
