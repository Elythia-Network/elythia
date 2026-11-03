package drive

import (
	"bytes"
	"log/slog"
	"runtime/debug"
)

// pendingPut is one Storage.Put running on its own goroutine (#3996).
//
// Storage.Put は context を取らないので、走り出した書き込みは途中で止められない。
// 呼び出し元は必ず wait まで待つ。待たずに戻ると、書き込みの成否を知らないまま
// 後始末を判断することになり、孤立したオブジェクトを残しうる。
type pendingPut struct {
	key      string
	done     chan struct{}
	url      string
	err      error
	panicked bool
	panicVal any
}

// startPut starts backend.Put(key, data) in the background. onFail, when not
// nil, runs on the write's goroutine as soon as the write fails or panics.
func startPut(backend Storage, key string, data []byte, onFail func()) *pendingPut {
	p := &pendingPut{key: key, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer func() {
			// 別の goroutine で panic すると recover できずにプロセスごと落ちる。
			// 逐次に書いていた頃と同じく、要求の goroutine で panic させ直す
			// (waitStoredObjects を参照)。起こし直した panic のスタックは
			// 呼び出し元のものになるので、元のスタックはここで記録しておく。
			if r := recover(); r != nil {
				p.panicked = true
				p.panicVal = r
				slog.Error("drive: ストレージへの書き込みでpanicしました", "accessKey", key, "panic", r, "stack", string(debug.Stack()))
			}
			if onFail != nil && p.failed() {
				onFail()
			}
		}()
		p.url, p.err = backend.Put(key, bytes.NewReader(data))
	}()
	return p
}

// finished reports whether the write has already completed.
func (p *pendingPut) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// failed reports whether a completed write failed or panicked.
// 呼べるのは done が閉じた後だけ (finished か wait の後)。
func (p *pendingPut) failed() bool {
	return p.err != nil || p.panicked
}

// wait blocks until the write completes and returns its result.
// panic は返さない。呼び出し元が後始末を済ませてから panic させ直す。
func (p *pendingPut) wait() (string, error) {
	<-p.done
	return p.url, p.err
}

// storedObjects is the outcome of the writes made by Upload.
// 代替画像のキーが空なら、その代替画像は保存されていない。
type storedObjects struct {
	url          string
	thumbnailKey string
	thumbnailURL string
	webpublicKey string
	webpublicURL string
}

// waitStoredObjects writes the thumbnail and webpublic alongside the original
// write (already started as original, nil when the original was pre-stored)
// and waits for all of them.
//
// The failure handling matches the earlier sequential code:
//   - an alternate image that fails to get a key or to be written is skipped
//     and the upload continues without it;
//   - when the original write fails, the error is returned and no alternate
//     image is left behind: writes are not started if the original has already
//     failed, and alternates written in the meantime are deleted;
//   - a panic inside Storage.Put is re-raised on the caller's goroutine.
//
// 逐次だった頃は、原本の書き込みが失敗すると代替画像を作る前に戻っていたので、
// 代替画像のオブジェクトが残ることは無かった。並行にした後もそれを保つため、
// 原本が失敗したら書けた代替画像を消してから戻る。
// 既知の限界: その Delete まで失敗すると、代替画像は行の無いオブジェクトとして残る。
func waitStoredObjects(backend Storage, original *pendingPut, thumbnail, webpublic *ProcessedImage) (storedObjects, error) {
	var out storedObjects

	// 原本が既に失敗していれば、代替画像を書き始めない。
	originalFailed := original != nil && original.finished() && original.failed()

	var thumbPut, webPut *pendingPut
	if !originalFailed {
		if thumbnail != nil {
			if key, err := newAccessKey(); err == nil {
				thumbPut = startPut(backend, key, thumbnail.Data, nil)
			}
		}
		if webpublic != nil {
			if key, err := newAccessKey(); err == nil {
				webPut = startPut(backend, key, webpublic.Data, nil)
			}
		}
	}

	// 3 つとも待ってから判断する。どれかを待たずに戻ると、その書き込みが
	// 後から成功しても誰も消さない。
	var originalErr error
	var written []string // 後始末で消す候補 (書けたもの)
	var panicked *pendingPut
	if original != nil {
		out.url, originalErr = original.wait()
		if original.panicked {
			panicked = original
		} else if originalErr == nil {
			written = append(written, original.key)
		}
	}
	if thumbPut != nil {
		if u, err := thumbPut.wait(); err == nil && !thumbPut.panicked {
			out.thumbnailKey, out.thumbnailURL = thumbPut.key, u
			written = append(written, thumbPut.key)
		} else if thumbPut.panicked && panicked == nil {
			panicked = thumbPut
		}
	}
	if webPut != nil {
		if u, err := webPut.wait(); err == nil && !webPut.panicked {
			out.webpublicKey, out.webpublicURL = webPut.key, u
			written = append(written, webPut.key)
		} else if webPut.panicked && panicked == nil {
			panicked = webPut
		}
	}

	if originalErr == nil && panicked == nil {
		return out, nil
	}
	// 原本の失敗では、原本は書けていないので written は代替画像だけになる。
	// panic のときは、書けた原本も行が作られないまま残るので一緒に消す。
	for _, key := range written {
		if err := backend.Delete(key); err != nil {
			slog.Warn("drive: アップロードの失敗の後、保存したオブジェクトを消せませんでした", "accessKey", key, "error", err)
		}
	}
	if panicked != nil {
		panic(panicked.panicVal)
	}
	return storedObjects{}, originalErr
}
