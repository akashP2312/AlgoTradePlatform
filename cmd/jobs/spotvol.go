package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"pmt/internal/bus"
	"pmt/internal/loaders"
	"pmt/internal/refdata"
)

// spotvolMinDays is the fewest paired days the job fits on; with less it reports
// and publishes nothing, so the riskengine keeps its configured defaults.
const spotvolMinDays = 60

// spotvolMinSide is the fewest down (or up) days each side of the beta needs.
const spotvolMinSide = 20

// betaFit is what the job publishes on risk.beta (read by pmt_iv.risk.Beta).
type betaFit struct {
	Dn   float64            `json:"dn"`   // VIX points per 1% on down days
	Up   float64            `json:"up"`   // VIX points per 1% on up days
	Und  map[string]float64 `json:"und"`  // each underlying's % move per 1% of the reference
	Src  string             `json:"src"`
	N    int                `json:"n"`
	Asof string             `json:"asof"`
}

// slopeOrigin is the least-squares slope of y on x through the origin.
func slopeOrigin(x, y []float64) float64 {
	var sxy, sxx float64
	for i := range x {
		sxy += x[i] * y[i]
		sxx += x[i] * x[i]
	}
	if sxx == 0 {
		return math.NaN()
	}
	return sxy / sxx
}

// slope is the least-squares slope of y on x with an intercept.
func slope(x, y []float64) float64 {
	n := float64(len(x))
	if n < 2 {
		return math.NaN()
	}
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx, my = mx/n, my/n
	var sxy, sxx float64
	for i := range x {
		sxy += (x[i] - mx) * (y[i] - my)
		sxx += (x[i] - mx) * (x[i] - mx)
	}
	if sxx == 0 {
		return math.NaN()
	}
	return sxy / sxx
}

