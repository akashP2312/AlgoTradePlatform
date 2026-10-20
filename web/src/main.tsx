import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "dockview-react/dist/styles/dockview.css";
import "./theme.css";
import { App } from "./App";
import { feed, wsURL } from "./data/feed";

feed.connect(wsURL());

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
