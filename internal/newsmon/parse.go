package newsmon

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
)

// RawItem is one entry as the feed gave it, before normalizing.
type RawItem struct {
	Title string
	Link  string
	GUID  string
	Desc  string
	Pub   string // the feed's timestamp, unparsed
	Cats  []string
}

type rssItem struct {
	Title string   `xml:"title"`
	Links []string `xml:"link"`
	GUID  string   `xml:"guid"`
	Desc  string   `xml:"description"`
	Pub   string   `xml:"pubDate"`
	Date  string   `xml:"http://purl.org/dc/elements/1.1/ date"`
	Cats  []string `xml:"category"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type atomEntry struct {
	Title     string     `xml:"title"`
	Links     []atomLink `xml:"link"`
	ID        string     `xml:"id"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
	Updated   string     `xml:"updated"`
	Published string     `xml:"published"`
	Cats      []struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
}

// feedDoc matches RSS 2.0 (channel/item), RSS 1.0 (rdf:RDF/item) and Atom (feed/entry).
type feedDoc struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
	Items   []rssItem   `xml:"item"`
	Entries []atomEntry `xml:"entry"`
}

// autoClose is xml.HTMLAutoClose less "link", which in RSS is an element with
// text, not HTML's void <link>.
var autoClose = func() []string {
	var s []string
	for _, t := range xml.HTMLAutoClose {
		if t != "link" {
			s = append(s, t)
		}
	}
	return s
}()

// ParseFeed reads an RSS or Atom document. It is lenient the way real feeds need:
// a UTF-8 BOM, HTML entities, unclosed HTML tags, and non-UTF-8 charsets.
func ParseFeed(b []byte) ([]RawItem, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.AutoClose = autoClose
	d.Entity = xml.HTMLEntity
	d.CharsetReader = func(label string, r io.Reader) (io.Reader, error) {
		e, err := htmlindex.Get(label)
		if err != nil {
			return nil, fmt.Errorf("charset %q: %w", label, err)
		}
		return e.NewDecoder().Reader(r), nil
	}
	var doc feedDoc
	if err := d.Decode(&doc); err != nil {
		return nil, fmt.Errorf("feed: %w", err)
	}
	var out []RawItem
	for _, it := range append(doc.Channel.Items, doc.Items...) {
		r := RawItem{Title: it.Title, GUID: strings.TrimSpace(it.GUID), Desc: it.Desc, Pub: it.Pub, Cats: it.Cats}
		for _, l := range it.Links {
			if l = strings.TrimSpace(l); l != "" {
				r.Link = l
				break
			}
		}
		if r.Pub == "" {
			r.Pub = it.Date
		}
		out = append(out, r)
	}
	for _, e := range doc.Entries {
		r := RawItem{Title: e.Title, GUID: strings.TrimSpace(e.ID), Desc: e.Summary, Pub: e.Published}
		if r.Desc == "" {
			r.Desc = e.Content
		}
		if r.Pub == "" {
			r.Pub = e.Updated
		}
		for _, l := range e.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				r.Link = strings.TrimSpace(l.Href)
				break
			}
		}
		for _, c := range e.Cats {
			r.Cats = append(r.Cats, c.Term)
		}
		out = append(out, r)
	}
	if len(out) == 0 && !bytes.Contains(b, []byte("<rss")) &&
		!bytes.Contains(b, []byte("<feed")) && !bytes.Contains(b, []byte("<rdf:RDF")) {
		return nil, fmt.Errorf("feed: not RSS or Atom")
	}
	return out, nil
}

// Zone abbreviations, in minutes east of UTC: Go's parser gives an abbreviation
// it does not know as the local zone a zero offset.
var zoneAbbr = map[string]int{
	"GMT": 0, "UTC": 0, "UT": 0, "Z": 0,
	"EST": -300, "EDT": -240, "CST": -360, "CDT": -300, "PST": -480, "PDT": -420,
	"IST": 330,
}

type layout struct {
	s        string
	zoned    bool // the layout carries an offset or zone
	dateOnly bool
}

var layouts = []layout{
	{time.RFC1123Z, true, false},
	{"Mon, 2 Jan 2006 15:04:05 -0700", true, false},
	{time.RFC1123, true, false},
	{"Mon, 2 Jan 2006 15:04:05 MST", true, false},
	{"Mon, 02 Jan 2006 15:04 -0700", true, false},
	{"Mon, 02 Jan 2006 15:04 MST", true, false},
	{time.RFC3339Nano, true, false},
	{time.RFC3339, true, false},
	{"02 Jan, 2006 -0700", true, true},
	{"02 Jan 2006 15:04:05 -0700", true, false},
	{"Mon, 02 Jan 2006 15:04:05", false, false},
	{"Mon, 2 Jan 2006 15:04:05", false, false},
	{"02-Jan-2006 15:04:05", false, false},
	{"02 Jan 2006 15:04:05", false, false},
	{"2006-01-02 15:04:05", false, false},
	{"2006-01-02T15:04:05", false, false},
	{"02 Jan, 2006", false, true},
	{"2006-01-02", false, true},
}

// ParseTime reads a feed timestamp. A timestamp without a zone is read in loc;
// dateOnly is set when it names a day but no time.
func ParseTime(s string, loc *time.Location) (t time.Time, dateOnly bool, err error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return time.Time{}, false, fmt.Errorf("no timestamp")
	}
	for _, l := range layouts {
		var v time.Time
		var err error
		if l.zoned {
			v, err = time.Parse(l.s, s)
		} else {
			v, err = time.ParseInLocation(l.s, s, loc)
		}
		if err != nil {
			continue
		}
		if l.zoned && strings.Contains(l.s, "MST") {
			if name, off := v.Zone(); off == 0 {
				if m, ok := zoneAbbr[name]; ok {
					v = time.Date(v.Year(), v.Month(), v.Day(), v.Hour(), v.Minute(), v.Second(), v.Nanosecond(), time.FixedZone(name, m*60))
				}
			}
		}
		return v, l.dateOnly, nil
	}
	return time.Time{}, false, fmt.Errorf("timestamp %q: unknown format", s)
}
