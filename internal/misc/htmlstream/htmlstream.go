// Package htmlstream reads HTML that comes from outside (remote notes, fetched
// pages) as a stream of element, text and comment events, without building a
// document tree.
//
// The events nest the way the elements would nest in the tree that
// golang.org/x/net/html's Parse builds for the same input, for the HTML that
// is found in practice: implied end tags (p, li, dd, dt, table parts, ...),
// end tags that do not match, void elements, raw text elements, the head, the
// active formatting elements (b, i, a, ... carried over to the next block) and
// SVG / MathML. It does not move nodes that the tree construction moves to
// another place (text and elements directly inside a table that are put before
// the table, and the block elements the adoption agency re-parents out of a
// misnested formatting element), reads the content of template with the body
// rules, and skips the rest of a frameset document.
//
// Time and memory are proportional to the length of the input: the work per
// token has an upper bound (closing an element counts toward the token that
// opened it), and the attributes that reopened formatting elements carry add
// up to no more than the input read so far. Memory is the open elements, the
// current token and the white space held until the body starts.
//
// The insertion modes follow golang.org/x/net/html v0.62.0 (parse.go,
// foreign.go, const.go), whose license is below.
//
// ---
//
// Copyright 2009 The Go Authors.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//   - Redistributions of source code must retain the above copyright
//     notice, this list of conditions and the following disclaimer.
//   - Redistributions in binary form must reproduce the above
//     copyright notice, this list of conditions and the following disclaimer
//     in the documentation and/or other materials provided with the
//     distribution.
//   - Neither the name of Google LLC nor the names of its
//     contributors may be used to endorse or promote products derived from
//     this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
package htmlstream

import (
	"errors"
	"io"
	"slices"
	"strings"

	"golang.org/x/net/html"
	a "golang.org/x/net/html/atom"
)

// Kind is the kind of an event.
type Kind uint8

// The kinds of events. EOF is returned once the input ends (or on an error,
// see Scanner.Err) and on every call after that.
const (
	EOF Kind = iota
	StartElement
	EndElement
	Text
	Comment
)

// ErrTooDeep is returned by Scanner.Err when the elements nest deeper than
// x/net/html's Parse accepts (it returns an error for such input).
var ErrTooDeep = errors.New("htmlstream: elements nested too deeply")

// Element is an element of the document. The same *Element is passed to the
// StartElement and the EndElement event of the element; a reopened formatting
// element is a new *Element with the same name and attributes.
type Element struct {
	// Atom is the atom of Name, or 0 when the name has no atom.
	Atom a.Atom
	// Name is the lower-cased tag name (SVG names keep their case, such as
	// "foreignObject").
	Name string
	// Namespace is "" for HTML elements, "svg" or "math" for foreign ones.
	Namespace string
	// Attr holds the attributes; the tokenizer has dropped repeated keys.
	// Callers must not modify it.
	Attr []html.Attribute
	// InHead is set for the elements put in the head (the ones before the body
	// starts, such as title, meta and link at the top of a page).
	InHead bool
}

// AttrVal returns the value of the first attribute whose key is key, in any
// namespace, and whether there was one.
func (e *Element) AttrVal(key string) (string, bool) {
	for _, at := range e.Attr {
		if at.Key == key {
			return at.Val, true
		}
	}
	return "", false
}

// HasAttr reports whether e has an attribute whose key is key.
func (e *Element) HasAttr(key string) bool {
	_, ok := e.AttrVal(key)
	return ok
}

const (
	// maxContentDepth is how many elements may be open at once. x/net/html
	// counts html and body (or head) too and fails above 512.
	maxContentDepth = 510
	// maxScopeSearch is how many open elements a search on the stack (for the
	// element an end tag closes, "is there a p to close", the table an
	// insertion mode is reset to, ...) looks at, from the current one down. An
	// element below that is treated as not open.
	maxScopeSearch = 64
	// maxActiveFormatting is how many active formatting elements are kept
	// after the last marker; the oldest one is dropped beyond it.
	maxActiveFormatting = 32
	// maxReconstructed is how many formatting elements a document may reopen
	// in total; later ones are not reopened.
	maxReconstructed = 4096
)

type mode uint8

const (
	initialIM mode = iota
	beforeHTMLIM
	beforeHeadIM
	inHeadIM
	afterHeadIM
	inBodyIM
	textIM
	inTableIM
	inCaptionIM
	inColumnGroupIM
	inTableBodyIM
	inRowIM
	inCellIM
	afterBodyIM
	afterAfterBodyIM
	ignoreAllIM
)

type frame struct {
	el      *Element
	special bool
	// removed marks an element taken off the stack while the elements opened
	// inside it are still open. It is skipped by every search and popped
	// together with them.
	removed  bool
	onStack  bool
	inAFE    bool
	hasChild bool
	// attrKey is the sorted attributes of a formatting element, used to find
	// identical ones.
	attrKey []html.Attribute
	// cost is reopenCost of a formatting element, counted once.
	cost int
	// htmlIP is htmlIntegrationPoint(el), found when the element opens.
	htmlIP bool
}

func (f *frame) live() bool { return f.onStack && !f.removed }

type event struct {
	kind Kind
	el   *Element
	text string
}

// Scanner reads events from HTML.
type Scanner struct {
	z    *html.Tokenizer
	tok  html.Token
	self bool

	oe    []*frame
	depth int
	// afe is the list of active formatting elements; nil entries are markers.
	afe []*frame

	im, originalIM mode
	quirks         bool
	head           bool // the head has been closed (elements go to the body)
	inHead         bool // new elements go in the head
	// betweenHeadAndBody is white space read after the head was closed, put out
	// when the body starts.
	betweenHeadAndBody strings.Builder
	form               *frame
	templates          int
	reconstructed      int
	// read is the bytes of the input read so far, and reopened the attributes
	// the reopened formatting elements carry (reopenCost); reopened stays at
	// most read.
	read, reopened int
	// attrsRead counts the attributes the scanner itself reads, and
	// attrsInTokens and attrBytesInTokens the attributes of the start tags
	// read (for tests).
	attrsRead, attrsInTokens, attrBytesInTokens int
	// reprocessLimit+1 is how many more times one token may be handled again
	// after its first handling; loopLimited counts the tokens dropped there
	// (for tests; it should stay 0).
	reprocessLimit, loopLimited int
	// approximated counts the places where the events differ from the tree
	// x/net/html builds (for tests).
	approximated int

	queue []event
	qpos  int
	cur   event
	err   error
	done  bool
}

// NewScanner returns a Scanner that reads HTML (UTF-8) from r.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{z: html.NewTokenizer(r), reprocessLimit: 16}
}

// Next advances to the next event and returns its kind.
func (s *Scanner) Next() Kind {
	for s.qpos >= len(s.queue) {
		s.queue = s.queue[:0]
		s.qpos = 0
		if s.done {
			s.cur = event{}
			return EOF
		}
		s.step()
	}
	s.cur = s.queue[s.qpos]
	s.queue[s.qpos] = event{}
	s.qpos++
	return s.cur.kind
}

// Element returns the element of the current StartElement or EndElement event.
func (s *Scanner) Element() *Element { return s.cur.el }

// Text returns the text of the current Text or Comment event.
func (s *Scanner) Text() string { return s.cur.text }

