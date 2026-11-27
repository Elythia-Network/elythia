package fetchrss

// The date formats below are gofeed v1.3.0's (internal/shared/dateparser.go,
// taken there from github.com/mjibson/goread), so isoDate stays as it was.
// gofeed is under the MIT License; see the notice in feedxml.go. goread's
// notice follows.
//
// ---
//
// ISC License
//
// Copyright (c) 2013 Matt Jibson <matt.jibson@gmail.com>
//
// Permission to use, copy, modify, and distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
// OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

import (
	"errors"
	"strings"
	"time"
)

var dateFormats = []string{
	time.RFC822,  // RSS
	time.RFC822Z, // RSS
	time.RFC3339, // Atom
	time.UnixDate,
	time.RubyDate,
	time.RFC850,
	time.RFC1123Z,
	time.RFC1123,
	time.ANSIC,
	"Mon, January 2 2006 15:04:05 -0700",
	"Mon, Jan 2 2006 15:04:05 -700",
	"Mon, Jan 2 2006 15:04:05 -0700",
	"Mon Jan 2 15:04 2006",
	"Mon Jan 02, 2006 3:04 pm",
	"Mon Jan 02 2006 15:04:05 -0700",
	"Mon Jan 02 2006 15:04:05 GMT-0700 (MST)",
	"Monday, January 2, 2006 03:04 PM",
	"Monday, January 2, 2006",
	"Monday, January 02, 2006",
	"Monday, 2 January 2006 15:04:05 -0700",
	"Monday, 2 Jan 2006 15:04:05 -0700",
	"Monday, 02 January 2006 15:04:05 -0700",
	"Monday, 02 January 2006 15:04:05",
	"Mon, 2 January 2006, 15:04 -0700",
	"Mon, 2 January 2006 15:04:05 -0700",
	"Mon, 2 January 2006",
	"Mon, 2 Jan 2006 3:04:05 PM -0700",
	"Mon, 2 Jan 2006 15:4:5 -0700 GMT",
	"Mon, 2, Jan 2006 15:4",
	"Mon, 2 Jan 2006, 15:04 -0700",
	"Mon, 2 Jan 2006 15:04 -0700",
	"Mon, 2 Jan 2006 15:04:05 UT",
	"Mon, 2 Jan 2006 15:04:05 -0700 MST",
	"Mon, 2 Jan 2006 15:04:05-0700",
	"Mon, 2 Jan 2006 15:04:05-07:00",
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05",
	"Mon, 2 Jan 2006 15:04",
	"Mon,2 Jan 2006",
	"Mon, 2 Jan 2006",
	"Mon, 2 Jan 06 15:04:05 -0700",
	"Mon, 2006-01-02 15:04",
	"Mon, 02 January 2006",
	"Mon, 02 Jan 2006 15 -0700",
	"Mon, 02 Jan 2006 15:04 -0700",
	"Mon, 02 Jan 2006 15:04:05 Z",
	"Mon, 02 Jan 2006 15:04:05 UT",
	"Mon, 02 Jan 2006 15:04:05 MST-07:00",
	"Mon, 02 Jan 2006 15:04:05 MST -0700",
	"Mon, 02 Jan 2006 15:04:05 GMT-0700",
	"Mon,02 Jan 2006 15:04:05 -0700",
	"Mon, 02 Jan 2006 15:04:05 -0700",
	"Mon, 02 Jan 2006 15:04:05 -07:00",
	"Mon, 02 Jan 2006 15:04:05 --0700",
	"Mon 02 Jan 2006 15:04:05 -0700",
	"Mon, 02 Jan 2006 15:04:05 -07",
	"Mon, 02 Jan 2006 15:04:05 00",
	"Mon, 02 Jan 2006 15:04:05",
	"Mon, 02 Jan 2006 15:4:5 Z",
	"Mon, 02 Jan 2006",
	"January 2, 2006 3:04 PM",
	"January 2, 2006, 3:04 p.m.",
	"January 2, 2006 15:04:05",
	"January 2, 2006 03:04 PM",
	"January 2, 2006",
	"January 02, 2006 15:04",
	"January 02, 2006 03:04 PM",
	"January 02, 2006",
	"Jan 2, 2006 3:04:05 PM",
	"Jan 2, 2006",
	"Jan 02 2006 03:04:05PM",
	"Jan 02, 2006",
	"6/1/2 15:04",
	"6-1-2 15:04",
	"2 January 2006 15:04:05 -0700",
	"2 January 2006",
	"2 Jan 2006 15:04:05 Z",
	"2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006",
	"2.1.2006 15:04:05",
	"2/1/2006",
	"2-1-2006",
	"2006 January 02",
	"2006-1-2T15:04:05Z",
	"2006-1-2 15:04:05",
	"2006-1-2",
	"2006-1-02T15:04:05Z",
	"2006-01-02T15:04Z",
	"2006-01-02T15:04-07:00",
	"2006-01-02T15:04:05Z",
	"2006-01-02T15:04:05-07:00:00",
	"2006-01-02T15:04:05:-0700",
	"2006-01-02T15:04:05-0700",
	"2006-01-02T15:04:05-07:00",
	"2006-01-02T15:04:05 -0700",
	"2006-01-02T15:04:05:00",
	"2006-01-02T15:04:05",
	"2006-01-02 at 15:04:05",
	"2006-01-02 15:04:05Z",
	"2006-01-02 15:04:05-0700",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04",
	"2006-01-02 00:00:00.0 15:04:05.0 -0700",
	"2006/01/02",
	"2006-01-02",
	"15:04 02.01.2006 -0700",
	"1/2/2006 3:04:05 PM",
	"1/2/2006",
	"06/1/2 15:04",
	"06-1-2 15:04",
	"02 Monday, Jan 2006 15:04",
	"02 Jan 2006 15:04:05 UT",
	"02 Jan 2006 15:04:05 -0700",
	"02 Jan 2006 15:04:05",
	"02 Jan 2006",
	"02.01.2006 15:04:05",
	"02/01/2006 15:04:05",
	"02.01.2006 15:04",
	"02/01/2006 - 15:04",
	"02.01.2006 -0700",
	"02/01/2006",
	"02-01-2006",
	"01/02/2006 3:04 PM",
	"01/02/2006 - 15:04",
	"01/02/2006",
	"01-02-2006",
}

