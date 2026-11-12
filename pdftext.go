package main

// A small PDF text reader, used only to draft the emergency summary from a
// Safety Data Sheet. It understands the usual structure of PDFs made by Word,
// browsers and SDS authoring tools: compressed streams, object streams, and
// fonts with a ToUnicode table. It cannot read scanned (image-only) or
// encrypted PDFs, and says so instead of guessing.

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

var (
	errNoText    = errors.New("this PDF has no readable text (it may be a scan)")
	errEncrypted = errors.New("this PDF is password-protected")
)

type pdfObject struct {
	dict   string
	stream []byte // decoded
	raw    bool   // true when the stream could not be decoded
}

type pdfFile struct {
	objs map[int]*pdfObject
}

var objHead = regexp.MustCompile(`(?m)(?:^|[\r\n\s])(\d+)\s+(\d+)\s+obj\b`)

func parsePDF(data []byte) (*pdfFile, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(data[:min(len(data), 1024)], "\x00 \r\n\t"), []byte("%PDF")) && !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF")) {
		return nil, errors.New("not a PDF")
	}
	if bytes.Contains(data, []byte("/Encrypt")) {
		return nil, errEncrypted
	}
	f := &pdfFile{objs: map[int]*pdfObject{}}
	locs := objHead.FindAllSubmatchIndex(data, -1)
	for i, m := range locs {
		num, _ := strconv.Atoi(string(data[m[2]:m[3]]))
		start := m[1]
		end := len(data)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := data[start:end]
		o := &pdfObject{}
		if si := bytes.Index(body, []byte("stream")); si >= 0 && (si == 0 || !bytes.HasSuffix(body[:si], []byte("end"))) {
			dictPart := body[:si]
			o.dict = string(dictPart)
			ds := si + len("stream")
			if ds < len(body) && body[ds] == '\r' {
				ds++
			}
			if ds < len(body) && body[ds] == '\n' {
				ds++
			}
			de := bytes.LastIndex(body, []byte("endstream"))
			if de < ds {
				de = len(body)
			}
			o.stream, o.raw = decodeStream(o.dict, bytes.TrimRight(body[ds:de], "\r\n"))
		} else {
			o.dict = string(body)
		}
		if _, dup := f.objs[num]; !dup || len(o.dict) > 0 {
			f.objs[num] = o
		}
	}
	// Objects packed inside object streams.
	for _, o := range f.objs {
		if o.raw || len(o.stream) == 0 || !strings.Contains(o.dict, "/ObjStm") {
			continue
		}
		n, _ := strconv.Atoi(dictGet(o.dict, "N"))
		first, _ := strconv.Atoi(dictGet(o.dict, "First"))
		fields := strings.Fields(string(o.stream[:min(len(o.stream), first)]))
		for i := 0; i+1 < len(fields) && i/2 < n; i += 2 {
			id, e1 := strconv.Atoi(fields[i])
			off, e2 := strconv.Atoi(fields[i+1])
			if e1 != nil || e2 != nil || first+off > len(o.stream) {
				continue
			}
			endOff := len(o.stream)
			if i+3 < len(fields) {
				if nx, err := strconv.Atoi(fields[i+3]); err == nil && first+nx <= len(o.stream) {
					endOff = first + nx
				}
			}
			if _, have := f.objs[id]; !have {
				f.objs[id] = &pdfObject{dict: string(o.stream[first+off : endOff])}
			}
		}
	}
	return f, nil
}

func decodeStream(dict string, b []byte) ([]byte, bool) {
	filter := dictGet(dict, "Filter")
	if filter == "" {
		return b, false
	}
	// Filters are applied in the order listed, e.g. [/ASCII85Decode /FlateDecode].
	for _, name := range regexp.MustCompile(`/[A-Za-z0-9]+`).FindAllString(filter, -1) {
		var err error
		switch name {
		case "/FlateDecode", "/Fl":
			var r io.ReadCloser
			if r, err = zlib.NewReader(bytes.NewReader(b)); err == nil {
				var out []byte
				out, err = io.ReadAll(io.LimitReader(r, 64<<20))
				if len(out) == 0 && err != nil {
					return nil, true
				}
				b, err = out, nil
			}
		case "/ASCII85Decode", "/A85":
			b, err = ascii85(b)
		case "/ASCIIHexDecode", "/AHx":
			b, err = asciiHex(b)
		default:
			return nil, true
		}
		if err != nil {
			return nil, true
		}
	}
	return b, false
}