// Err returns the error that ended the events: ErrTooDeep, or an error from
// the reader other than io.EOF. It is nil when the input was read to the end.
// x/net/html's Parse returns an error in the same cases, so a caller that
// treated a Parse error as "no result" should do the same for Err.
func (s *Scanner) Err() error { return s.err }

func (s *Scanner) emit(k Kind, el *Element, text string) {
	if s.done {
		// fail の後に同じトークンの処理が積もうとした事象も捨てる。
		return
	}
	s.queue = append(s.queue, event{kind: k, el: el, text: text})
}

// step reads one token and queues its events.
func (s *Scanner) step() {
	top := s.top()
	s.z.AllowCDATA(top != nil && top.el.Namespace != "")
	s.z.Next()
	s.read += len(s.z.Raw())
	s.tok = s.z.Token()
	s.attrsInTokens += len(s.tok.Attr)
	for _, at := range s.tok.Attr {
		s.attrBytesInTokens += len(at.Namespace) + len(at.Key) + len(at.Val)
	}
	if s.tok.Type == html.ErrorToken {
		if err := s.z.Err(); err != nil && err != io.EOF {
			s.fail(err)
			return
		}
	}
	s.self = false
	if s.tok.Type == html.SelfClosingTagToken {
		s.self = true
		s.tok.Type = html.StartTagToken
	}
	s.parseCurrentToken()
	if s.tok.Type == html.ErrorToken && !s.done {
		s.finish()
	}
}

func (s *Scanner) parseCurrentToken() {
	for i := 0; ; i++ {
		if s.done {
			return
		}
		var consumed bool
		if s.inForeignContent() {
			consumed = s.parseForeignContent()
		} else {
			consumed = s.run(s.im)
		}
		if consumed {
			return
		}
		// 一つのトークンを処理し直す回数は、挿入モードの遷移の数で決まり小さい。
		// 規則の取り違えで回り続けないよう、上限で打ち切る (テストで 0 を確かめる)。
		if i > s.reprocessLimit {
			s.loopLimited++
			return
		}
	}
}

// finish closes every open element at the end of the input.
func (s *Scanner) finish() {
	for len(s.oe) > 0 {
		s.popTop()
	}
	s.done = true
}

func (s *Scanner) fail(err error) {
	s.err = err
	s.done = true
	// 積んだまま渡していない事象は捨て、以後も積まない (emit)。それより前に渡した
	// 事象は取り消せないので、呼び出し元は Err が nil でなければ結果を使わない
	// (x/net/html の Parse もエラーのときは木を返さない)。
	s.queue = s.queue[:0]
	s.qpos = 0
}

func (s *Scanner) run(m mode) bool {
	switch m {
	case initialIM:
		return s.initialIM()
	case beforeHTMLIM:
		return s.beforeHTMLIM()
	case beforeHeadIM:
		return s.beforeHeadIM()
	case inHeadIM:
		return s.inHeadIM()
	case afterHeadIM:
		return s.afterHeadIM()
	case inBodyIM:
		return s.inBodyIM()
	case textIM:
		return s.textIM()
	case inTableIM:
		return s.inTableIM()
	case inCaptionIM:
		return s.inCaptionIM()
	case inColumnGroupIM:
		return s.inColumnGroupIM()
	case inTableBodyIM:
		return s.inTableBodyIM()
	case inRowIM:
		return s.inRowIM()
	case inCellIM:
		return s.inCellIM()
	case afterBodyIM:
		return s.afterBodyIM()
	case afterAfterBodyIM:
		return s.afterAfterBodyIM()
	}
	// ignoreAllIM: 残りのトークンを捨てる。
	return true
}

// --- the stack of open elements ---

func (s *Scanner) top() *frame {
	if len(s.oe) == 0 {
		return nil
	}
	return s.oe[len(s.oe)-1]
}

func (s *Scanner) topAtom() a.Atom {
	if t := s.top(); t != nil && t.el.Namespace == "" {
		return t.el.Atom
	}
	return 0
}

func (s *Scanner) push(el *Element) *frame {
	if s.depth+1 > maxContentDepth {
		s.fail(ErrTooDeep)
		return nil
	}
	if t := s.top(); t != nil {
		t.hasChild = true
	}
	el.InHead = s.inHead
	f := &frame{el: el, special: isSpecial(el), onStack: true, htmlIP: s.htmlIntegrationPoint(el)}
	s.oe = append(s.oe, f)
	s.depth++
	if el.Atom == a.Template && el.Namespace == "" {
		s.templates++
	}
	s.emit(StartElement, el, "")
	return f
}

func (s *Scanner) popTop() {
	f := s.oe[len(s.oe)-1]
	s.oe[len(s.oe)-1] = nil
	s.oe = s.oe[:len(s.oe)-1]
	f.onStack = false
	if !f.removed {
		s.depth--
		if f.el.Atom == a.Template && f.el.Namespace == "" {
			s.templates--
		}
	}
	s.emit(EndElement, f.el, "")
	// 途中から外した要素が一番上に来たら、続けて閉じる。
	for len(s.oe) > 0 && s.oe[len(s.oe)-1].removed {
		s.popTop()
	}
}

// truncate pops elements until the stack has i elements.
func (s *Scanner) truncate(i int) {
	for len(s.oe) > i {
		s.popTop()
	}
}

// remove takes f off the stack. When elements opened inside f are still open,
// f stays in place (marked) and is closed together with them.
func (s *Scanner) remove(f *frame) {
	if !f.onStack || f.removed {
		return
	}
	if s.top() == f {
		s.popTop()
		return
	}
	f.removed = true
	s.depth--
	if f.el.Atom == a.Template && f.el.Namespace == "" {
		s.templates--
	}
}

func (s *Scanner) newElement() *Element {
	return &Element{Atom: s.tok.DataAtom, Name: s.tok.Data, Attr: s.tok.Attr}
}

func (s *Scanner) addElement() *frame {
	return s.push(s.newElement())
}

// addVoid adds an element that has no content.
func (s *Scanner) addVoid() {
	if s.addElement() != nil {
		s.popTop()
	}
}

func (s *Scanner) addText(text string) {
	if text == "" {
		return
	}
	if t := s.top(); t != nil {
		t.hasChild = true
	}
	s.emit(Text, nil, text)
}

func (s *Scanner) addComment() {
	if t := s.top(); t != nil {
		t.hasChild = true
	}
	s.emit(Comment, nil, s.tok.Data)
}

// bodyInScope reports whether the body is in the default scope (no table,
// cell, ... is open).
func (s *Scanner) bodyInScope() bool {
	n := 0
	for i := len(s.oe) - 1; i >= 0; i-- {
		f := s.oe[i]
		if n++; n > maxScopeSearch {
			return false
		}
		if !f.removed && slices.Contains(defaultScopeStop[f.el.Namespace], f.el.Atom) {
			return false
		}
	}
	return true
}

// indexOf returns the index of f in s.oe, looking at most at maxScopeSearch
// open elements, or -1.
func (s *Scanner) indexOf(f *frame) int {
	n := 0
	for i := len(s.oe) - 1; i >= 0 && n < maxScopeSearch; i-- {
		n++
		if s.oe[i] == f {
			return i
		}
	}
	return -1
}

