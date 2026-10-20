// Per-browser conveniences (layouts, link targets, command history) in
// localStorage. Every access is guarded: a private window or blocked storage just
// means nothing is remembered.

const PREFIX = "pmt.";

export function load<T>(key: string): T | undefined {
  try {
    const s = localStorage.getItem(PREFIX + key);
    return s == null ? undefined : (JSON.parse(s) as T);
  } catch {
    return undefined;
  }
}

export function save(key: string, v: unknown): void {
  try {
    localStorage.setItem(PREFIX + key, JSON.stringify(v));
  } catch {
    // storage full or blocked
  }
}

export function remove(key: string): void {
  try {
    localStorage.removeItem(PREFIX + key);
  } catch {
    // blocked
  }
}
