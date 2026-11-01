import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "@xyflow/react/dist/style.css";
import "./styles.css";
import App from "./App";

// The app is as tall as what the person sees. iOS Safari does not shrink the
// page for its keyboard: it scrolls the page under it, which moves the whole
// app and hides the bottom of it. So the app follows the visual viewport,
// keeps the page unscrolled, and knows when the keyboard is up.
const vv = window.visualViewport;
if (vv) {
  const fit = () => {
    const root = document.documentElement;
    root.style.setProperty("--app-h", `${vv.height}px`);
    root.classList.toggle("kb", window.innerHeight - vv.height > 120);
    if (window.scrollY || vv.offsetTop) window.scrollTo(0, 0);
  };
  vv.addEventListener("resize", fit);
  vv.addEventListener("scroll", fit);
  fit();
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
