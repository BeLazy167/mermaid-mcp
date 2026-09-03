import { renderMermaid } from "@mermaid-js/mermaid-cli";
import puppeteer from "puppeteer";

const browser = await puppeteer.launch({
  executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium",
  headless: true,
  pipe: true,
  args: ["--disable-background-networking", "--proxy-server=http://0.0.0.0:9", "--host-resolver-rules=MAP * 0.0.0.0"],
});
try {
  const context = await browser.createBrowserContext();
  try {
    const { data } = await renderMermaid(context, "graph TD; A-->B", "svg", { mermaidConfig: {securityLevel:"strict", htmlLabels:false, flowchart:{htmlLabels:false}} });
    if (!Buffer.from(data).includes(Buffer.from("<svg"))) throw new Error("missing svg");
  } finally { await context.close(); }
} finally { await browser.close(); }
