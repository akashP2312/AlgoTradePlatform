// The bottom line: socket state, the worst feed state, the news risk state with a
// countdown to the next HIGH event, and the IST server clock. Above it, a banner
// whenever the risk state is not NORMAL (or newsmon has gone quiet).

import { feed } from "./data/feed";
import { usePoll, useSub } from "./data/frame";
import { countdown, LEVEL_TEXT, levelCls, stale, SUBJ_RISK, type RiskState } from "./data/news";
import { useLinks, LINKS } from "./cmd/links";
import { clock } from "./fmt";

export function StatusBar({ onCommand }: { onCommand: () => void }) {
  const links = useLinks();
  useSub([SUBJ_RISK]);
  const v = usePoll(() => ({
    state: feed.status.state,
    risk: feed.store.get<RiskState>(SUBJ_RISK),
    feed: feed.store.get<{ state: string; phase: string }>("sys.ticker.feed"),
    now: feed.serverNow(),
  }), 2);
  return (
    <div className="statusbar">
      <span className={`state ${v.state === "open" ? "st-ok" : v.state === "connecting" ? "st-warn" : "st-bad"}`}>WS {v.state.toUpperCase()}</span>
      {v.feed && <span className={`state st-${{ ok: "ok", stale: "warn", halt: "bad", down: "bad" }[v.feed.state] ?? "quiet"}`}>FEED {v.feed.state.toUpperCase()}</span>}
      {v.feed && <span className="dim">{v.feed.phase}</span>}
      <RiskChip r={v.risk} now={v.now} />
      <span className="links">
        {LINKS.map((l) => links[l] && <span key={l} className={`chip link-${l}`}>{links[l]}</span>)}
      </span>
      <span className="spacer" />
      <span className="dim" style={{ cursor: "pointer" }} title="open the command line" onClick={onCommand}>` command · HELP</span>
      <span className="n clock">{clock(v.now)} IST</span>
    </div>
  );
}

function RiskChip({ r, now }: { r?: RiskState; now: number }) {
  if (stale(r)) return <span className="state st-quiet" title="newsmon has not published a risk state in the last minute: treat it as unknown, not NORMAL">RISK ?</span>;
  const nh = r!.next_high;
  return (<>
    <span className={`state ${levelCls(r!.state)}`} title={r!.reasons.map((x) => x.msg).join("\n") || "no scheduled event window"}>RISK {LEVEL_TEXT[r!.state]}</span>
    {nh && <span className="dim" title={`${nh.name} at ${clock(nh.at)} IST${nh.verify ? " (time unverified)" : ""} · ECO for the calendar`}>
      HIGH {nh.name} in {countdown(nh.at - now)}{nh.verify ? " ?" : ""}
    </span>}
  </>);
}

/** A banner over the status bar while the risk state is not NORMAL. */
export function RiskBanner() {
  useSub([SUBJ_RISK]);
  const v = usePoll(() => ({ r: feed.store.get<RiskState>(SUBJ_RISK), now: feed.serverNow() }), 2);
  const r = v.r;
  if (!r) return null;
  if (stale(r)) return <div className="risk-banner st-quiet">News risk state unknown: newsmon has not published for over a minute.</div>;
  if (r.state === "NORMAL") return null;
  const top = r.reasons[0];
  return (
    <div className={`risk-banner ${levelCls(r.state)}`}>
      <b>{LEVEL_TEXT[r.state]}</b>
      <span>{top?.msg ?? ""}{r.reasons.length > 1 ? ` (+${r.reasons.length - 1} more)` : ""}</span>
      {r.expires_at ? <span>changes in {countdown(r.expires_at - v.now)} at {clock(r.expires_at)}</span> : null}
      <span className="why">a filter on new entries, not a signal; open positions are untouched</span>
    </div>
  );
}
