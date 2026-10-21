// The shell: dockview panels, the command line, the status bar, and the keys.

import { useEffect, useState } from "react";
import { DockviewReact, type DockviewTheme } from "dockview-react";
import { CommandLine } from "./cmd/CommandLine";
import { FnPanel } from "./panels/FnPanel";
import { RiskBanner, StatusBar } from "./StatusBar";
import { attach, closeActive, cycleLink, cyclePanel, loadWorkspace, toggleMaximize, workspaces } from "./shell";

const theme: DockviewTheme = { name: "pmt", className: "dockview-theme-pmt", colorScheme: "dark" };
const components = { fn: FnPanel };

function Watermark() {
  return <div className="watermark dim">Press ` and type a command, e.g. SYS</div>;
}

export function App() {
  const [cmd, setCmd] = useState(false);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const typing = e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement;
      // e.code too: on layouts where ` is a dead key (US International, ABC
      // Extended) e.key is "Dead", not "`".
      if ((e.key === "`" || e.code === "Backquote") && !typing && !e.altKey && !e.metaKey && !e.ctrlKey) {
        e.preventDefault();
        setCmd(true);
        return;
      }
      if (!e.altKey || e.metaKey || e.ctrlKey) return;
      // e.code, not e.key: on macOS Alt turns letters into symbols.
      const act: Record<string, () => void> = {
        KeyL: cycleLink,
        KeyM: toggleMaximize,
        KeyW: closeActive,
        BracketRight: () => cyclePanel(1),
        BracketLeft: () => cyclePanel(-1),
      };
      const m = /^Digit([1-9])$/.exec(e.code);
      const f = act[e.code] ?? (m ? () => { const w = workspaces()[+m[1] - 1]; if (w) loadWorkspace(w); } : undefined);
      if (f) {
        e.preventDefault();
        f();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  return (
    <div className="app">
      <div className="dock">
        <DockviewReact
          components={components}
          watermarkComponent={Watermark}
          theme={theme}
          onReady={(e) => attach(e.api)}
        />
      </div>
      <RiskBanner />
      <StatusBar onCommand={() => setCmd(true)} />
      <CommandLine open={cmd} setOpen={setCmd} />
    </div>
  );
}
