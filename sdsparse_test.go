package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func starterPDF(t *testing.T) []byte {
	t.Helper()
	m, _ := filepath.Glob("starter/sheets/*.pdf")
	b, err := os.ReadFile(m[0])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDraftFromRealSheet(t *testing.T) {
	d, err := extractDraft(starterPDF(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", d)
	want := map[string]string{
		"manufacturer": "Ecolab Inc.", "revisionDate": "2025-08-13", "signalWord": "Warning",
		"hazards": "Causes eye irritation.", "eyes": "Rinse with plenty of water.",
		"skin": "Rinse with plenty of water.", "swallowed": "Rinse mouth. Get medical attention if symptoms occur.",
		"inhalation": "Get medical attention if symptoms occur.",
	}
	got := map[string]string{"manufacturer": d.Manufacturer, "revisionDate": d.RevisionDate, "signalWord": d.SignalWord,
		"hazards": d.Hazards, "eyes": d.FirstAidEyes, "skin": d.FirstAidSkin, "swallowed": d.FirstAidSwallowed, "inhalation": d.FirstAidInhalation}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if d.Name != "Keystone Greaselift Degreaser & Fryer Cleaner RTU" {
		t.Errorf("name = %q", d.Name)
	}
	if len(d.Pictograms) != 0 {
		t.Errorf("this sheet prints no pictogram, got %v", d.Pictograms)
	}
	if !strings.Contains(d.PPE, "Eyes:") || !strings.Contains(d.PPE, "Respiratory:") {
		t.Errorf("ppe = %q", d.PPE)
	}
}

func TestPictogramsFromHazardCodes(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"H314 Causes severe skin burns and eye damage.\nH319 Causes serious eye irritation.", "corrosive"},
		{"H225 Highly flammable liquid and vapor.\nH319 Causes serious eye irritation.\nH336 May cause drowsiness.", "flame,bang"},
		{"H302 Harmful if swallowed.\nH410 Very toxic to aquatic life with long lasting effects.", "bang,environment"},
		{"H301 Toxic if swallowed.\nH302 Harmful if swallowed.", "skull"},
		{"H272 May intensify fire; oxidizer.\nH314 Causes severe skin burns.\nH400 Very toxic to aquatic life.", "oxidizer,corrosive,environment"},
		{"H350 May cause cancer.", "health"},
		{"H320 Causes eye irritation.", ""},
		{"Hazard statements: Causes serious eye irritation.", "bang"},
		{"GHS05 GHS07", "corrosive,bang"},
	}
	for _, c := range cases {
		if got := strings.Join(pictograms(c.text), ","); got != c.want {
			t.Errorf("%q -> %q, want %q", c.text, got, c.want)
		}
	}
}

func TestDatesAndSections(t *testing.T) {
	for in, want := range map[string]string{"Revision Date: 03/27/2024": "2024-03-27", "Date of issue: 27 March 2024": "2024-03-27", "Revised: March 5, 2023": "2023-03-05", "Revision date 2022-11-09": "2022-11-09", "Issue date: 27.03.2024": "2024-03-27"} {
		if got, _ := sheetDate(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if _, amb := sheetDate("Revision date: 03/04/2024"); !amb {
		t.Error("03/04/2024 should be flagged as ambiguous")
	}
	if got, _ := sheetDate("Issuing date : 01/01/2020\nRevision date : 05/06/2021"); got != "2021-05-06" {
		t.Errorf("revision date should win over issue date, got %q", got)
	}
}

func TestOtherFirstAidLayouts(t *testing.T) {
	s4 := []string{"4.1 Description of first aid measures", "General advice: Show this sheet to the doctor.", "Eye contact", "Flush with water for 15 minutes.", "Hold eyelids open.", "Skin contact: Wash with soap and water.", "If inhaled: Move to fresh air.", "IF SWALLOWED: Rinse mouth. Do NOT induce vomiting.", "Notes to physician: Treat symptomatically."}
	fa := firstAid(s4)
	if fa["eyes"] != "Flush with water for 15 minutes. Hold eyelids open." || fa["skin"] != "Wash with soap and water." || fa["inhalation"] != "Move to fresh air." || fa["swallowed"] != "Rinse mouth. Do NOT induce vomiting." {
		t.Errorf("%v", fa)
	}
}

func TestNoTextAndEncrypted(t *testing.T) {
	if _, err := extractDraft([]byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF")); err == nil {
		t.Error("a PDF without text should be refused")
	}
	if _, err := extractDraft([]byte("%PDF-1.4\n/Encrypt 5 0 R\n")); err == nil {
		t.Error("an encrypted PDF should be refused")
	}
	if _, err := extractDraft([]byte("not a pdf at all")); err == nil {
		t.Error("garbage should be refused")
	}
}

func TestFolderDropArrivesDrafted(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.s.pdfDir(), "928998-03.pdf"), starterPDF(t), 0o644); err != nil {
		t.Fatal(err)
	}
	e.s.mu.Lock()
	e.s.scan()
	var got *Product
	for _, p := range e.s.lib.Products {
		if p.File == "928998-03.pdf" {
			got = p
		}
	}
	e.s.mu.Unlock()
	if got == nil {
		t.Fatal("dropped PDF was not picked up")
	}
	if got.Name != "Keystone Greaselift Degreaser & Fryer Cleaner RTU" || got.Manufacturer != "Ecolab Inc." || got.RevisionDate != "2025-08-13" || got.SignalWord != "Warning" {
		t.Errorf("not drafted: %+v", got)
	}
	if !got.NeedsReview || got.SummaryVerifiedAt != "" {
		t.Error("a drafted product must still need review and be unverified")
	}
}

func TestExtractEndpoints(t *testing.T) {
	e := newEnv(t)
	code, out := e.send("/api/extract", nil, starterPDF(t))
	if code != 200 || out["manufacturer"] != "Ecolab Inc." {
		t.Fatalf("extract = %d %v", code, out)
	}
	code, out = e.send("/api/extract", nil, pdf("no text here"))
	if code != 200 || out["unreadable"] == nil {
		t.Fatalf("unreadable PDF should come back as a friendly message: %d %v", code, out)
	}
}

func TestPictogramsAndHazardsAcrossLayouts(t *testing.T) {
	wrap := func(s2 string) *Draft {
		return draftFromText([]string{"SECTION 2. HAZARDS IDENTIFICATION\n" + s2 + "\nSECTION 3. COMPOSITION\nx\nSECTION 4. FIRST AID MEASURES\nIf in eyes: rinse.\nSECTION 5. FIRE"})
	}
	cases := []struct{ name, s2, picts, hazContains string }{
		{"classes and aquatic", "Skin corrosion : Category 1B\nSerious eye damage : Category 1\nAcute aquatic toxicity : Category 1\nSignal Word : Danger", "corrosive,environment", ""},
		{"abbreviated classes", "2.1 Classification\nSkin Corr. 1B; Eye Dam. 1; Aquatic Acute 1\nSignal word: DANGER", "corrosive,environment", ""},
		{"pictogram words", "Pictogram(s): Corrosion, Exclamation mark\nSignal word: Danger", "corrosive,bang", ""},
		{"flame over circle", "Pictogram: Flame over circle\nSignal word: Danger", "oxidizer", ""},
		{"classification table", "Skin irritation Category 2\nEye irritation Category 2A\nSpecific target organ toxicity - single exposure Category 3\nSignal word: Warning", "bang", ""},
		{"codes only", "Hazard statements: H315, H319, H335\nSignal word: Warning", "bang", "Causes skin irritation. Causes serious eye irritation. May cause respiratory irritation."},
		{"not classified", "Acute toxicity : Not classified\nSkin corrosion : Not classified\nSignal word: No signal word", "", ""},
		{"eye irritation 2B", "Eye irritation : Category 2B\nSignal Word : Warning\nHazard Statements : Causes eye irritation.", "", "Causes eye irritation."},
	}
	for _, c := range cases {
		d := wrap(c.s2)
		if got := strings.Join(d.Pictograms, ","); got != c.picts {
			t.Errorf("%s: pictograms %q, want %q", c.name, got, c.picts)
		}
		if c.hazContains != "" && !strings.Contains(d.Hazards, c.hazContains) {
			t.Errorf("%s: hazards %q, want it to contain %q", c.name, d.Hazards, c.hazContains)
		}
	}
}

func TestLocationsMigrate(t *testing.T) {
	s := &store{}
	s.lib.Locations = append([]string{}, oldDefaultLocations...)
	if !s.migrateLocations() || s.lib.Locations[0] != "Drystock" || len(s.lib.Locations) != 6 {
		t.Errorf("old default list should become the new one: %v", s.lib.Locations)
	}
	s.lib.Locations = []string{"Dish Room", "Walk-in"}
	if s.migrateLocations() || len(s.lib.Locations) != 2 {
		t.Error("a customised list must not be replaced")
	}
}
