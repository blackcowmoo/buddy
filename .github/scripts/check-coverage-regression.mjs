#!/usr/bin/env node

function argument(name) {
  const index = process.argv.indexOf(`--${name}`);
  if (index === -1 || !process.argv[index + 1]) {
    throw new Error(`Missing --${name}`);
  }
  return process.argv[index + 1];
}

function percentage(value, name) {
  const parsed = Number.parseFloat(value.replace(/%$/, ""));
  if (!Number.isFinite(parsed)) {
    throw new Error(`${name} is not a percentage: ${value}`);
  }
  return parsed;
}

const label = argument("label");
const base = percentage(argument("base"), "base coverage");
const head = percentage(argument("head"), "head coverage");

if (head < base) {
  console.error(
    `${label} coverage decreased from ${base.toFixed(2)}% to ${head.toFixed(2)}%. ` +
      "Test-only simplify changes must not decrease coverage.",
  );
  process.exit(1);
}

console.log(`${label} coverage: ${head.toFixed(2)}% (base ${base.toFixed(2)}%)`);
