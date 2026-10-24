package drive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// ctxProcessor reports the context Process receives.
type ctxProcessor struct{ ctxs chan context.Context }

func (p *ctxProcessor) Process(ctx context.Context, _ URLUploadInput) { p.ctxs <- ctx }

// The upload from a URL runs in the group of the server generation (#3661):
// it receives the context of the group, which stopping the generation
// cancels, and after the group has stopped it does not start.
func TestFilesUploadFromURL_RunsInTheBackgroundGroup(t *testing.T) {
	h, _, _ := newHandler(t)
	rp := &ctxProcessor{ctxs: make(chan context.Context, 1)}
	h.SetURLUploader(rp)
	g := bgwork.New()
	h.background = g

	upload := func() {
		c, rec := newJSONReq(t, `{"url":"https://example.com/a.png"}`)
		setUser(c, "u1")
		require.NoError(t, h.FilesUploadFromURL(c))
		require.Equal(t, http.StatusNoContent, rec.Code)
	}
	upload()
	var ctx context.Context
	select {
	case ctx = <-rp.ctxs:
	case <-time.After(2 * time.Second):
		t.Fatal("Process was not invoked")
	}
	require.NoError(t, ctx.Err())
	require.Zero(t, g.Stop(context.Background()))
	assert.Error(t, ctx.Err(), "世代を止めても、URL からのアップロードの context が切れない")

	upload()
	select {
	case <-rp.ctxs:
		t.Fatal("世代を止めた後にアップロードを始めた")
	case <-time.After(100 * time.Millisecond):
	}
	assert.EqualValues(t, 1, g.Dropped())
}

// eofHookBody calls onEOF when the body has been read to the end.
type eofHookBody struct {
	io.ReadCloser
	onEOF func()
}

func (b *eofHookBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF && b.onEOF != nil {
		b.onEOF()
		b.onEOF = nil
	}
	return n, err
}

// cancelAfterBodyTransport cancels the generation context once the response
// body has been read, as if the generation stop began between the download
// and the save.
type cancelAfterBodyTransport struct{ cancel context.CancelFunc }

func (tr cancelAfterBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	res.Body = &eofHookBody{ReadCloser: res.Body, onEOF: tr.cancel}
	return res, nil
}

// cancelingUploader cancels the generation context in the middle of the save
// and records whether the save saw the cancellation.
type cancelingUploader struct {
	cancel   context.CancelFunc
	calls    int
	ctxErrIn error
}

func (u *cancelingUploader) Upload(ctx context.Context, _ coredrive.UploadInput) (*model.DriveFile, error) {
	u.calls++
	u.cancel()
	u.ctxErrIn = ctx.Err()
	return &model.DriveFile{ID: "f1", Name: "a.png", Type: "image/png"}, nil
}

// TestURLUploader_GenerationStopAfterTheDownload checks the two sides of
// #3661 review M-3: a download that finishes after the generation stop
// began is not saved (and the user gets no urlUploadFinished), and a save
// that has started is not cut short by the stop (cutting it would skip the
// thumbnails and let the sensitive check fail open).
func TestURLUploader_GenerationStopAfterTheDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("file-bytes"))
	}))
	defer srv.Close()
	idGen, _ := id.NewGenerator("aidx")
	in := URLUploadInput{User: &model.User{ID: "u1"}, URL: srv.URL + "/a.png"}

	t.Run("stopped before the save", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		up := &mockDriveUploader{ret: &model.DriveFile{ID: "f1"}}
		pub := &mockMainPublisher{}
		client := &http.Client{Transport: cancelAfterBodyTransport{cancel: cancel}}
		NewURLUploader(client, up, pub, idGen, "mk-go-test", 0, nil).Process(ctx, in)
		assert.Nil(t, up.gotInput, "世代が止まり始めた後に取得したファイルを保存した")
		assert.Empty(t, pub.events, "保存していないのに urlUploadFinished を送った")
	})
	t.Run("stopped during the save", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		up := &cancelingUploader{cancel: cancel}
		pub := &mockMainPublisher{}
		NewURLUploader(http.DefaultClient, up, pub, idGen, "mk-go-test", 0, nil).Process(ctx, in)
		require.Equal(t, 1, up.calls)
		assert.NoError(t, up.ctxErrIn, "保存の途中で世代の停止の取り消しが届いた (劣化したファイルが確定しうる)")
		assert.Len(t, pub.events, 1)
	})
}
