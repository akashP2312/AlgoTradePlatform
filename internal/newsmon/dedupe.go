package newsmon

import (
	"time"

	"pmt/internal/bus"
)

// Deduper remembers headlines for a TTL and says when a new one is a copy: the
// same URL, the same GUID from the same source, the same title, or (across
// sources only) a title sharing at least Similar of its content words.
type Deduper struct {
	TTL     time.Duration
	Similar float64

	keys   map[string]seenKey
	recent []recentTitle
}

type seenKey struct {
	id, src string
	at      time.Time
}

type recentTitle struct {
	id, src string
	words   []string
	at      time.Time
}

// minWords is the shortest title near-duplicate matching applies to: below it a
// shared word or two is coincidence.
const minWords = 4

// minTitleKey is the shortest title that matches across sources verbatim. A
// shorter one ("Press Release") is generic: it only matches within its source,
// and only for entries with neither link nor GUID to tell them apart.
const minTitleKey = 16

func NewDeduper(ttl time.Duration, similar float64) *Deduper {
	return &Deduper{TTL: ttl, Similar: similar, keys: map[string]seenKey{}}
}

func (d *Deduper) itemKeys(it *Item) []string {
	var ks []string
	if it.canon != "" {
		ks = append(ks, "u:"+it.canon)
	}
	if it.GUID != "" {
		ks = append(ks, "g:"+it.Source+"|"+it.GUID)
	}
	if tk := titleKey(it.Title); len(tk) >= minTitleKey {
		ks = append(ks, "t:"+tk)
	} else if tk != "" && it.canon == "" && it.GUID == "" {
		ks = append(ks, "t:"+it.Source+"|"+tk)
	}
	return ks
}

// Check reports whether it copies a headline seen within the TTL, and which one
// (its id and source). A headline that is new is remembered; so are a copy's
// keys, under the original, so later copies of the copy match too.
func (d *Deduper) Check(it *Item) (origID, origSrc string, dup bool) {
	now := bus.FromMs(it.Seen)
	ks := d.itemKeys(it)
	for _, k := range ks {
		if e, ok := d.keys[k]; ok && now.Sub(e.at) < d.TTL {
			origID, origSrc, dup = e.id, e.src, true
			break
		}
	}
	ws := words(it.Title)
	if !dup && len(ws) >= minWords {
		for i := len(d.recent) - 1; i >= 0; i-- {
			r := &d.recent[i]
			if r.src != it.Source && now.Sub(r.at) < d.TTL && len(r.words) >= minWords && jaccard(ws, r.words) >= d.Similar {
				origID, origSrc, dup = r.id, r.src, true
				break
			}
		}
	}
	id, src := it.ID, it.Source
	if dup {
		id, src = origID, origSrc
	}
	for _, k := range ks {
		if _, ok := d.keys[k]; !ok || !dup {
			d.keys[k] = seenKey{id: id, src: src, at: now}
		}
	}
	if !dup {
		d.recent = append(d.recent, recentTitle{id: it.ID, src: it.Source, words: ws, at: now})
	}
	return origID, origSrc, dup
}

// Prune forgets what is older than the TTL.
func (d *Deduper) Prune(now time.Time) {
	for k, e := range d.keys {
		if now.Sub(e.at) >= d.TTL {
			delete(d.keys, k)
		}
	}
	i := 0
	for i < len(d.recent) && now.Sub(d.recent[i].at) >= d.TTL {
		i++
	}
	d.recent = append(d.recent[:0], d.recent[i:]...)
}

// jaccard is |a∩b| / |a∪b| of two sorted, distinct word lists.
func jaccard(a, b []string) float64 {
	i, j, n := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			n++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	u := len(a) + len(b) - n
	if u == 0 {
		return 0
	}
	return float64(n) / float64(u)
}
