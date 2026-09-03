import { readFile, writeFile } from "node:fs/promises";

async function replaceExactlyOnce(path, from, to) {
  const source = await readFile(path, "utf8");
  if (source.split(from).length !== 2) {
    throw new Error(`unexpected dependency source: ${path}`);
  }
  await writeFile(path, source.replace(from, to));
}

const modules = "/app/node_modules";
await replaceExactlyOnce(
  `${modules}/@mermaid-js/mermaid-cli/src/puppeteerIntercept.js`,
  `![...this.#allowedDirs].some((dir) =>\n        path.relative(filePath, dir).startsWith(".."),\n      )`,
  `![...this.#allowedDirs].some((dir) => {\n        const relativePath = path.relative(dir, filePath);\n        return (\n          relativePath !== ".." &&\n          !relativePath.startsWith(\`..\${path.sep}\`) &&\n          !path.isAbsolute(relativePath)\n        );\n      })`,
);
await replaceExactlyOnce(
  `${modules}/@puppeteer/browsers/lib/launch.js`,
  `opts.detached ??= true;`,
  `opts.detached ??= false;`,
);
