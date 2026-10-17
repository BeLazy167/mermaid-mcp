import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import process from "node:process";
import readline from "node:readline";

import { initWasm, Resvg } from "@resvg/resvg-wasm";
import { render as renderSVG } from "pele";

const PROTOCOL_VERSION = 1;
const MAX_OUTPUT_BYTES = Number.parseInt(process.env.MERMAID_MAX_OUTPUT_BYTES ?? "", 10);
const MAX_OUTPUT_DIMENSION = 8192;
const MAX_OUTPUT_PIXELS = 16_777_216;
const BACKGROUND_COLOR = "#ffffff";
const WASM_PATH = new URL("./node_modules/@resvg/resvg-wasm/index_bg.wasm", import.meta.url);
const FONT_DIR = "/usr/share/fonts/noto";
const FONT_FILES = Object.freeze([
  "NotoSans-Regular.ttf",
  "NotoSans-Bold.ttf",
  "NotoSans-Italic.ttf",
  "NotoSans-BoldItalic.ttf",
  "NotoSansMono-Regular.ttf",
  "NotoSansMono-Bold.ttf",
  "NotoSansCJK-Regular.ttc",
]);
const FIXED_RENDER_OPTIONS = Object.freeze({
  limit: 50_000,
  maxEdges: 500,
  links: false,
  images: false,
});

let fontBuffers;

function send(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

function errorMessage(error) {
  if (error instanceof Error) {
    return error.message;
  }
  return String(error);
}

process.once("SIGTERM", () => process.exit(0));
process.once("SIGINT", () => process.exit(0));

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

function dimensionsExceeded(width, height) {
  return (
    !Number.isFinite(width) ||
    !Number.isFinite(height) ||
    width <= 0 ||
    height <= 0 ||
    width > MAX_OUTPUT_DIMENSION ||
    height > MAX_OUTPUT_DIMENSION ||
    width * height > MAX_OUTPUT_PIXELS
  );
}

/**
 * Replaces every CSS `var()` inside SVG tags with its resolved value.
 *
 * Pele themes its output with CSS custom properties. resvg and many image
 * viewers ignore them, so the worker bakes in Pele's default theme. Text
 * content is left alone because only markup between `<` and `>` is rewritten.
 *
 * Args:
 *   svg: The SVG markup returned by Pele.
 *
 * Returns:
 *   The SVG with static colors and fonts and a white page background.
 */
function resolveVariables(svg) {
  const tags = /<[^>]*>/g;
  const definitions = new Map();
  for (const [tag] of svg.matchAll(tags)) {
    for (const [, name, value] of tag.matchAll(/(--[\w-]+)\s*:\s*([^;"]+)/g)) {
      definitions.set(name, value.trim());
    }
  }
  const resolve = (text) => {
    let output = "";
    let index = 0;
    let start;
    while ((start = text.indexOf("var(", index)) !== -1) {
      let depth = 1;
      let end = start + 4;
      while (depth > 0 && end < text.length) {
        if (text[end] === "(") {
          depth++;
        } else if (text[end] === ")") {
          depth--;
        }
        end++;
      }
      const inner = text.slice(start + 4, end - 1);
      const comma = inner.indexOf(",");
      const name = (comma === -1 ? inner : inner.slice(0, comma)).trim();
      const fallback = comma === -1 ? "" : inner.slice(comma + 1).trim();
      output += text.slice(index, start) + resolve(definitions.get(name) ?? fallback);
      index = end;
    }
    return output + text.slice(index);
  };
  return svg
    .replace(tags, resolve)
    .replace(/^<svg\b([^>]*?)\bstyle="/, `<svg$1style="background-color:${BACKGROUND_COLOR};`);
}

function rasterize(svg) {
  const resvg = new Resvg(svg, {
    background: BACKGROUND_COLOR,
    font: {
      fontBuffers,
      defaultFontFamily: "Noto Sans",
      sansSerifFamily: "Noto Sans",
      monospaceFamily: "Noto Sans Mono",
    },
  });
  try {
    const image = resvg.render();
    try {
      return image.asPng();
    } finally {
      image.free();
    }
  } finally {
    resvg.free();
  }
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

  let rendered;
  try {
    rendered = renderSVG(request.diagram, FIXED_RENDER_OPTIONS);
  } catch (error) {
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: errorMessage(error),
      kind: "render",
      fatal: false,
    });
    return true;
  }

  if (dimensionsExceeded(rendered.width, rendered.height)) {
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: "rendered image dimensions exceed the safety limit",
      kind: "too_large",
      fatal: false,
    });
    return true;
  }

  const svg = resolveVariables(rendered.svg);
  let data;
  try {
    data = request.format === "png" ? rasterize(svg) : svg;
  } catch (error) {
    process.stderr.write(`rasterize: ${errorMessage(error)}\n`);
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: "renderer infrastructure failed",
      kind: "internal",
      fatal: true,
    });
    return false;
  }

  if (Buffer.byteLength(data) > MAX_OUTPUT_BYTES) {
    send({
      type: "result",
      id: request.id,
      ok: false,
      error: `rendered image exceeds the ${MAX_OUTPUT_BYTES}-byte limit`,
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
  await initWasm(await readFile(WASM_PATH));
  fontBuffers = await Promise.all(FONT_FILES.map((file) => readFile(path.join(FONT_DIR, file))));
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
}

try {
  await main();
} catch (error) {
  process.stderr.write(`renderer worker failed: ${errorMessage(error)}\n`);
  process.exitCode = 1;
}
