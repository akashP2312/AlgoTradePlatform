// The bottom line: socket state, the worst feed state, and the IST server clock.

import { feed } from "./data/feed";
import { usePoll } from "./data/frame";
import { useLinks, LINKS } from "./cmd/links";
import { clock } from "./fmt";

export function StatusBar() {
  const links = useLinks();
  const v = usePoll(() => ({
    state: feed.status.state,
    feed: feed.store.get<{ state: string; phase: string }>("sys.ticker.feed"),
    now: feed.serverNow(),
  }), 2);
  return (
    <div className="statusbar">
      <span className={`state ${v.state === "open" ? "st-ok" : v.state === "connecting" ? "st-warn" : "st-bad"}`}>WS {v.state.toUpperCase()}</span>
      {v.feed && <span className={`state st-${{ ok: "ok", stale: "warn", halt: "bad", down: "bad" }[v.feed.state] ?? "quiet"}`}>FEED {v.feed.state.toUpperCase()}</span>}
      {v.feed && <span className="dim">{v.feed.phase}</span>}
      <span className="links">
        {LINKS.map((l) => links[l] && <span key={l} className={`chip link-${l}`}>{links[l]}</span>)}
      </span>
      <span className="spacer" />
      <span className="dim">` command · HELP</span>
      <span className="n clock">{clock(v.now)} IST</span>
    </div>
  );
}
