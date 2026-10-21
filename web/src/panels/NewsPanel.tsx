// NEWS: the headlines newsmon holds, newest first, with the rules' impact score, the
// tone, and which of them hold the risk state now. Context only: a headline scores
// how much it may move markets, never which way to trade. NEWS ALL shows the noise
// (stock tips and the like) too, NEWS 80 only impact 80 and up, NEWS SRC the sources.

import { useState } from "react";
import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import {
  filterItems, HIGH_IMPACT, LEVEL_TEXT, levelCls, parseView, SRC_OK, stale, SUBJ_FEED, SUBJ_RISK, viewArgs,
  type Feed, type Item, type RiskState, type SourceStatus, type View,
} from "../data/news";
import { age, clock } from "../fmt";
import { desktopState, enableDesktop, loadPrefs, savePrefs } from "../Notifier";
import { Msg, since } from "./common";

const MOOD: Record<string, [string, string]> = { neg: ["▼", "dn"], pos: ["▲", "up"], neu: ["·", "dim"] };
const SRC_CLS: Partial<Record<SourceStatus["state"], string>> = { ok: "up", starting: "dim", backoff: "amber", half_open: "amber", open: "dn", blocked: "dn" };
const LIMIT = 300;

function ImpactCell({ i }: { i: Item }) {
  const x = i.impact ?? 0;
  const why = [`rules: ${i.rules?.join(", ") || "none"}`];
  if (i.llm) why.push(`model ${i.llm.model}: ${i.llm.impact}${i.llm.why ? ` (${i.llm.why})` : ""}`);
  why.push(i.confirmed ? `confirmed ${clock(i.confirmed)}` : "not confirmed: one untrusted source");
  return (
    <td className={`n ${x >= HIGH_IMPACT ? "dn" : x >= 50 ? "amber" : "dim"}`} title={why.join("\n")}>
      {x || ""}{i.confirmed && x >= HIGH_IMPACT ? "✓" : ""}{i.llm ? "*" : ""}
    </td>
  );
}

