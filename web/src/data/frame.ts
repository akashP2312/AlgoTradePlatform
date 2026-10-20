// Panels read the store on an animation frame, throttled to a few redraws a second,
// and only when the store changed. Hot screens (chains, grids) draw through refs or
// a grid API from useFrame; light ones like SYS can use usePoll, which re-renders
// the panel at most hz times a second with a fresh selection.

import { useEffect, useReducer, useRef } from "react";
import { feed } from "./feed";

/** Calls draw on animation frames, at most hz times a second and only after a store
 * write, plus once a second regardless so ages keep counting while nothing ticks. */
export function useFrame(draw: () => void, hz = 8): void {
  const ref = useRef(draw);
  ref.current = draw;
  useEffect(() => {
    let raf = 0;
    let last = 0;
    let seen = -1;
    const every = 1000 / hz;
    const loop = (t: number) => {
      raf = requestAnimationFrame(loop);
      if (t - last < every) return;
      if (feed.store.version === seen && t - last < 1000) return;
      last = t;
      seen = feed.store.version;
      ref.current();
    };
    raf = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(raf);
  }, [hz]);
}

/** Re-renders at most hz times a second, returning select()'s latest result. */
export function usePoll<T>(select: () => T, hz = 4): T {
  const [, bump] = useReducer((n: number) => n + 1, 0);
  useFrame(bump, hz);
  return select();
}

/** Holds a subscription for the component's lifetime. */
export function useSub(subjects: string[]): void {
  const key = subjects.join("\n");
  useEffect(() => (key ? feed.sub(key.split("\n")) : undefined), [key]);
}