// contains reports whether an element with atom t is open (HTML namespace).
func (s *Scanner) contains(t a.Atom) bool {
	n := 0
	for i := len(s.oe) - 1; i >= 0 && n < maxScopeSearch; i-- {
		f := s.oe[i]
		n++
		if f.removed {
			continue
		}
		if f.el.Atom == t && f.el.Namespace == "" {
			return true
		}
	}
	return false
}

type scope uint8

const (
	defaultScope scope = iota
	listItemScope
	buttonScope
	tableScope
)

var defaultScopeStop = map[string][]a.Atom{
	"":     {a.Applet, a.Caption, a.Html, a.Table, a.Td, a.Th, a.Marquee, a.Object, a.Template, a.Select},
	"math": {a.AnnotationXml, a.Mi, a.Mn, a.Mo, a.Ms, a.Mtext},
	"svg":  {a.Desc, a.ForeignObject, a.Title},
}

// indexInScope returns the index in s.oe of the highest element whose atom is
// one of match and that is in scope, or -1.
func (s *Scanner) indexInScope(sc scope, match ...a.Atom) int {
	n := 0
	for i := len(s.oe) - 1; i >= 0 && n < maxScopeSearch; i-- {
		f := s.oe[i]
		n++
		if f.removed {
			continue
		}
		t := f.el.Atom
		if f.el.Namespace == "" {
			if slices.Contains(match, t) {
				return i
			}
			switch sc {
			case listItemScope:
				if t == a.Ol || t == a.Ul {
					return -1
				}
			case buttonScope:
				if t == a.Button {
					return -1
				}
			case tableScope:
				if t == a.Table || t == a.Template {
					return -1
				}
			}
		}
		if sc != tableScope && slices.Contains(defaultScopeStop[f.el.Namespace], t) {
			return -1
		}
	}
	return -1
}

func (s *Scanner) elementInScope(sc scope, match ...a.Atom) bool {
	return s.indexInScope(sc, match...) != -1
}

func (s *Scanner) popUntil(sc scope, match ...a.Atom) bool {
	if i := s.indexInScope(sc, match...); i != -1 {
		s.truncate(i)
		return true
	}
	return false
}

// clearStackToContext pops elements until one of stop (or the bottom).
func (s *Scanner) clearStackToContext(stop ...a.Atom) {
	for len(s.oe) > 0 {
		t := s.top()
		if t.el.Namespace == "" && (slices.Contains(stop, t.el.Atom) || t.el.Atom == a.Template) {
			return
		}
		s.popTop()
	}
}

func (s *Scanner) generateImpliedEndTags(except ...string) {
	for len(s.oe) > 0 {
		t := s.top()
		switch t.el.Atom {
		case a.Dd, a.Dt, a.Li, a.Optgroup, a.Option, a.P, a.Rb, a.Rp, a.Rt, a.Rtc:
			if t.el.Namespace == "" && !slices.Contains(except, t.el.Name) {
				s.popTop()
				continue
			}
		}
		return
	}
}

// --- the list of active formatting elements ---

// segment returns the index of the first entry after the last marker.
func (s *Scanner) segment() int {
	for i := len(s.afe) - 1; i >= 0; i-- {
		if s.afe[i] == nil {
			return i + 1
		}
	}
	return 0
}

func (s *Scanner) afeIndex(f *frame) int {
	for i := len(s.afe) - 1; i >= 0 && s.afe[i] != nil; i-- {
		if s.afe[i] == f {
			return i
		}
	}
	return -1
}

func (s *Scanner) afeRemove(f *frame) {
	if i := s.afeIndex(f); i != -1 {
		s.afe = slices.Delete(s.afe, i, i+1)
		f.inAFE = false
	}
}

func (s *Scanner) pushMarker() { s.afe = append(s.afe, nil) }

func (s *Scanner) clearActiveFormattingElements() {
	for len(s.afe) > 0 {
		f := s.afe[len(s.afe)-1]
		s.afe = s.afe[:len(s.afe)-1]
		if f == nil {
			return
		}
		f.inAFE = false
	}
}

func (s *Scanner) addFormattingElement() {
	f := s.addElement()
	if f == nil {
		return
	}
	key := slices.Clone(f.el.Attr)
	slices.SortFunc(key, func(x, y html.Attribute) int {
		if c := strings.Compare(x.Namespace, y.Namespace); c != 0 {
			return c
		}
		if c := strings.Compare(x.Key, y.Key); c != 0 {
			return c
		}
		return strings.Compare(x.Val, y.Val)
	})
	f.attrKey = key
	f.cost = s.reopenCost(f.el)
	start := s.segment()
	identical := 0
	for i := len(s.afe) - 1; i >= start; i-- {
		n := s.afe[i]
		if n.el.Atom != f.el.Atom || n.el.Namespace != "" || !slices.Equal(n.attrKey, key) {
			continue
		}
		identical++
		if identical >= 3 {
			s.afe = slices.Delete(s.afe, i, i+1)
			n.inAFE = false
		}
	}
	if len(s.afe)-start >= maxActiveFormatting {
		s.afe[start].inAFE = false
		s.afe = slices.Delete(s.afe, start, start+1)
	}
	f.inAFE = true
	s.afe = append(s.afe, f)
}

func (s *Scanner) reconstructActiveFormattingElements() {
	if len(s.afe) == 0 {
		return
	}
	n := s.afe[len(s.afe)-1]
	if n == nil || n.live() {
		return
	}
	i := len(s.afe) - 1
	for n != nil && !n.live() {
		if i == 0 {
			i = -1
			break
		}
		i--
		n = s.afe[i]
	}
	for i+1 < len(s.afe) {
		i++
		old := s.afe[i]
		if s.reconstructed >= maxReconstructed || s.reopened+old.cost > s.read {
			return
		}
		s.reconstructed++
		s.reopened += old.cost
		el := &Element{Atom: old.el.Atom, Name: old.el.Name, Attr: old.el.Attr}
		f := s.push(el)
		if f == nil {
			return
		}
		f.attrKey = old.attrKey
		f.cost = old.cost
		f.inAFE = true
		old.inAFE = false
		s.afe[i] = f
	}
}

// reopenCost is the size of e's attributes, counted toward the input read so
// far when e is reopened.
func (s *Scanner) reopenCost(e *Element) int {
	s.attrsRead += len(e.Attr)
	n := 0
	for _, at := range e.Attr {
		n += len(at.Namespace) + len(at.Key) + len(at.Val)
	}
	return n
}

// inBodyEndTagFormatting handles the end tag of a formatting element. It
// closes the element and what is open inside it, and leaves the formatting
// elements inside it to be reopened. (The adoption agency would also move the
// block elements opened inside it; that is not done.)
func (s *Scanner) inBodyEndTagFormatting(t a.Atom, name string) {
	if top := s.top(); top != nil && top.el.Name == name && top.el.Namespace == "" && !top.inAFE {
		s.popTop()
		return
	}
	var fe *frame
	for j := len(s.afe) - 1; j >= 0 && s.afe[j] != nil; j-- {
		if s.afe[j].el.Atom == t {
			fe = s.afe[j]
			break
		}
	}
	if fe == nil {
		s.inBodyEndTagOther(t, name)
		return
	}
	if !fe.live() {
		s.afeRemove(fe)
		return
	}
	if !s.elementInScope(defaultScope, t) {
		return
	}
	i := s.indexOf(fe)
	if i < 0 {
		return
	}
	for _, f := range s.oe[i+1:] {
		if f.special && !f.removed {
			// ブロックの要素が中で開いたまま閉じられた (adoption agency が組み替える形)。
			s.approximated++
			break
		}
	}
	s.truncate(i)
	s.afeRemove(fe)
}

