package main

// Drafts the emergency summary from the text of a Safety Data Sheet. SDSs follow
// a standard 16-section layout, so the pieces are found by section: 1 (product
// and company), 2 (hazards), 4 (first aid), 8 (protective equipment). The result
// is only ever a draft: a manager still checks it against the sheet and verifies it.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Draft struct {
	Name               string   `json:"name,omitempty"`
	Manufacturer       string   `json:"manufacturer,omitempty"`
	RevisionDate       string   `json:"revisionDate,omitempty"`
	SignalWord         string   `json:"signalWord,omitempty"`
	Pictograms         []string `json:"pictograms"`
	Hazards            string   `json:"hazards,omitempty"`
	FirstAidEyes       string   `json:"firstAidEyes,omitempty"`
	FirstAidSkin       string   `json:"firstAidSkin,omitempty"`
	FirstAidInhalation string   `json:"firstAidInhalation,omitempty"`
	FirstAidSwallowed  string   `json:"firstAidSwallowed,omitempty"`
	PPE                string   `json:"ppe,omitempty"`
	Found              []string `json:"found"`
	Missing            []string `json:"missing"`
	Notes              []string `json:"notes"`
}

// ---------------------------------------------------------------- sections

var sectionHead = regexp.MustCompile(`(?i)^\s*(?:section\s*)?(\d{1,2})\s*[.:)\-–—]?\s+([A-Za-z][A-Za-z ,/&'()\-–]{3,})`)

var sectionWords = map[int]*regexp.Regexp{
	1:  regexp.MustCompile(`(?i)identif|product|company`),
	2:  regexp.MustCompile(`(?i)hazard`),
	3:  regexp.MustCompile(`(?i)composition|ingredient`),
	4:  regexp.MustCompile(`(?i)first[ -]?aid`),
	5:  regexp.MustCompile(`(?i)fire`),
	6:  regexp.MustCompile(`(?i)accidental|release|spill`),
	7:  regexp.MustCompile(`(?i)handling|storage`),
	8:  regexp.MustCompile(`(?i)exposure|protect|control`),
	9:  regexp.MustCompile(`(?i)physical|chemical prop`),
	10: regexp.MustCompile(`(?i)stability|reactiv`),
	11: regexp.MustCompile(`(?i)toxic`),
	12: regexp.MustCompile(`(?i)ecolog`),
	13: regexp.MustCompile(`(?i)disposal`),
	14: regexp.MustCompile(`(?i)transport`),
	15: regexp.MustCompile(`(?i)regulat`),
	16: regexp.MustCompile(`(?i)other|information`),
}

// splitSections returns the lines of each section, keyed by section number.
func splitSections(lines []string) map[int][]string {
	type head struct{ num, at int }
	var heads []head
	for i, l := range lines {
		m := sectionHead.FindStringSubmatch(l)
		if m == nil || len(l) > 110 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if re, ok := sectionWords[n]; ok && re.MatchString(m[2]) {
			heads = append(heads, head{n, i})
		}
	}
	best := map[int][]string{}
	for k, h := range heads {
		end := len(lines)
		if k+1 < len(heads) {
			end = heads[k+1].at
		}
		body := lines[h.at+1 : end]
		// A table of contents repeats the headings with nothing under them; keep the longest body.
		if len(strings.Join(body, "")) > len(strings.Join(best[h.num], "")) {
			best[h.num] = body
		}
	}
	return best
}

// ---------------------------------------------------------------- helpers

var genericLabel = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 ,/()'&\-–.]{2,70}?\s*:(\s|$)`)

func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexAny(cut, ".;"); i > max/2 {
		return cut[:i+1]
	}
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return cut + "..."
}

