// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import assert from "node:assert/strict";
import console from "node:console";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";
import ts from "../node_modules/typescript/lib/typescript.js";

const NON_TEST_MEMBERS = new Set([
  "describe",
  "beforeEach",
  "afterEach",
  "beforeAll",
  "afterAll",
  "use",
  "slow",
  "setTimeout",
  "step",
  "info",
  "expect",
  "extend",
]);

function collectMarkdownHeadings(markdownText) {
  const requirements = new Set();
  const scenarios = new Set();
  let inFence = false;
  for (const rawLine of markdownText.split(/\r?\n/)) {
    const trimmed = rawLine.trim();
    if (/^(?:```|~~~)/.test(trimmed)) {
      inFence = !inFence;
      continue;
    }
    if (inFence) {
      continue;
    }
    const reqMatch = /^### Requirement:\s+(.+?)\s*$/.exec(rawLine);
    if (reqMatch) {
      requirements.add(reqMatch[1]);
      continue;
    }
    const scenMatch = /^#### Scenario:\s+([^\s]+)(?:\s|$)/.exec(rawLine);
    if (scenMatch) {
      scenarios.add(scenMatch[1]);
    }
  }
  return { requirements, scenarios };
}

export function loadOpenSpecIndex(repoRoot) {
  const byCapability = new Map();
  const byRelPath = new Map();
  const allScenarios = new Set();

  const recordSpecFile = (capability, absPath) => {
    if (!fs.existsSync(absPath)) {
      return;
    }
    const relPath = path.relative(repoRoot, absPath).split(path.sep).join("/");
    const parsed = collectMarkdownHeadings(fs.readFileSync(absPath, "utf8"));
    byRelPath.set(relPath, parsed.requirements);
    let capSet = byCapability.get(capability);
    if (!capSet) {
      capSet = new Set();
      byCapability.set(capability, capSet);
    }
    for (const req of parsed.requirements) {
      capSet.add(req);
    }
    for (const scen of parsed.scenarios) {
      allScenarios.add(scen);
    }
  };

  const specsDir = path.join(repoRoot, "openspec", "specs");
  if (fs.existsSync(specsDir)) {
    for (const entry of fs.readdirSync(specsDir, { withFileTypes: true })) {
      if (entry.isDirectory()) {
        recordSpecFile(entry.name, path.join(specsDir, entry.name, "spec.md"));
      }
    }
  }

  const changesDir = path.join(repoRoot, "openspec", "changes");
  if (fs.existsSync(changesDir)) {
    for (const change of fs.readdirSync(changesDir, { withFileTypes: true })) {
      if (!change.isDirectory() || change.name === "archive") {
        continue;
      }
      const changeSpecsDir = path.join(changesDir, change.name, "specs");
      if (!fs.existsSync(changeSpecsDir)) {
        continue;
      }
      for (const cap of fs.readdirSync(changeSpecsDir, {
        withFileTypes: true,
      })) {
        if (cap.isDirectory()) {
          recordSpecFile(
            cap.name,
            path.join(changeSpecsDir, cap.name, "spec.md"),
          );
        }
      }
    }
  }

  return { byCapability, byRelPath, allScenarios };
}

function isPlaywrightTestCallee(expr) {
  if (ts.isIdentifier(expr)) {
    return expr.text === "test";
  }
  if (ts.isPropertyAccessExpression(expr)) {
    if (NON_TEST_MEMBERS.has(expr.name.text)) {
      return false;
    }
    return isPlaywrightTestCallee(expr.expression);
  }
  return false;
}

function extractAttachedLineComments(sourceText, statementStartPos) {
  const lines = sourceText.slice(0, statementStartPos).split(/\r?\n/);
  // Last element is the indentation prefix on the declaration line itself.
  const declIndent = lines.pop() ?? "";
  if (declIndent.trim().length > 0) {
    return [];
  }
  const collected = [];
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    const trimmed = lines[i].trim();
    if (trimmed.length === 0) {
      break;
    }
    if (!trimmed.startsWith("//")) {
      break;
    }
    collected.unshift(trimmed);
  }
  return collected;
}

function validateCitationLine(commentLine, specIndex) {
  if (/^\/\/\s+aip:verifies\b/.test(commentLine)) {
    return `malformed spaced '// aip:verifies' citation (use '//aip:verifies <ID>')`;
  }
  if (commentLine.startsWith("//aip:verifies")) {
    const rest = commentLine.slice("//aip:verifies".length).trim();
    if (!rest) {
      return "empty //aip:verifies scenario list";
    }
    for (const id of rest.split(/\s+/)) {
      if (!specIndex.allScenarios.has(id)) {
        return `dangling scenario ID '${id}'`;
      }
    }
    return null;
  }
  const specMatch = /^\/\/\s*spec:\s*(.+?)\s*$/.exec(commentLine);
  if (!specMatch) {
    return "ignored";
  }
  const target = specMatch[1];
  if (target.includes("..")) {
    return `escaping spec reference '${target}'`;
  }
  const hashIdx = target.indexOf("#Requirement:");
  if (hashIdx > 0) {
    const relPath = target.slice(0, hashIdx).trim();
    const reqTitle = target.slice(hashIdx + "#Requirement:".length).trim();
    const reqs = specIndex.byRelPath.get(relPath);
    if (!reqs || !reqs.has(reqTitle)) {
      return `dangling requirement '${target}'`;
    }
    return null;
  }
  const slashIdx = target.indexOf(" / ");
  if (slashIdx > 0) {
    const capability = target.slice(0, slashIdx).trim();
    const reqTitle = target.slice(slashIdx + 3).trim();
    const reqs = specIndex.byCapability.get(capability);
    if (!reqs || !reqs.has(reqTitle)) {
      return `dangling requirement '${target}'`;
    }
    return null;
  }
  return `malformed // spec: citation '${target}'`;
}

export function validateSourceSpecAttribution(filePath, sourceText, specIndex) {
  const sf = ts.createSourceFile(
    filePath,
    sourceText,
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TS,
  );
  const errors = [];
  let testCount = 0;

  const visit = (node) => {
    if (
      ts.isExpressionStatement(node) &&
      ts.isCallExpression(node.expression) &&
      isPlaywrightTestCallee(node.expression.expression)
    ) {
      testCount += 1;
      const stmtStart = node.getStart(sf, false);
      const { line } = sf.getLineAndCharacterOfPosition(stmtStart);
      const comments = extractAttachedLineComments(sourceText, stmtStart);
      let validCitations = 0;
      for (const c of comments) {
        const err = validateCitationLine(c, specIndex);
        if (err === "ignored") {
          continue;
        }
        if (err !== null) {
          errors.push(`${filePath}:${line + 1}: ${err}`);
        } else {
          validCitations += 1;
        }
      }
      if (validCitations === 0) {
        errors.push(
          `${filePath}:${line + 1}: Playwright test() missing attached '// spec:' or '//aip:verifies' citation`,
        );
      }
    }
    ts.forEachChild(node, visit);
  };

  visit(sf);
  return { testCount, errors };
}

function listE2eFiles(dir) {
  if (!fs.existsSync(dir)) {
    return [];
  }
  const found = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      found.push(...listE2eFiles(full));
    } else if (entry.isFile() && entry.name.endsWith(".e2e.ts")) {
      found.push(full);
    }
  }
  return found.sort();
}

export function validateRepoE2eAttribution(repoRoot) {
  const specIndex = loadOpenSpecIndex(repoRoot);
  const e2eDir = path.join(repoRoot, "ui", "e2e");
  const files = listE2eFiles(e2eDir);
  if (files.length === 0) {
    return {
      ok: false,
      testCount: 0,
      errors: ["ui/e2e: expected at least one *.e2e.ts Playwright suite"],
    };
  }
  const allErrors = [];
  let totalTests = 0;
  for (const absFile of files) {
    const relFile = path.relative(repoRoot, absFile).split(path.sep).join("/");
    const text = fs.readFileSync(absFile, "utf8");
    const { testCount, errors } = validateSourceSpecAttribution(
      relFile,
      text,
      specIndex,
    );
    totalTests += testCount;
    allErrors.push(...errors);
  }
  if (totalTests === 0) {
    allErrors.push("ui/e2e: zero test() declarations discovered in *.e2e.ts");
  }
  return {
    ok: allErrors.length === 0,
    testCount: totalTests,
    errors: allErrors,
  };
}

function runSelfTests() {
  const fakeIndex = {
    byCapability: new Map([
      ["cop-ui", new Set(["Full-stack browser E2E requirement"])],
    ]),
    byRelPath: new Map([
      [
        "openspec/specs/cop-ui/spec.md",
        new Set(["Full-stack browser E2E requirement"]),
      ],
    ]),
    allScenarios: new Set(["LiveBrowserSync"]),
  };

  const validSrc = `
import { test } from "./fixtures";
test.describe("suite", () => {
  // spec: cop-ui / Full-stack browser E2E requirement
  test("passes shorthand", async () => {});

  // spec: openspec/specs/cop-ui/spec.md#Requirement: Full-stack browser E2E requirement
  test.only("passes explicit path", async () => {});

  //aip:verifies LiveBrowserSync
  test("passes scenario", async () => {});
});
`;
  const okRes = validateSourceSpecAttribution(
    "sample.e2e.ts",
    validSrc,
    fakeIndex,
  );
  assert.equal(okRes.testCount, 3);
  assert.deepEqual(okRes.errors, []);

  // Uncited test must fail closed.
  const uncitedRes = validateSourceSpecAttribution(
    "uncited.e2e.ts",
    `test("no citation", async () => {});\n`,
    fakeIndex,
  );
  assert.equal(uncitedRes.errors.length, 1);
  assert.match(uncitedRes.errors[0], /missing attached/);

  // Blank line detaching citation must fail closed.
  const detachedRes = validateSourceSpecAttribution(
    "detached.e2e.ts",
    `// spec: cop-ui / Full-stack browser E2E requirement\n\ntest("detached", async () => {});\n`,
    fakeIndex,
  );
  assert.equal(detachedRes.errors.length, 1);
  assert.match(detachedRes.errors[0], /missing attached/);

  // Dangling requirement title must fail closed.
  const danglingRes = validateSourceSpecAttribution(
    "dangling.e2e.ts",
    `// spec: cop-ui / Nonexistent requirement\ntest("dangling", async () => {});\n`,
    fakeIndex,
  );
  assert.equal(danglingRes.errors.length >= 1, true);
  assert.match(danglingRes.errors[0], /dangling requirement/);

  // Spaced // aip:verifies must fail closed.
  const spacedAipRes = validateSourceSpecAttribution(
    "spaced.e2e.ts",
    `// aip:verifies LiveBrowserSync\ntest("spaced", async () => {});\n`,
    fakeIndex,
  );
  assert.equal(spacedAipRes.errors.length >= 1, true);
  assert.match(spacedAipRes.errors[0], /malformed spaced/);
}

const isMain =
  process.argv[1] &&
  path.resolve(process.argv[1]) ===
    path.resolve(fileURLToPath(import.meta.url));

if (isMain) {
  const repoRoot = path.resolve(
    path.dirname(fileURLToPath(import.meta.url)),
    "..",
    "..",
  );
  if (process.argv.includes("--self-test")) {
    runSelfTests();
    console.log("e2e test-spec self-test: ok");
    process.exit(0);
  }
  const result = validateRepoE2eAttribution(repoRoot);
  if (!result.ok) {
    for (const err of result.errors) {
      console.error(err);
    }
    process.exit(1);
  }
  console.log(
    `e2e test-spec: ok (${result.testCount} Playwright test declaration(s) verified)`,
  );
}
