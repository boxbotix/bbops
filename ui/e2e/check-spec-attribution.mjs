// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import assert from "node:assert/strict";
import console from "node:console";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";
import ts from "../node_modules/typescript/lib/typescript.js";

const HOOK_MEMBERS = new Set([
  "beforeEach",
  "afterEach",
  "beforeAll",
  "afterAll",
]);

const DECLARATION_MODIFIERS = new Set(["only", "skip", "fixme", "fail"]);
const CONDITIONAL_MODIFIERS = new Set(["skip", "fixme", "fail"]);

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

function unwrapExpression(expr) {
  let current = expr;
  while (
    current &&
    (ts.isParenthesizedExpression(current) ||
      ts.isAsExpression(current) ||
      ts.isSatisfiesExpression(current) ||
      ts.isNonNullExpression(current))
  ) {
    current = current.expression;
  }
  return current;
}

function isStringLike(expr) {
  const unwrapped = unwrapExpression(expr);
  return (
    !!unwrapped &&
    (ts.isStringLiteral(unwrapped) ||
      ts.isNoSubstitutionTemplateLiteral(unwrapped) ||
      ts.isTemplateExpression(unwrapped))
  );
}

function isFunctionLike(expr) {
  const unwrapped = unwrapExpression(expr);
  return (
    !!unwrapped &&
    (ts.isArrowFunction(unwrapped) || ts.isFunctionExpression(unwrapped))
  );
}

function isTestRootExpression(expr, bindings) {
  const unwrapped = unwrapExpression(expr);
  if (!unwrapped) {
    return false;
  }
  if (ts.isIdentifier(unwrapped)) {
    return bindings.testIds.has(unwrapped.text);
  }
  if (
    ts.isPropertyAccessExpression(unwrapped) &&
    ts.isIdentifier(unwrapped.expression) &&
    bindings.namespaces.has(unwrapped.expression.text) &&
    unwrapped.name.text === "test"
  ) {
    return true;
  }
  return false;
}

function collectPlaywrightBindings(sf) {
  const testIds = new Set();
  const shadowedNonTestIds = new Set();
  const namespaces = new Set();

  for (const stmt of sf.statements) {
    if (!ts.isImportDeclaration(stmt) || !stmt.importClause) {
      continue;
    }
    if (stmt.importClause.isTypeOnly) {
      continue;
    }
    const namedBindings = stmt.importClause.namedBindings;
    if (!namedBindings) {
      continue;
    }
    if (ts.isNamespaceImport(namedBindings)) {
      namespaces.add(namedBindings.name.text);
      continue;
    }
    if (ts.isNamedImports(namedBindings)) {
      for (const spec of namedBindings.elements) {
        if (spec.isTypeOnly) {
          continue;
        }
        const importedName = (spec.propertyName ?? spec.name).text;
        const localName = spec.name.text;
        if (importedName === "test") {
          testIds.add(localName);
        } else {
          shadowedNonTestIds.add(localName);
        }
      }
    }
  }

  if (!shadowedNonTestIds.has("test")) {
    testIds.add("test");
  }

  const varDecls = [];
  const collectVars = (node) => {
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name)) {
      varDecls.push(node);
    }
    ts.forEachChild(node, collectVars);
  };
  collectVars(sf);

  const bindings = { testIds, namespaces };
  let changed = true;
  while (changed) {
    changed = false;
    for (const decl of varDecls) {
      const localName = decl.name.text;
      if (testIds.has(localName) || !decl.initializer) {
        continue;
      }
      const init = unwrapExpression(decl.initializer);
      if (!init) {
        continue;
      }
      if (isTestRootExpression(init, bindings)) {
        testIds.add(localName);
        changed = true;
        continue;
      }
      if (
        ts.isCallExpression(init) &&
        ts.isPropertyAccessExpression(init.expression) &&
        init.expression.name.text === "extend" &&
        isTestRootExpression(init.expression.expression, bindings)
      ) {
        testIds.add(localName);
        changed = true;
      }
    }
  }

  return bindings;
}

function decomposePlaywrightCallee(expr, bindings) {
  const members = [];
  let cursor = unwrapExpression(expr);
  while (cursor) {
    if (isTestRootExpression(cursor, bindings)) {
      return members;
    }
    if (ts.isPropertyAccessExpression(cursor)) {
      members.unshift(cursor.name.text);
      cursor = unwrapExpression(cursor.expression);
      continue;
    }
    return null;
  }
  return null;
}