// inBodyEndTagOther is the "any other end tag" rule.
func (s *Scanner) inBodyEndTagOther(t a.Atom, name string) {
	n := 0
	for i := len(s.oe) - 1; i >= 0 && n < maxScopeSearch; i-- {
		f := s.oe[i]
		n++
		if f.removed {
			continue
		}
		if f.el.Namespace == "" && f.el.Atom == t && (t != 0 || f.el.Name == name) {
			s.truncate(i)
			return
		}
		if f.special {
			return
		}
	}
}

func (s *Scanner) resetInsertionMode() {
	for i, n := len(s.oe)-1, 0; i >= 0 && n < maxScopeSearch; i, n = i-1, n+1 {
		f := s.oe[i]
		if f.removed {
			continue
		}
		if f.el.Namespace != "" {
			continue
		}
		switch f.el.Atom {
		case a.Td, a.Th:
			s.im = inCellIM
		case a.Tr:
			s.im = inRowIM
		case a.Tbody, a.Thead, a.Tfoot:
			s.im = inTableBodyIM
		case a.Caption:
			s.im = inCaptionIM
		case a.Colgroup:
			s.im = inColumnGroupIM
		case a.Table:
			s.im = inTableIM
		default:
			continue
		}
		return
	}
	s.im = inBodyIM
}

// parseImpliedToken handles a tag the input did not have, as if it had.
func (s *Scanner) parseImpliedToken(t html.TokenType, at a.Atom) {
	real, self := s.tok, s.self
	s.tok = html.Token{Type: t, DataAtom: at, Data: at.String()}
	s.self = false
	s.parseCurrentToken()
	s.tok, s.self = real, self
}

const whitespace = " \t\r\n\f"

// --- insertion modes before the body ---

func (s *Scanner) initialIM() bool {
	switch s.tok.Type {
	case html.TextToken:
		s.tok.Data = strings.TrimLeft(s.tok.Data, whitespace)
		if s.tok.Data == "" {
			return true
		}
	case html.CommentToken:
		return true
	case html.DoctypeToken:
		s.quirks = doctypeQuirks(s.tok.Data)
		s.im = beforeHTMLIM
		return true
	}
	s.quirks = true
	s.im = beforeHTMLIM
	return false
}

func (s *Scanner) beforeHTMLIM() bool {
	switch s.tok.Type {
	case html.DoctypeToken, html.CommentToken:
		return true
	case html.TextToken:
		s.tok.Data = strings.TrimLeft(s.tok.Data, whitespace)
		if s.tok.Data == "" {
			return true
		}
	case html.StartTagToken:
		if s.tok.DataAtom == a.Html {
			s.im = beforeHeadIM
			return true
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Head, a.Body, a.Html, a.Br:
		default:
			return true
		}
	}
	s.im = beforeHeadIM
	return false
}

func (s *Scanner) beforeHeadIM() bool {
	switch s.tok.Type {
	case html.TextToken:
		s.tok.Data = strings.TrimLeft(s.tok.Data, whitespace)
		if s.tok.Data == "" {
			return true
		}
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Head:
			s.inHead = true
			s.im = inHeadIM
			return true
		case a.Html:
			return true
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Head, a.Body, a.Html, a.Br:
		default:
			return true
		}
	case html.CommentToken:
		s.addComment()
		return true
	case html.DoctypeToken:
		return true
	}
	s.inHead = true
	s.im = inHeadIM
	return false
}

// headStartTag handles the start tags that the head rules take. It reports
// false for the other tags.
func (s *Scanner) headStartTag() (consumed, handled bool) {
	switch s.tok.DataAtom {
	case a.Base, a.Basefont, a.Bgsound, a.Link, a.Meta:
		s.addVoid()
		return true, true
	case a.Noscript, a.Script, a.Title, a.Noframes, a.Style:
		// x/net/html の Parse は scripting を有効にして読むので、noscript も中身を
		// テキストとして読む。
		s.addRawText()
		return true, true
	case a.Template:
		// x/net/html は template の中を専用の挿入モードで読むが、ここでは
		// 本文と同じ規則で読む。
		s.approximated++
		if s.addElement() != nil {
			s.pushMarker()
		}
		return true, true
	}
	return false, false
}

func (s *Scanner) addRawText() {
	if s.addElement() != nil {
		s.originalIM = s.im
		s.im = textIM
	}
}

func (s *Scanner) inHeadIM() bool {
	switch s.tok.Type {
	case html.TextToken:
		rest := strings.TrimLeft(s.tok.Data, whitespace)
		if len(rest) < len(s.tok.Data) {
			s.addText(s.tok.Data[:len(s.tok.Data)-len(rest)])
			if rest == "" {
				return true
			}
			s.tok.Data = rest
		}
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Html, a.Head:
			return true
		}
		if c, ok := s.headStartTag(); ok {
			return c
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Head:
			s.closeHead()
			return true
		case a.Body, a.Html, a.Br:
		case a.Template:
			s.endTemplate()
			return true
		default:
			return true
		}
	case html.CommentToken:
		s.addComment()
		return true
	case html.DoctypeToken:
		return true
	}
	s.closeHead()
	return false
}

func (s *Scanner) closeHead() {
	s.inHead = false
	s.head = true
	s.im = afterHeadIM
}

func (s *Scanner) endTemplate() {
	if !s.contains(a.Template) {
		return
	}
	s.generateImpliedEndTags()
	for i := len(s.oe) - 1; i >= 0; i-- {
		if f := s.oe[i]; !f.removed && f.el.Namespace == "" && f.el.Atom == a.Template {
			s.truncate(i)
			break
		}
	}
	s.clearActiveFormattingElements()
	s.resetInsertionMode()
	if !s.head {
		s.im = inHeadIM
	}
}

func (s *Scanner) afterHeadIM() bool {
	switch s.tok.Type {
	case html.TextToken:
		rest := strings.TrimLeft(s.tok.Data, whitespace)
		if len(rest) < len(s.tok.Data) {
			// head と body の間の空白は、後から head に入る要素より後ろに並ぶ。
			s.betweenHeadAndBody.WriteString(s.tok.Data[:len(s.tok.Data)-len(rest)])
			if rest == "" {
				return true
			}
			s.tok.Data = rest
		}
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Html, a.Head:
			return true
		case a.Body:
			s.startBody()
			return true
		case a.Frameset:
			// frameset の文書は本文を持たない。x/net/html は frameset の要素を作るが、
			// ここでは残りを読み捨てる。
			s.approximated++
			s.im = ignoreAllIM
			return true
		case a.Base, a.Basefont, a.Bgsound, a.Link, a.Meta, a.Noframes, a.Script, a.Style, a.Template, a.Title:
			// head を閉じた後でも、これらは head に入る。
			s.inHead = true
			c, _ := s.headStartTag()
			s.inHead = false
			return c
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Body, a.Html, a.Br:
		case a.Template:
			s.endTemplate()
			return true
		default:
			return true
		}
	case html.CommentToken:
		s.addComment()
		return true
	case html.DoctypeToken:
		return true
	}
	s.startBody()
	return s.tok.Type == html.ErrorToken
}