// tidyName turns an ALL-CAPS product name into normal capitals, keeping short codes such as RTU.
func tidyName(s string) string {
	s = cleanName(s)
	if s != strings.ToUpper(s) {
		return s
	}
	small := map[string]bool{"AND": true, "OF": true, "THE": true, "FOR": true, "TO": true, "IN": true, "WITH": true, "A": true}
	words := strings.Fields(s)
	for i, w := range words {
		letters := strings.Trim(w, "&/-()")
		switch {
		case small[letters] && i > 0:
			words[i] = strings.ToLower(w)
		case len(letters) <= 3 && regexp.MustCompile(`[A-Z]`).MatchString(letters) && !small[letters]:
			// short codes stay as they are
		default:
			words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
		}
	}
	return strings.Join(words, " ")
}

func cleanName(s string) string {
	s = strings.NewReplacer("™", "", "®", "", "©", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// labelled finds "Label : value" (value may continue on the following lines) and
// returns the value. If the label stands alone the value is on the next lines.
func labelled(lines []string, re *regexp.Regexp, maxLines int) string {
	for i, l := range lines {
		m := re.FindStringSubmatchIndex(l)
		if m == nil {
			continue
		}
		rest := strings.TrimLeft(l[m[1]:], " \t:–-.")
		var parts []string
		if rest != "" {
			parts = append(parts, rest)
		}
		for j := i + 1; j < len(lines) && len(parts) < maxLines+1; j++ {
			n := strings.TrimSpace(lines[j])
			if n == "" {
				if len(parts) > 0 {
					break
				}
				continue
			}
			if genericLabel.MatchString(n) || strings.HasPrefix(n, ":") || sectionHead.MatchString(n) && len(n) < 60 {
				break
			}
			parts = append(parts, n)
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	return ""
}

// ---------------------------------------------------------------- hazards

var hCode = regexp.MustCompile(`\bH(\d{3})[A-Za-z]{0,2}\b`)
var hLine = regexp.MustCompile(`\bH\d{3}[A-Za-z]{0,2}(?:\s*[+,]\s*H?\d{3}[A-Za-z]{0,2})*\s*[:\-–]?\s*([A-Z][^\n]{4,160})`)

var codePictos = map[string][]string{
	"explosive":   {"200", "201", "202", "203", "204", "205", "240", "241"},
	"flame":       {"220", "221", "222", "223", "224", "225", "226", "228", "230", "231", "241", "242", "250", "251", "252", "260", "261"},
	"oxidizer":    {"270", "271", "272"},
	"gas":         {"280", "281"},
	"corrosive":   {"290", "314", "318"},
	"skull":       {"300", "301", "310", "311", "330", "331"},
	"bang":        {"302", "312", "315", "317", "319", "332", "335", "336", "420"},
	"health":      {"304", "334", "340", "341", "350", "351", "360", "361", "370", "371", "372", "373"},
	"environment": {"400", "410", "411"},
}

var phrasePictos = []struct {
	re   *regexp.Regexp
	pict string
}{
	{regexp.MustCompile(`(?i)\bexplosive|may mass explode|heating may cause an explosion`), "explosive"},
	{regexp.MustCompile(`(?i)\bflammable|catches fire|heating may cause a fire`), "flame"},
	{regexp.MustCompile(`(?i)may cause or intensify fire|oxidizer`), "oxidizer"},
	{regexp.MustCompile(`(?i)contains gas under pressure`), "gas"},
	{regexp.MustCompile(`(?i)severe skin burns|causes serious eye damage|corrosive to metals`), "corrosive"},
	{regexp.MustCompile(`(?i)(fatal|toxic) (if swallowed|in contact with skin|if inhaled)`), "skull"},
	{regexp.MustCompile(`(?i)causes serious eye irritation|causes skin irritation|harmful if (swallowed|inhaled)|harmful in contact with skin|allergic skin reaction|respiratory irritation|drowsiness or dizziness`), "bang"},
	{regexp.MustCompile(`(?i)may cause cancer|suspected of causing|damage to organs|enters airways|allergy or asthma|may damage (fertility|the unborn)`), "health"},
	{regexp.MustCompile(`(?i)toxic to aquatic life`), "environment"},
}

// hText is the standard wording of each hazard statement, used when a sheet lists only the codes.
var hText = map[string]string{
	"200": "Unstable explosive.", "201": "Explosive; mass explosion hazard.", "202": "Explosive; severe projection hazard.", "203": "Explosive; fire, blast or projection hazard.",
	"204": "Fire or projection hazard.", "205": "May mass explode in fire.", "220": "Extremely flammable gas.", "221": "Flammable gas.", "222": "Extremely flammable aerosol.",
	"223": "Flammable aerosol.", "224": "Extremely flammable liquid and vapor.", "225": "Highly flammable liquid and vapor.", "226": "Flammable liquid and vapor.", "228": "Flammable solid.",
	"229": "Pressurized container: may burst if heated.", "230": "May react explosively even in the absence of air.", "240": "Heating may cause an explosion.",
	"241": "Heating may cause a fire or explosion.", "242": "Heating may cause a fire.", "250": "Catches fire spontaneously if exposed to air.", "251": "Self-heating; may catch fire.",
	"252": "Self-heating in large quantities; may catch fire.", "260": "In contact with water releases flammable gases which may ignite spontaneously.",
	"261": "In contact with water releases flammable gas.", "270": "May cause or intensify fire; oxidizer.", "271": "May cause fire or explosion; strong oxidizer.", "272": "May intensify fire; oxidizer.",
	"280": "Contains gas under pressure; may explode if heated.", "281": "Contains refrigerated gas; may cause cryogenic burns or injury.", "290": "May be corrosive to metals.",
	"300": "Fatal if swallowed.", "301": "Toxic if swallowed.", "302": "Harmful if swallowed.", "304": "May be fatal if swallowed and enters airways.", "310": "Fatal in contact with skin.",
	"311": "Toxic in contact with skin.", "312": "Harmful in contact with skin.", "314": "Causes severe skin burns and eye damage.", "315": "Causes skin irritation.",
	"316": "Causes mild skin irritation.", "317": "May cause an allergic skin reaction.", "318": "Causes serious eye damage.", "319": "Causes serious eye irritation.", "320": "Causes eye irritation.",
	"330": "Fatal if inhaled.", "331": "Toxic if inhaled.", "332": "Harmful if inhaled.", "334": "May cause allergy or asthma symptoms or breathing difficulties if inhaled.",
	"335": "May cause respiratory irritation.", "336": "May cause drowsiness or dizziness.", "340": "May cause genetic defects.", "341": "Suspected of causing genetic defects.",
	"350": "May cause cancer.", "351": "Suspected of causing cancer.", "360": "May damage fertility or the unborn child.", "361": "Suspected of damaging fertility or the unborn child.",
	"362": "May cause harm to breast-fed children.", "370": "Causes damage to organs.", "371": "May cause damage to organs.", "372": "Causes damage to organs through prolonged or repeated exposure.",
	"373": "May cause damage to organs through prolonged or repeated exposure.", "400": "Very toxic to aquatic life.", "401": "Toxic to aquatic life.", "402": "Harmful to aquatic life.",
	"410": "Very toxic to aquatic life with long lasting effects.", "411": "Toxic to aquatic life with long lasting effects.", "412": "Harmful to aquatic life with long lasting effects.",
	"413": "May cause long lasting harmful effects to aquatic life.", "420": "Harms public health and the environment by destroying ozone in the upper atmosphere.",
}

// classPictos reads hazard classes written as names and categories, for sheets that give
// "Skin corrosion: Category 1B" or "Skin Corr. 1B" rather than H-codes.
var classRules = []struct {
	re   *regexp.Regexp
	pict string
}{
	{regexp.MustCompile(`(?i)skin corr[a-z.]*[^\n]{0,40}?\b1[abc]?\b|serious eye damage[^\n]{0,60}?\b1\b|eye dam\.?\s*1\b|corrosive to metals|met\.? corr`), "corrosive"},
	{regexp.MustCompile(`(?i)skin irrit[a-z.]*[^\n]{0,40}?\b2\b|eye irrit[a-z.]*[^\n]{0,60}?\b2a?\b|skin sens[a-z.]*|stot[ -]?se\.?\s*3|specific target organ[^\n]{0,90}?single[^\n]{0,60}?\b3\b|acute tox[a-z.]*[^\n]{0,60}?\b4\b|acute tox\.?\s*4\b`), "bang"},
	{regexp.MustCompile(`(?i)acute tox[a-z.]*[^\n]{0,60}?\b[123]\b|acute tox\.?\s*[123]\b`), "skull"},
	{regexp.MustCompile(`(?i)flammable (?:liquids?|gas(?:es)?|solids?|aerosols?)[^\n]{0,50}?\b[123]\b|flam\.? (?:liq|gas|sol|aer)[a-z.]*\s*[123]\b|self-heating|pyrophoric|water-react`), "flame"},
	{regexp.MustCompile(`(?i)oxidi[sz]ing|ox\.? (?:liq|sol|gas)`), "oxidizer"},
	{regexp.MustCompile(`(?i)gases? under pressure|press\.? gas`), "gas"},
	{regexp.MustCompile(`(?i)carcinogen|germ cell|reproductive tox|aspiration|respiratory sens|resp\.? sens|stot[ -]?re\b|repeated exposure|specific target organ[^\n]{0,90}?(?:repeated|\b[12]\b)`), "health"},
	{regexp.MustCompile(`(?i)(?:aquatic[ -](?:acute|chronic)|(?:acute|chronic)[ -]aquatic)[^\n]{0,40}?\b[12]\b|hazardous to the aquatic[^\n]{0,80}?\b[12]\b`), "environment"},
}

var pictoWords = []struct {
	re   *regexp.Regexp
	pict string
}{
	{regexp.MustCompile(`(?i)exclamation`), "bang"}, {regexp.MustCompile(`(?i)flame over circle`), "oxidizer"},
	{regexp.MustCompile(`(?i)flame`), "flame"}, {regexp.MustCompile(`(?i)corrosion`), "corrosive"}, {regexp.MustCompile(`(?i)skull`), "skull"},
	{regexp.MustCompile(`(?i)health hazard`), "health"}, {regexp.MustCompile(`(?i)gas cylinder`), "gas"}, {regexp.MustCompile(`(?i)exploding bomb`), "explosive"},
	{regexp.MustCompile(`(?i)environment`), "environment"},
}

var pictoLabel = regexp.MustCompile(`(?is)pictograms?[^\n]*(?:\n[^\n]*){0,2}`)

var ghsNumber = map[string]string{"1": "explosive", "2": "flame", "3": "oxidizer", "4": "gas", "5": "corrosive", "6": "skull", "7": "bang", "8": "health", "9": "environment"}

// pictograms works out which GHS diamonds the label carries from the hazard codes and statements.
func pictograms(s2 string) []string {
	set := map[string]bool{}
	byCode := map[string]bool{}
	for _, m := range hCode.FindAllStringSubmatch(s2, -1) {
		byCode[m[1]] = true
	}
	for p, codes := range codePictos {
		for _, c := range codes {
			if byCode[c] {
				set[p] = true
			}
		}
	}
	for _, m := range regexp.MustCompile(`(?i)GHS\s*0?([1-9])\b`).FindAllStringSubmatch(s2, -1) {
		set[ghsNumber[m[1]]] = true
	}
	// Pictograms named in words ("Pictogram(s): Corrosion, Exclamation mark").
	circle := regexp.MustCompile(`(?i)flame over circle`)
	for _, blk := range pictoLabel.FindAllString(s2, -1) {
		if circle.MatchString(blk) {
			set["oxidizer"] = true
			blk = circle.ReplaceAllString(blk, " ")
		}
		for _, pw := range pictoWords {
			if pw.pict != "oxidizer" && pw.re.MatchString(blk) {
				set[pw.pict] = true
			}
		}
	}
	// With no hazard codes on the sheet, work from the written classes and statements.
	if len(byCode) == 0 {
		for _, cr := range classRules {
			if cr.re.MatchString(s2) {
				set[cr.pict] = true
			}
		}
		for _, pp := range phrasePictos {
			if pp.re.MatchString(s2) {
				set[pp.pict] = true
			}
		}
	}
	// Label rules: a corrosive diamond replaces the exclamation mark for skin and eye irritation,
	// and the skull replaces it for "harmful" acute toxicity.
	if set["bang"] {
		onlyIrritation := true
		for _, c := range codePictos["bang"] {
			if byCode[c] && c != "315" && c != "319" {
				onlyIrritation = false
			}
		}
		if set["corrosive"] && onlyIrritation && len(byCode) > 0 {
			delete(set, "bang")
		}
		if set["skull"] && (byCode["302"] || byCode["312"] || byCode["332"]) {
			onlyHarmful := true
			for _, c := range codePictos["bang"] {
				if byCode[c] && c != "302" && c != "312" && c != "332" {
					onlyHarmful = false
				}
			}
			if onlyHarmful {
				delete(set, "bang")
			}
		}
	}
	var out []string
	for _, k := range []string{"flame", "oxidizer", "explosive", "corrosive", "gas", "skull", "bang", "health", "environment"} {
		if set[k] {
			out = append(out, k)
		}
	}
	return out
}

var signalRe = regexp.MustCompile(`(?is)signal\s*words?\s*[:\-–]?\s*(danger|warning)\b`)

func hazardText(s2 []string) string {
	var sents []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.TrimSpace(strings.Trim(t, " ;,"))
		if t == "" {
			return
		}
		if !strings.HasSuffix(t, ".") {
			t += "."
		}
		if k := strings.ToLower(t); !seen[k] {
			seen[k] = true
			sents = append(sents, t)
		}
	}
	for _, l := range s2 {
		if m := hLine.FindStringSubmatch(l); m != nil {
			add(m[1])
		}
	}
	if len(sents) == 0 {
		v := labelled(s2, regexp.MustCompile(`(?i)^\W*hazard statements?`), 6)
		if rest := strings.TrimSpace(hCode.ReplaceAllString(v, "")); hCode.MatchString(v) && len(strings.Trim(rest, " ,;.+-–")) < 8 {
			v = "" // only codes were listed: use the standard wording instead
		}
		if v != "" {
			for _, part := range regexp.MustCompile(`(?:\.\s+|;\s+)`).Split(v, -1) {
				if len(part) > 3 && !regexp.MustCompile(`(?i)^precaution`).MatchString(part) {
					add(part)
				}
			}
		}
	}
	if len(sents) == 0 {
		seenCode := map[string]bool{}
		for _, l := range s2 {
			for _, m := range hCode.FindAllStringSubmatch(l, -1) {
				if txt, ok := hText[m[1]]; ok && !seenCode[m[1]] {
					seenCode[m[1]] = true
					add(txt)
				}
			}
		}
	}
	return clip(strings.Join(sents, " "), 520)
}

// ---------------------------------------------------------------- first aid and PPE

var routeRe = map[string]*regexp.Regexp{
	"eyes":       regexp.MustCompile(`(?i)^\W*(?:(?:in case of|if in|if on|if|after|following|first aid|contact with)\s+)?(?:the\s+)?eyes?\b(?:\s*(?:contact|exposure|/\s*face))?`),
	"skin":       regexp.MustCompile(`(?i)^\W*(?:(?:in case of|if in|if on|if|after|following|first aid|contact with)\s+)?(?:the\s+)?(?:skin|dermal)\b(?:\s*(?:contact|exposure))?`),
	"inhalation": regexp.MustCompile(`(?i)^\W*(?:(?:in case of|if|after|following|first aid)\s+)?(?:inhal\w*|breath\w*)\b(?:\s*(?:contact|exposure))?`),
	"swallowed":  regexp.MustCompile(`(?i)^\W*(?:(?:in case of|if|after|following|first aid)\s+)?(?:ingest\w*|swallow\w*)\b(?:\s*(?:contact|exposure))?`),
}

func firstAid(s4 []string) map[string]string {
	out := map[string]string{}
	cur := ""
	for _, raw := range s4 {
		l := strings.TrimSpace(raw)
		if l == "" {
			cur = ""
			continue
		}
		matched := ""
		var rest string
		for k, re := range routeRe {
			if m := re.FindStringIndex(l); m != nil {
				r := strings.TrimLeft(l[m[1]:], " \t:–-.")
				// "Skin protection", "eye wash station" and the like are not instructions.
				if len(l[:m[1]]) > 0 && (len(r) == 0 || strings.Contains(l[m[1]:min(len(l), m[1]+3)], ":") || regexp.MustCompile(`^[\s:–-]`).MatchString(l[m[1]:]) || r != "") {
					if !regexp.MustCompile(`(?i)^(protection|wash|irrigat)`).MatchString(strings.TrimLeft(l[m[1]:], " ")) {
						matched, rest = k, r
						break
					}
				}
			}
		}
		switch {
		case matched != "" && out[matched] == "":
			cur = matched
			out[cur] = rest
		case matched != "":
			cur = matched
		case genericLabel.MatchString(l) && !strings.HasPrefix(l, ":"):
			cur = ""
		case cur != "":
			out[cur] = strings.TrimSpace(out[cur] + " " + l)
		}
	}
	for k, v := range out {
		out[k] = clip(v, 380)
	}
	return out
}

var ppeRes = []struct {
	label string
	re    *regexp.Regexp
}{
	{"Eyes", regexp.MustCompile(`(?i)^\W*(?:eye|eye\s*/\s*face|eye and face|face|eye or face)\s*(?:and face\s*)?protection`)},
	{"Hands", regexp.MustCompile(`(?i)^\W*(?:hand|glove)s?\s*protection|^\W*gloves?\b`)},
	{"Skin/body", regexp.MustCompile(`(?i)^\W*(?:skin|body|skin and body|protective clothing|clothing)\s*(?:and body\s*)?protection|^\W*protective clothing`)},
	{"Respiratory", regexp.MustCompile(`(?i)^\W*respiratory\s*protection|^\W*respirators?\b`)},
}

func ppeText(s8 []string) string {
	var parts []string
	for _, p := range ppeRes {
		v := labelled(s8, p.re, 4)
		if v == "" {
			continue
		}
		parts = append(parts, p.label+": "+clip(v, 210))
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------- identity

var monthNum = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}

const datePat = `(\d{1,2}[/.\-]\d{1,2}[/.\-]\d{4}|\d{4}[/.\-]\d{1,2}[/.\-]\d{1,2}|[A-Za-z]{3,9}\.? \d{1,2},? \d{4}|\d{1,2}[ \-][A-Za-z]{3,9}\.?[ \-,]+\d{4})`

var revLabel = regexp.MustCompile(`(?i)(revision date|date of (?:last )?revision|revised(?: on)?|last revised|revision)\s*[:\-–]?\s*` + datePat)
var issueLabel = regexp.MustCompile(`(?i)(issuing date|issue date|date of issue|date issued|date prepared|preparation date|date of preparation|version date|effective date|sds date|date)\s*[:\-–]?\s*` + datePat)

func toISO(s string) (iso string, ambiguous bool) {
	s = strings.TrimSpace(s)
	if m := regexp.MustCompile(`^(\d{4})[/.\-](\d{1,2})[/.\-](\d{1,2})$`).FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return mk(y, mo, d)
	}
	if m := regexp.MustCompile(`^(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4})$`).FindStringSubmatch(s); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		if a > 12 { // day first
			iso, _ := mk(y, b, a)
			return iso, false
		}
		iso, _ := mk(y, a, b) // month first, as is usual on U.S. sheets
		return iso, a != b && b <= 12
	}
	if m := regexp.MustCompile(`^([A-Za-z]{3})[A-Za-z]*\.? (\d{1,2}),? (\d{4})$`).FindStringSubmatch(s); m != nil {
		if mo, ok := monthNum[strings.ToLower(m[1])]; ok {
			d, _ := strconv.Atoi(m[2])
			y, _ := strconv.Atoi(m[3])
			return mk(y, mo, d)
		}
	}
	if m := regexp.MustCompile(`^(\d{1,2})[ \-]([A-Za-z]{3})[A-Za-z]*\.?[ \-,]+(\d{4})$`).FindStringSubmatch(s); m != nil {
		if mo, ok := monthNum[strings.ToLower(m[2])]; ok {
			d, _ := strconv.Atoi(m[1])
			y, _ := strconv.Atoi(m[3])
			return mk(y, mo, d)
		}
	}
	return "", false
}

func mk(y, m, d int) (string, bool) {
	if y < 1990 || y > 2100 || m < 1 || m > 12 || d < 1 || d > 31 {
		return "", false
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d), false
}

func sheetDate(text string) (iso string, ambiguous bool) {
	for _, re := range []*regexp.Regexp{revLabel, issueLabel} {
		var best string
		var amb bool
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if d, a := toISO(m[2]); d != "" && d > best {
				best, amb = d, a
			}
		}
		if best != "" {
			return best, amb
		}
	}
	return "", false
}

var nameRe = regexp.MustCompile(`(?i)^\W*(?:product name|product identifier|trade name|product)\s*[:\-–]`)
var makerRe = regexp.MustCompile(`(?i)^\W*(?:manufacturer(?:\s*/\s*supplier)?|manufacturer or supplier'?s? details|company(?: name)?|supplier|producer|company/undertaking identification|manufactured by|distributor)\s*[:\-–]`)

// ---------------------------------------------------------------- the draft

func isOK(s string) bool { return strings.TrimSpace(s) != "" }

// stripRunning removes the headers and footers repeated on every page (company name, product
// name, "Page 2 of 8"), which would otherwise be glued into the middle of a paragraph.
func stripRunning(pages []string) []string {
	if len(pages) < 2 {
		return pages
	}
	digits := regexp.MustCompile(`\d+`)
	norm := func(s string) string {
		return digits.ReplaceAllString(strings.ToLower(strings.Join(strings.Fields(s), " ")), "#")
	}
	edge := func(lines []string) []int {
		var idx []int
		for i, l := range lines {
			if strings.TrimSpace(l) != "" {
				idx = append(idx, i)
			}
		}
		var out []int
		for k, i := range idx {
			if k < 4 || k >= len(idx)-4 {
				out = append(out, i)
			}
		}
		return out
	}
	count := map[string]int{}
	split := make([][]string, len(pages))
	for i, p := range pages {
		split[i] = strings.Split(p, "\n")
		seen := map[string]bool{}
		for _, j := range edge(split[i]) {
			k := norm(split[i][j])
			if len(k) > 2 && !seen[k] {
				seen[k] = true
				count[k]++
			}
		}
	}
	out := make([]string, len(pages))
	for i := range split {
		drop := map[int]bool{}
		for _, j := range edge(split[i]) {
			if count[norm(split[i][j])] >= max(2, len(pages)*2/3) && !sectionHead.MatchString(split[i][j]) {
				drop[j] = true
			}
		}
		var keep []string
		for j, l := range split[i] {
			if !drop[j] {
				keep = append(keep, l)
			}
		}
		out[i] = strings.Join(keep, "\n")
	}
	return out
}

func draftFromText(pages []string) *Draft {
	pages = stripRunning(pages)
	all := strings.Join(pages, "\n")
	var lines []string
	for _, l := range strings.Split(all, "\n") {
		lines = append(lines, strings.TrimRight(l, " \t\r"))
	}
	secs := splitSections(lines)
	d := &Draft{Pictograms: []string{}, Found: []string{}, Missing: []string{}, Notes: []string{}}

	if s1 := secs[1]; len(s1) > 0 {
		if v := labelled(s1, nameRe, 2); v != "" {
			d.Name = tidyName(v)
		}
		for i, l := range s1 {
			if makerRe.MatchString(l) && !regexp.MustCompile(`(?i)emergency`).MatchString(l) {
				v := strings.TrimLeft(makerRe.ReplaceAllString(l, ""), " \t:–-")
				if v == "" {
					for _, n := range s1[i+1:] {
						if strings.TrimSpace(n) != "" {
							v = strings.TrimSpace(n)
							break
						}
					}
				}
				if v != "" && !regexp.MustCompile(`^\d`).MatchString(v) {
					d.Manufacturer = cleanName(v)
					break
				}
			}
		}
	}
	if iso, amb := sheetDate(all); iso != "" {
		d.RevisionDate = iso
		if amb {
			d.Notes = append(d.Notes, "The sheet's date could be read as month/day or day/month. It was read as month/day. Check it.")
		}
	}

	s2 := secs[2]
	s2text := strings.Join(s2, "\n")
	src := s2text
	if len(s2) == 0 {
		src = all
	}
	if m := signalRe.FindStringSubmatch(src); m != nil {
		d.SignalWord = strings.ToUpper(m[1][:1]) + strings.ToLower(m[1][1:])
	}
	if len(s2) > 0 {
		d.Hazards = hazardText(s2)
		d.Pictograms = pictograms(s2text)
	}

	if s4 := secs[4]; len(s4) > 0 {
		fa := firstAid(s4)
		d.FirstAidEyes, d.FirstAidSkin, d.FirstAidInhalation, d.FirstAidSwallowed = fa["eyes"], fa["skin"], fa["inhalation"], fa["swallowed"]
	}
	if s8 := secs[8]; len(s8) > 0 {
		d.PPE = ppeText(s8)
	}

	have := func(label string, v string) {
		if isOK(v) {
			d.Found = append(d.Found, label)
		} else {
			d.Missing = append(d.Missing, label)
		}
	}
	have("Product name", d.Name)
	have("Manufacturer", d.Manufacturer)
	have("SDS date", d.RevisionDate)
	have("Signal word", d.SignalWord)
	have("Hazards", d.Hazards)
	have("First aid: eyes", d.FirstAidEyes)
	have("First aid: skin", d.FirstAidSkin)
	have("First aid: inhalation", d.FirstAidInhalation)
	have("First aid: swallowed", d.FirstAidSwallowed)
	have("Protective equipment", d.PPE)
	if len(d.Pictograms) > 0 {
		d.Found = append(d.Found, "Pictograms")
	}
	sort.Strings(d.Notes)
	if len(secs) == 0 {
		d.Notes = append(d.Notes, "This sheet does not follow the standard 16-section layout, so nothing could be located. Type the summary from Sections 2, 4 and 8.")
	}
	d.Notes = append(d.Notes, "Pictograms are worked out from the hazard codes and statements in Section 2. Tick only the diamonds actually printed on the sheet.")
	return d
}

// extractDraft reads a PDF and drafts its summary.
func extractDraft(data []byte) (*Draft, error) {
	pages, err := extractText(data)
	if err != nil {
		return nil, err
	}
	return draftFromText(pages), nil
}
