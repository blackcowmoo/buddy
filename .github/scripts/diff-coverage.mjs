#!/usr/bin/env node
// Reports which lines *added by this PR* are not covered by tests, as a
// collapsible Markdown fragment. Supports two coverage formats:
//   --format go    a `go test -coverprofile` profile
//   --format lcov  an lcov.info file (e.g. from @vitest/coverage-v8)
//
// Must be run from the repo root so `git diff` paths line up with the
// repo-relative paths the coverage tools report (after prefix mapping).

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i === -1 ? fallback : process.argv[i + 1];
}

const format = arg("format");
const coveragePath = arg("coverage");
const baseSha = arg("base");
const headSha = arg("head");
const pathPrefix = arg("path-prefix", "");
const modulePrefix = arg("module-prefix", "");
const repo = arg("repo");
const label = arg("label", pathPrefix);

function toRepoPath(reportedPath) {
  if (format === "go" && modulePrefix && reportedPath.startsWith(modulePrefix)) {
    return pathPrefix + reportedPath.slice(modulePrefix.length);
  }
  if (format === "lcov") {
    return pathPrefix ? `${pathPrefix}/${reportedPath}` : reportedPath;
  }
  return reportedPath;
}

// go coverprofile line: "<pkg-path>/<file>.go:startLine.startCol,endLine.endCol numStmt count"
function parseGoProfile(text) {
  const uncovered = new Map();
  for (const line of text.split("\n")) {
    if (!line || line.startsWith("mode:")) continue;
    const m = line.match(/^(\S+):(\d+)\.\d+,(\d+)\.\d+ \d+ (\d+)$/);
    if (!m) continue;
    const [, file, startLine, endLine, count] = m;
    if (count !== "0") continue;
    const repoPath = toRepoPath(file);
    if (!uncovered.has(repoPath)) uncovered.set(repoPath, new Set());
    for (let l = Number(startLine); l <= Number(endLine); l++) uncovered.get(repoPath).add(l);
  }
  return uncovered;
}

function parseLcov(text) {
  const uncovered = new Map();
  let current = null;
  for (const line of text.split("\n")) {
    if (line.startsWith("SF:")) {
      current = toRepoPath(line.slice(3).trim());
      if (!uncovered.has(current)) uncovered.set(current, new Set());
    } else if (line.startsWith("DA:") && current) {
      const [ln, hits] = line.slice(3).split(",").map(Number);
      if (hits === 0) uncovered.get(current).add(ln);
    } else if (line === "end_of_record") {
      current = null;
    }
  }
  return uncovered;
}

// Lines this PR actually added/modified, per file — only these are eligible
// to be reported, so pre-existing uncovered lines elsewhere stay out of it.
function parseDiffAddedLines(diffText) {
  const added = new Map();
  let current = null;
  for (const line of diffText.split("\n")) {
    if (line.startsWith("+++ ")) {
      const p = line.slice(4).trim();
      current = p === "/dev/null" ? null : p.replace(/^b\//, "");
      if (current && !added.has(current)) added.set(current, new Set());
      continue;
    }
    const m = line.match(/^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@/);
    if (m && current) {
      const start = Number(m[1]);
      const count = m[2] === undefined ? 1 : Number(m[2]);
      for (let i = 0; i < count; i++) added.get(current).add(start + i);
    }
  }
  return added;
}

function toRanges(sortedLines) {
  const ranges = [];
  let start = null;
  let prev = null;
  for (const n of sortedLines) {
    if (start === null) {
      start = n;
      prev = n;
      continue;
    }
    if (n === prev + 1) {
      prev = n;
      continue;
    }
    ranges.push([start, prev]);
    start = n;
    prev = n;
  }
  if (start !== null) ranges.push([start, prev]);
  return ranges;
}

if (!existsSync(coveragePath)) {
  console.log(`_${label}: no coverage report found — skipping._`);
  process.exit(0);
}

const uncovered =
  format === "go"
    ? parseGoProfile(readFileSync(coveragePath, "utf8"))
    : parseLcov(readFileSync(coveragePath, "utf8"));

const diffText = execFileSync(
  "git",
  ["diff", "--unified=0", "--no-color", baseSha, headSha, "--", pathPrefix],
  { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 },
);
const added = parseDiffAddedLines(diffText);

const fileEntries = [];
let totalNewUncovered = 0;
for (const [file, addedLines] of [...added.entries()].sort(([a], [b]) => a.localeCompare(b))) {
  const uncoveredForFile = uncovered.get(file);
  if (!uncoveredForFile) continue;
  const hit = [...addedLines].filter((l) => uncoveredForFile.has(l)).sort((a, b) => a - b);
  if (hit.length === 0) continue;
  totalNewUncovered += hit.length;
  const ranges = toRanges(hit);
  const parts = ranges.map(([s, e]) => {
    const text = s === e ? `L${s}` : `L${s}-L${e}`;
    return repo ? `[${text}](https://github.com/${repo}/blob/${headSha}/${file}#${text})` : text;
  });
  fileEntries.push(`- \`${file}\`: ${parts.join(", ")}`);
}

if (fileEntries.length === 0) {
  console.log(`**${label}**: all lines added by this PR are covered.`);
} else {
  console.log("<details>");
  console.log(`<summary><strong>${label}</strong>: ${totalNewUncovered} uncovered line(s) added by this PR</summary>`);
  console.log("");
  console.log(fileEntries.join("\n"));
  console.log("");
  console.log("</details>");
}