function Sources({ ss, now }: { ss: SourceStatus[]; now: number }) {
  return (
    <table className="grid">
      <thead><tr>
        <th className="l">Source</th><th className="l">Trust</th><th className="l">State</th><th className="n">Polls</th><th className="n">New</th>
        <th className="n">Last ok</th><th className="n">Next</th><th className="n">Lag med</th><th className="l">Note</th>
      </tr></thead>
      <tbody>
        {ss.map((s) => (
          <tr key={s.id}>
            <td className="l">{s.name} <span className="dim">{s.region}</span></td>
            <td className="l dim">{s.trust}</td>
            <td className={`l ${SRC_CLS[s.state] ?? "dim"}`}>{s.state}{s.fails ? ` ×${s.fails}` : ""}</td>
            <td className="n">{s.polls}</td>
            <td className="n">{s.new}</td>
            <td className="n dim">{s.last_ok ? age(now - s.last_ok) : "—"}</td>
            <td className="n dim">{s.next && s.next > now ? age(s.next - now) : ""}</td>
            <td className="n dim">{s.lag_med ? age(s.lag_med) : ""}</td>
            <td className="l dim detail">{s.err ?? s.note}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function NotifyControls() {
  const [p, setP] = useState(loadPrefs);
  const [d, setD] = useState(desktopState);
  const set = (q: typeof p) => {
    savePrefs(q);
    setP(q);
  };
  return (<>
    <button className={p.toasts ? "on" : ""} title="pop-ups in this tab for risk changes and confirmed high-impact headlines"
      onClick={() => set({ ...p, toasts: !p.toasts })}>alerts {p.toasts ? "on" : "off"}</button>
    {d !== "unsupported" && (
      <button className={p.desktop && d === "granted" ? "on" : ""} title={d === "denied" ? "blocked in the browser's site settings" : "a system notification while this tab is in the background"}
        disabled={d === "denied"}
        onClick={async () => {
          if (p.desktop) return set({ ...p, desktop: false });
          const g = await enableDesktop();
          setD(g);
          if (g === "granted") set({ ...p, desktop: true });
        }}>desktop {d === "denied" ? "blocked" : p.desktop && d === "granted" ? "on" : "off"}</button>
    )}
  </>);
}

export function NewsPanel({ args, setArgs }: { args?: string[]; setArgs: (a: string[]) => void }) {
  useSub([SUBJ_FEED, SUBJ_RISK]);
  const { f, r, now } = usePoll(() => ({
    f: feed.store.get<Feed>(SUBJ_FEED),
    r: feed.store.get<RiskState>(SUBJ_RISK),
    now: feed.serverNow(),
  }), 1);
  const v = parseView(args);
  const set = (w: Partial<View>) => setArgs(viewArgs({ ...v, ...w }));
  if (!f) return <Msg text={`waiting for ${SUBJ_FEED} (is newsmon running?)`} />;

  const live = r && !stale(r) ? r : undefined;
  const holding = new Map((live?.reasons ?? []).filter((x) => x.kind === "headline" && x.item).map((x) => [x.item!, x]));
  const shown = filterItems(f.items, v);
  const hidden = f.items.length - shown.length;
  const ok = f.sources.filter((s) => SRC_OK.has(s.state)).length;
  const old = since(f.ts) > 60_000;

  return (
    <div className="gp">
      <div className="omon-head">
        <span className="name">NEWS</span>
        {live ? <span className={`state ${levelCls(live.state)}`} title={live.reasons.map((x) => x.msg).join("\n") || "nothing holds"}>RISK {LEVEL_TEXT[live.state]}</span>
          : <span className="state st-quiet">RISK ? (newsmon quiet)</span>}
        {f.tone && f.tone.n > 0 && (
          <span title={`average tone of ${f.tone.n} headlines in the last ${age(f.tone.window)} (noise excluded): ${f.tone.neg} negative, ${f.tone.neu} neutral, ${f.tone.pos} positive. Context only.`}>
            tone <span className={MOOD[f.tone.mood][1]}>{f.tone.score >= 0 ? "+" : ""}{f.tone.score.toFixed(2)} {f.tone.mood}</span>
            <span className="dim"> {f.tone.neg}/{f.tone.neu}/{f.tone.pos}</span>
          </span>
        )}
        <button className={v.all ? "on" : ""} title="show stock tips and other noise too" onClick={() => set({ all: !v.all })}>ALL</button>
        <button className={v.min >= HIGH_IMPACT ? "on" : ""} title={`only impact ${HIGH_IMPACT} and up`} onClick={() => set({ min: v.min >= HIGH_IMPACT ? 0 : HIGH_IMPACT })}>≥{HIGH_IMPACT}</button>
        <button className={v.src ? "on" : ""} onClick={() => set({ src: !v.src })}
          title={f.sources.filter((s) => !SRC_OK.has(s.state)).map((s) => `${s.name}: ${s.state}${s.err ? ` (${s.err})` : ""}`).join("\n") || "all sources polling"}>
          sources <span className={ok === f.sources.length ? "up" : "amber"}>{ok}/{f.sources.length}</span>
        </button>
        <NotifyControls />
        <span className="dim">{f.rules} rules</span>
        {f.rules_err && <span className="state st-bad" title={f.rules_err}>rules file rejected, previous rules kept</span>}
        {old && <span className="state st-quiet">feed {age(since(f.ts))} old</span>}
      </div>
      <div className="panel-body mon news">
        <p className="dim">
          Context only, never a trade signal. Impact is how much a headline may move markets, not which way; ✓ confirmed
          (a trusted source, or two or more carrying it), * rescored by the model. Times IST.
          {hidden > 0 && <> {hidden} hidden{v.min ? ` under ${v.min}` : ""}{!v.all ? " or noise" : ""}.</>}
        </p>
        {v.src && <Sources ss={f.sources} now={now} />}
        <table className="grid">
          <thead><tr>
            <th className="l">IST</th><th className="n">Age</th><th className="l">Source</th><th className="n">Imp</th>
            <th className="l"></th><th className="l">Headline</th><th className="l">Tags</th>
          </tr></thead>
          <tbody>
            {shown.slice(0, LIMIT).map((i) => {
              const h = holding.get(i.id);
              const [m, mc] = MOOD[i.mood ?? "neu"];
              return (
                <tr key={i.id} className={h ? "hold" : i.backfill ? "stale" : undefined}>
                  <td className="l" title={i.pub ? `published ${new Date(i.pub).toISOString()}${i.date_only ? " (date only)" : ""}\nseen ${clock(i.seen)}` : `no publish time; seen ${clock(i.seen)}`}>
                    {clock(i.ts).slice(0, 5)}
                  </td>
                  <td className="n dim">{age(now - i.ts)}</td>
                  <td className="l" title={i.also?.length ? `also: ${i.also.join(", ")}` : i.trust}>
                    {i.src}{i.also?.length ? <span className="dim"> +{i.also.length}</span> : null}
                  </td>
                  <ImpactCell i={i} />
                  <td className={`l ${mc}`} title={i.tone ? `tone ${i.tone.toFixed(2)}` : "neutral"}>{m}</td>
                  <td className="l detail">
                    {i.url ? <a href={i.url} target="_blank" rel="noreferrer" title={i.summary}>{i.title}</a> : <span title={i.summary}>{i.title}</span>}
                    {h && <span className={`eco-badge ${levelCls(h.level)}`} title={`${h.msg}\nuntil ${clock(h.until)}`}>HOLDS {LEVEL_TEXT[h.level]}</span>}
                  </td>
                  <td className="l dim">{i.tags?.join(" ")}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
        {shown.length > LIMIT && <p className="dim">{shown.length - LIMIT} older not shown.</p>}
        {shown.length === 0 && <p className="dim">No headlines{hidden ? " match this view" : " yet"}.</p>}
      </div>
    </div>
  );
}