func (s *Scanner) startBody() {
	s.addText(s.betweenHeadAndBody.String())
	s.betweenHeadAndBody.Reset()
	s.im = inBodyIM
}

// --- in body ---

func (s *Scanner) inBodyIM() bool {
	switch s.tok.Type {
	case html.TextToken:
		d := s.tok.Data
		if t := s.top(); t != nil && t.el.Namespace == "" && (t.el.Atom == a.Pre || t.el.Atom == a.Listing) && !t.hasChild {
			d = strings.TrimPrefix(d, "\r")
			d = strings.TrimPrefix(d, "\n")
		}
		d = strings.ReplaceAll(d, "\x00", "")
		if d == "" {
			return true
		}
		s.reconstructActiveFormattingElements()
		s.addText(d)
	case html.StartTagToken:
		return s.inBodyStartTag()
	case html.EndTagToken:
		return s.inBodyEndTag()
	case html.CommentToken:
		s.addComment()
	}
	return true
}

func (s *Scanner) inBodyStartTag() bool {
	switch s.tok.DataAtom {
	case a.Frameset:
		// x/net/html は、まだ文字を読んでいなければ本文を捨てて frameset にするが、
		// ここでは無視する。
		s.approximated++
	case a.Html, a.Body, a.Head,
		a.Caption, a.Col, a.Colgroup, a.Frame, a.Tbody, a.Td, a.Tfoot, a.Th, a.Thead, a.Tr:
	case a.Base, a.Basefont, a.Bgsound, a.Link, a.Meta, a.Noframes, a.Script, a.Style, a.Template, a.Title:
		c, _ := s.headStartTag()
		return c
	case a.Address, a.Article, a.Aside, a.Blockquote, a.Center, a.Details, a.Dialog, a.Dir, a.Div, a.Dl, a.Fieldset, a.Figcaption, a.Figure, a.Footer, a.Header, a.Hgroup, a.Main, a.Menu, a.Nav, a.Ol, a.P, a.Search, a.Section, a.Summary, a.Ul:
		s.popUntil(buttonScope, a.P)
		s.addElement()
	case a.H1, a.H2, a.H3, a.H4, a.H5, a.H6:
		s.popUntil(buttonScope, a.P)
		switch s.topAtom() {
		case a.H1, a.H2, a.H3, a.H4, a.H5, a.H6:
			s.popTop()
		}
		s.addElement()
	case a.Pre, a.Listing:
		s.popUntil(buttonScope, a.P)
		s.addElement()
	case a.Form:
		if s.form != nil && s.templates == 0 {
			return true
		}
		s.popUntil(buttonScope, a.P)
		f := s.addElement()
		if s.templates == 0 {
			s.form = f
		}
	case a.Li, a.Dd, a.Dt:
		s.closeListItem()
		s.popUntil(buttonScope, a.P)
		s.addElement()
	case a.Plaintext:
		s.popUntil(buttonScope, a.P)
		s.addElement()
	case a.Button:
		if s.elementInScope(defaultScope, a.Button) {
			s.generateImpliedEndTags()
			s.popUntil(defaultScope, a.Button)
		}
		s.reconstructActiveFormattingElements()
		s.addElement()
	case a.A:
		for i := len(s.afe) - 1; i >= 0 && s.afe[i] != nil; i-- {
			if n := s.afe[i]; n.el.Atom == a.A {
				s.inBodyEndTagFormatting(a.A, "a")
				s.remove(n)
				s.afeRemove(n)
				break
			}
		}
		s.reconstructActiveFormattingElements()
		s.addFormattingElement()
	case a.B, a.Big, a.Code, a.Em, a.Font, a.I, a.S, a.Small, a.Strike, a.Strong, a.Tt, a.U:
		s.reconstructActiveFormattingElements()
		s.addFormattingElement()
	case a.Nobr:
		s.reconstructActiveFormattingElements()
		if s.elementInScope(defaultScope, a.Nobr) {
			s.inBodyEndTagFormatting(a.Nobr, "nobr")
			s.reconstructActiveFormattingElements()
		}
		s.addFormattingElement()
	case a.Applet, a.Marquee, a.Object:
		s.reconstructActiveFormattingElements()
		if s.addElement() != nil {
			s.pushMarker()
		}
	case a.Table:
		if !s.quirks {
			s.popUntil(buttonScope, a.P)
		}
		if s.addElement() != nil {
			s.im = inTableIM
		}
	case a.Area, a.Br, a.Embed, a.Img, a.Keygen, a.Wbr:
		s.reconstructActiveFormattingElements()
		s.addVoid()
	case a.Input:
		s.popUntil(defaultScope, a.Select)
		s.reconstructActiveFormattingElements()
		s.addVoid()
	case a.Param, a.Source, a.Track:
		s.addVoid()
	case a.Hr:
		if s.elementInScope(buttonScope, a.P) {
			s.generateImpliedEndTags("p")
			s.popUntil(defaultScope, a.P)
		}
		if s.elementInScope(defaultScope, a.Select) {
			s.generateImpliedEndTags()
		}
		s.addVoid()
	case a.Image:
		s.tok.DataAtom = a.Img
		s.tok.Data = a.Img.String()
		return false
	case a.Textarea:
		s.addRawText()
	case a.Xmp:
		s.popUntil(buttonScope, a.P)
		s.reconstructActiveFormattingElements()
		s.addRawText()
	case a.Iframe, a.Noembed, a.Noscript:
		s.addRawText()
	case a.Select:
		if s.popUntil(defaultScope, a.Select) {
			return true
		}
		s.reconstructActiveFormattingElements()
		s.addElement()
	case a.Option:
		if s.elementInScope(defaultScope, a.Select) {
			s.generateImpliedEndTags("optgroup")
		} else if s.topAtom() == a.Option {
			s.popTop()
		}
		s.reconstructActiveFormattingElements()
		s.addElement()
	case a.Optgroup:
		if s.elementInScope(defaultScope, a.Select) {
			s.generateImpliedEndTags()
		} else if s.topAtom() == a.Option {
			s.popTop()
		}
		s.reconstructActiveFormattingElements()
		s.addElement()
	case a.Rb, a.Rtc:
		if s.elementInScope(defaultScope, a.Ruby) {
			s.generateImpliedEndTags()
		}
		s.addElement()
	case a.Rp, a.Rt:
		if s.elementInScope(defaultScope, a.Ruby) {
			s.generateImpliedEndTags("rtc")
		}
		s.addElement()
	case a.Math, a.Svg:
		s.reconstructActiveFormattingElements()
		adjustForeignAttributes(s.tok.Attr)
		el := s.newElement()
		el.Namespace = s.tok.Data
		if s.push(el) != nil && s.self {
			s.popTop()
		}
	default:
		s.reconstructActiveFormattingElements()
		s.addElement()
	}
	return true
}

