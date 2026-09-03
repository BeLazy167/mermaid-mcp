import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";
import { Interceptor } from "./node_modules/@mermaid-js/mermaid-cli/src/puppeteerIntercept.js";

const interceptor = new Interceptor();
const allowedFile = "/app/node_modules/mermaid/dist/mermaid.esm.min.mjs";
const allowedURL = await interceptor.fileUrlToInterceptUrl(pathToFileURL(allowedFile));
assert.equal((await interceptor.interceptUrlToFileUrl(allowedURL)).pathname, allowedFile);
await assert.rejects(
  interceptor.interceptUrlToFileUrl(
    "https://mermaid-cli-intercept.invalid/app/node_modules/puppeteer/package.json",
  ),
  /not in an allowed directory/,
);
