// The shell: the dockview API and what commands do to it. Every panel is one
// component, "fn", whose params say which function it shows, so running a command
// in a panel just updates its params, and a saved layout restores every panel.

import type { DockviewApi, IDockviewPanel } from "dockview-react";
import { fn, parse, type Parsed } from "./cmd/registry";
import { linkSec, setLink, nextLink, type Link } from "./cmd/links";
import { load, remove, save } from "./persist";

export interface PanelParams {
  fn: string;
  sec?: string;
  args?: string[];
  link?: Link | null;
}

let api: DockviewApi | null = null;
let seq = 0;

export function attach(a: DockviewApi): void {
  api = a;
  const saved = load<{ layout: unknown; seq: number }>("layout");
  if (saved) {
    try {
      a.fromJSON(saved.layout as Parameters<DockviewApi["fromJSON"]>[0]);
      seq = saved.seq;
    } catch {
      a.clear();
    }
  }
  if (!a.panels.length) open({ fn: "SYS" }, true);
  a.onDidLayoutChange(() => save("layout", { layout: a.toJSON(), seq }));
}

/** The security a panel shows: its link colour's, else its own. */
export function effectiveSec(p: PanelParams): string | undefined {
  return linkSec(p.link) ?? p.sec;
}

export function title(p: PanelParams): string {
  const s = effectiveSec(p);
  return s && fn(p.fn)?.sec !== "none" ? `${s} ${p.fn}` : p.fn;
}

function active(): IDockviewPanel | undefined {
  return api?.activePanel;
}

function open(p: PanelParams, first = false): void {
  if (!api) return;
  const ref = active();
  api.addPanel({
    id: `p${++seq}`,
    component: "fn",
    title: title(p),
    params: p,
    position: first || !ref ? undefined : { referencePanel: ref, direction: "right" },
  });
}

function update(panel: IDockviewPanel, p: PanelParams): void {
  panel.api.updateParameters(p as unknown as Record<string, unknown>);
  panel.api.setTitle(title(p));
}

/** Refreshes every panel title, e.g. after a link colour moved. */
export function retitle(): void {
  for (const p of api?.panels ?? []) p.api.setTitle(title(p.params as unknown as PanelParams));
}

/**
 * Runs a command line. Enter replaces the focused panel's function; newPanel
 * (Shift+Enter) opens it beside the focused panel. Returns an error to show, or "".
 */
export function run(line: string, newPanel: boolean): string {
  const c: Parsed = parse(line);
  if (c.error) return c.error;
  if (c.fn === "W") return workspace(c.args);
  const panel = active();
  const cur = panel ? (panel.params as unknown as PanelParams) : undefined;
  const f = c.fn ?? cur?.fn;
  if (!f) return "no function";
  const spec = fn(f)!;
  const sec = c.sec ?? (spec.sec === "none" ? undefined : cur ? effectiveSec(cur) : undefined);
  if (spec.sec === "required" && !sec) return `${spec.code} needs a security, e.g. NIFTY ${spec.code}`;
  const p: PanelParams = { fn: spec.code, sec, args: c.args, link: newPanel ? null : (cur?.link ?? null) };
  if (p.link && sec) setLink(p.link, sec);
  if (newPanel || !panel) open(p);
  else update(panel, p);
  retitle();
  return "";
}

export function cycleLink(): void {
  const panel = active();
  if (!panel) return;
  const p = panel.params as unknown as PanelParams;
  const link = nextLink(p.link);
  const sec = effectiveSec(p);
  // Joining a colour that points nowhere yet points it at this panel's security.
  if (link && sec && !linkSec(link)) setLink(link, sec);
  update(panel, { ...p, sec, link });
  retitle();
}

export function toggleMaximize(): void {
  const panel = active();
  if (!api || !panel) return;
  if (api.hasMaximizedGroup()) api.exitMaximizedGroup();
  else api.maximizeGroup(panel);
}

export function closeActive(): void {
  const panel = active();
  if (api && panel && api.panels.length > 1) api.removePanel(panel);
}

export function cyclePanel(step: 1 | -1): void {
  if (!api || !api.panels.length) return;
  const ps = api.panels;
  const i = ps.findIndex((p) => p.id === active()?.id);
  ps[(i + step + ps.length) % ps.length].api.setActive();
}

// ---- workspaces -------------------------------------------------------

export function workspaces(): string[] {
  return (load<string[]>("workspaces") ?? []).sort();
}

export function loadWorkspace(name: string): string {
  const w = load<{ layout: unknown; seq: number }>(`ws.${name}`);
  if (!api || !w) return `no workspace ${name}`;
  api.fromJSON(w.layout as Parameters<DockviewApi["fromJSON"]>[0]);
  seq = Math.max(seq, w.seq);
  retitle();
  return "";
}

function workspace(args: string[]): string {
  if (!api) return "not ready";
  const [verb, name] = [args[0]?.toUpperCase(), args[1]?.toUpperCase()];
  const names = workspaces();
  if (verb === "SAVE" && name) {
    save(`ws.${name}`, { layout: api.toJSON(), seq });
    save("workspaces", [...new Set([...names, name])]);
    return "";
  }
  if (verb === "DEL" && name) {
    remove(`ws.${name}`);
    save("workspaces", names.filter((n) => n !== name));
    return "";
  }
  if (verb) return loadWorkspace(verb);
  open({ fn: "W" });
  return "";
}
