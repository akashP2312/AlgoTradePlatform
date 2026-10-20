// OMON: one expiry's option chain, calls left and puts right of the strike. Quotes,
// IVs and greeks come from the IV engine's surface for the expiry; OI from each
// option's ticks, against the previous close's (/api/oi0) for the change. Strikes
// inside the forward ± 1σ to expiry are shaded, the ATM strike is outlined, and a
// bid/ask wider than 30% of mid is flagged. NIFTY OMON X2 (or a date) picks the
// expiry; the buttons above switch it. Double-click a side for that option's Q.

import { useEffect, useMemo, useRef, useState } from "react";
import { AgGridReact } from "ag-grid-react";
import {
  AllCommunityModule, colorSchemeDark, ModuleRegistry, themeQuartz,
  type CellClassParams, type ColDef, type ColGroupDef, type ValueFormatterParams,
  type GridApi,
} from "ag-grid-community";
import { keyString, subjectOf } from "../data/contract";
import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import { expiries, expiryArg, nearFuture, useRefData } from "../data/ref";
import type { IvSurface, Leg, Tick } from "../data/types";
import { age, num } from "../fmt";
import { run } from "../shell";
import { ivSubject, lots, Msg, ncdf, since, STALE_MS } from "./common";

ModuleRegistry.registerModules([AllCommunityModule]);

const WIDE = 0.3; // spread / mid above this is flagged
const OI0_RETRY_MS = 30_000;
const EXPIRY_BUTTONS = 6;

const theme = themeQuartz.withPart(colorSchemeDark).withParams({
  backgroundColor: "#0b0d10",
  foregroundColor: "#d6dbe2",
  headerTextColor: "#7a8492",
  borderColor: "#222832",
  rowBorder: { color: "#161b22" },
  fontFamily: "JetBrains Mono, SF Mono, Menlo, Consolas, monospace",
  fontSize: 11,
  rowHeight: 20,
  headerHeight: 22,
  spacing: 3,
  wrapperBorderRadius: 0,
});

interface Side {
  leg: Leg;
  oi?: number;
  doi?: number; // against the previous close
  itm: number | null; // N(d2) for the call, N(−d2) for the put
  stale: boolean;
}

interface Row {
  k: number;
  atm: boolean;
  in1s: boolean;
  c: Side | null;
  p: Side | null;
}

interface OI0 {
  d: string | null;
  src: string | null;
  rows: [number, number | null, number | null][];
}

type Which = "c" | "p";

