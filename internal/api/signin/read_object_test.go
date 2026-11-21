package signin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
)

// failingBody fails every read with err.
type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }

// TestReadSigninObject_KeepsTheBodyLimit413 pins that a body cut by
// BodyLimit answers with its 413, and that another read error is a 400
// (#4239). echo の v5 の HTTPErrorHandler は包んだ外側の status を採るので、413 を
// 400 で包むと 400 になる。
func TestReadSigninObject_KeepsTheBodyLimit413(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"body limit", echo.ErrStatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge},
		{"other read error", errors.New("connection reset"), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/signin-flow", failingBody{tc.err})
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			_, err := readSigninObject(echo.New().NewContext(req, httptest.NewRecorder()))
			assert.Equal(t, tc.want, echo.StatusCode(err), err)
		})
	}
}