// closeListItem closes the open li (for li) or dd / dt (for dd and dt) before
// a new one starts.
func (s *Scanner) closeListItem() {
	isItem := func(t a.Atom) bool {
		if s.tok.DataAtom == a.Li {
			return t == a.Li
		}
		return t == a.Dd || t == a.Dt
	}
	n := 0
	for i := len(s.oe) - 1; i >= 0 && n < maxScopeSearch; i-- {
		f := s.oe[i]
		n++
		if f.removed {
			continue
		}
		t := f.el.Atom
		if f.el.Namespace == "" && isItem(t) {
			s.truncate(i)
			return
		}
		if f.el.Namespace == "" && (t == a.Address || t == a.Div || t == a.P) {
			continue
		}
		if f.special {
			return
		}
	}
}

func (s *Scanner) inBodyEndTag() bool {
	switch s.tok.DataAtom {
	case a.Body:
		if s.bodyInScope() {
			s.im = afterBodyIM
		}
	case a.Html:
		if s.bodyInScope() {
			s.im = afterBodyIM
			return false
		}
	case a.Address, a.Article, a.Aside, a.Blockquote, a.Button, a.Center, a.Details, a.Dialog, a.Dir, a.Div, a.Dl, a.Fieldset, a.Figcaption, a.Figure, a.Footer, a.Header, a.Hgroup, a.Listing, a.Main, a.Menu, a.Nav, a.Ol, a.Pre, a.Search, a.Section, a.Select, a.Summary, a.Ul:
		if !s.elementInScope(defaultScope, s.tok.DataAtom) {
			return true
		}
		s.generateImpliedEndTags()
		s.popUntil(defaultScope, s.tok.DataAtom)
	case a.Form:
		if s.templates > 0 {
			i := s.indexInScope(defaultScope, a.Form)
			if i == -1 {
				return true
			}
			s.generateImpliedEndTags()
			if t := s.top(); t == nil || t.el.Atom != a.Form {
				return true
			}
			s.popUntil(defaultScope, a.Form)
		} else {
			node := s.form
			s.form = nil
			i := s.indexInScope(defaultScope, a.Form)
			if node == nil || i == -1 || s.oe[i] != node {
				return true
			}
			s.generateImpliedEndTags()
			s.remove(node)
		}
	case a.P:
		if !s.elementInScope(buttonScope, a.P) {
			s.parseImpliedToken(html.StartTagToken, a.P)
		}
		s.popUntil(buttonScope, a.P)
	case a.Li:
		s.popUntil(listItemScope, a.Li)
	case a.Dd, a.Dt:
		s.popUntil(defaultScope, s.tok.DataAtom)
	case a.H1, a.H2, a.H3, a.H4, a.H5, a.H6:
		s.popUntil(defaultScope, a.H1, a.H2, a.H3, a.H4, a.H5, a.H6)
	case a.A, a.B, a.Big, a.Code, a.Em, a.Font, a.I, a.Nobr, a.S, a.Small, a.Strike, a.Strong, a.Tt, a.U:
		s.inBodyEndTagFormatting(s.tok.DataAtom, s.tok.Data)
	case a.Applet, a.Marquee, a.Object:
		if s.popUntil(defaultScope, s.tok.DataAtom) {
			s.clearActiveFormattingElements()
		}
	case a.Br:
		s.tok.Type = html.StartTagToken
		return false
	case a.Template:
		s.endTemplate()
	default:
		s.inBodyEndTagOther(s.tok.DataAtom, s.tok.Data)
	}
	return true
}

func (s *Scanner) textIM() bool {
	switch s.tok.Type {
	case html.ErrorToken:
		s.popTop()
	case html.TextToken:
		d := s.tok.Data
		if t := s.top(); t != nil && t.el.Atom == a.Textarea && !t.hasChild {
			d = strings.TrimPrefix(d, "\r")
			d = strings.TrimPrefix(d, "\n")
		}
		s.addText(d)
		return true
	case html.EndTagToken:
		s.popTop()
	}
	s.im = s.originalIM
	return s.tok.Type == html.EndTagToken
}

// --- tables ---

func (s *Scanner) inTableIM() bool {
	switch s.tok.Type {
	case html.TextToken:
		s.tok.Data = strings.ReplaceAll(s.tok.Data, "\x00", "")
		switch s.topAtom() {
		case a.Table, a.Tbody, a.Tfoot, a.Thead, a.Tr:
			if strings.Trim(s.tok.Data, whitespace) == "" {
				s.addText(s.tok.Data)
				return true
			}
		}
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Caption:
			s.clearStackToContext(a.Table)
			s.pushMarker()
			if s.addElement() != nil {
				s.im = inCaptionIM
			}
			return true
		case a.Colgroup:
			s.clearStackToContext(a.Table)
			if s.addElement() != nil {
				s.im = inColumnGroupIM
			}
			return true
		case a.Col:
			s.parseImpliedToken(html.StartTagToken, a.Colgroup)
			return false
		case a.Tbody, a.Tfoot, a.Thead:
			s.clearStackToContext(a.Table)
			if s.addElement() != nil {
				s.im = inTableBodyIM
			}
			return true
		case a.Td, a.Th, a.Tr:
			s.parseImpliedToken(html.StartTagToken, a.Tbody)
			return false
		case a.Table:
			if s.popUntil(tableScope, a.Table) {
				s.resetInsertionMode()
				return false
			}
			return true
		case a.Style, a.Script, a.Template:
			c, _ := s.headStartTag()
			return c
		case a.Input:
			for _, t := range s.tok.Attr {
				if t.Key == "type" && strings.EqualFold(t.Val, "hidden") {
					s.addVoid()
					return true
				}
			}
		case a.Form:
			if s.templates > 0 || s.form != nil {
				return true
			}
			f := s.addElement()
			if f != nil {
				s.popTop()
				s.form = f
			}
			return true
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Table:
			if s.popUntil(tableScope, a.Table) {
				s.resetInsertionMode()
			}
			return true
		case a.Body, a.Caption, a.Col, a.Colgroup, a.Html, a.Tbody, a.Td, a.Tfoot, a.Th, a.Thead, a.Tr:
			return true
		case a.Template:
			s.endTemplate()
			return true
		}
	case html.CommentToken:
		s.addComment()
		return true
	case html.DoctypeToken:
		return true
	case html.ErrorToken:
		return s.inBodyIM()
	}
	// x/net/html は、ここに来た文字と要素を表の前へ移す (foster parenting)。
	// ここでは移さずに、その場で本文の規則で扱う。
	switch s.topAtom() {
	case a.Table, a.Tbody, a.Tfoot, a.Thead, a.Tr:
		if s.tok.Type != html.ErrorToken {
			s.approximated++
		}
	}
	return s.inBodyIM()
}

func (s *Scanner) inCaptionIM() bool {
	switch s.tok.Type {
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Caption, a.Col, a.Colgroup, a.Tbody, a.Td, a.Tfoot, a.Thead, a.Tr:
			if !s.popUntil(tableScope, a.Caption) {
				return true
			}
			s.clearActiveFormattingElements()
			s.im = inTableIM
			return false
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Caption:
			if s.popUntil(tableScope, a.Caption) {
				s.clearActiveFormattingElements()
				s.im = inTableIM
			}
			return true
		case a.Table:
			if !s.popUntil(tableScope, a.Caption) {
				return true
			}
			s.clearActiveFormattingElements()
			s.im = inTableIM
			return false
		case a.Body, a.Col, a.Colgroup, a.Html, a.Tbody, a.Td, a.Tfoot, a.Th, a.Thead, a.Tr:
			return true
		}
	}
	return s.inBodyIM()
}