export function OmonPanel({ sec, args, setArgs }: { sec?: string; args?: string[]; setArgs: (a: string[]) => void }) {
  const r = useRefData();
  const und = sec ?? "";
  const xs = r ? expiries(und, r) : [];
  let x = xs[0] ?? "";
  let err = "";
  for (const a of args ?? []) {
    const e = r ? expiryArg(und, a, r) : null;
    if (e) x = e;
    else err = `don't understand "${a}": give an expiry, e.g. X2 or 2026-10-27`;
  }
  const chain = r?.chains[und]?.find((c) => c.x === x);
  const lot = chain?.lot ?? 0;
  const exch = (r && nearFuture(und, r)?.c.split("|")[0]) || "NFO";
  const sSubj = und && x ? ivSubject(und, x) : "";

  // The strikes come from the surface; subscribe to their ticks for OI once known.
  const strikes = usePoll(() => (sSubj ? feed.store.get<IvSurface>(sSubj)?.rows.map((w) => w.k).join(",") ?? "" : ""), 1);
  const legs = useMemo(() => {
    const m = new Map<string, string>();
    for (const k of strikes ? strikes.split(",").map(Number) : []) {
      for (const kind of ["CE", "PE"] as const) m.set(`${k}${kind}`, subjectOf(keyString({ exch, name: und, x, k, kind })));
    }
    return m;
  }, [strikes, exch, und, x]);
  useSub([sSubj, ...legs.values()].filter(Boolean));

  // The previous close's OI, for the change column.
  const [oi0, setOi0] = useState<OI0 | null>(null);
  useEffect(() => {
    if (!und || !x) return;
    let dead = false;
    let id: ReturnType<typeof setTimeout>;
    setOi0(null);
    const load = async () => {
      try {
        const rsp = await fetch(`/api/oi0?und=${encodeURIComponent(und)}&x=${x}`);
        if (rsp.ok) return !dead && setOi0(await rsp.json());
      } catch {
        /* gateway down: retry */
      }
      if (!dead) id = setTimeout(load, OI0_RETRY_MS);
    };
    load();
    return () => {
      dead = true;
      clearTimeout(id);
    };
  }, [und, x]);
  const base = useMemo(() => new Map((oi0?.rows ?? []).map(([k, c, p]) => [k, { c, p }])), [oi0]);

  const v = usePoll(() => {
    const s = sSubj ? feed.store.get<IvSurface>(sSubj) : undefined;
    if (!s?.rows.length) return { s, rows: [] as Row[] };
    let atmK = s.rows[0].k;
    for (const w of s.rows) if (Math.abs(w.k - s.f) < Math.abs(atmK - s.f)) atmK = w.k;
    const sd = s.atm && s.tte > 0 ? s.atm * Math.sqrt(s.tte) : 0;
    const [lo, hi] = sd ? [s.f * Math.exp(-sd), s.f * Math.exp(sd)] : [0, 0];
    const side = (k: number, w: Which, leg: Leg | null | undefined, fit: number | null): Side | null => {
      if (!leg) return null;
      const t = feed.store.get<Tick>(legs.get(`${k}${w === "c" ? "CE" : "PE"}`) ?? "");
      const b = base.get(k)?.[w];
      const vol = leg.iv ?? fit;
      let itm: number | null = null;
      if (vol && s.tte > 0) {
        const d2 = (Math.log(s.f / k) - 0.5 * vol * vol * s.tte) / (vol * Math.sqrt(s.tte));
        itm = ncdf(w === "c" ? d2 : -d2);
      }
      const a = since(t?.r);
      return { leg, oi: t?.oi, doi: t?.oi != null && b != null ? t.oi - b : undefined, itm, stale: leg.age > STALE_MS || a < 0 || a > STALE_MS };
    };
    return {
      s,
      rows: s.rows.map((w): Row => ({
        k: w.k,
        atm: w.k === atmK,
        in1s: sd > 0 && w.k >= lo && w.k <= hi,
        c: side(w.k, "c", w.c, w.fit),
        p: side(w.k, "p", w.p, w.fit),
      })),
    };
  }, 2);

  const cols = useMemo(() => columns(lot), [lot]);
  const grid = useRef<AgGridReact<Row>>(null);
  // Once per expiry, after the grid has the rows: scroll the ATM strike to the middle.
  const centred = useRef("");
  const centre = (api: GridApi<Row>) => {
    if (centred.current === x) return;
    let i = -1;
    api.forEachNode((n) => { if (n.data?.atm) i = n.rowIndex ?? -1; });
    if (i < 0) return;
    centred.current = x;
    // The viewport may not have its height yet on the first rows; wait for layout.
    requestAnimationFrame(() => setTimeout(() => !api.isDestroyed() && api.ensureIndexVisible(i, "middle"), 0));
  };

  if (!r) return <Msg text="loading reference data…" />;
  if (!r.chains[und]?.length) return <Msg text={`${und || "no security"}: no options`} />;
  if (err) return <Msg text={err} />;
  const s = v.s;
  const shown = xs.slice(0, EXPIRY_BUTTONS);
  if (x && !shown.includes(x)) shown.push(x);

  return (
    <div className="omon">
      <div className="omon-head">
        <span className="name">{und}</span>
        {shown.map((e) => (
          <button key={e} className={e === x ? "on" : ""} onClick={() => setArgs(e === xs[0] ? [] : [e])}>{e.slice(5)}</button>
        ))}
        {s ? (
          <>
            <span><span className="dim">F </span>{num(s.f, 2)}<span className="dim"> {s.fsrc}</span></span>
            <span><span className="dim">DTE </span>{num(s.dte, 2)}</span>
            <span><span className="dim">ATM </span>{s.atm != null ? `${num(s.atm * 100, 2)}%` : "—"}</span>
            <span><span className="dim">±1σ </span>{s.atm != null ? num(s.f * s.atm * Math.sqrt(s.tte), 0) : "—"}</span>
            <span><span className="dim">RR25 </span>{s.rr25 != null ? num(s.rr25 * 100, 2) : "—"}</span>
            <span><span className="dim">BF25 </span>{s.bf25 != null ? num(s.bf25 * 100, 2) : "—"}</span>
            {s.fit && !s.fit.ok && <span className="st-warn">fit failed</span>}
            {!!s.fit?.arb?.length && <span className="st-warn" title={s.fit.arb.join(", ")}>fit arb</span>}
            <span className={since(s.ts) > STALE_MS ? "st-warn" : "dim"}>{age(since(s.ts))}</span>
          </>
        ) : (
          <span className="dim">waiting for {sSubj}</span>
        )}
        <span className="dim">ΔOI vs {oi0?.d ? `${oi0.d} ${oi0.src === "eod" ? "bhavcopy" : "last snapshot"}` : "—"} · OI in lots</span>
      </div>
      <div className="omon-grid">
        <AgGridReact<Row>
          ref={grid}
          theme={theme}
          rowData={v.rows}
          columnDefs={cols}
          getRowId={(p) => String(p.data.k)}
          defaultColDef={{ sortable: false, resizable: true, suppressMovable: true, width: 62 }}
          rowClassRules={{ "atm": (p) => !!p.data?.atm, "in1s": (p) => !!p.data?.in1s }}
          suppressCellFocus
          onRowDataUpdated={(e) => centre(e.api)}
          onCellDoubleClicked={(e) => {
            const id = e.colDef.colId ?? "";
            const w = id.startsWith("p") ? "PE" : "CE";
            if (e.data && id !== "k") run(`${und.replaceAll(" ", "_")} Q ${e.data.k}${w} ${x}`, true);
          }}
          overlayNoRowsTemplate={s ? "no strikes quoted" : "waiting for the IV engine"}
        />
      </div>
    </div>
  );
}

