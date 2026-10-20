// Number and age formatting. Indian grouping (12,34,567) throughout; large rupee
// amounts in lakh or crore.

const cache = new Map<number, Intl.NumberFormat>();

function nf(dp: number): Intl.NumberFormat {
  let f = cache.get(dp);
  if (!f) {
    f = new Intl.NumberFormat("en-IN", { minimumFractionDigits: dp, maximumFractionDigits: dp });
    cache.set(dp, f);
  }
  return f;
}

/** A number with fixed decimals and lakh/crore grouping; "" for null. */
export function num(x: number | null | undefined, dp = 2): string {
  if (x == null || !Number.isFinite(x)) return "";
  return nf(dp).format(x);
}

/** Rupees: below a lakh in full, then L and Cr. */
export function inr(x: number | null | undefined): string {
  if (x == null || !Number.isFinite(x)) return "";
  const a = Math.abs(x);
  if (a >= 1e7) return `₹${num(x / 1e7, 2)} Cr`;
  if (a >= 1e5) return `₹${num(x / 1e5, 2)} L`;
  return `₹${num(x, 0)}`;
}

/** An age in ms as 850ms, 4.2s, 3m 05s, 2h 10m; "—" when never seen (negative). */
export function age(ms: number | null | undefined): string {
  if (ms == null || ms < 0 || !Number.isFinite(ms)) return "—";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(Math.floor(s % 60)).padStart(2, "0")}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, "0")}m`;
}

/** HH:MM:SS in IST for a ms timestamp. */
export function clock(ms: number): string {
  return new Date(ms).toLocaleTimeString("en-GB", { timeZone: "Asia/Kolkata", hour12: false });
}
