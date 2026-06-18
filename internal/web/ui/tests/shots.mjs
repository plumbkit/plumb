import { chromium } from "playwright-core";
import { execFile, spawn } from "node:child_process";
import { mkdtempSync, rmSync, mkdirSync } from "node:fs";
import { join } from "node:path";
const exe = "/Users/gilberto/Library/Caches/ms-playwright/chromium-1134/chrome-mac/Chromium.app/Contents/MacOS/Chromium";
const bin = process.argv[2];
const port = 38916;
mkdirSync("/tmp/pws", { recursive: true });
const home = mkdtempSync("/tmp/pws/s-");
const env = { ...process.env, HOME: home, XDG_CONFIG_HOME: join(home,".config"), XDG_CACHE_HOME: join(home,".cache"), XDG_DATA_HOME: join(home,".local/share"), XDG_STATE_HOME: join(home,".local/state") };
const sh = (a) => new Promise((res,rej)=>execFile(bin,a,{env},(e,o,s)=>e?rej(new Error(s||e.message)):res(o)));
const daemon = spawn(bin,["daemon"],{env,stdio:"ignore"});
await new Promise(r=>setTimeout(r,2500));
const out = await sh(["web","--no-open","--port",String(port)]);
const url = out.match(/http:\/\/127\.0\.0\.1:\d+\/\?t=[a-f0-9]+/)[0];
const b = await chromium.launch({ executablePath: exe });
const p = await b.newPage({ viewport:{width:1360,height:1700}, deviceScaleFactor:2 });
await p.goto(url,{waitUntil:"domcontentloaded"});
await p.waitForSelector("nav button");
mkdirSync("/tmp/plumb-web-shots",{recursive:true});
for (const s of ["Dashboard","Sessions","Memory","Logs","Settings"]) {
  await p.locator(`nav button:has-text("${s}")`).click();
  await p.waitForTimeout(1600);
  await p.screenshot({ path:`/tmp/plumb-web-shots/${s.toLowerCase()}.png`, fullPage:true });
}
await b.close(); daemon.kill("SIGKILL"); rmSync(home,{recursive:true,force:true});
console.log("shots written to /tmp/plumb-web-shots");
