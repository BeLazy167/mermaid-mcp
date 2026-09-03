import assert from "node:assert/strict";
import test from "node:test";

import worker, { Budget } from "./index.js";

class MemoryState {
  constructor() {
    this.value = undefined;
    this.storage = {
      get: async () => this.value,
      put: async (_key, value) => { this.value = structuredClone(value); },
    };
  }
  blockConcurrencyWhile(callback) {
    this.ready = callback();
  }
}

test("Budget enforces the shared minute limit", async () => {
  const state = new MemoryState();
  const budget = new Budget(state, { MISS_PER_MINUTE: "2", MISS_PER_DAY: "3" });
  await state.ready;
  assert.equal((await budget.fetch()).status, 204);
  assert.equal((await budget.fetch()).status, 204);
  const denied = await budget.fetch();
  assert.equal(denied.status, 429);
  assert.equal(denied.headers.get("Retry-After"), "60");
  assert.equal(state.value.minuteCount, 2);
});

test("edge handler protects and forwards admission", async () => {
  const token = "0123456789abcdef0123456789abcdef";
  let forwarded = 0;
  const env = {
    ADMISSION_TOKEN: token,
    BUDGET: {
      idFromName: (name) => name,
      get: () => ({ fetch: async () => { forwarded += 1; return new Response(null, { status: 204 }); } }),
    },
  };
  const denied = await worker.fetch(new Request("https://admission.example/admit", { method: "POST" }), env);
  assert.equal(denied.status, 401);
  const allowed = await worker.fetch(new Request("https://admission.example/admit", {
    method: "POST",
    headers: { Authorization: `Bearer ${token}` },
  }), env);
  assert.equal(allowed.status, 204);
  assert.equal(forwarded, 1);
});
