package ui

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

func TestPrettyJSONOnlyMovesWhitespace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{`{"a":1,"b":[1,2,{"c":"x,y:{z}"}],"e":{},"f":[]}`,
			"{\n  \"a\": 1,\n  \"b\": [\n    1,\n    2,\n    {\n      \"c\": \"x,y:{z}\"\n    }\n  ],\n  \"e\": {},\n  \"f\": []\n}", true},
		{`{"big":12345678901234567890,"dup":1,"dup":2}`,
			"{\n  \"big\": 12345678901234567890,\n  \"dup\": 1,\n  \"dup\": 2\n}", true},
		{`{"s":"quote \" brace } é"}`, "{\n  \"s\": \"quote \\\" brace } é\"\n}", true},
		{` [ ] `, "[]", true},
		{`{"a":1`, "", false},
		{`{"a":1} … [display truncated]`, "", false},
		{`{"a":1}{"b":2}`, "", false},
		{`"str"`, "", false},
		{`{"a":[1}`, "", false},
	} {
		got, ok := PrettyJSON(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("PrettyJSON(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLogLevelClassifiesLikeTheWebConsole(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`{"level":"ERROR"}`:                 "error",
		`  {"log.level":"warning"}`:         "warn",
		`{"severity":"notice"}`:             "info",
		`{"lvl":"trace"}`:                   "debug",
		`{"level":"loud","severity":"err"}`: "",      // first string key wins, unrecognised
		`{"level":5,"severity":"err"}`:      "error", // non-string keys are skipped
		`[{"level":"error"}]`:               "",
		`ERROR: plain`:                      "",
		`{"level":"error"`:                  "",
	} {
		if got := LogLevel(in); got != want {
			t.Errorf("LogLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLogLevelTablesMatchTheWebConsole pins logLevelKeys and logLevelMap to
// LEVEL_KEYS and LEVEL_MAP in the web console's page, so a line never reads as
// one level in the browser and another in the terminal.
func TestLogLevelTablesMatchTheWebConsole(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../web/console.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)

	keys := regexp.MustCompile(`var LEVEL_KEYS = \[([^\]]*)\];`).FindStringSubmatch(page)
	if keys == nil {
		t.Fatal("console.html no longer declares LEVEL_KEYS")
	}
	if got, want := strings.ReplaceAll(keys[1], " ", ""), `"`+strings.Join(logLevelKeys, `","`)+`"`; got != want {
		t.Errorf("LEVEL_KEYS = %s, Go has %s", got, want)
	}

	block := regexp.MustCompile(`(?s)var LEVEL_MAP = \{(.*?)\};`).FindStringSubmatch(page)
	if block == nil {
		t.Fatal("console.html no longer declares LEVEL_MAP")
	}
	var web []string
	for _, kv := range regexp.MustCompile(`(\w+): "(\w+)"`).FindAllStringSubmatch(block[1], -1) {
		web = append(web, kv[1]+"="+kv[2])
	}
	var goSide []string
	for k, v := range logLevelMap {
		goSide = append(goSide, k+"="+v)
	}
	sort.Strings(web)
	sort.Strings(goSide)
	if strings.Join(web, ",") != strings.Join(goSide, ",") {
		t.Errorf("LEVEL_MAP differs:\n web %v\n  go %v", web, goSide)
	}
}

func TestRedactStatusesCoversDetailToo(t *testing.T) {
	t.Parallel()
	raw := "http://admin:hunter2@localhost:7100/health?api_key=sk-live-DEADBEEF"
	sts := []supervisor.Status{{Health: raw, Detail: "HEAD " + raw + ": refused"}, {}}
	RedactStatuses(sts)
	for _, s := range []string{sts[0].Health, sts[0].Detail} {
		if strings.Contains(s, "hunter2") || strings.Contains(s, "DEADBEEF") {
			t.Errorf("credential survived redaction: %q", s)
		}
	}
}