function columns(lot: number): (ColDef<Row> | ColGroupDef<Row>)[] {
  const fmt = (f: (n: number) => string) => (p: ValueFormatterParams<Row>) => (p.value == null ? "" : f(p.value));
  const pct = fmt((n) => num(n * 100, 1));
  const side = (w: Which): ColDef<Row>[] => {
    const g = <T,>(f: (s: Side) => T) => (p: { data?: Row }) => {
      const s = p.data?.[w];
      return s ? f(s) : null;
    };
    const cls = (extra?: (s: Side) => string) => (p: CellClassParams<Row>) => {
      const s = p.data?.[w];
      const itm = p.data && s?.itm != null && s.itm > 0.5;
      return ["n", itm ? "itm" : "", s?.stale ? "stale" : "", s && extra ? extra(s) : ""].join(" ");
    };
    const wide = (s: Side) => (s.leg.spr > WIDE ? "wide" : "");
    const c: ColDef<Row>[] = [
      { colId: `${w}oi`, headerName: "OI", valueGetter: g((s) => s.oi), valueFormatter: (p) => lots(p.value ?? undefined, lot), cellClass: cls(), width: 70 },
      {
        colId: `${w}doi`, headerName: "ΔOI", valueGetter: g((s) => s.doi), width: 66,
        valueFormatter: (p) => (p.value == null ? "" : (p.value > 0 ? "+" : "") + lots(p.value, lot)),
        cellClass: cls((s) => (s.doi ? (s.doi > 0 ? "up" : "dn") : "")),
      },
      { colId: `${w}iv`, headerName: "IV", valueGetter: g((s) => s.leg.iv), valueFormatter: pct, cellClass: cls(), width: 54 },
      { colId: `${w}d`, headerName: "Δ", valueGetter: g((s) => s.leg.delta), valueFormatter: fmt((n) => num(n, 2)), cellClass: cls(), width: 52 },
      { colId: `${w}t`, headerName: "θ/d", valueGetter: g((s) => s.leg.theta), valueFormatter: fmt((n) => num(n, 2)), cellClass: cls(), width: 58 },
      { colId: `${w}v`, headerName: "ν", valueGetter: g((s) => s.leg.vega), valueFormatter: fmt((n) => num(n, 2)), cellClass: cls(), width: 52 },
      { colId: `${w}b`, headerName: "Bid", valueGetter: g((s) => s.leg.b || null), valueFormatter: fmt((n) => num(n, 2)), cellClass: cls(wide), width: 66 },
      { colId: `${w}a`, headerName: "Ask", valueGetter: g((s) => s.leg.a || null), valueFormatter: fmt((n) => num(n, 2)), cellClass: cls(wide), width: 66 },
      {
        colId: `${w}itm`, headerName: "ITM%", valueGetter: g((s) => s.itm), valueFormatter: pct, cellClass: cls(), width: 54,
        headerTooltip: "P(expires in the money) = N(±d2) at the leg's IV",
      },
      {
        colId: `${w}tch`, headerName: "Tch%", valueGetter: g((s) => (s.itm == null ? null : s.itm >= 0.5 ? 1 : 2 * s.itm)), valueFormatter: pct, cellClass: cls(), width: 54,
        headerTooltip: "P(touch before expiry) ≈ 2 × P(ITM) out of the money; 100% once the strike is crossed",
      },
    ];
    return w === "c" ? c : [...c].reverse();
  };
  return [
    { headerName: "CALLS", headerClass: "omon-calls", children: side("c") },
    { colId: "k", headerName: "Strike", field: "k", valueFormatter: (p) => num(p.value, 0), cellClass: "strike n", width: 72 },
    { headerName: "PUTS", headerClass: "omon-puts", children: side("p") },
  ];
}
