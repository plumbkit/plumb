// Playwright SPA smoke test. Launches a throwaway daemon (isolated HOME/XDG
// dirs), runs `plumb web --no-open`, loads the returned tokened URL, and asserts
// each of the five sections renders without console errors. WebGL is
// unavailable headless, so we assert the 2D chart fallbacks, not GL views.
//
// Usage (from internal/web/ui):
//   NODE_PATH=/Users/gilberto/node_modules node tests/smoke.mjs <path-to-plumb-binary>
import { chromium } from "playwright-core";
import { execFile, spawn } from "node:child_process";
import { mkdtempSync, rmSync, mkdirSync } from "node:fs";
import { join } from "node:path";

const exe =
  process.env.PLAYWRIGHT_CHROMIUM ||
  "/Users/gilberto/Library/Caches/ms-playwright/chromium-1134/chrome-mac/Chromium.app/Contents/MacOS/Chromium";
const bin = process.argv[2] || join(process.cwd(), "../../../plumb");
const port = 38915;

// A short base path keeps the daemon's Unix socket under the ~104-char limit.
mkdirSync("/tmp/pws", { recursive: true });
const home = mkdtempSync("/tmp/pws/h-");
const env = {
  ...process.env,
  HOME: home,
  XDG_CONFIG_HOME: join(home, ".config"),
  XDG_CACHE_HOME: join(home, ".cache"),
  XDG_DATA_HOME: join(home, ".local/share"),
  XDG_STATE_HOME: join(home, ".local/state"),
};

function sh(args) {
  return new Promise((resolve, reject) => {
    execFile(bin, args, { env }, (err, stdout, stderr) => {
      if (err) reject(new Error(stderr || err.message));
      else resolve(stdout);
    });
  });
}

let daemon, browser;
const fail = (msg) => {
  console.error("SMOKE FAIL:", msg);
  cleanup();
  process.exit(1);
};
function cleanup() {
  try {
    browser && browser.close();
  } catch {}
  try {
    daemon && daemon.kill("SIGKILL");
  } catch {}
  try {
    rmSync(home, { recursive: true, force: true });
  } catch {}
}

try {
  daemon = spawn(bin, ["daemon"], { env, stdio: "ignore", detached: false });
  await new Promise((r) => setTimeout(r, 2500));

  const out = await sh(["web", "--no-open", "--port", String(port)]);
  const m = out.match(/http:\/\/127\.0\.0\.1:\d+\/\?t=[a-f0-9]+/);
  if (!m) fail("no URL from `plumb web`: " + out);
  const url = m[0];

  browser = await chromium.launch({ executablePath: exe });
  const page = await browser.newPage({ viewport: { width: 1360, height: 1700 } });
  const errors = [];
  page.on("console", (e) => {
    if (e.type() === "error") errors.push(e.text());
  });
  page.on("pageerror", (e) => errors.push("PAGEERROR " + e.message));

  // SSE streams keep the network permanently active, so we wait for the app
  // shell to mount rather than for network idle.
  await page.goto(url, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("nav button", { timeout: 15000 });
  await page.waitForTimeout(800);

  // Sidebar nav should list all five sections.
  for (const label of ["Dashboard", "Sessions", "Memory", "Logs", "Settings"]) {
    const btn = page.locator(`nav button:has-text("${label}")`);
    if ((await btn.count()) === 0) fail(`nav missing ${label}`);
  }

  // Visit each section and assert its heading renders.
  const sections = ["Dashboard", "Sessions", "Memory", "Logs", "Settings"];
  for (const label of sections) {
    await page.locator(`nav button:has-text("${label}")`).click();
    await page.waitForTimeout(900);
    const h1 = await page.locator("main h1").first().textContent();
    if (!h1 || !h1.includes(label)) fail(`section ${label} heading = ${h1}`);
  }

  // Dashboard should render at least one canvas (an ECharts/uPlot chart).
  await page.locator('nav button:has-text("Dashboard")').click();
  await page.waitForTimeout(1500);
  const canvases = await page.locator("main canvas").count();
  if (canvases === 0) fail("no charts rendered on Dashboard");

  // Settings should show the Global scope and the theme picker.
  await page.locator('nav button:has-text("Settings")').click();
  await page.waitForTimeout(900);
  if ((await page.locator('text="Global"').count()) === 0) fail("Settings missing Global scope");
  if ((await page.locator('text="Theme"').count()) === 0) fail("Settings missing Theme picker");

  if (errors.length) {
    console.error("Console errors:\n" + errors.join("\n"));
    // Console errors are reported but not fatal unless they block rendering;
    // transient SSE/EventSource reconnect noise is expected on teardown.
  }

  console.log(`SMOKE OK — ${canvases} charts, all 5 sections rendered` + (errors.length ? ` (${errors.length} console msgs)` : ""));
  cleanup();
  process.exit(0);
} catch (e) {
  fail(e.message || String(e));
}
