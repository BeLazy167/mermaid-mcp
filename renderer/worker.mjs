import { writeFile } from "node:fs/promises";
import path from "node:path";
import process from "node:process";
import readline from "node:readline";

import { renderMermaid } from "@mermaid-js/mermaid-cli";
import puppeteer from "puppeteer";

const PROTOCOL_VERSION = 1;
const MAX_OUTPUT_BYTES = Number.parseInt(process.env.MERMAID_MAX_OUTPUT_BYTES ?? "", 10);
const MAX_OUTPUT_DIMENSION = 8192;
const MAX_OUTPUT_PIXELS = 16_777_216;
const FIXED_RENDER_OPTIONS = Object.freeze({
  viewport: Object.freeze({ width: 1920, height: 1080, deviceScaleFactor: 1 }),
  backgroundColor: "white",
  mermaidConfig: Object.freeze({
    securityLevel: "strict",
    maxTextSize: 50_000,
    maxEdges: 500,
    htmlLabels: false,
    flowchart: Object.freeze({ htmlLabels: false }),
  }),
  iconPacks: Object.freeze([]),
  iconPacksNamesAndUrls: Object.freeze([]),
});

let browser;
let closing = false;
let browserDisconnected = false;

function send(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

function errorMessage(error) {
  if (error instanceof Error) {
    return error.message;
  }
  return String(error);
}

async function closeBrowser() {
  if (closing) {
    return;
  }
  closing = true;
  try {
    await browser?.close();
  } catch (error) {
    process.stderr.write(`close Chromium: ${errorMessage(error)}\n`);
  }
}

async function terminateFromSignal() {
  await closeBrowser();
  process.exit(0);
}

process.once("SIGTERM", () => {
  void terminateFromSignal();
});
process.once("SIGINT", () => {
  void terminateFromSignal();
});

function validRequest(request) {
  return (
    request !== null &&
    typeof request === "object" &&
    request.type === "render" &&
    Number.isSafeInteger(request.id) &&
    request.id > 0 &&
    typeof request.diagram === "string" &&
    (request.format === "png" || request.format === "svg") &&
    typeof request.outputPath === "string" &&
    path.isAbsolute(request.outputPath)
  );
}

function isInfrastructureError(error) {
  const name = error instanceof Error ? error.constructor.name : "";
  const message = errorMessage(error).toLowerCase();
  return (
    browserDisconnected ||
    !browser.connected ||
    ["ProtocolError", "TargetCloseError", "TimeoutError"].includes(name) ||
    ["EACCES", "EIO", "EMFILE", "ENFILE", "ENOSPC"].includes(error?.code) ||
    message.includes("protocol error") ||
    message.includes("session closed") ||
    message.includes("target closed") ||
    message.includes("connection closed")
  );
}

function outputDimensions(data, format) {
  if (format === "png") {
    const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data);
    if (buffer.length < 24) {
      return undefined;
    }
    return [buffer.readUInt32BE(16), buffer.readUInt32BE(20)];
  }
  const root = String(data).match(/<svg\b[^>]*>/i)?.[0];
  if (root === undefined) {
    return undefined;
  }
  const viewBox = root.match(/\bviewBox\s*=\s*["']([^"']+)["']/i)?.[1];
  if (viewBox !== undefined) {
    const values = viewBox.trim().split(/[\s,]+/).map(Number);
    if (values.length === 4) {
      return [values[2], values[3]];
    }
  }
  const width = root.match(/\bwidth\s*=\s*["']([0-9.]+)(?:px)?["']/i)?.[1];
  const height = root.match(/\bheight\s*=\s*["']([0-9.]+)(?:px)?["']/i)?.[1];
  return width !== undefined && height !== undefined ? [Number(width), Number(height)] : undefined;
}

function outputLimitError(data, format) {
  if (Buffer.byteLength(data) > MAX_OUTPUT_BYTES) {
    return `rendered image exceeds the ${MAX_OUTPUT_BYTES}-byte limit`;
  }
  const dimensions = outputDimensions(data, format);
  if (
    dimensions !== undefined &&
    (!dimensions.every(Number.isFinite) ||
      dimensions[0] <= 0 ||
      dimensions[1] <= 0 ||
      dimensions[0] > MAX_OUTPUT_DIMENSION ||
      dimensions[1] > MAX_OUTPUT_DIMENSION ||
      dimensions[0] * dimensions[1] > MAX_OUTPUT_PIXELS)
  ) {
    return "rendered image dimensions exceed the safety limit";
  }
  return undefined;
}

async function render(request) {
  if (!validRequest(request)) {
    send({
      type: "result",
      id: Number.isSafeInteger(request?.id) ? request.id : 0,
      ok: false,
      error: "invalid worker request",
      kind: "internal",
      fatal: true,
    });
    return false;
  }
  if (browserDisconnected || !browser.connected) {
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: "Chromium disconnected",
      kind: "internal",
      fatal: true,
    });
    return false;
  }

  let context;
  try {
    context = await browser.createBrowserContext();
  } catch (error) {
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: "could not create isolated browser context",
      kind: "internal",
      fatal: true,
    });
    return false;
  }

  let data;
  let failure;
  let fatal = false;
  try {
    ({ data } = await renderMermaid(
      context,
      request.diagram,
      request.format,
      FIXED_RENDER_OPTIONS,
    ));
  } catch (error) {
    failure = error;
    fatal = isInfrastructureError(error);
  } finally {
    try {
      await context.close();
    } catch (error) {
      failure ??= error;
      fatal = true;
    }
  }

  if (failure !== undefined) {
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: fatal ? "renderer infrastructure failed" : errorMessage(failure),
      kind: fatal ? "internal" : "render",
      fatal,
    });
    return !fatal;
  }

  const limitError = outputLimitError(data, request.format);
  if (limitError !== undefined) {
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: limitError,
      kind: "too_large",
      fatal: false,
    });
    return true;
  }
  try {
    await writeFile(request.outputPath, data, { flag: "wx", mode: 0o600 });
  } catch (error) {
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: "could not write rendered output",
      kind: "internal",
      fatal: true,
    });
    return false;
  }
  send({ type: "result", id: request.id, ok: true });
  return true;
}

