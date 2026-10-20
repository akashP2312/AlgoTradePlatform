// Linked panels: each link colour points at one security, and every panel on that
// colour follows it. Running a command with a security in a linked panel moves the
// whole colour.

import { useSyncExternalStore } from "react";
import { load, save } from "../persist";

export const LINKS = ["amber", "blue", "green"] as const;
export type Link = (typeof LINKS)[number];

type State = Partial<Record<Link, string>>;

let state: State = load<State>("links") ?? {};
const listeners = new Set<() => void>();

export function linkSec(l: Link | null | undefined): string | undefined {
  return l ? state[l] : undefined;
}

export function setLink(l: Link, sec: string): void {
  if (state[l] === sec) return;
  state = { ...state, [l]: sec };
  save("links", state);
  listeners.forEach((f) => f());
}

/** The next link colour in the cycle none → amber → blue → green → none. */
export function nextLink(l: Link | null | undefined): Link | null {
  const i = l ? LINKS.indexOf(l) : -1;
  return i + 1 < LINKS.length ? LINKS[i + 1] : null;
}

export function useLinks(): State {
  return useSyncExternalStore(
    (f) => (listeners.add(f), () => listeners.delete(f)),
    () => state,
  );
}
