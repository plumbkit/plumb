import { mount } from "svelte";
import "./app.css";
import App from "./App.svelte";

// On first load the daemon hands back the token as a ?t= query param. Persist it
// (sessionStorage) and strip it from the URL so it does not linger in history.
const params = new URLSearchParams(window.location.search);
const t = params.get("t");
if (t) {
  sessionStorage.setItem("plumb_web_token", t);
}

const app = mount(App, { target: document.getElementById("app") });

export default app;