async function main() {
  if (!Number.isSafeInteger(MAX_OUTPUT_BYTES) || MAX_OUTPUT_BYTES <= 0) {
    throw new Error("MERMAID_MAX_OUTPUT_BYTES must be a positive integer");
  }
  const launchOptions = {
    headless: true,
    pipe: true,
    args: [
      "--disable-background-networking",
      "--disable-dev-shm-usage",
      "--proxy-server=http://0.0.0.0:9",
      "--proxy-bypass-list=<-loopback>",
      "--host-resolver-rules=MAP * 0.0.0.0",
      "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
    ],
  };
  if (process.env.MERMAID_CHROMIUM_PATH) {
    launchOptions.executablePath = process.env.MERMAID_CHROMIUM_PATH;
  }
  browser = await puppeteer.launch(launchOptions);
  browser.once("disconnected", () => {
    browserDisconnected = true;
    if (!closing) {
      process.stderr.write("Chromium disconnected\n");
      process.exitCode = 1;
      process.stdin.destroy();
    }
  });
  send({ type: "ready", protocol: PROTOCOL_VERSION });

  const lines = readline.createInterface({
    input: process.stdin,
    crlfDelay: Infinity,
    terminal: false,
  });
  for await (const line of lines) {
    let request;
    try {
      request = JSON.parse(line);
    } catch (error) {
      process.stderr.write(`invalid worker JSON: ${errorMessage(error)}\n`);
      break;
    }
    if (!(await render(request))) {
      break;
    }
  }
  await closeBrowser();
}

try {
  await main();
} catch (error) {
  process.stderr.write(`renderer worker failed: ${errorMessage(error)}\n`);
  await closeBrowser();
  process.exitCode = 1;
}