func ascii85(b []byte) ([]byte, error) {
	s := bytes.TrimSpace(b)
	s = bytes.TrimPrefix(s, []byte("<~"))
	if i := bytes.Index(s, []byte("~>")); i >= 0 {
		s = s[:i]
	}
	var out []byte
	var group [5]byte
	n := 0
	flush := func(count int) {
		for k := n; k < 5; k++ {
			group[k] = 'u' - '!'
		}
		var v uint32
		for k := 0; k < 5; k++ {
			v = v*85 + uint32(group[k])
		}
		w := []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
		out = append(out, w[:count]...)
	}
	for _, c := range s {
		switch {
		case isSpace(c):
		case c == 'z' && n == 0:
			out = append(out, 0, 0, 0, 0)
		case c >= '!' && c <= 'u':
			group[n] = c - '!'
			n++
			if n == 5 {
				flush(4)
				n = 0
			}
		default:
			return nil, errors.New("bad ASCII85 data")
		}
	}
	if n > 1 {
		flush(n - 1)
	}
	return out, nil
}

func asciiHex(b []byte) ([]byte, error) {
	s := bytes.TrimSpace(b)
	if i := bytes.IndexByte(s, '>'); i >= 0 {
		s = s[:i]
	}
	h := strings.Join(strings.Fields(string(s)), "")
	if len(h)%2 == 1 {
		h += "0"
	}
	out := make([]byte, 0, len(h)/2)
	for k := 0; k+2 <= len(h); k += 2 {
		v, err := strconv.ParseUint(h[k:k+2], 16, 8)
		if err != nil {
			return nil, err
		}
		out = append(out, byte(v))
	}
	return out, nil
}

// ---------------------------------------------------------------- dictionaries

// dictGet returns the raw value of /key in the top level of a dictionary.
func dictGet(d, key string) string {
	m := dictItems(d)
	return m[key]
}

func dictItems(d string) map[string]string {
	out := map[string]string{}
	i := strings.Index(d, "<<")
	if i < 0 {
		return out
	}
	i += 2
	for i < len(d) {
		for i < len(d) && isSpace(d[i]) {
			i++
		}
		if i >= len(d) || strings.HasPrefix(d[i:], ">>") {
			break
		}
		if d[i] != '/' {
			i++
			continue
		}
		k, ni := readName(d, i)
		val, nj := readValue(d, ni)
		out[k] = val
		i = nj
	}
	return out
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}
func isDelim(c byte) bool {
	return isSpace(c) || strings.IndexByte("()<>[]{}/%", c) >= 0
}

func readName(s string, i int) (string, int) {
	j := i + 1
	for j < len(s) && !isDelim(s[j]) {
		j++
	}
	return s[i+1 : j], j
}

var refTail = regexp.MustCompile(`^\s+\d+\s+R\b`)

func readValue(s string, i int) (string, int) {
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if i >= len(s) {
		return "", i
	}
	switch {
	case strings.HasPrefix(s[i:], "<<"):
		depth, j := 0, i
		for j < len(s)-1 {
			if s[j] == '<' && s[j+1] == '<' {
				depth++
				j += 2
			} else if s[j] == '>' && s[j+1] == '>' {
				depth--
				j += 2
				if depth == 0 {
					return s[i:j], j
				}
			} else {
				j++
			}
		}
		return s[i:], len(s)
	case s[i] == '[':
		depth, j := 0, i
		for j < len(s) {
			if s[j] == '[' {
				depth++
			} else if s[j] == ']' {
				depth--
				if depth == 0 {
					return s[i : j+1], j + 1
				}
			} else if s[j] == '(' {
				_, nj := readString(s, j)
				j = nj
				continue
			}
			j++
		}
		return s[i:], len(s)
	case s[i] == '(':
		_, j := readString(s, i)
		return s[i:j], j
	case s[i] == '/':
		n, j := readName(s, i)
		return "/" + n, j
	case s[i] == '<':
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			return s[i:], len(s)
		}
		return s[i : i+j+1], i + j + 1
	}
	j := i
	for j < len(s) && !isDelim(s[j]) {
		j++
	}
	tok := s[i:j]
	if m := refTail.FindString(s[j:]); m != "" && regexp.MustCompile(`^\d+$`).MatchString(tok) {
		return tok + m, j + len(m)
	}
	return tok, j
}