// fitBeta measures the spot-vol beta from the reference index's daily % change
// against VIX's daily change in points, fitted separately on down and up days
// (vol rises faster on a fall than it eases on a rally), and each other index's
// beta to the reference. closes maps a series name ("VIX", the reference, each
// underlying) to date -> close; only dates present in both series of a pair count.
func fitBeta(closes map[string]map[string]float64, vixName, ref string) (betaFit, error) {
	rets := func(name string) map[string]float64 {
		c := closes[name]
		days := make([]string, 0, len(c))
		for d := range c {
			days = append(days, d)
		}
		sort.Strings(days)
		out := map[string]float64{}
		for i := 1; i < len(days); i++ {
			if p := c[days[i-1]]; p > 0 {
				out[days[i]] = c[days[i]]/p - 1
			}
		}
		return out
	}
	diffs := func(name string) map[string]float64 {
		c := closes[name]
		days := make([]string, 0, len(c))
		for d := range c {
			days = append(days, d)
		}
		sort.Strings(days)
		out := map[string]float64{}
		for i := 1; i < len(days); i++ {
			out[days[i]] = c[days[i]] - c[days[i-1]]
		}
		return out
	}
	rr := rets(ref)
	dv := diffs(vixName)
	var xd, yd, xu, yu []float64
	n := 0
	asof := ""
	for d, x := range rr {
		v, ok := dv[d]
		if !ok {
			continue
		}
		n++
		if d > asof {
			asof = d
		}
		if x < 0 {
			xd, yd = append(xd, 100*x), append(yd, v)
		} else if x > 0 {
			xu, yu = append(xu, 100*x), append(yu, v)
		}
	}
	if n < spotvolMinDays {
		return betaFit{}, fmt.Errorf("%d paired %s/%s days in index_eod, need %d", n, ref, vixName, spotvolMinDays)
	}
	if len(xd) < spotvolMinSide || len(xu) < spotvolMinSide {
		return betaFit{}, fmt.Errorf("%d down and %d up days, need %d each", len(xd), len(xu), spotvolMinSide)
	}
	f := betaFit{Dn: slopeOrigin(xd, yd), Up: slopeOrigin(xu, yu), Und: map[string]float64{}, Src: "index_eod", N: n, Asof: asof}
	if math.IsNaN(f.Dn) || math.IsNaN(f.Up) {
		return betaFit{}, errors.New("degenerate spot-vol fit")
	}
	names := make([]string, 0, len(closes))
	for k := range closes {
		if k != ref && k != vixName {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, u := range names {
		ur := rets(u)
		var x, y []float64
		for d, r := range rr {
			if v, ok := ur[d]; ok {
				x, y = append(x, r), append(y, v)
			}
		}
		if len(x) < spotvolMinDays {
			continue // too little history: the engine keeps its configured beta
		}
		if b := slope(x, y); !math.IsNaN(b) && b > 0 {
			f.Und[u] = math.Round(b*1000) / 1000
		}
	}
	f.Dn = math.Round(f.Dn*1000) / 1000
	f.Up = math.Round(f.Up*1000) / 1000
	return f, nil
}

func spotvol(e *env, args []string) (string, error) {
	fl := flag.NewFlagSet("spotvol", flag.ContinueOnError)
	ref := fl.String("ref", "NIFTY", "reference underlying (risk.ref in pmt.yaml)")
	dry := fl.Bool("n", false, "print the fit, do not publish")
	if err := fl.Parse(args); err != nil {
		return "", err
	}
	days := 250
	if fl.NArg() > 0 {
		v, err := strconv.Atoi(fl.Arg(0))
		if err != nil || v < spotvolMinDays {
			return "", fmt.Errorf("days must be a number >= %d", spotvolMinDays)
		}
		days = v
	}
	insts, err := refdata.Current(e.ctx, e.pool)
	if err != nil {
		return "", err
	}
	// index_eod is keyed by contract; name each series as the riskengine does.
	names := map[string]string{}
	const vixName = "VIX"
	for _, spec := range append([]string{loaders.VIXSymbol}, indexSpecs(e)...) {
		exch, sym, ok := strings.Cut(spec, ":")
		if !ok {
			continue
		}
		in, ok := refdata.BySymbol(insts, exch, sym)
		if !ok {
			continue
		}
		if spec == loaders.VIXSymbol {
			names[in.Contract] = vixName
		} else {
			names[in.Contract] = specUnderlying(e, spec)
		}
	}
	if len(names) < 2 {
		return "", errors.New("VIX or the indices are not in refdata: is the instrument dump loaded?")
	}
	contracts := make([]string, 0, len(names))
	for c := range names {
		contracts = append(contracts, c)
	}
	rows, err := e.pool.Query(e.ctx, `
		SELECT contract, trade_date, close FROM (
			SELECT contract, trade_date, close,
			       row_number() OVER (PARTITION BY contract ORDER BY trade_date DESC) AS rn
			FROM index_eod WHERE contract = ANY($1) AND close > 0) t
		WHERE rn <= $2`, contracts, days+1)
	if err != nil {
		return "", err
	}
	closes := map[string]map[string]float64{}
	for rows.Next() {
		var c string
		var d time.Time
		var px float64
		if err := rows.Scan(&c, &d, &px); err != nil {
			rows.Close()
			return "", err
		}
		nm := names[c]
		if closes[nm] == nil {
			closes[nm] = map[string]float64{}
		}
		closes[nm][d.Format(time.DateOnly)] = px
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	f, err := fitBeta(closes, vixName, *ref)
	if err != nil {
		// Not a failure: the engine keeps its defaults until the history is there.
		return "spotvol: not published, " + err.Error() + " (run the vix job over a past range to backfill)", nil
	}
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("spotvol %s: n %d, dn %.3f, up %.3f vol pts per 1%%, und %v", f.Asof, f.N, f.Dn, f.Up, f.Und)
	if *dry {
		return msg + " (not published)", nil
	}
	c, err := e.bus()
	if err != nil {
		return "", err
	}
	kv, err := c.KV(e.ctx, bus.KVRisk)
	if err != nil {
		return "", err
	}
	if _, err := kv.Put(e.ctx, "beta", b); err != nil {
		return "", err
	}
	if err := c.NC.Publish("risk.beta", b); err != nil {
		return "", err
	}
	return msg, nil
}

func indexSpecs(e *env) []string {
	var out []string
	for _, u := range e.cfg.Universe.Underlyings {
		if u.Index != "" {
			out = append(out, u.Index)
		}
	}
	return out
}

func specUnderlying(e *env, spec string) string {
	for _, u := range e.cfg.Universe.Underlyings {
		if u.Index == spec {
			return u.Name
		}
	}
	return spec
}
