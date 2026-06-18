// api.js — thin client over the daemon's /api endpoints. The auth token arrives
// as a ?t= query param on first load and is then set as an HttpOnly cookie by
// the server, so same-origin requests authenticate automatically. We still send
// the stored token as a query param on the first calls, as a belt-and-braces
// fallback for environments that drop the cookie.

function tokenParam() {
  const t = sessionStorage.getItem("plumb_web_token");
  return t ? `t=${encodeURIComponent(t)}` : "";
}

function withToken(path) {
  const tp = tokenParam();
  if (!tp) return path;
  return path + (path.includes("?") ? "&" : "?") + tp;
}

export async function getJSON(path) {
  const res = await fetch(withToken(path), { credentials: "same-origin" });
  if (!res.ok) {
    throw new Error(`${path}: ${res.status} ${res.statusText}`);
  }
  return res.json();
}

export async function postJSON(path, body) {
  const res = await fetch(withToken(path), {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try {
      const j = await res.json();
      if (j.error) msg = j.error;
    } catch {
      // non-JSON error body — keep the status text
    }
    throw new Error(msg);
  }
  return res.json();
}

// stream opens an SSE connection and invokes onMessage with each parsed event
// payload. Returns a close function. EventSource cannot set headers, so the
// token rides as a query param (the server also accepts the cookie).
export function stream(path, onMessage, onError) {
  const es = new EventSource(withToken(path));
  es.onmessage = (e) => {
    try {
      onMessage(JSON.parse(e.data));
    } catch {
      onMessage(e.data);
    }
  };
  if (onError) es.onerror = onError;
  return () => es.close();
}
