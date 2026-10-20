// The one dockview component: shows whichever function its params name, on its own
// security or its link colour's.

import type { IDockviewPanelProps } from "dockview-react";
import { useLinks } from "../cmd/links";
import { effectiveSec, type PanelParams } from "../shell";
import { SysPanel } from "./SysPanel";
import { HelpPanel, Placeholder, WorkspacesPanel } from "./misc";

export function FnPanel({ params }: IDockviewPanelProps<PanelParams>) {
  useLinks(); // re-render when a link colour moves
  const sec = effectiveSec(params);
  const body = (() => {
    switch (params.fn) {
      case "SYS": return <SysPanel />;
      case "W": return <WorkspacesPanel />;
      case "HELP": return <HelpPanel />;
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
