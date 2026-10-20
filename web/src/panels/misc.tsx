// The small panels: workspaces, help, and the placeholder for screens not built yet.

import { FNS, fn } from "../cmd/registry";
import { loadWorkspace, workspaces } from "../shell";

export function WorkspacesPanel() {
  const names = workspaces();
  return (
    <div className="panel-body">
      <h3>Workspaces</h3>
      {!names.length ? (
        <p className="dim">None saved. Arrange panels, then type <kbd>W SAVE INTRADAY</kbd>.</p>
      ) : (
        <ul className="list">
          {names.map((n) => (
            <li key={n}><button onClick={() => loadWorkspace(n)}>{n}</button></li>
          ))}
        </ul>
      )}
      <p className="dim">W &lt;name&gt; loads · W SAVE &lt;name&gt; saves the current layout · W DEL &lt;name&gt; deletes</p>
    </div>
  );
}

const KEYS: [string, string][] = [
  ["`", "open the command line"],
  ["Enter", "run in the focused panel"],
  ["Shift+Enter", "run in a new panel beside it"],
  ["↑ ↓", "command history / suggestions"],
  ["Tab", "accept a suggestion"],
  ["Alt+] Alt+[", "next / previous panel"],
  ["Alt+L", "cycle the panel's link colour"],
  ["Alt+M", "maximize / restore the panel"],
  ["Alt+W", "close the panel"],
  ["Alt+1…9", "load saved workspace 1…9 (alphabetical)"],
];

export function HelpPanel() {
  return (
    <div className="panel-body">
      <h3>Command grammar</h3>
      <p><kbd>&lt;SECURITY&gt; &lt;FUNCTION&gt; [args]</kbd> e.g. <kbd>NIFTY OMON</kbd>, <kbd>BNF STRD</kbd>. A security alone re-points the panel.</p>
      <table className="grid">
        <tbody>
          {FNS.map((f) => (
            <tr key={f.code}><td><kbd>{f.code}</kbd></td><td>{f.title}{!f.built && <span className="dim"> (coming)</span>}</td></tr>
          ))}
        </tbody>
      </table>
      <h3>Keys</h3>
      <table className="grid">
        <tbody>
          {KEYS.map(([k, d]) => (
            <tr key={k}><td><kbd>{k}</kbd></td><td>{d}</td></tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function Placeholder({ code, sec }: { code: string; sec?: string }) {
  return (
    <div className="panel-body placeholder">
      <h3>{sec ? `${sec} ${code}` : code}</h3>
      <p className="dim">{fn(code)?.title}</p>
      <p className="dim">This screen is not built yet.</p>
    </div>
  );
}
