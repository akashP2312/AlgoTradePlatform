// The news risk filter as newsmon publishes it: news.risk (the state, its reasons,
// when it next changes, the next HIGH event) and news.events (the calendar). The
// state filters new entries; it is never a trade signal.

import { clock } from "../fmt";
import { since } from "../panels/common";

export const SUBJ_RISK = "news.risk";
export const SUBJ_EVENTS = "news.events";
export const SUBJ_FEED = "news.feed";
/** newsmon republishes every few seconds; past this the state is unknown, not NORMAL. */
export const RISK_STALE_MS = 60_000;

export type Level = "NORMAL" | "CAUTION" | "NO_NEW_ENTRIES";

export interface Reason {
  level: Level; kind: string; event?: string; item?: string; source?: string; score?: number; name: string; impact?: string;
  at?: number; until: number; msg: string; verify?: boolean;
}
export interface RiskState {
  ts: number; state: Level; reasons: Reason[]; since?: number; expires_at?: number;
  next_high?: { id: string; name: string; at: number; verify?: boolean };
}
export interface EventRow {
  id: string; name: string; country: string; impact: "HIGH" | "MEDIUM" | "LOW"; category: string;
  affects: string[]; verify: boolean; source?: string; note?: string; derived?: boolean;
  at?: number; date?: string; tbc?: boolean;
}
export interface Calendar { ts: number; loaded?: number; err?: string; events: EventRow[] }

export const LEVEL_TEXT: Record<Level, string> = { NORMAL: "NORMAL", CAUTION: "CAUTION", NO_NEW_ENTRIES: "NO NEW ENTRIES" };
export const levelCls = (l: Level | undefined) => (l === "NO_NEW_ENTRIES" ? "st-bad" : l === "CAUTION" ? "st-warn" : "st-ok");

export const stale = (r: RiskState | undefined) => !r || since(r.ts) > RISK_STALE_MS;

/** A countdown: 4m 12s under an hour, 2h 13m under a day, else 3d 04h. */
export function countdown(ms: number): string {
  if (ms <= 0) return "now";
  const s = Math.floor(ms / 1000);
  const m = Math.floor(s / 60);
  const h = Math.floor(m / 60);
  const pad = (n: number) => String(n).padStart(2, "0");
  if (h < 1) return `${m}m ${pad(s % 60)}s`;
  if (h < 24) return `${h}h ${pad(m % 60)}m`;
  return `${Math.floor(h / 24)}d ${pad(h % 24)}h`;
}

// --- The headline feed (news.feed) --------------------------------------------

export interface Item {
  id: string; src: string; trust: string; region: string; title: string; summary?: string; url?: string; cats?: string[];
  pub?: number; date_only?: boolean; seen: number; ts: number; lag_ms?: number; also?: string[];
  impact?: number; tags?: string[]; rules?: string[]; backfill?: boolean; confirmed?: number;
  llm?: { impact: number; why?: string; model: string };
  tone?: number; mood?: "neg" | "neu" | "pos";
}
export interface SourceStatus {
  id: string; name: string; trust: string; region: string;
  state: "starting" | "ok" | "backoff" | "open" | "half_open" | "blocked" | "disabled" | "stub";
  err?: string; fails?: number; last_try?: number; last_ok?: number; next?: number;
  polls: number; new: number; lag_med?: number; lag_max?: number; note?: string;
}
export interface Tone { window: number; n: number; score: number; mood: "neg" | "neu" | "pos"; neg: number; neu: number; pos: number }
export interface Feed { ts: number; items: Item[]; sources: SourceStatus[]; tone?: Tone; rules: number; rules_err?: string }

/** news.classify.caution_impact: a confirmed headline at or over this may hold CAUTION. */
export const HIGH_IMPACT = 80;

/** The NEWS panel's view: ALL shows noise too, a number is the lowest impact shown, SRC opens the sources. */
export interface View { all: boolean; min: number; src: boolean }

export function parseView(args: string[] | undefined): View {
  const v: View = { all: false, min: 0, src: false };
  for (const a of args ?? []) {
    const u = a.toUpperCase();
    if (u === "ALL") v.all = true;
    else if (u === "SRC") v.src = true;
    else if (/^\d{1,3}$/.test(u)) v.min = Math.min(100, Number(u));
  }
  return v;
}

