// The tick store: the latest value per subject, kept outside React. The WS client
// writes into it; panels read from it on an animation frame (see frame.ts), so a
// burst of ticks costs one map write each and never a React render.

export interface Entry {
  v: unknown;
  at: number; // browser ms the value arrived
}

export class Store {
  private m = new Map<string, Entry>();
  /** Bumped on every write, so a reader can skip a frame when nothing changed. */
  version = 0;

  set(subject: string, v: unknown, at: number): void {
    this.m.set(subject, { v, at });
    this.version++;
  }

  get<T = unknown>(subject: string): T | undefined {
    return this.m.get(subject)?.v as T | undefined;
  }

  entry(subject: string): Entry | undefined {
    return this.m.get(subject);
  }

  /** Every stored subject matching a NATS-style pattern, sorted by subject. */
  match(pattern: string): [string, Entry][] {
    const out: [string, Entry][] = [];
    for (const kv of this.m) if (matches(pattern, kv[0])) out.push(kv);
    return out.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  }

  get size(): number {
    return this.m.size;
  }
}

/** NATS subject matching: `*` is one token, a trailing `>` is one or more. */
export function matches(pattern: string, subject: string): boolean {
  if (pattern === subject) return true;
  const p = pattern.split(".");
  const s = subject.split(".");
  for (let i = 0; i < p.length; i++) {
    if (p[i] === ">") return i === p.length - 1 && s.length > i;
    if (i >= s.length) return false;
    if (p[i] !== "*" && p[i] !== s[i]) return false;
  }
  return p.length === s.length;
}
