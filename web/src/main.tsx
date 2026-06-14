import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { SearchApp } from "./v2/SearchApp";
import "./v2/v2.css";

const rootEl = document.getElementById("root");
if (rootEl === null) {
  throw new Error("missing #root element");
}

createRoot(rootEl).render(
  <StrictMode>
    <SearchApp />
  </StrictMode>,
);
