package loaders

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PartOIURL is NSE's participant-wise OI file for date. Verify the path.
func PartOIURL(base string, date time.Time) string {
	return fmt.Sprintf("%s/content/nsccl/fao_participant_oi_%s.csv", strings.TrimRight(base, "/"), date.Format("02012006"))
}

// partCols maps the file's long/short column pairs to our instrument names.
// Index options come as separate call and put columns.
var partCols = []struct{ inst, long, short string }{
	{"fut_idx", "Future Index Long", "Future Index Short"},
	{"fut_stk", "Future Stock Long", "Future Stock Short"},
	{"opt_idx_ce", "Option Index Call Long", "Option Index Call Short"},
	{"opt_idx_pe", "Option Index Put Long", "Option Index Put Short"},
	{"opt_stk_ce", "Option Stock Call Long", "Option Stock Call Short"},
	{"opt_stk_pe", "Option Stock Put Long", "Option Stock Put Short"},
	{"total", "Total Long Contracts", "Total Short Contracts"},
}

// ParsePartOI parses the participant OI CSV. The first line is a title; the
// header is the first line that starts with "Client Type".
func ParsePartOI(data []byte, date time.Time) ([]PartOI, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	i := bytes.Index(data, []byte("Client Type"))
	if i < 0 {
		return nil, errors.New("participant oi: no Client Type header")
	}
	data = data[bytes.LastIndexByte(data[:i], '\n')+1:]
	t, err := readTable(data)
	if err != nil {
		return nil, err
	}
	// Header cells sometimes carry stray tabs or double spaces.
	norm := map[string]int{}
	for h, k := range t.idx {
		norm[strings.Join(strings.Fields(h), " ")] = k
	}
	t.idx = norm
	cols := []string{"Client Type"}
	for _, c := range partCols {
		cols = append(cols, c.long, c.short)
	}
	if err := t.need(cols...); err != nil {
		return nil, fmt.Errorf("participant oi: %w", err)
	}
	var out []PartOI
	for n, row := range t.rows {
		ct := strings.ToUpper(t.get(row, "Client Type"))
		if ct == "" {
			continue
		}
		switch ct {
		case "CLIENT", "DII", "FII", "PRO", "TOTAL":
		default:
			return nil, fmt.Errorf("participant oi line %d: client type %q", n+2, ct)
		}
		for _, c := range partCols {
			var fe fieldErr
			l, s := fe.i(t.get(row, c.long)), fe.i(t.get(row, c.short))
			if len(fe.errs) > 0 || l == nil || s == nil {
				return nil, fmt.Errorf("participant oi line %d %s: bad or empty value", n+2, c.inst)
			}
			out = append(out, PartOI{TradeDate: date, ClientType: strings.ToLower(ct), Instrument: c.inst, Long: *l, Short: *s})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("participant oi: no rows")
	}
	return out, nil
}