// readString reads a literal (...) string, returning its bytes.
func readString(s string, i int) ([]byte, int) {
	var out []byte
	depth := 0
	j := i
	for j < len(s) {
		c := s[j]
		switch {
		case c == '\\' && j+1 < len(s):
			j++
			e := s[j]
			switch e {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, 8)
			case 'f':
				out = append(out, 12)
			case '\r':
				if j+1 < len(s) && s[j+1] == '\n' {
					j++
				}
			case '\n':
			default:
				if e >= '0' && e <= '7' {
					v, n := 0, 0
					for n < 3 && j < len(s) && s[j] >= '0' && s[j] <= '7' {
						v = v*8 + int(s[j]-'0')
						j++
						n++
					}
					j--
					out = append(out, byte(v))
				} else {
					out = append(out, e)
				}
			}
		case c == '(':
			depth++
			if depth > 1 {
				out = append(out, c)
			}
		case c == ')':
			depth--
			if depth == 0 {
				return out, j + 1
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
		j++
	}
	return out, len(s)
}

var refNum = regexp.MustCompile(`(\d+)\s+\d+\s+R`)

func refID(v string) int {
	m := refNum.FindStringSubmatch(v)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// resolve follows a reference to the dictionary text it points to; other values are returned as they are.
func (f *pdfFile) resolve(v string) string {
	v = strings.TrimSpace(v)
	if id := refID(v); id >= 0 && refTail.MatchString(" "+strings.TrimLeft(strings.TrimLeft(v, "0123456789"), " ")) || (id >= 0 && strings.HasSuffix(v, "R")) {
		if o := f.objs[id]; o != nil {
			return strings.TrimSpace(o.dict)
		}
	}
	return v
}

// ---------------------------------------------------------------- pages

type pdfPage struct {
	contents []int
	fonts    map[string]string // resource name -> font dict text (resolved)
}

func (f *pdfFile) pages() []pdfPage {
	var root string
	for _, o := range f.objs {
		if strings.Contains(o.dict, "/Catalog") && strings.Contains(o.dict, "/Pages") {
			root = dictGet(o.dict, "Pages")
			break
		}
	}
	var out []pdfPage
	seen := map[int]bool{}
	var walk func(ref string, inherited map[string]string)
	walk = func(ref string, inherited map[string]string) {
		id := refID(ref)
		o := f.objs[id]
		if o == nil || seen[id] {
			return
		}
		seen[id] = true
		items := dictItems(o.dict)
		res := inherited
		if r, ok := items["Resources"]; ok {
			res = f.fontMap(r)
		}
		if kids, ok := items["Kids"]; ok && !(strings.Contains(items["Type"], "Page") && !strings.Contains(items["Type"], "Pages")) {
			for _, k := range refNum.FindAllString(f.resolveList(kids), -1) {
				walk(k, res)
			}
			return
		}
		pg := pdfPage{fonts: res}
		c := items["Contents"]
		c = f.resolveList(c)
		for _, m := range refNum.FindAllStringSubmatch(c, -1) {
			n, _ := strconv.Atoi(m[1])
			pg.contents = append(pg.contents, n)
		}
		out = append(out, pg)
	}
	if root != "" {
		walk(root, nil)
	}
	if len(out) == 0 { // damaged page tree: take the page objects in file order
		var ids []int
		for id, o := range f.objs {
			if t := dictGet(o.dict, "Type"); t == "/Page" {
				ids = append(ids, id)
			}
		}
		sort.Ints(ids)
		for _, id := range ids {
			items := dictItems(f.objs[id].dict)
			pg := pdfPage{fonts: f.fontMap(items["Resources"])}
			for _, m := range refNum.FindAllStringSubmatch(f.resolveList(items["Contents"]), -1) {
				n, _ := strconv.Atoi(m[1])
				pg.contents = append(pg.contents, n)
			}
			out = append(out, pg)
		}
	}
	return out
}

// resolveList turns "N 0 R" pointing at an array object into the array text.
func (f *pdfFile) resolveList(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "[") {
		return v
	}
	if id := refID(v); id >= 0 {
		if o := f.objs[id]; o != nil && strings.HasPrefix(strings.TrimSpace(o.dict), "[") {
			return o.dict
		}
	}
	return v
}

func (f *pdfFile) fontMap(res string) map[string]string {
	out := map[string]string{}
	res = f.resolve(res)
	fonts := dictGet(res, "Font")
	if fonts == "" {
		return out
	}
	fonts = f.resolve(fonts)
	for k, v := range dictItems(fonts) {
		out[k] = f.resolve(v)
		if id := refID(v); id >= 0 {
			out[k] = "#" + strconv.Itoa(id) + " " + out[k]
		}
	}
	return out
}

// ---------------------------------------------------------------- fonts

type pdfFont struct {
	twoByte bool
	toUni   map[uint32]string
	known   bool               // can be turned into text
	w       map[uint32]float64 // glyph widths in 1/1000 em
	dw      float64
}

var hexRun = regexp.MustCompile(`<([0-9A-Fa-f\s]*)>`)

func (f *pdfFile) loadFont(dict string, cache map[string]*pdfFont) *pdfFont {
	if c, ok := cache[dict]; ok {
		return c
	}
	if strings.HasPrefix(dict, "#") {
		if sp := strings.IndexByte(dict, ' '); sp > 0 {
			dict = dict[sp+1:]
		}
	}
	ft := &pdfFont{known: true, dw: 500, w: map[uint32]float64{}}
	items := dictItems(dict)
	sub := items["Subtype"]
	enc := items["Encoding"]
	if sub == "/Type0" {
		ft.twoByte = true
		ft.known = false
	}
	if tu := items["ToUnicode"]; tu != "" {
		if o := f.objs[refID(tu)]; o != nil && len(o.stream) > 0 {
			ft.toUni = parseCMap(o.stream)
			if len(ft.toUni) > 0 {
				ft.known = true
			}
		}
	}
	if sub == "/Type0" && ft.toUni == nil {
		ft.known = false
	}
	f.loadWidths(ft, items)
	if sub == "/Type0" && strings.Contains(enc, "Identity") == false && ft.toUni == nil {
		ft.known = false
	}
	cache[dict] = ft
	return ft
}

// loadWidths reads glyph widths so that text drawn in pieces can be joined without stray spaces.
func (f *pdfFile) loadWidths(ft *pdfFont, items map[string]string) {
	if ft.twoByte {
		d := f.resolveList(items["DescendantFonts"])
		id := refID(d)
		if o := f.objs[id]; o != nil {
			di := dictItems(o.dict)
			if v, err := strconv.ParseFloat(strings.TrimSpace(di["DW"]), 64); err == nil {
				ft.dw = v
			} else {
				ft.dw = 1000
			}
			arr := strings.TrimSpace(f.resolveList(di["W"]))
			arr = strings.TrimSuffix(strings.TrimPrefix(arr, "["), "]")
			toks := strings.Fields(strings.NewReplacer("[", " [ ", "]", " ] ").Replace(arr))
			for i := 0; i < len(toks); {
				c, err := strconv.ParseFloat(toks[i], 64)
				if err != nil || i+1 >= len(toks) {
					i++
					continue
				}
				if toks[i+1] == "[" {
					j := i + 2
					k := uint32(c)
					for j < len(toks) && toks[j] != "]" {
						if w, err := strconv.ParseFloat(toks[j], 64); err == nil {
							ft.w[k] = w
						}
						k++
						j++
					}
					i = j + 1
				} else if i+2 < len(toks) {
					hi, _ := strconv.ParseFloat(toks[i+1], 64)
					w, _ := strconv.ParseFloat(toks[i+2], 64)
					for k := uint32(c); k <= uint32(hi) && k-uint32(c) < 0x10000; k++ {
						ft.w[k] = w
					}
					i += 3
				} else {
					i++
				}
			}
		}
		return
	}
	first, _ := strconv.Atoi(strings.TrimSpace(items["FirstChar"]))
	arr := strings.TrimSpace(f.resolveList(items["Widths"]))
	arr = strings.TrimSuffix(strings.TrimPrefix(arr, "["), "]")
	for i, tk := range strings.Fields(arr) {
		if w, err := strconv.ParseFloat(tk, 64); err == nil {
			ft.w[uint32(first+i)] = w
		}
	}
}

func parseCMap(b []byte) map[uint32]string {
	out := map[uint32]string{}
	s := string(b)
	hexVal := func(h string) (uint32, int) {
		h = strings.Join(strings.Fields(h), "")
		v, _ := strconv.ParseUint(h, 16, 32)
		return uint32(v), len(h)
	}
	uni := func(h string) string {
		h = strings.Join(strings.Fields(h), "")
		var u []uint16
		for i := 0; i+4 <= len(h); i += 4 {
			v, _ := strconv.ParseUint(h[i:i+4], 16, 32)
			u = append(u, uint16(v))
		}
		if len(h) == 2 {
			v, _ := strconv.ParseUint(h, 16, 32)
			return string(rune(v))
		}
		return string(utf16.Decode(u))
	}
	for {
		i := strings.Index(s, "beginbfchar")
		j := strings.Index(s, "beginbfrange")
		if i < 0 && j < 0 {
			break
		}
		if i >= 0 && (j < 0 || i < j) {
			end := strings.Index(s[i:], "endbfchar")
			if end < 0 {
				break
			}
			block := s[i+len("beginbfchar") : i+end]
			hs := hexRun.FindAllStringSubmatch(block, -1)
			for k := 0; k+1 < len(hs); k += 2 {
				src, _ := hexVal(hs[k][1])
				out[src] = uni(hs[k+1][1])
			}
			s = s[i+end+9:]
		} else {
			end := strings.Index(s[j:], "endbfrange")
			if end < 0 {
				break
			}
			block := s[j+len("beginbfrange") : j+end]
			// entries: <lo> <hi> <dst>  or  <lo> <hi> [<d1> <d2> ...]
			pos := 0
			for pos < len(block) {
				m := hexRun.FindStringSubmatchIndex(block[pos:])
				if m == nil {
					break
				}
				lo, _ := hexVal(block[pos+m[2] : pos+m[3]])
				pos += m[1]
				m2 := hexRun.FindStringSubmatchIndex(block[pos:])
				if m2 == nil {
					break
				}
				hi, _ := hexVal(block[pos+m2[2] : pos+m2[3]])
				pos += m2[1]
				rest := strings.TrimLeft(block[pos:], " \r\n\t")
				if strings.HasPrefix(rest, "[") {
					close := strings.IndexByte(rest, ']')
					if close < 0 {
						break
					}
					arr := hexRun.FindAllStringSubmatch(rest[:close], -1)
					for k, a := range arr {
						if lo+uint32(k) <= hi {
							out[lo+uint32(k)] = uni(a[1])
						}
					}
					pos += (len(block) - pos - len(rest)) + close + 1
					pos = len(block) - len(rest[close+1:])
				} else {
					m3 := hexRun.FindStringSubmatchIndex(block[pos:])
					if m3 == nil {
						break
					}
					dst := block[pos+m3[2] : pos+m3[3]]
					pos += m3[1]
					base := []rune(uni(dst))
					for c := lo; c <= hi && c-lo < 0x10000; c++ {
						if len(base) == 0 {
							break
						}
						r := append([]rune{}, base...)
						r[len(r)-1] += rune(c - lo)
						out[c] = string(r)
					}
				}
			}
			s = s[j+end+10:]
		}
	}
	return out
}

var winAnsiHigh = map[byte]rune{0x80: '€', 0x91: '\'', 0x92: '\'', 0x93: '"', 0x94: '"', 0x95: '•', 0x96: '-', 0x97: '-', 0x99: '™', 0xA0: ' ', 0xAE: '®', 0xA9: '©'}

func (ft *pdfFont) width(code uint32) float64 {
	if w, ok := ft.w[code]; ok {
		return w
	}
	return ft.dw
}

// decode returns the text of a string and its width in 1/1000 em.
func (ft *pdfFont) decode(b []byte) (string, float64) {
	var sb strings.Builder
	width := 0.0
	if ft.twoByte {
		for i := 0; i+1 < len(b); i += 2 {
			code := uint32(b[i])<<8 | uint32(b[i+1])
			width += ft.width(code)
			if ft.known {
				sb.WriteString(ft.toUni[code])
			}
		}
		return sb.String(), width
	}
	if !ft.known {
		return "", 0
	}
	for _, c := range b {
		width += ft.width(uint32(c))
		if ft.toUni != nil {
			if s, ok := ft.toUni[uint32(c)]; ok {
				sb.WriteString(s)
				continue
			}
		}
		if r, ok := winAnsiHigh[c]; ok {
			sb.WriteRune(r)
		} else if c >= 32 {
			sb.WriteRune(rune(c))
		}
	}
	return sb.String(), width
}

// ---------------------------------------------------------------- content streams

type textState struct {
	font *pdfFont
	size float64
}

// pageText turns one page's content stream into lines of text.
func (f *pdfFile) pageText(pg pdfPage, cache map[string]*pdfFont) string {
	var content []byte
	for _, id := range pg.contents {
		if o := f.objs[id]; o != nil && len(o.stream) > 0 {
			content = append(content, o.stream...)
			content = append(content, '\n')
		}
	}
	type run struct {
		y, x, w, size float64
		text          string
	}
	var runs []run
	var ts textState
	tm := [6]float64{1, 0, 0, 1, 0, 0}
	lm := tm
	var lead float64
	effSize := func() float64 {
		if v := ts.size * absf(tm[3]); v > 0 {
			return v
		}
		return ts.size
	}
	move := func(tx, ty float64) {
		lm[4] += tx*lm[0] + ty*lm[2]
		lm[5] += tx*lm[1] + ty*lm[3]
		tm = lm
	}
	var stack []interface{}
	push := func(v interface{}) { stack = append(stack, v) }
	num := func(i int) float64 {
		if i < 0 || i >= len(stack) {
			return 0
		}
		if v, ok := stack[i].(float64); ok {
			return v
		}
		return 0
	}
	show := func(b []byte) {
		if ts.font == nil {
			return
		}
		t, w := ts.font.decode(b)
		adv := w * ts.size / 1000 * tm[0]
		if t != "" {
			runs = append(runs, run{tm[5], tm[4], adv, effSize(), t})
		}
		tm[4] += adv
	}
	s := string(content)
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case isSpace(c):
			i++
		case c == '%':
			for i < len(s) && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		case c == '(':
			b, ni := readString(s, i)
			push(b)
			i = ni
		case c == '<' && i+1 < len(s) && s[i+1] == '<':
			_, ni := readValue(s, i)
			push(nil)
			i = ni
		case c == '<':
			j := strings.IndexByte(s[i:], '>')
			if j < 0 {
				i = len(s)
				break
			}
			h := strings.Join(strings.Fields(s[i+1:i+j]), "")
			if len(h)%2 == 1 {
				h += "0"
			}
			var b []byte
			for k := 0; k+2 <= len(h); k += 2 {
				v, _ := strconv.ParseUint(h[k:k+2], 16, 8)
				b = append(b, byte(v))
			}
			push(b)
			i += j + 1
		case c == '[':
			// an array of strings and spacing numbers (for TJ)
			j := i + 1
			var parts []interface{}
			for j < len(s) && s[j] != ']' {
				switch {
				case isSpace(s[j]):
					j++
				case s[j] == '(':
					b, nj := readString(s, j)
					parts = append(parts, b)
					j = nj
				case s[j] == '<':
					k := strings.IndexByte(s[j:], '>')
					if k < 0 {
						j = len(s)
						break
					}
					h := strings.Join(strings.Fields(s[j+1:j+k]), "")
					if len(h)%2 == 1 {
						h += "0"
					}
					var b []byte
					for q := 0; q+2 <= len(h); q += 2 {
						v, _ := strconv.ParseUint(h[q:q+2], 16, 8)
						b = append(b, byte(v))
					}
					parts = append(parts, b)
					j += k + 1
				default:
					k := j
					for k < len(s) && !isDelim(s[k]) {
						k++
					}
					if k == j {
						k++
					}
					if v, err := strconv.ParseFloat(s[j:k], 64); err == nil {
						parts = append(parts, v)
					}
					j = k
				}
			}
			push(parts)
			i = j + 1
		case c == '/':
			n, ni := readName(s, i)
			push("/" + n)
			i = ni
		case c == ']' || c == '>' || c == ')' || c == '{' || c == '}':
			i++
		default:
			j := i
			for j < len(s) && !isDelim(s[j]) {
				j++
			}
			tok := s[i:j]
			i = j
			if v, err := strconv.ParseFloat(tok, 64); err == nil {
				push(v)
				continue
			}
			n := len(stack)
			switch tok {
			case "BT":
				tm = [6]float64{1, 0, 0, 1, 0, 0}
				lm = tm
			case "Tf":
				if n >= 2 {
					if name, ok := stack[n-2].(string); ok {
						if d, ok := pg.fonts[strings.TrimPrefix(name, "/")]; ok {
							ts.font = f.loadFont(d, cache)
						} else {
							ts.font = nil
						}
					}
					ts.size = num(n - 1)
				}
			case "Td", "TD":
				if n >= 2 {
					move(num(n-2), num(n-1))
					if tok == "TD" {
						lead = -num(n - 1)
					}
				}
			case "Tm":
				if n >= 6 {
					tm = [6]float64{num(n - 6), num(n - 5), num(n - 4), num(n - 3), num(n - 2), num(n - 1)}
					lm = tm
				}
			case "TL":
				lead = num(n - 1)
			case "T*":
				move(0, -lead)
			case "Tj", "'", "\"":
				if tok != "Tj" {
					move(0, -lead)
				}
				if n >= 1 {
					if b, ok := stack[n-1].([]byte); ok {
						show(b)
					}
				}
			case "TJ":
				if n >= 1 {
					if parts, ok := stack[n-1].([]interface{}); ok {
						var sb strings.Builder
						x0, adv := tm[4], 0.0
						for _, p := range parts {
							switch v := p.(type) {
							case []byte:
								if ts.font != nil {
									tx, w := ts.font.decode(v)
									sb.WriteString(tx)
									adv += w * ts.size / 1000 * tm[0]
								}
							case float64:
								adv -= v / 1000 * ts.size * tm[0]
								if v < -200 {
									sb.WriteByte(' ')
								}
							}
						}
						if sb.Len() > 0 {
							runs = append(runs, run{tm[5], x0, adv, effSize(), sb.String()})
						}
						tm[4] = x0 + adv
					}
				}
			}
			stack = stack[:0]
		}
	}
	// Group runs into lines by their height on the page, left to right.
	var lines []string
	var cur []run
	flush := func() {
		if len(cur) == 0 {
			return
		}
		sort.SliceStable(cur, func(a, b int) bool { return cur[a].x < cur[b].x })
		var sb strings.Builder
		for k, r := range cur {
			if k > 0 && !strings.HasSuffix(sb.String(), " ") && !strings.HasPrefix(r.text, " ") {
				prev := cur[k-1]
				if r.x-(prev.x+prev.w) > 0.15*r.size {
					sb.WriteByte(' ')
				}
			}
			sb.WriteString(r.text)
		}
		lines = append(lines, strings.TrimRight(sb.String(), " "))
		cur = nil
	}
	// runs arrive in drawing order; keep that order between lines, merge neighbours at the same height
	for _, r := range runs {
		if len(cur) > 0 && absf(cur[len(cur)-1].y-r.y) > 1.5 {
			flush()
		}
		cur = append(cur, r)
	}
	flush()
	return strings.Join(lines, "\n")
}

func absf(a float64) float64 {
	if a < 0 {
		return -a
	}
	return a
}

// extractText returns the text of every page.
func extractText(data []byte) ([]string, error) {
	f, err := parsePDF(data)
	if err != nil {
		return nil, err
	}
	cache := map[string]*pdfFont{}
	var out []string
	total := 0
	for _, pg := range f.pages() {
		t := f.pageText(pg, cache)
		total += len(strings.TrimSpace(t))
		out = append(out, t)
	}
	if total < 40 {
		return nil, errNoText
	}
	return out, nil
}
