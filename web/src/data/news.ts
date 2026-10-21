// The news risk filter as newsmon publishes it: news.risk (the state, its reasons,
// when it next changes, the next HIGH event) and news.events (the calendar). The
// state filters new entries; it is never a trade signal.

import { since } from "../panels/common";

export const SUBJ_RISK = "news.risk";
export const SUBJ_EVENTS = "news.events";
/** newsmon republishes every few seconds; past this the state is unknown, not NORMAL. */
export const RISK_STALE_MS = 60_000;

export type Level = "NORMAL" | "CAUTION" | "NO_NEW_ENTRIES";

export interface Reason {
  level: Level; kind: string; event?: string; name: string; impact?: string;
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
