import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import readline from "node:readline";

const directory = await mkdtemp(path.join(tmpdir(), "render-contract-"));
const worker = spawn(process.execPath, [new URL("./worker.mjs", import.meta.url).pathname], {
  env: { ...process.env, MERMAID_MAX_OUTPUT_BYTES: "10000000" },
  stdio: ["pipe", "pipe", "inherit"],
});
const replies = readline.createInterface({ input: worker.stdout })[Symbol.asyncIterator]();

async function next() {
  return JSON.parse((await replies.next()).value);
}

async function render(id, diagram, format) {
  const outputPath = path.join(directory, `${id}.${format}`);
  worker.stdin.write(`${JSON.stringify({ type: "render", id, diagram, format, outputPath })}\n`);
  return { reply: await next(), outputPath };
}

try {
  assert.deepEqual(await next(), { type: "ready", protocol: 1 });

  const svg = await render(1, "graph TD; A-->B", "svg");
  assert.deepEqual(svg.reply, { type: "result", id: 1, ok: true });
  const svgData = await readFile(svg.outputPath, "utf8");
  assert.ok(svgData.startsWith("<svg"));
  assert.ok(!svgData.includes("var("), "SVG still uses CSS variables");

  const png = await render(2, "sequenceDiagram\nA->>B: Render", "png");
  assert.deepEqual(png.reply, { type: "result", id: 2, ok: true });
  const pngData = await readFile(png.outputPath);
  assert.ok(pngData.subarray(0, 8).equals(Buffer.from("\x89PNG\r\n\x1a\n", "latin1")));

  const text = await render(3, 'graph TD; A["var(--x) <b>"]-->B', "svg");
  assert.equal(text.reply.ok, true);
  assert.ok((await readFile(text.outputPath, "utf8")).includes("var(--x)"), "label text changed");

  const invalid = await render(4, "graph TD; A-->", "svg");
  assert.equal(invalid.reply.ok, false);
  assert.equal(invalid.reply.kind, "render");
  assert.equal(invalid.reply.fatal, false);
} finally {
  worker.kill();
  await rm(directory, { recursive: true, force: true });
}
