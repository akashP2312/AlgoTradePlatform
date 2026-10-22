// Alerts for the news risk filter: a pop-up in this tab when the risk state changes,
// a reason is added, or a confirmed high-impact headline arrives, and optionally a
// system notification while the tab is in the background. Opening the page is quiet:
// what is already there is the baseline. Like the state itself, an alert is a filter
// on new entries, never a trade signal.

import { useEffect, useRef, useState } from "react";
import { feed } from "./data/feed";
import { usePoll, useSub } from "./data/frame";
import { levelCls, Notes, SUBJ_FEED, SUBJ_RISK, type Feed, type Note, type RiskState } from "./data/news";
import { load, save } from "./persist";

export interface Prefs { toasts: boolean; desktop: boolean }
const KEY = "news.notify";
const MAX = 5;
const TTL: Record<Note["level"], number> = { NORMAL: 10_000, CAUTION: 20_000, NO_NEW_ENTRIES: 60_000 };

export const loadPrefs = (): Prefs => ({ toasts: true, desktop: false, ...load<Partial<Prefs>>(KEY) });
export const savePrefs = (p: Prefs) => save(KEY, p);

export type Desktop = NotificationPermission | "unsupported";
export const desktopState = (): Desktop => (typeof Notification === "undefined" ? "unsupported" : Notification.permission);

/** Asks for notification permission; only from a click, as browsers require. */
export async function enableDesktop(): Promise<Desktop> {
  if (typeof Notification === "undefined") return "unsupported";
  if (Notification.permission !== "default") return Notification.permission;
  try {
    return await Notification.requestPermission();
  } catch {
    return Notification.permission;
  }
}

function desktop(n: Note) {
  try {
    const d = new Notification(n.title, { body: `${n.body}\nA filter on new entries, not a signal.`, tag: n.id });
    d.onclick = () => {
      window.focus();
      d.close();
    };
  } catch {
    // some browsers allow notifications only from a service worker
  }
}

interface Toast extends Note { until: number }

export function Notifier() {
  useSub([SUBJ_RISK, SUBJ_FEED]);
  const { r, f } = usePoll(() => ({ r: feed.store.get<RiskState>(SUBJ_RISK), f: feed.store.get<Feed>(SUBJ_FEED) }), 1);
  const notes = useRef(new Notes());
  const [toasts, setToasts] = useState<Toast[]>([]);

  const push = (ns: Note[]) => {
    if (ns.length === 0) return;
    const p = loadPrefs();
    if (p.desktop && document.hidden && desktopState() === "granted") ns.forEach(desktop);
    if (!p.toasts) return;
    const now = Date.now();
    setToasts((ts) => [...ns.map((n) => ({ ...n, until: now + TTL[n.level] })), ...ts].slice(0, MAX));
  };
  useEffect(() => push(notes.current.risk(r)), [r?.ts]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => push(notes.current.feed(f)), [f?.ts]); // eslint-disable-line react-hooks/exhaustive-deps

  // Expire on a timer of our own so a toast goes even while nothing publishes.
  useEffect(() => {
    if (toasts.length === 0) return;
    const next = Math.min(...toasts.map((t) => t.until));
    const id = setTimeout(() => setToasts((ts) => ts.filter((t) => t.until > Date.now())), Math.max(0, next - Date.now()) + 50);
    return () => clearTimeout(id);
  }, [toasts]);

  if (toasts.length === 0) return null;
  const close = (id: string) => setToasts((ts) => ts.filter((t) => t.id !== id));
  return (
    <div className="toasts" role="status" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} className={`toast ${t.level === "NORMAL" ? "toast-ok" : levelCls(t.level)}`} onClick={() => close(t.id)} title="click to dismiss">
          <b>{t.title}</b>
          <div>{t.url ? <a href={t.url} target="_blank" rel="noreferrer" onClick={(e) => e.stopPropagation()}>{t.body}</a> : t.body}</div>
          <div className="toast-foot">a filter on new entries, not a signal · NEWS / ECO for detail</div>
        </div>
      ))}
    </div>
  );
}