// dateFormatsWithNamedZone name the zone, which is loaded separately.
var dateFormatsWithNamedZone = []string{
	"Mon, January 02, 2006, 15:04:05 MST",
	"Mon, January 02, 2006 15:04:05 MST",
	"Mon, Jan 2, 2006 15:04 MST",
	"Mon, Jan 2 2006 15:04 MST",
	"Mon, Jan 2, 2006 15:04:05 MST",
	"Mon Jan 2 15:04:05 2006 MST",
	"Mon, Jan 02,2006 15:04:05 MST",
	"Monday, January 2, 2006 15:04:05 MST",
	"Monday, 2 January 2006 15:04:05 MST",
	"Monday, 2 Jan 2006 15:04:05 MST",
	"Monday, 02 January 2006 15:04:05 MST",
	"Mon, 2 January 2006 15:04 MST",
	"Mon, 2 January 2006, 15:04:05 MST",
	"Mon, 2 January 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:4:5 MST",
	"Mon, 2 Jan 2006 15:04 MST",
	"Mon, 2 Jan 2006 15:04:05MST",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon 2 Jan 2006 15:04:05 MST",
	"mon,2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 15:04:05 MST",
	"Mon, 2 Jan 06 15:04:05 MST",
	"Mon,02 January 2006 14:04:05 MST",
	"Mon, 02 Jan 2006 3:04:05 PM MST",
	"Mon,02 Jan 2006 15:04 MST",
	"Mon, 02 Jan 2006 15:04 MST",
	"Mon, 02 Jan 2006, 15:04:05 MST",
	"Mon, 02 Jan 2006 15:04:05MST",
	"Mon, 02 Jan 2006 15:04:05 MST",
	"Mon , 02 Jan 2006 15:04:05 MST",
	"Mon, 02 Jan 06 15:04:05 MST",
	"January 2, 2006 15:04:05 MST",
	"January 02, 2006 15:04:05 MST",
	"Jan 2, 2006 3:04:05 PM MST",
	"Jan 2, 2006 15:04:05 MST",
	"2 January 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 MST",
	"2006-01-02 15:04:05 MST",
	"1/2/2006 3:04:05 PM MST",
	"1/2/2006 15:04:05 MST",
	"02 Jan 2006 15:04 MST",
	"02 Jan 2006 15:04:05 MST",
	"02/01/2006 15:04 MST",
	"02-01-2006 15:04:05 MST",
	"01/02/2006 15:04:05 MST",
}

// dateReader parses the dates of one feed and stops it at its deadline.
//
// 時間帯の名前ごとの time.LoadLocation の結果は、1 つのフィードを読む間だけ覚える
// (プロセスで共有する表は持たない)。名前はフィードが決めるので、共有すると、
// 1 つのフィードが表を埋めたり、一時的な失敗を後のフィードにまで残したりできる。
type dateReader struct {
	deadline  time.Time // ゼロなら打ち切らない
	locations map[string]locationResult
}

type locationResult struct {
	loc *time.Location
	err error
}

func newDateReader(deadline time.Time) *dateReader {
	return &dateReader{deadline: deadline}
}

// expired reports whether the deadline has passed.
//
// 日付を読む単位 (項目ごと) で確かめる。
func (r *dateReader) expired() bool {
	return !r.deadline.IsZero() && time.Now().After(r.deadline)
}

// parse parses a feed date with gofeed's list of formats.
func (r *dateReader) parse(ds string) (time.Time, error) {
	d := strings.TrimSpace(ds)
	if d == "" {
		return time.Time{}, errors.New("date string is empty")
	}
	for _, f := range dateFormats {
		if t, err := time.Parse(f, d); err == nil {
			return t, nil
		}
	}
	for _, f := range dateFormatsWithNamedZone {
		t, err := time.Parse(f, d)
		if err != nil {
			continue
		}
		loc, err := r.loadLocation(t.Location().String())
		if err != nil {
			return t, nil
		}
		// gofeed と同じく、ここは前後の空白を落とす前の文字列で読み直す。
		if t, err := time.ParseInLocation(f, ds, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("failed to parse date")
}

// loadLocation is time.LoadLocation with the result remembered per name while
// reading one feed. time.LoadLocation は名前ごとに zoneinfo をディスクから探すので、
// 日付のたびに呼ぶと重い。
func (r *dateReader) loadLocation(name string) (*time.Location, error) {
	if l, ok := r.locations[name]; ok {
		return l.loc, l.err
	}
	loc, err := time.LoadLocation(name)
	if r.locations == nil {
		r.locations = map[string]locationResult{}
	}
	r.locations[name] = locationResult{loc: loc, err: err}
	return loc, err
}