/** Args for v, in a fixed order so the panel title stays stable. */
export function viewArgs(v: View): string[] {
  return [...(v.all ? ["ALL"] : []), ...(v.min > 0 ? [String(v.min)] : []), ...(v.src ? ["SRC"] : [])];
}

export const isNoise = (i: Item) => i.tags?.includes("noise") ?? false;

/** The items v shows, newest first as newsmon sends them. */
export function filterItems(items: Item[], v: View): Item[] {
  return items.filter((i) => (v.all || !isNoise(i)) && (i.impact ?? 0) >= v.min);
}

export const SRC_OK = new Set<SourceStatus["state"]>(["ok", "starting"]);

// --- Notifications --------------------------------------------------------------

export interface Note { id: string; level: Level; title: string; body: string; url?: string }

/** What a reason is about, so a reason that stays is not announced twice. */
export const reasonKey = (x: Reason) => `${x.kind}:${x.event ?? x.item ?? x.name}`;

const SEEN_MAX = 1000;

/** Turns successive news.risk and news.feed values into notes: a change of risk state,
 * a reason added while the state holds, and a new confirmed high-impact headline.
 * The first value of each is the baseline and says nothing, so a reload is quiet. */
export class Notes {
  private state?: Level;
  private reasons = new Set<string>();
  private feedSeen = false;
  private seen = new Set<string>(); // note ids and headline ids already announced, oldest first

  private mark(id: string): boolean {
    if (this.seen.has(id)) return false;
    this.seen.add(id);
    if (this.seen.size > SEEN_MAX) this.seen.delete(this.seen.values().next().value!);
    return true;
  }

  risk(r: RiskState | undefined): Note[] {
    if (!r || stale(r)) return [];
    const keys = new Set(r.reasons.map(reasonKey));
    const first = this.state === undefined;
    const prev = this.state;
    const known = this.reasons;
    this.state = r.state;
    this.reasons = keys;
    // A headline reason's item counts as announced whether it came here or in the feed.
    const items = r.reasons.filter((x) => x.kind === "headline" && x.item);
    if (first) {
      for (const x of items) this.mark(`item:${x.item}`);
      return [];
    }
    const out: Note[] = [];
    if (r.state !== prev) {
      const top = r.reasons[0];
      out.push({
        id: `state:${r.since ?? r.ts}:${r.state}`, level: r.state,
        title: r.state === "NORMAL" ? "News risk back to NORMAL" : `News risk: ${LEVEL_TEXT[r.state]}`,
        body: r.state === "NORMAL" ? "No reason holds; new entries are not filtered."
          : `${top?.msg ?? ""}${r.reasons.length > 1 ? ` (+${r.reasons.length - 1} more)` : ""}${r.expires_at ? ` · until ${clock(r.expires_at)}` : ""}`,
      });
      for (const x of items) this.mark(`item:${x.item}`);
    } else {
      for (const x of r.reasons) {
        const k = reasonKey(x);
        if (known.has(k)) continue;
        if (x.kind === "headline" && x.item && !this.mark(`item:${x.item}`)) continue;
        out.push({ id: `reason:${k}:${x.until}`, level: x.level, title: `Also holding ${LEVEL_TEXT[x.level]}: ${x.name}`, body: x.msg });
      }
    }
    return out.filter((n) => this.mark(n.id));
  }

  feed(f: Feed | undefined): Note[] {
    if (!f) return [];
    const hot = f.items.filter((i) => i.confirmed && !i.backfill && (i.impact ?? 0) >= HIGH_IMPACT);
    if (!this.feedSeen) {
      this.feedSeen = true;
      for (const i of hot) this.mark(`item:${i.id}`);
      return [];
    }
    const out: Note[] = [];
    for (const i of hot) {
      if (!this.mark(`item:${i.id}`)) continue;
      const n = 1 + (i.also?.length ?? 0);
      out.push({
        id: `item:${i.id}`, level: "CAUTION", url: i.url,
        title: `Headline, impact ${i.impact}: ${i.src}${n > 1 ? ` +${n - 1}` : ""}`,
        body: i.title,
      });
    }
    return out;
  }
}
