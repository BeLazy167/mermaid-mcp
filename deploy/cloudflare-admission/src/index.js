function response(status, message = "") {
  return new Response(status === 204 ? null : message, {
    status,
    headers: status === 429 ? { "Retry-After": "60" } : undefined,
  });
}

async function tokenMatches(actual, expected) {
  if (typeof expected !== "string" || expected.length < 32) return false;
  const encoder = new TextEncoder();
  const [actualHash, expectedHash] = await Promise.all([
    crypto.subtle.digest("SHA-256", encoder.encode(actual)),
    crypto.subtle.digest("SHA-256", encoder.encode(`Bearer ${expected}`)),
  ]);
  const left = new Uint8Array(actualHash);
  const right = new Uint8Array(expectedHash);
  let difference = 0;
  for (let index = 0; index < left.length; index += 1) difference |= left[index] ^ right[index];
  return difference === 0;
}

function positiveLimit(value, name) {
  const parsed = Number.parseInt(value, 10);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) throw new Error(`${name} must be a positive integer`);
  return parsed;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (request.method !== "POST" || url.pathname !== "/admit") return response(404, "not found");
    if (!(await tokenMatches(request.headers.get("Authorization") ?? "", env.ADMISSION_TOKEN))) {
      return response(401, "unauthorized");
    }
    const id = env.BUDGET.idFromName("global");
    return env.BUDGET.get(id).fetch(request);
  },
};

export class Budget {
  constructor(state, env) {
    this.state = state;
    this.minuteLimit = positiveLimit(env.MISS_PER_MINUTE, "MISS_PER_MINUTE");
    this.dayLimit = positiveLimit(env.MISS_PER_DAY, "MISS_PER_DAY");
    this.counters = undefined;
    state.blockConcurrencyWhile(async () => {
      this.counters = (await state.storage.get("counters")) ?? {
        minute: 0,
        minuteCount: 0,
        day: 0,
        dayCount: 0,
      };
    });
  }

  async fetch() {
    const now = Date.now();
    const minute = Math.floor(now / 60_000);
    const day = Math.floor(now / 86_400_000);
    if (this.counters.minute !== minute) {
      this.counters.minute = minute;
      this.counters.minuteCount = 0;
    }
    if (this.counters.day !== day) {
      this.counters.day = day;
      this.counters.dayCount = 0;
    }
    if (this.counters.minuteCount >= this.minuteLimit || this.counters.dayCount >= this.dayLimit) {
      return response(429, "render budget exhausted");
    }
    this.counters.minuteCount += 1;
    this.counters.dayCount += 1;
    await this.state.storage.put("counters", this.counters);
    return response(204);
  }
}
