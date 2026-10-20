// SYS: is the data trustworthy right now? The gateway connection, the ticker's
// feed state per underlying, and every service's last health beat.

import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import { age, clock, num } from "../fmt";

interface UndFeed {
  state: string;
  index_age: number;
  fut_age: number;
  since: number;
}

interface FeedStatus {
  at: number;
  phase: string;
  state: string;
  sockets: string;
  last_any: number;
  und: Record<string, UndFeed>;
}

interface Health {
  service: string;
  at: number;
  ok: boolean;
  detail?: Record<string, string>;
}

// A health beat is every 5s; three missed beats and the row goes grey. Beats are
// timed from their arrival, not their `at`: in a dry run some services run on the
// shifted clock and some (the gateway) on the real one.
const HEALTH_STALE_MS = 15_000;

const STATE_CLASS: Record<string, string> = {
  ok: "st-ok", pre: "st-quiet", closed: "st-quiet", stale: "st-warn", halt: "st-bad", down: "st-bad",
};

function State({ s }: { s: string }) {
  return <span className={`state ${STATE_CLASS[s] ?? "st-quiet"}`}>{s.toUpperCase()}</span>;
}

export function SysPanel() {
  useSub(["sys.>"]);
  const view = usePoll(() => {
    const st = feed.status;
    const now = feed.serverNow();
    const fs = feed.store.get<FeedStatus>("sys.ticker.feed");
    const health = feed.store.match("sys.*.health").map(([, e]) => ({ h: e.v as Health, a: Date.now() - e.at }));
    return { st: { ...st }, now, fs, health, skew: now - Date.now(), subs: feed.subscriptions, size: feed.store.size };
  }, 4);
  const { st, now, fs, health } = view;

  return (
    <div className="panel-body sys">
      <section>
        <h3>Gateway</h3>
        <table className="kv">
          <tbody>
            <tr><th>Socket</th><td><span className={`state ${st.state === "open" ? "st-ok" : st.state === "connecting" ? "st-warn" : "st-bad"}`}>{st.state.toUpperCase()}</span> <span className="dim">for {age(Date.now() - st.since)}</span></td></tr>
            <tr><th>Flush</th><td className="n">{st.flushMs ? `${st.flushMs}ms` : "—"}</td></tr>
            <tr><th>Frames</th><td className="n">{num(st.frames, 0)} <span className="dim">last {st.lastFrame ? age(Date.now() - st.lastFrame) : "—"} ago</span></td></tr>
            <tr><th>Subscribed</th><td className="n">{num(view.subs, 0)} <span className="dim">· {num(view.size, 0)} subjects cached</span></td></tr>
            <tr><th>Server clock</th><td className="n">{clock(now)} <span className="dim">skew {view.skew >= 0 ? "+" : ""}{age(Math.abs(view.skew))}</span></td></tr>
            <tr><th>Reconnects</th><td className="n">{st.reconnects}{st.lastError && <span className="dim"> · {st.lastError}</span>}</td></tr>
          </tbody>
        </table>
      </section>

      <section>
        <h3>Feed {fs && <State s={fs.state} />}</h3>
        {!fs ? (
          <p className="dim">No feed status yet (sys.ticker.feed, every 1s).</p>
        ) : (
          <>
            <table className="kv">
              <tbody>
                <tr><th>Phase</th><td>{fs.phase}</td></tr>
                <tr><th>Sockets</th><td>{fs.sockets}</td></tr>
                <tr><th>Last tick</th><td className="n">{fs.last_any ? `${age(now - fs.last_any)} ago` : "—"}</td></tr>
                <tr><th>Status age</th><td className={`n ${now - fs.at > 3000 ? "stale" : ""}`}>{age(now - fs.at)}</td></tr>
              </tbody>
            </table>
            <table className="grid">
              <thead>
                <tr><th>Underlying</th><th>State</th><th className="n">Index age</th><th className="n">Future age</th><th className="n">In state</th></tr>
              </thead>
              <tbody>
                {Object.keys(fs.und).sort().map((u) => {
                  const r = fs.und[u];
                  return (
                    <tr key={u}>
                      <td>{u}</td>
                      <td><State s={r.state} /></td>
                      <td className="n">{age(r.index_age)}</td>
                      <td className="n">{age(r.fut_age)}</td>
                      <td className="n">{r.since ? age(now - r.since) : "—"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </>
        )}
      </section>

      <section>
        <h3>Services</h3>
        {!health.length ? (
          <p className="dim">No health beats yet (sys.*.health, every 5s).</p>
        ) : (
          <table className="grid">
            <thead>
              <tr><th>Service</th><th>OK</th><th className="n">Beat</th><th>Detail</th></tr>
            </thead>
            <tbody>
              {health.map(({ h, a }) => {
                const stale = a > HEALTH_STALE_MS;
                return (
                  <tr key={h.service} className={stale ? "stale" : ""}>
                    <td>{h.service}</td>
                    <td>{stale ? <span className="state st-quiet">STALE</span> : h.ok ? <span className="state st-ok">OK</span> : <span className="state st-bad">FAIL</span>}</td>
                    <td className="n">{age(a)}</td>
                    <td className="detail">
                      {Object.entries(h.detail ?? {}).sort().map(([k, v]) => (
                        <span key={k}><span className="dim">{k}</span> {v}</span>
                      ))}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}
