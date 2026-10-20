// The command line: backtick opens it, Enter runs in the focused panel, Shift+Enter
// in a new one. Typed text only ever changes the layout; nothing is sent anywhere.

import { useEffect, useRef, useState } from "react";
import { feed } from "../data/feed";
import { load, save } from "../persist";
import { run } from "../shell";
import { suggest, type Suggestion } from "./registry";

const HISTORY = 100;

function securities(): string[] {
  const und = feed.store.get<{ und?: Record<string, unknown> }>("sys.ticker.feed")?.und;
  return Object.keys(und ?? {}).sort();
}

export function CommandLine({ open, setOpen }: { open: boolean; setOpen: (o: boolean) => void }) {
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const [sel, setSel] = useState(-1);
  const hist = useRef<string[]>(load<string[]>("history") ?? []);
  const hpos = useRef(-1);
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (open) input.current?.focus();
  }, [open]);

  const sugg: Suggestion[] = open && text.trim() ? suggest(text, securities()) : [];

  function close() {
    setOpen(false);
    setText("");
    setError("");
    setSel(-1);
    hpos.current = -1;
  }

  function submit(newPanel: boolean) {
    const line = sel >= 0 && sugg[sel] ? sugg[sel].text : text;
    const err = run(line, newPanel);
    if (err) {
      setError(err);
      return;
    }
    const h = hist.current.filter((x) => x !== line.trim());
    hist.current = [line.trim(), ...h].slice(0, HISTORY);
    save("history", hist.current);
    close();
  }

  function onKey(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Escape") return close();
    if (e.key === "Enter") {
      e.preventDefault();
      return submit(e.shiftKey);
    }
    if (e.key === "Tab" && sugg.length) {
      e.preventDefault();
      setText(sugg[Math.max(sel, 0)].text);
      setSel(-1);
      return;
    }
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const up = e.key === "ArrowUp";
      if (sugg.length && (sel >= 0 || !up)) {
        // Walk the suggestions; up past the first returns to history.
        setSel((s) => Math.max(-1, Math.min(sugg.length - 1, s + (up ? -1 : 1))));
        return;
      }
      const h = hist.current;
      if (!h.length) return;
      hpos.current = Math.max(-1, Math.min(h.length - 1, hpos.current + (up ? 1 : -1)));
      setText(hpos.current < 0 ? "" : h[hpos.current]);
      setSel(-1);
    }
  }

  if (!open) return null;
  return (
    <div className="cmdline" onMouseDown={(e) => e.target === e.currentTarget && close()}>
      <div className="cmdbox">
        <input
          ref={input}
          value={text}
          spellCheck={false}
          autoComplete="off"
          placeholder="NIFTY OMON · SYS · W SAVE INTRADAY · HELP"
          onChange={(e) => {
            setText(e.target.value);
            setError("");
            setSel(-1);
          }}
          onKeyDown={onKey}
          onBlur={() => setTimeout(() => document.activeElement !== input.current && close(), 0)}
        />
        {error && <div className="cmderr">{error}</div>}
        {!!sugg.length && (
          <ul className="sugg">
            {sugg.map((s, i) => (
              <li
                key={s.text}
                className={i === sel ? "on" : ""}
                onMouseDown={(e) => {
                  e.preventDefault();
                  setText(s.text);
                  setSel(-1);
                  input.current?.focus();
                }}
              >
                <b>{s.label}</b> <span className="dim">{s.title}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
