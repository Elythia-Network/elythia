package htmlstream

import "strings"

// doctypeQuirks reports whether a DOCTYPE (the text after "<!DOCTYPE") puts
// the document in quirks mode, the way x/net/html's parseDoctype decides it
// (doctype.go, under the license in htmlstream.go).
func doctypeQuirks(s string) bool {
	quirks := false
	space := strings.IndexAny(s, whitespace)
	if space == -1 {
		space = len(s)
	}
	// 名前の比較は大文字と小文字を区別する (x/net/html と同じ)。
	if s[:space] != "html" {
		quirks = true
	}
	s = strings.TrimLeft(s[space:], whitespace)
	if len(s) < 6 {
		return quirks || s != ""
	}
	type id struct{ key, val string }
	var ids []id
	key := strings.ToLower(s[:6])
	s = s[6:]
	for key == "public" || key == "system" {
		s = strings.TrimLeft(s, whitespace)
		if s == "" {
			break
		}
		quote := s[0]
		if quote != '"' && quote != '\'' {
			break
		}
		s = s[1:]
		var v string
		if q := strings.IndexByte(s, quote); q == -1 {
			v, s = s, ""
		} else {
			v, s = s[:q], s[q+1:]
		}
		ids = append(ids, id{key, v})
		if key == "public" {
			key = "system"
		} else {
			key = ""
		}
	}
	if key != "" || s != "" {
		return true
	}
	if len(ids) == 0 {
		return quirks
	}
	if ids[0].key == "public" {
		public := strings.ToLower(ids[0].val)
		switch public {
		case "-//w3o//dtd w3 html strict 3.0//en//", "-/w3d/dtd html 4.0 transitional/en", "html":
			quirks = true
		default:
			for _, q := range quirkyIDs {
				if strings.HasPrefix(public, q) {
					quirks = true
					break
				}
			}
		}
		if len(ids) == 1 && (strings.HasPrefix(public, "-//w3c//dtd html 4.01 frameset//") ||
			strings.HasPrefix(public, "-//w3c//dtd html 4.01 transitional//")) {
			quirks = true
		}
	}
	if last := ids[len(ids)-1]; last.key == "system" &&
		strings.EqualFold(last.val, "http://www.ibm.com/data/dtd/v11/ibmxhtml1-transitional.dtd") {
		quirks = true
	}
	return quirks
}

var quirkyIDs = []string{
	"+//silmaril//dtd html pro v0r11 19970101//",
	"-//advasoft ltd//dtd html 3.0 aswedit + extensions//",
	"-//as//dtd html 3.0 aswedit + extensions//",
	"-//ietf//dtd html 2.0 level 1//",
	"-//ietf//dtd html 2.0 level 2//",
	"-//ietf//dtd html 2.0 strict level 1//",
	"-//ietf//dtd html 2.0 strict level 2//",
	"-//ietf//dtd html 2.0 strict//",
	"-//ietf//dtd html 2.0//",
	"-//ietf//dtd html 2.1e//",
	"-//ietf//dtd html 3.0//",
	"-//ietf//dtd html 3.2 final//",
	"-//ietf//dtd html 3.2//",
	"-//ietf//dtd html 3//",
	"-//ietf//dtd html level 0//",
	"-//ietf//dtd html level 1//",
	"-//ietf//dtd html level 2//",
	"-//ietf//dtd html level 3//",
	"-//ietf//dtd html strict level 0//",
	"-//ietf//dtd html strict level 1//",
	"-//ietf//dtd html strict level 2//",
	"-//ietf//dtd html strict level 3//",
	"-//ietf//dtd html strict//",
	"-//ietf//dtd html//",
	"-//metrius//dtd metrius presentational//",
	"-//microsoft//dtd internet explorer 2.0 html strict//",
	"-//microsoft//dtd internet explorer 2.0 html//",
	"-//microsoft//dtd internet explorer 2.0 tables//",
	"-//microsoft//dtd internet explorer 3.0 html strict//",
	"-//microsoft//dtd internet explorer 3.0 html//",
	"-//microsoft//dtd internet explorer 3.0 tables//",
	"-//netscape comm. corp.//dtd html//",
	"-//netscape comm. corp.//dtd strict html//",
	"-//o'reilly and associates//dtd html 2.0//",
	"-//o'reilly and associates//dtd html extended 1.0//",
	"-//o'reilly and associates//dtd html extended relaxed 1.0//",
	"-//softquad software//dtd hotmetal pro 6.0::19990601::extensions to html 4.0//",
	"-//softquad//dtd hotmetal pro 4.0::19971010::extensions to html 4.0//",
	"-//spyglass//dtd html 2.0 extended//",
	"-//sq//dtd html 2.0 hotmetal + extensions//",
	"-//sun microsystems corp.//dtd hotjava html//",
	"-//sun microsystems corp.//dtd hotjava strict html//",
	"-//w3c//dtd html 3 1995-03-24//",
	"-//w3c//dtd html 3.2 draft//",
	"-//w3c//dtd html 3.2 final//",
	"-//w3c//dtd html 3.2//",
	"-//w3c//dtd html 3.2s draft//",
	"-//w3c//dtd html 4.0 frameset//",
	"-//w3c//dtd html 4.0 transitional//",
	"-//w3c//dtd html experimental 19960712//",
	"-//w3c//dtd html experimental 970421//",
	"-//w3c//dtd w3 html//",
	"-//w3o//dtd w3 html 3.0//",
	"-//webtechs//dtd mozilla html 2.0//",
	"-//webtechs//dtd mozilla html//",
}
