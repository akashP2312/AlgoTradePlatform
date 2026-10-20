package gateway

import (
	"fmt"
	"strings"
)

// Prefixes a browser may subscribe to. book.* is the broker's positions and margins,
// risk.* the riskengine's view of them, both read-only. Orders, alerts and the session bucket are not exposed, and nothing a
// browser sends is ever published to the bus.
var allowed = []string{"tick.", "analytics.", "sys.", "book.", "risk."}

// checkSubject validates a subscription: an allowed prefix, no empty tokens,
// '*' only as a whole token and '>' only as the last one.
func checkSubject(s string) error {
	if len(s) > 256 {
		return fmt.Errorf("subject too long")
	}
	ok := false
	for _, p := range allowed {
		if strings.HasPrefix(s, p) && len(s) > len(p) {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("%q: subscribe to tick.*, analytics.*, sys.*, book.* or risk.* only", s)
	}
	toks := strings.Split(s, ".")
	for i, t := range toks {
		switch {
		case t == "":
			return fmt.Errorf("%q: empty token", s)
		case t == ">" && i != len(toks)-1:
			return fmt.Errorf("%q: '>' must be last", s)
		case t != "*" && t != ">" && strings.ContainsAny(t, "*> \t\r\n"):
			return fmt.Errorf("%q: bad token %q", s, t)
		}
	}
	return nil
}

func isPattern(s string) bool {
	for _, t := range strings.Split(s, ".") {
		if t == "*" || t == ">" {
			return true
		}
	}
	return false
}

// match reports whether subject matches pattern with NATS wildcard rules.
func match(pattern, subject string) bool {
	for {
		pt, prest, pmore := strings.Cut(pattern, ".")
		st, srest, smore := strings.Cut(subject, ".")
		switch {
		case pt == ">":
			return true
		case pt != "*" && pt != st:
			return false
		case !pmore || !smore:
			return pmore == smore
		}
		pattern, subject = prest, srest
	}
}