func (s *Scanner) inColumnGroupIM() bool {
	switch s.tok.Type {
	case html.TextToken:
		rest := strings.TrimLeft(s.tok.Data, whitespace)
		if len(rest) < len(s.tok.Data) {
			s.addText(s.tok.Data[:len(s.tok.Data)-len(rest)])
			if rest == "" {
				return true
			}
			s.tok.Data = rest
		}
	case html.CommentToken:
		s.addComment()
		return true
	case html.DoctypeToken:
		return true
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Html:
			return s.inBodyIM()
		case a.Col:
			s.addVoid()
			return true
		case a.Template:
			c, _ := s.headStartTag()
			return c
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Colgroup:
			if s.topAtom() == a.Colgroup {
				s.popTop()
				s.im = inTableIM
			}
			return true
		case a.Col:
			return true
		case a.Template:
			s.endTemplate()
			return true
		}
	case html.ErrorToken:
		return s.inBodyIM()
	}
	if s.topAtom() != a.Colgroup {
		return true
	}
	s.popTop()
	s.im = inTableIM
	return false
}

func (s *Scanner) inTableBodyIM() bool {
	switch s.tok.Type {
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Tr:
			s.clearStackToContext(a.Tbody, a.Tfoot, a.Thead)
			if s.addElement() != nil {
				s.im = inRowIM
			}
			return true
		case a.Td, a.Th:
			s.parseImpliedToken(html.StartTagToken, a.Tr)
			return false
		case a.Caption, a.Col, a.Colgroup, a.Tbody, a.Tfoot, a.Thead:
			if s.popUntil(tableScope, a.Tbody, a.Thead, a.Tfoot) {
				s.im = inTableIM
				return false
			}
			return true
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Tbody, a.Tfoot, a.Thead:
			if s.elementInScope(tableScope, s.tok.DataAtom) {
				s.clearStackToContext(a.Tbody, a.Tfoot, a.Thead)
				s.popTop()
				s.im = inTableIM
			}
			return true
		case a.Table:
			if s.popUntil(tableScope, a.Tbody, a.Thead, a.Tfoot) {
				s.im = inTableIM
				return false
			}
			return true
		case a.Body, a.Caption, a.Col, a.Colgroup, a.Html, a.Td, a.Th, a.Tr:
			return true
		}
	case html.CommentToken:
		s.addComment()
		return true
	}
	return s.inTableIM()
}

func (s *Scanner) inRowIM() bool {
	switch s.tok.Type {
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Td, a.Th:
			s.clearStackToContext(a.Tr)
			if s.addElement() != nil {
				s.pushMarker()
				s.im = inCellIM
			}
			return true
		case a.Caption, a.Col, a.Colgroup, a.Tbody, a.Tfoot, a.Thead, a.Tr:
			if s.elementInScope(tableScope, a.Tr) {
				s.closeRow()
				return false
			}
			return true
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Tr:
			if s.elementInScope(tableScope, a.Tr) {
				s.closeRow()
			}
			return true
		case a.Table:
			if s.elementInScope(tableScope, a.Tr) {
				s.closeRow()
				return false
			}
			return true
		case a.Tbody, a.Tfoot, a.Thead:
			if s.elementInScope(tableScope, s.tok.DataAtom) && s.elementInScope(tableScope, a.Tr) {
				s.closeRow()
				return false
			}
			return true
		case a.Body, a.Caption, a.Col, a.Colgroup, a.Html, a.Td, a.Th:
			return true
		}
	}
	return s.inTableIM()
}

func (s *Scanner) closeRow() {
	s.clearStackToContext(a.Tr)
	if len(s.oe) > 0 {
		s.popTop()
	}
	s.im = inTableBodyIM
}

func (s *Scanner) inCellIM() bool {
	switch s.tok.Type {
	case html.StartTagToken:
		switch s.tok.DataAtom {
		case a.Caption, a.Col, a.Colgroup, a.Tbody, a.Td, a.Tfoot, a.Th, a.Thead, a.Tr:
			if s.popUntil(tableScope, a.Td, a.Th) {
				s.clearActiveFormattingElements()
				s.im = inRowIM
				return false
			}
			return true
		}
	case html.EndTagToken:
		switch s.tok.DataAtom {
		case a.Td, a.Th:
			if !s.popUntil(tableScope, s.tok.DataAtom) {
				return true
			}
			s.clearActiveFormattingElements()
			s.im = inRowIM
			return true
		case a.Body, a.Caption, a.Col, a.Colgroup, a.Html:
			return true
		case a.Table, a.Tbody, a.Tfoot, a.Thead, a.Tr:
			if !s.elementInScope(tableScope, s.tok.DataAtom) {
				return true
			}
			if s.popUntil(tableScope, a.Td, a.Th) {
				s.clearActiveFormattingElements()
			}
			s.im = inRowIM
			return false
		}
	}
	return s.inBodyIM()
}

// --- after the body ---

func (s *Scanner) afterBodyIM() bool {
	switch s.tok.Type {
	case html.ErrorToken:
		return true
	case html.TextToken:
		if strings.TrimLeft(s.tok.Data, whitespace) == "" {
			return s.inBodyIM()
		}
	case html.StartTagToken:
		if s.tok.DataAtom == a.Html {
			return true
		}
	case html.EndTagToken:
		if s.tok.DataAtom == a.Html {
			s.im = afterAfterBodyIM
			return true
		}
	case html.CommentToken:
		// html の要素の子になる。本文の外なので、事象には出さない。
		return true
	}
	s.im = inBodyIM
	return false
}

func (s *Scanner) afterAfterBodyIM() bool {
	switch s.tok.Type {
	case html.ErrorToken:
		return true
	case html.TextToken:
		if strings.TrimLeft(s.tok.Data, whitespace) == "" {
			return s.inBodyIM()
		}
	case html.StartTagToken:
		if s.tok.DataAtom == a.Html {
			return true
		}
	case html.CommentToken:
		return true
	case html.DoctypeToken:
		return true
	}
	s.im = inBodyIM
	return false
}

// --- foreign content (SVG, MathML) ---

func (s *Scanner) inForeignContent() bool {
	t := s.top()
	if t == nil || t.el.Namespace == "" {
		return false
	}
	if mathMLTextIntegrationPoint(t.el) {
		if s.tok.Type == html.StartTagToken && s.tok.DataAtom != a.Mglyph && s.tok.DataAtom != a.Malignmark {
			return false
		}
		if s.tok.Type == html.TextToken {
			return false
		}
	}
	if t.el.Namespace == "math" && t.el.Atom == a.AnnotationXml && s.tok.Type == html.StartTagToken && s.tok.DataAtom == a.Svg {
		return false
	}
	if t.htmlIP && (s.tok.Type == html.StartTagToken || s.tok.Type == html.TextToken) {
		return false
	}
	return s.tok.Type != html.ErrorToken
}

