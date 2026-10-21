package newsmon

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Rules score headlines 0-100. A rule matches when its text has any of its Any
// phrases, all of its All phrases and none of its Not phrases; a headline's
// impact is the highest of its matching rules', less Damp.By when the headline
// is speculative ("ahead of", "preview") and the rule allows damping. The score
// says how much a headline may move markets, never which way to trade.
type Rules struct {
	Damp  Damp   `yaml:"damp"`
	Rules []Rule `yaml:"rules"`
}

// Damp lowers the score of headlines about what may happen, not what did.
type Damp struct {
	By  int      `yaml:"by"`
	Any []string `yaml:"any"`

	re []*regexp.Regexp
}

// Rule is one entry of the rules file. Phrases match whole words, ignoring case
// and runs of spaces; a phrase starting "re:" is a regular expression instead.
type Rule struct {
	ID      string   `yaml:"id"`
	Impact  int      `yaml:"impact"`
	Tags    []string `yaml:"tags"`
	Any     []string `yaml:"any"`
	All     []string `yaml:"all"`
	Not     []string `yaml:"not"`
	Trust   []string `yaml:"trust"`   // only for these trusts; empty: any
	Sources []string `yaml:"sources"` // only for these sources; empty: any
	Field   string   `yaml:"field"`   // title (default) or title_summary
	NoDamp  bool     `yaml:"no_damp"` // speculative wording does not lower this rule
	Note    string   `yaml:"note"`

	any, all, not []*regexp.Regexp
}

// Score is a headline's classification.
type Score struct {
	Impact int
	Tags   []string
	Rules  []string // matching rule ids, highest first
}

// LoadRules reads and checks the rules file.
func LoadRules(path string) (*Rules, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRules(b)
}

// ParseRules parses and checks a rules file.
func ParseRules(b []byte) (*Rules, error) {
	var rs Rules
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	if rs.Damp.By < 0 || rs.Damp.By > 100 {
		return nil, fmt.Errorf("rules: damp.by must be in 0..100")
	}
	var err error
	if rs.Damp.re, err = compile(rs.Damp.Any); err != nil {
		return nil, fmt.Errorf("rules: damp: %w", err)
	}
	seen := map[string]bool{}
	for i := range rs.Rules {
		r := &rs.Rules[i]
		switch {
		case r.ID == "":
			return nil, fmt.Errorf("rules[%d]: id is required", i)
		case seen[r.ID]:
			return nil, fmt.Errorf("rules: duplicate id %q", r.ID)
		case r.Impact < 0 || r.Impact > 100:
			return nil, fmt.Errorf("rules %s: impact must be in 0..100", r.ID)
		case len(r.Any) == 0 && len(r.All) == 0:
			return nil, fmt.Errorf("rules %s: needs any or all", r.ID)
		case r.Field != "" && r.Field != "title" && r.Field != "title_summary":
			return nil, fmt.Errorf("rules %s: field must be title or title_summary", r.ID)
		}
		for _, t := range r.Trust {
			if t != "official" && t != "exchange" && t != "media" {
				return nil, fmt.Errorf("rules %s: trust %q is not official, exchange or media", r.ID, t)
			}
		}
		seen[r.ID] = true
		for _, p := range []struct {
			src []string
			dst *[]*regexp.Regexp
		}{{r.Any, &r.any}, {r.All, &r.all}, {r.Not, &r.not}} {
			if *p.dst, err = compile(p.src); err != nil {
				return nil, fmt.Errorf("rules %s: %w", r.ID, err)
			}
		}
	}
	return &rs, nil
}

var spaceRe = regexp.MustCompile(`\s+`)

func compile(ps []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(ps))
	for _, p := range ps {
		var expr string
		if rest, ok := strings.CutPrefix(p, "re:"); ok {
			expr = "(?i)" + rest
		} else {
			p = strings.TrimSpace(p)
			if p == "" {
				return nil, fmt.Errorf("empty phrase")
			}
			expr = spaceRe.ReplaceAllString(regexp.QuoteMeta(p), `\s+`)
			if isWord(p[0]) {
				expr = `\b` + expr
			}
			if isWord(p[len(p)-1]) {
				expr += `\b`
			}
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("phrase %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func anyOf(rs []*regexp.Regexp, s string) bool {
	for _, r := range rs {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

func allOf(rs []*regexp.Regexp, s string) bool {
	for _, r := range rs {
		if !r.MatchString(s) {
			return false
		}
	}
	return true
}

// quotes folds typographic apostrophes and dashes so phrases match either form.
var quotes = strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`, "–", "-", "—", "-")

// Classify scores one headline.
func (rs *Rules) Classify(it *Item) Score {
	var sc Score
	if rs == nil {
		return sc
	}
	title := quotes.Replace(it.Title)
	both := title + " \n " + quotes.Replace(it.Summary)
	damped := anyOf(rs.Damp.re, title)
	type hit struct {
		id string
		n  int
	}
	var hits []hit
	for i := range rs.Rules {
		r := &rs.Rules[i]
		if len(r.Trust) > 0 && !slices.Contains(r.Trust, it.Trust) || len(r.Sources) > 0 && !slices.Contains(r.Sources, it.Source) {
			continue
		}
		text := title
		if r.Field == "title_summary" {
			text = both
		}
		if len(r.any) > 0 && !anyOf(r.any, text) || !allOf(r.all, text) || anyOf(r.not, text) {
			continue
		}
		n := r.Impact
		if damped && !r.NoDamp {
			n = max(0, n-rs.Damp.By)
		}
		hits = append(hits, hit{r.ID, n})
		for _, t := range r.Tags {
			if !slices.Contains(sc.Tags, t) {
				sc.Tags = append(sc.Tags, t)
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].n > hits[j].n })
	for _, h := range hits {
		sc.Rules = append(sc.Rules, h.id)
		sc.Impact = max(sc.Impact, h.n)
	}
	sort.Strings(sc.Tags)
	return sc
}
