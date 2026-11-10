package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The page speaks the website's languages. Each one is a flat dictionary
// of key to text under locales/, English being the reference: a key
// missing elsewhere falls back to it, and the tests keep every dictionary
// to the same keys and the same format verbs.

//go:embed locales/*.json
var localeFS embed.FS

// Language is one entry of the page's language menu.
type Language struct {
	Code string
	Name string // in the language itself
}

// Languages are the page's, in the order of the menu.
var Languages = []Language{
	{"en", "English"}, {"de", "Deutsch"}, {"es", "Español"}, {"fr", "Français"}, {"it", "Italiano"},
	{"ja", "日本語"}, {"ko", "한국어"}, {"pt", "Português"}, {"ru", "Русский"}, {"zh", "中文"},
}

const langCookie = "redundanet_lang"

var catalogs = loadCatalogs()

func loadCatalogs() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, l := range Languages {
		b, err := localeFS.ReadFile("locales/" + l.Code + ".json")
		if err != nil {
			continue // a language without its dictionary reads as English
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			panic("api: locales/" + l.Code + ".json: " + err.Error())
		}
		out[l.Code] = m
	}
	if out["en"] == nil {
		panic("api: the English dictionary is missing")
	}
	return out
}

func knownLang(code string) bool {
	for _, l := range Languages {
		if l.Code == code {
			return true
		}
	}
	return false
}

// langOf is the page's language for a request: the one chosen on the page,
// else the browser's first preference the page speaks, else English.
func langOf(r *http.Request) string {
	if c, err := r.Cookie(langCookie); err == nil && knownLang(c.Value) {
		return c.Value
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		if i := strings.IndexAny(tag, "-_"); i > 0 {
			tag = tag[:i]
		}
		if knownLang(tag) {
			return tag
		}
	}
	return "en"
}

// tr translates for one language.
type tr string

// T is the text for key, with args formatted in as by fmt.Sprintf.
func (l tr) T(key string, args ...any) string {
	text, ok := catalogs[string(l)][key]
	if !ok {
		if text, ok = catalogs["en"][key]; !ok {
			return key
		}
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// Since says how long ago t was, in the page's language.
func (l tr) Since(t time.Time) string {
	if t.IsZero() {
		return l.T("time.never")
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return l.T("time.now")
	case d < time.Hour:
		return l.T("time.minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return l.T("time.hours", int(d.Hours()))
	case d < 60*24*time.Hour:
		return l.T("time.days", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}

// Bytes is n in the unit a person reads.
func (tr) Bytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTP"[exp])
}