func (s *Scanner) parseForeignContent() bool {
	switch s.tok.Type {
	case html.TextToken:
		s.addText(strings.ReplaceAll(s.tok.Data, "\x00", "�"))
	case html.CommentToken:
		s.addComment()
	case html.StartTagToken:
		b := breakout[s.tok.Data]
		if s.tok.DataAtom == a.Font {
			for _, at := range s.tok.Attr {
				if at.Key == "color" || at.Key == "face" || at.Key == "size" {
					b = true
					break
				}
			}
		}
		if b {
			for len(s.oe) > 0 {
				t := s.top()
				if t.el.Namespace == "" || t.htmlIP || mathMLTextIntegrationPoint(t.el) {
					break
				}
				s.popTop()
			}
			return s.run(s.im)
		}
		ns := s.top().el.Namespace
		if ns == "svg" {
			if x := svgTagNameAdjustments[s.tok.Data]; x != "" {
				s.tok.DataAtom = a.Lookup([]byte(x))
				s.tok.Data = x
			}
		}
		adjustForeignAttributes(s.tok.Attr)
		el := s.newElement()
		el.Namespace = ns
		if s.push(el) == nil {
			return true
		}
		// 外来の要素の中身は生のテキストとして読まない (SVG の title など)。
		s.z.NextIsNotRawText()
		if s.self {
			s.popTop()
		}
	case html.EndTagToken:
		n := 0
		for i := len(s.oe) - 1; i >= 0 && n < maxScopeSearch; i-- {
			f := s.oe[i]
			n++
			if f.removed {
				continue
			}
			if strings.EqualFold(f.el.Name, s.tok.Data) {
				s.truncate(i)
				return true
			}
			if i > 0 && s.oe[i-1].el.Namespace == "" {
				break
			}
		}
		return s.run(s.im)
	}
	return true
}

func (s *Scanner) htmlIntegrationPoint(e *Element) bool {
	switch e.Namespace {
	case "math":
		if e.Name == "annotation-xml" {
			s.attrsRead += len(e.Attr)
			for _, at := range e.Attr {
				if at.Key == "encoding" && (strings.EqualFold(at.Val, "text/html") || strings.EqualFold(at.Val, "application/xhtml+xml")) {
					return true
				}
			}
		}
	case "svg":
		switch e.Name {
		case "desc", "foreignObject", "title":
			return true
		}
	}
	return false
}

func mathMLTextIntegrationPoint(e *Element) bool {
	if e.Namespace != "math" {
		return false
	}
	switch e.Name {
	case "mi", "mo", "mn", "ms", "mtext":
		return true
	}
	return false
}

func adjustForeignAttributes(aa []html.Attribute) {
	for i, at := range aa {
		switch at.Key {
		case "xlink:actuate", "xlink:arcrole", "xlink:href", "xlink:role", "xlink:show",
			"xlink:title", "xlink:type", "xml:lang", "xml:space", "xmlns:xlink":
			j := strings.IndexByte(at.Key, ':')
			aa[i].Namespace = at.Key[:j]
			aa[i].Key = at.Key[j+1:]
		}
	}
}

func isSpecial(e *Element) bool {
	switch e.Namespace {
	case "":
		return specialElements[e.Atom]
	case "math":
		switch e.Name {
		case "mi", "mo", "mn", "ms", "mtext", "annotation-xml":
			return true
		}
	case "svg":
		switch e.Name {
		case "foreignObject", "desc", "title":
			return true
		}
	}
	return false
}

var specialElements = map[a.Atom]bool{
	a.Address: true, a.Applet: true, a.Area: true, a.Article: true, a.Aside: true,
	a.Base: true, a.Basefont: true, a.Bgsound: true, a.Blockquote: true, a.Body: true,
	a.Br: true, a.Button: true, a.Caption: true, a.Center: true, a.Col: true,
	a.Colgroup: true, a.Dd: true, a.Details: true, a.Dir: true, a.Div: true,
	a.Dl: true, a.Dt: true, a.Embed: true, a.Fieldset: true, a.Figcaption: true,
	a.Figure: true, a.Footer: true, a.Form: true, a.Frame: true, a.Frameset: true,
	a.H1: true, a.H2: true, a.H3: true, a.H4: true, a.H5: true,
	a.H6: true, a.Head: true, a.Header: true, a.Hgroup: true, a.Hr: true,
	a.Html: true, a.Iframe: true, a.Img: true, a.Input: true, a.Keygen: true,
	a.Li: true, a.Link: true, a.Listing: true, a.Main: true, a.Marquee: true,
	a.Menu: true, a.Meta: true, a.Nav: true, a.Noembed: true, a.Noframes: true,
	a.Noscript: true, a.Object: true, a.Ol: true, a.P: true, a.Param: true,
	a.Plaintext: true, a.Pre: true, a.Script: true, a.Section: true, a.Select: true,
	a.Source: true, a.Style: true, a.Summary: true, a.Table: true, a.Tbody: true,
	a.Td: true, a.Template: true, a.Textarea: true, a.Tfoot: true, a.Th: true,
	a.Thead: true, a.Title: true, a.Tr: true, a.Track: true, a.Ul: true,
	a.Wbr: true, a.Xmp: true,
}

var breakout = map[string]bool{
	"b": true, "big": true, "blockquote": true, "body": true, "br": true,
	"center": true, "code": true, "dd": true, "div": true, "dl": true,
	"dt": true, "em": true, "embed": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "head": true,
	"hr": true, "i": true, "img": true, "li": true, "listing": true,
	"menu": true, "meta": true, "nobr": true, "ol": true, "p": true,
	"pre": true, "ruby": true, "s": true, "small": true, "span": true,
	"strong": true, "strike": true, "sub": true, "sup": true, "table": true,
	"tt": true, "u": true, "ul": true, "var": true,
}

var svgTagNameAdjustments = map[string]string{
	"altglyph": "altGlyph", "altglyphdef": "altGlyphDef", "altglyphitem": "altGlyphItem",
	"animatecolor": "animateColor", "animatemotion": "animateMotion", "animatetransform": "animateTransform",
	"clippath": "clipPath", "feblend": "feBlend", "fecolormatrix": "feColorMatrix",
	"fecomponenttransfer": "feComponentTransfer", "fecomposite": "feComposite",
	"feconvolvematrix": "feConvolveMatrix", "fediffuselighting": "feDiffuseLighting",
	"fedisplacementmap": "feDisplacementMap", "fedistantlight": "feDistantLight",
	"feflood": "feFlood", "fefunca": "feFuncA", "fefuncb": "feFuncB", "fefuncg": "feFuncG",
	"fefuncr": "feFuncR", "fegaussianblur": "feGaussianBlur", "feimage": "feImage",
	"femerge": "feMerge", "femergenode": "feMergeNode", "femorphology": "feMorphology",
	"feoffset": "feOffset", "fepointlight": "fePointLight", "fespecularlighting": "feSpecularLighting",
	"fespotlight": "feSpotLight", "fetile": "feTile", "feturbulence": "feTurbulence",
	"foreignobject": "foreignObject", "glyphref": "glyphRef", "lineargradient": "linearGradient",
	"radialgradient": "radialGradient", "textpath": "textPath",
}
