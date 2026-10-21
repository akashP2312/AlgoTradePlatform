// The one dockview component: shows whichever function its params name, on its own
// security or its link colour's.

import type { IDockviewPanelProps } from "dockview-react";
import { useLinks } from "../cmd/links";
import { effectiveSec, title, type PanelParams } from "../shell";
import { BotPanel } from "./BotPanel";
import { EcoPanel } from "./EcoPanel";
import { GpPanel } from "./GpPanel";
import { MonPanel } from "./MonPanel";
import { OmonPanel } from "./OmonPanel";
import { PosPanel } from "./PosPanel";
import { RiskPanel } from "./RiskPanel";
import { QPanel } from "./QPanel";
import { StrdPanel } from "./StrdPanel";
import { SysPanel } from "./SysPanel";
import { HelpPanel, Placeholder, WorkspacesPanel } from "./misc";

export function FnPanel({ params, api }: IDockviewPanelProps<PanelParams>) {
  useLinks(); // re-render when a link colour moves
  const sec = effectiveSec(params);
  // Screens with their own controls (an expiry button, say) change their args here,
  // so the choice is in the params and a saved layout keeps it.
  const setArgs = (args: string[]) => {
    const p = { ...params, args };
    api.updateParameters(p as unknown as Record<string, unknown>);
    api.setTitle(title(p));
  };
  const body = (() => {
    switch (params.fn) {
      case "SYS": return <SysPanel />;
      case "W": return <WorkspacesPanel />;
      case "HELP": return <HelpPanel />;
      case "Q": return <QPanel sec={sec} args={params.args} />;
      case "MON": return <MonPanel args={params.args} />;
      case "GP": return <GpPanel sec={sec} args={params.args} />;
      case "OMON": return <OmonPanel sec={sec} args={params.args} setArgs={setArgs} />;
      case "STRD": return <StrdPanel sec={sec} args={params.args} setArgs={setArgs} />;
      case "POS": return <PosPanel />;
      case "RISK": return <RiskPanel />;
      case "BOT": return <BotPanel />;
      case "ECO": return <EcoPanel />;
      default: return <Placeholder code={params.fn} sec={sec} />;
    }
  })();
  return (
    <div className={`fn-panel ${params.link ? `link-${params.link}` : ""}`}>
      {params.link && <div className="link-bar" title={`linked: ${params.link}`} />}
      {body}
    </div>
  );
}
