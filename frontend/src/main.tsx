import React from "react";
import ReactDOM from "react-dom/client";
import "./styles.css";
import "./ui/ui.css";
import App from "./App";
import { initI18n } from "./i18n";
import { isMac } from "./lib/platform";

if (isMac) document.documentElement.classList.add("mac");

// The saved UI language may be a lazily loaded chunk; load it first.
initI18n().finally(() => {
  ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
    <React.StrictMode>
      <App />
    </React.StrictMode>,
  );
});