function classifyPlaywrightCall(callExpr, bindings, insideTestOrHook) {
  const members = decomposePlaywrightCallee(callExpr.expression, bindings);
  if (!members) {
    return "other";
  }
  if (members.includes("describe")) {
    return "suite";
  }
  if (members.length === 1 && HOOK_MEMBERS.has(members[0])) {
    return "hook";
  }
  if (insideTestOrHook) {
    return "annotation";
  }
  for (const member of members) {
    if (!DECLARATION_MODIFIERS.has(member)) {
      return "annotation";
    }
  }
  const args = callExpr.arguments;
  if (args.length < 2) {
    return "annotation";
  }
  const lastModifier = members.length > 0 ? members[members.length - 1] : null;
  const firstArg = args[0];
  const lastArg = args[args.length - 1];
  if (lastModifier && CONDITIONAL_MODIFIERS.has(lastModifier)) {
    if (
      isFunctionLike(lastArg) ||
      (isStringLike(firstArg) &&
        !isStringLike(lastArg) &&
        unwrapExpression(lastArg)?.kind !== ts.SyntaxKind.UndefinedKeyword)
    ) {
      return "declaration";
    }
    return "annotation";
  }
  return "declaration";
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
  const bindings = collectPlaywrightBindings(sf);
  const errors = [];
  let testCount = 0;

  const visit = (node, insideTestOrHook) => {
    if (
      ts.isExpressionStatement(node) &&
      ts.isCallExpression(node.expression)
    ) {
      const kind = classifyPlaywrightCall(
        node.expression,
        bindings,
        insideTestOrHook,
      );
      if (kind === "declaration") {
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
        ts.forEachChild(node, (child) => visit(child, true));
        return;
      }
      if (kind === "hook") {
        ts.forEachChild(node, (child) => visit(child, true));
        return;
      }
    }
    ts.forEachChild(node, (child) => visit(child, insideTestOrHook));
  };

  visit(sf, false);
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
    if (testCount === 0) {
      allErrors.push(
        `${relFile}: zero Playwright test() declarations discovered in *.e2e.ts`,
      );
    }
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

  // Imported/aliased/extended Playwright bindings must fail closed when uncited.
  const aliasedUncitedSrc = `
import { test as scenario } from "./fixtures";
import * as fx from "@playwright/test";
const extendedScenario = scenario.extend({});
const nsExtended = fx.test.extend({});

scenario("uncited alias", async () => {});
extendedScenario("uncited extend", async () => {});
fx.test("uncited namespace", async () => {});
nsExtended.skip("uncited skipped declaration", async () => {});
`;
  const aliasedRes = validateSourceSpecAttribution(
    "aliased.e2e.ts",
    aliasedUncitedSrc,
    fakeIndex,
  );
  assert.equal(aliasedRes.testCount, 4);
  assert.equal(aliasedRes.errors.length, 4);

  // Runtime and suite annotations (skip/fixme/fail without a declaration body) must NOT be misclassified as test declarations, while declaration overloads (skip/fixme/fail with title + body) ARE checked.
  const annotationsSrc = `
import { test as scenario } from "./fixtures";
scenario.describe("conditional suite", () => {
  scenario.skip(({ browserName }) => browserName === "webkit", "chromium only");
  scenario.fixme(false, "suite level condition");
  scenario.beforeEach(async () => {
    scenario.skip(false, "hook guard");
  });

  // spec: cop-ui / Full-stack browser E2E requirement
  scenario("attributed test with runtime annotations", async () => {
    scenario.skip(false, "runtime condition");
    scenario.fixme();
    scenario.fail(false, "conditional expected failure");
  });

  //aip:verifies LiveBrowserSync
  scenario.skip("attributed skipped declaration", async () => {});

  // spec: cop-ui / Full-stack browser E2E requirement
  scenario.fixme("attributed fixme declaration", { tag: "@wip" }, async () => {});
});
`;
  const annotationsRes = validateSourceSpecAttribution(
    "annotations.e2e.ts",
    annotationsSrc,
    fakeIndex,
  );
  assert.equal(annotationsRes.testCount, 3);
  assert.deepEqual(annotationsRes.errors, []);

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

  // Per-file zero-declaration gate: a second *.e2e.ts with 0 discovered tests must fail closed even when another *.e2e.ts has valid tests.
  const tmpRepo = fs.mkdtempSync(path.join(os.tmpdir(), "bbops-e2e-spec-"));
  try {
    fs.mkdirSync(path.join(tmpRepo, "openspec", "specs", "cop-ui"), {
      recursive: true,
    });
    fs.writeFileSync(
      path.join(tmpRepo, "openspec", "specs", "cop-ui", "spec.md"),
      "### Requirement: Full-stack browser E2E requirement\n#### Scenario: LiveBrowserSync\n",
      "utf8",
    );
    fs.mkdirSync(path.join(tmpRepo, "ui", "e2e"), { recursive: true });
    fs.writeFileSync(
      path.join(tmpRepo, "ui", "e2e", "valid.e2e.ts"),
      `import { test } from "./fixtures";\n// spec: cop-ui / Full-stack browser E2E requirement\ntest("valid", async () => {});\n`,
      "utf8",
    );
    fs.writeFileSync(
      path.join(tmpRepo, "ui", "e2e", "empty.e2e.ts"),
      `export const helperOnly = 42;\n`,
      "utf8",
    );
    const repoRes = validateRepoE2eAttribution(tmpRepo);
    assert.equal(repoRes.ok, false);
    assert.equal(
      repoRes.errors.some((e) =>
        e.includes("ui/e2e/empty.e2e.ts: zero Playwright test() declarations"),
      ),
      true,
    );
  } finally {
    fs.rmSync(tmpRepo, { recursive: true, force: true });
  }
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
