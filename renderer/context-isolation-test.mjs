import assert from "node:assert/strict";
import puppeteer from "puppeteer";

const browser = await puppeteer.launch({
  executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium",
  headless: true,
  pipe: true,
  args: [
    "--disable-background-networking",
    "--proxy-server=http://0.0.0.0:9",
    "--host-resolver-rules=MAP * 0.0.0.0",
  ],
});
try {
  const first = await browser.createBrowserContext();
  await first.setCookie({ name: "probe", value: "secret", url: "https://isolation.invalid" });
  assert.equal((await first.cookies("https://isolation.invalid"))[0]?.value, "secret");
  await first.close();

  const second = await browser.createBrowserContext();
  assert.deepEqual(await second.cookies("https://isolation.invalid"), []);
  await second.close();
} finally {
  await browser.close();
}
