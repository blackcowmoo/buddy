import assert from "node:assert/strict";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { test } from "node:test";

const execFileAsync = promisify(execFile);
const script = join(dirname(fileURLToPath(import.meta.url)), "check-coverage-regression.mjs");

async function run(base, head) {
  try {
    const result = await execFileAsync(process.execPath, [script, "--label", "web", "--base", base, "--head", head]);
    return { code: 0, output: result.stdout };
  } catch (error) {
    return { code: error.code, output: `${error.stdout ?? ""}${error.stderr ?? ""}` };
  }
}

test("passes when coverage is unchanged or higher", async () => {
  assert.equal((await run("80.00%", "80.00%")).code, 0);
  assert.equal((await run("80.00%", "80.01%")).code, 0);
});

test("fails when coverage decreases", async () => {
  const result = await run("80.00%", "79.99%");
  assert.equal(result.code, 1);
  assert.match(result.output, /coverage decreased/);
});

test("rejects invalid coverage values", async () => {
  const result = await run("not-a-number", "80%");
  assert.equal(result.code, 1);
  assert.match(result.output, /not a percentage/);
});
