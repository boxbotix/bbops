// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import assert from "node:assert/strict";
import { Buffer } from "node:buffer";
import { execFileSync } from "node:child_process";
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

function isPlaywrightPackageSpecifier(specifier) {
  const normalized = specifier.replace(/\\/g, "/");
  return (
    normalized === "@playwright/test" ||
    normalized === "playwright/test" ||
    normalized === "playwright" ||
    normalized.endsWith("/@playwright/test") ||
    normalized.endsWith("/playwright/test")
  );
}

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

const COMPARISON_OR_LOGICAL_OPERATORS = new Set([
  ts.SyntaxKind.EqualsEqualsToken,
  ts.SyntaxKind.EqualsEqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsToken,
  ts.SyntaxKind.ExclamationEqualsEqualsToken,
  ts.SyntaxKind.LessThanToken,
  ts.SyntaxKind.LessThanEqualsToken,
  ts.SyntaxKind.GreaterThanToken,
  ts.SyntaxKind.GreaterThanEqualsToken,
  ts.SyntaxKind.AmpersandAmpersandToken,
  ts.SyntaxKind.BarBarToken,
]);

function isDirectStringExpr(expr) {
  const unwrapped = unwrapExpression(expr);
  if (!unwrapped) {
    return false;
  }
  if (
    ts.isStringLiteral(unwrapped) ||
    ts.isNoSubstitutionTemplateLiteral(unwrapped) ||
    ts.isTemplateExpression(unwrapped)
  ) {
    return true;
  }
  return (
    ts.isCallExpression(unwrapped) &&
    ts.isIdentifier(unwrapped.expression) &&
    unwrapped.expression.text === "String"
  );
}

function isDirectFunctionExpr(expr) {
  const unwrapped = unwrapExpression(expr);
  return (
    !!unwrapped &&
    (ts.isArrowFunction(unwrapped) || ts.isFunctionExpression(unwrapped))
  );
}

function isDirectBooleanExpr(expr) {
  const unwrapped = unwrapExpression(expr);
  if (!unwrapped) {
    return false;
  }
  if (
    unwrapped.kind === ts.SyntaxKind.TrueKeyword ||
    unwrapped.kind === ts.SyntaxKind.FalseKeyword
  ) {
    return true;
  }
  if (
    ts.isPrefixUnaryExpression(unwrapped) &&
    unwrapped.operator === ts.SyntaxKind.ExclamationToken
  ) {
    return true;
  }
  if (
    ts.isBinaryExpression(unwrapped) &&
    COMPARISON_OR_LOGICAL_OPERATORS.has(unwrapped.operatorToken.kind)
  ) {
    return true;
  }
  return (
    ts.isCallExpression(unwrapped) &&
    ts.isIdentifier(unwrapped.expression) &&
    unwrapped.expression.text === "Boolean"
  );
}

function isStringLike(expr, bindings) {
  if (isDirectStringExpr(expr)) {
    return true;
  }
  const unwrapped = unwrapExpression(expr);
  return (
    !!unwrapped &&
    ts.isIdentifier(unwrapped) &&
    bindings.stringIds.has(unwrapped.text)
  );
}

function isFunctionLike(expr, bindings) {
  if (isDirectFunctionExpr(expr)) {
    return true;
  }
  const unwrapped = unwrapExpression(expr);
  return (
    !!unwrapped &&
    ts.isIdentifier(unwrapped) &&
    bindings.functionIds.has(unwrapped.text)
  );
}

function isBooleanLike(expr, bindings) {
  if (isDirectBooleanExpr(expr)) {
    return true;
  }
  const unwrapped = unwrapExpression(expr);
  return (
    !!unwrapped &&
    ts.isIdentifier(unwrapped) &&
    bindings.booleanIds.has(unwrapped.text)
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
    ts.isIdentifier(unwrapped.expression)
  ) {
    const ns = unwrapped.expression.text;
    if (bindings.namespaces.has(ns) && unwrapped.name.text === "test") {
      return true;
    }
    if (
      bindings.defaultExportNamespaces.has(ns) &&
      unwrapped.name.text === "default"
    ) {
      return true;
    }
  }
  return false;
}

function isExtendCallOfTestRoot(expr, bindings) {
  const unwrapped = unwrapExpression(expr);
  return (
    !!unwrapped &&
    ts.isCallExpression(unwrapped) &&
    ts.isPropertyAccessExpression(unwrapped.expression) &&
    unwrapped.expression.name.text === "extend" &&
    isTestRootExpression(unwrapped.expression.expression, bindings)
  );
}

function resolveRelativeModuleFile(fromAbsFile, relSpecifier) {
  const base = path.resolve(path.dirname(fromAbsFile), relSpecifier);
  const candidates = [
    base,
    `${base}.ts`,
    `${base}.mts`,
    `${base}.js`,
    `${base}.mjs`,
    path.join(base, "index.ts"),
    path.join(base, "index.js"),
  ];
  for (const cand of candidates) {
    if (fs.existsSync(cand) && fs.statSync(cand).isFile()) {
      return cand;
    }
  }
  return null;
}

function doesModuleDefaultExportTest(
  specifier,
  fromAbsFile,
  visitedFiles = new Set(),
) {
  if (isPlaywrightPackageSpecifier(specifier)) {
    return true;
  }
  if (!specifier.startsWith("./") && !specifier.startsWith("../")) {
    return false;
  }
  if (fromAbsFile && fs.existsSync(fromAbsFile)) {
    const targetFile = resolveRelativeModuleFile(fromAbsFile, specifier);
    if (targetFile) {
      if (visitedFiles.has(targetFile)) {
        return false;
      }
      const nextVisited = new Set(visitedFiles);
      nextVisited.add(targetFile);
      const childText = fs.readFileSync(targetFile, "utf8");
      const childSf = ts.createSourceFile(
        targetFile,
        childText,
        ts.ScriptTarget.Latest,
        true,
        ts.ScriptKind.TS,
      );
      const childBindings = collectPlaywrightBindings(
        childSf,
        targetFile,
        nextVisited,
      );
      for (const stmt of childSf.statements) {
        if (ts.isExportAssignment(stmt) && !stmt.isExportEquals) {
          if (
            isTestRootExpression(stmt.expression, childBindings) ||
            isExtendCallOfTestRoot(stmt.expression, childBindings)
          ) {
            return true;
          }
        }
        if (
          ts.isExportDeclaration(stmt) &&
          stmt.exportClause &&
          ts.isNamedExports(stmt.exportClause)
        ) {
          const reexportSpec =
            stmt.moduleSpecifier && ts.isStringLiteral(stmt.moduleSpecifier)
              ? stmt.moduleSpecifier.text
              : null;
          for (const el of stmt.exportClause.elements) {
            if (el.name.text !== "default") {
              continue;
            }
            const origName = (el.propertyName ?? el.name).text;
            if (reexportSpec) {
              if (
                origName === "test" ||
                (origName === "default" &&
                  doesModuleDefaultExportTest(
                    reexportSpec,
                    targetFile,
                    nextVisited,
                  ))
              ) {
                return true;
              }
            } else if (childBindings.testIds.has(origName)) {
              return true;
            }
          }
        }
      }
      return false;
    }
  }
  // Fallback for in-memory synthetic unit tests without on-disk sibling modules.
  const baseName = path.basename(specifier).replace(/\.[cm]?[jt]sx?$/, "");
  return baseName === "fixtures" || baseName.endsWith("-fixture");
}

function collectPlaywrightBindings(
  sf,
  absFilePath = null,
  visitedFiles = new Set(),
) {
  const testIds = new Set();
  const shadowedNonTestIds = new Set();
  const namespaces = new Set();
  const defaultExportNamespaces = new Set();
  const functionIds = new Set();
  const stringIds = new Set();
  const booleanIds = new Set();

  for (const stmt of sf.statements) {
    if (!ts.isImportDeclaration(stmt) || !stmt.importClause) {
      continue;
    }
    if (stmt.importClause.isTypeOnly) {
      continue;
    }
    const specifier = ts.isStringLiteral(stmt.moduleSpecifier)
      ? stmt.moduleSpecifier.text
      : "";
    const moduleHasDefaultTest = doesModuleDefaultExportTest(
      specifier,
      absFilePath,
      visitedFiles,
    );

    if (stmt.importClause.name) {
      const defaultLocal = stmt.importClause.name.text;
      if (moduleHasDefaultTest) {
        testIds.add(defaultLocal);
      } else {
        shadowedNonTestIds.add(defaultLocal);
      }
    }

    const namedBindings = stmt.importClause.namedBindings;
    if (!namedBindings) {
      continue;
    }
    if (ts.isNamespaceImport(namedBindings)) {
      namespaces.add(namedBindings.name.text);
      if (moduleHasDefaultTest) {
        defaultExportNamespaces.add(namedBindings.name.text);
      }
      continue;
    }
    if (ts.isNamedImports(namedBindings)) {
      for (const spec of namedBindings.elements) {
        if (spec.isTypeOnly) {
          continue;
        }
        const importedName = (spec.propertyName ?? spec.name).text;
        const localName = spec.name.text;
        if (
          importedName === "test" ||
          (importedName === "default" && moduleHasDefaultTest)
        ) {
          testIds.add(localName);
        } else {
          shadowedNonTestIds.add(localName);
        }
      }
    }
  }

  const varDecls = [];
  const collectDecls = (node) => {
    if (ts.isFunctionDeclaration(node) && node.name) {
      functionIds.add(node.name.text);
      if (node.name.text === "test") {
        shadowedNonTestIds.add("test");
      }
    }
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name)) {
      varDecls.push(node);
    }
    ts.forEachChild(node, collectDecls);
  };
  collectDecls(sf);

  if (!shadowedNonTestIds.has("test")) {
    testIds.add("test");
  }

  const bindings = {
    testIds,
    namespaces,
    defaultExportNamespaces,
    functionIds,
    stringIds,
    booleanIds,
  };

  let changed = true;
  while (changed) {
    changed = false;
    for (const decl of varDecls) {
      const localName = decl.name.text;
      if (!decl.initializer) {
        continue;
      }
      const init = unwrapExpression(decl.initializer);
      if (!init) {
        continue;
      }
      if (
        !testIds.has(localName) &&
        (isTestRootExpression(init, bindings) ||
          isExtendCallOfTestRoot(init, bindings))
      ) {
        testIds.add(localName);
        changed = true;
        continue;
      }
      if (!functionIds.has(localName) && isFunctionLike(init, bindings)) {
        functionIds.add(localName);
        changed = true;
      }
      if (!stringIds.has(localName) && isStringLike(init, bindings)) {
        stringIds.add(localName);
        changed = true;
      }
      if (!booleanIds.has(localName) && isBooleanLike(init, bindings)) {
        booleanIds.add(localName);
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
  if (lastModifier && CONDITIONAL_MODIFIERS.has(lastModifier)) {
    if (args.length >= 3) {
      return "declaration";
    }
    const firstArg = args[0];
    const secondArg = args[1];
    if (
      (isBooleanLike(firstArg, bindings) ||
        isFunctionLike(firstArg, bindings)) &&
      !isFunctionLike(secondArg, bindings)
    ) {
      return "annotation";
    }
    if (
      isStringLike(secondArg, bindings) ||
      unwrapExpression(secondArg)?.kind === ts.SyntaxKind.UndefinedKeyword
    ) {
      return "annotation";
    }
    return "declaration";
  }
  return "declaration";
}

function getPlaywrightCallSite(sf, callExpr) {
  const callee = unwrapExpression(callExpr.expression);
  const targetNode = ts.isPropertyAccessExpression(callee)
    ? callee.name
    : callExpr;
  const pos = sf.getLineAndCharacterOfPosition(targetNode.getStart(sf, false));
  return `${pos.line + 1}:${pos.character + 1}`;
}

function countConciseDeclarationsInNode(rootNode, bindings, insideTestOrHook) {
  let count = 0;
  const walk = (node, inBody) => {
    if (ts.isBlock(node)) {
      return;
    }
    if (ts.isCallExpression(node)) {
      const kind = classifyPlaywrightCall(node, bindings, inBody);
      if (kind === "suite") {
        return;
      }
      if (kind === "hook") {
        return;
      }
      if (kind === "declaration") {
        count += 1;
        return;
      }
    }
    ts.forEachChild(node, (child) => walk(child, inBody));
  };
  walk(rootNode, insideTestOrHook);
  return count;
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

export function validateSourceSpecAttribution(
  filePath,
  sourceText,
  specIndex,
  absFilePath = null,
) {
  const sf = ts.createSourceFile(
    filePath,
    sourceText,
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TS,
  );
  const bindings = collectPlaywrightBindings(sf, absFilePath);
  const errors = [];
  const callSites = new Set();
  let testCount = 0;

  const visit = (node, insideTestOrHook, enclosingConciseStmt) => {
    if (ts.isBlock(node)) {
      ts.forEachChild(node, (child) => visit(child, insideTestOrHook, null));
      return;
    }

    if (ts.isExpressionStatement(node)) {
      const directCall = ts.isCallExpression(node.expression)
        ? classifyPlaywrightCall(node.expression, bindings, insideTestOrHook)
        : "other";
      let nextConciseStmt = null;
      if (
        directCall !== "declaration" &&
        directCall !== "suite" &&
        directCall !== "hook" &&
        countConciseDeclarationsInNode(
          node.expression,
          bindings,
          insideTestOrHook,
        ) === 1
      ) {
        nextConciseStmt = node;
      }
      ts.forEachChild(node, (child) =>
        visit(child, insideTestOrHook, nextConciseStmt),
      );
      return;
    }

    if (ts.isStatement(node) && !ts.isSourceFile(node)) {
      ts.forEachChild(node, (child) => visit(child, insideTestOrHook, null));
      return;
    }

    if (ts.isCallExpression(node)) {
      const kind = classifyPlaywrightCall(node, bindings, insideTestOrHook);
      if (kind === "declaration") {
        testCount += 1;
        callSites.add(getPlaywrightCallSite(sf, node));
        const callStart = node.getStart(sf, false);
        const { line } = sf.getLineAndCharacterOfPosition(callStart);
        let comments = extractAttachedLineComments(sourceText, callStart);
        if (comments.length === 0 && enclosingConciseStmt) {
          comments = extractAttachedLineComments(
            sourceText,
            enclosingConciseStmt.getStart(sf, false),
          );
        }
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
        ts.forEachChild(node, (child) => visit(child, true, null));
        return;
      }
      if (kind === "hook") {
        ts.forEachChild(node, (child) => visit(child, true, null));
        return;
      }
      if (kind === "suite") {
        ts.forEachChild(node, (child) => visit(child, insideTestOrHook, null));
        return;
      }
    }

    ts.forEachChild(node, (child) =>
      visit(child, insideTestOrHook, enclosingConciseStmt),
    );
  };

  visit(sf, false, null);
  return { testCount, errors, callSites };
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

function resolvePlaywrightCli(repoRoot) {
  const localBin = path.join(
    repoRoot,
    "ui",
    "node_modules",
    ".bin",
    "playwright",
  );
  if (fs.existsSync(localBin)) {
    return localBin;
  }
  const fallbackBin = path.resolve(
    path.dirname(fileURLToPath(import.meta.url)),
    "..",
    "node_modules",
    ".bin",
    "playwright",
  );
  return fs.existsSync(fallbackBin) ? fallbackBin : null;
}

export function collectPlaywrightListedSites(repoRoot) {
  const playwrightBin = resolvePlaywrightCli(repoRoot);
  if (!playwrightBin) {
    return {
      ok: false,
      byFile: new Map(),
      errors: [
        "ui/e2e: Playwright CLI binary not found for --list verification",
      ],
    };
  }

  const uiDir = path.join(repoRoot, "ui");
  const existingConfig = path.join(uiDir, "playwright.config.ts");
  const tempWorkDir = fs.mkdtempSync(path.join(os.tmpdir(), "bbops-pw-list-"));
  const tempReportPath = path.join(tempWorkDir, "report.json");
  let configToUse = existingConfig;

  let rawJson = "";
  try {
    if (!fs.existsSync(existingConfig)) {
      const tempConfigPath = path.join(
        tempWorkDir,
        "playwright.list.config.mjs",
      );
      const e2eTestDir = path.join(uiDir, "e2e");
      fs.writeFileSync(
        tempConfigPath,
        `export default { testDir: ${JSON.stringify(e2eTestDir)}, testMatch: "**/*.e2e.ts", outputDir: ${JSON.stringify(path.join(tempWorkDir, "results"))} };\n`,
        "utf8",
      );
      configToUse = tempConfigPath;
    }

    const childEnv = { ...process.env };
    for (const key of Object.keys(childEnv)) {
      if (
        (key.startsWith("PLAYWRIGHT_") &&
          (key.includes("_OUTPUT_") || key === "PLAYWRIGHT_HTML_REPORT")) ||
        key === "PW_TEST_REPORTER" ||
        key === "PW_TEST_HTML_REPORT_OPEN" ||
        key === "JEST_WORKER_ID"
      ) {
        delete childEnv[key];
      }
    }
    childEnv.PLAYWRIGHT_JSON_OUTPUT_FILE = tempReportPath;
    childEnv.BBOPS_PLAYWRIGHT_OUTPUT_DIR = path.join(tempWorkDir, "results");

    try {
      execFileSync(
        playwrightBin,
        ["test", "-c", configToUse, "--list", "--reporter=json"],
        {
          cwd: fs.existsSync(uiDir) ? uiDir : repoRoot,
          env: childEnv,
          encoding: "utf8",
          stdio: ["ignore", "pipe", "pipe"],
        },
      );
      if (fs.existsSync(tempReportPath)) {
        rawJson = fs.readFileSync(tempReportPath, "utf8");
      }
    } catch (err) {
      if (fs.existsSync(tempReportPath)) {
        rawJson = fs.readFileSync(tempReportPath, "utf8");
      } else {
        const stderrText =
          err && typeof err === "object" && "stderr" in err && err.stderr
            ? String(err.stderr).trim()
            : String(err);
        return {
          ok: false,
          byFile: new Map(),
          errors: [`ui/e2e: playwright test --list failed: ${stderrText}`],
        };
      }
    }
  } finally {
    fs.rmSync(tempWorkDir, { recursive: true, force: true });
  }

  let parsed;
  try {
    parsed = JSON.parse(rawJson);
  } catch (err) {
    return {
      ok: false,
      byFile: new Map(),
      errors: [
        `ui/e2e: failed to parse playwright --list JSON: ${String(err)}`,
      ],
    };
  }

  const errors = [];
  if (Array.isArray(parsed.errors)) {
    for (const item of parsed.errors) {
      const loc = item?.location
        ? `${item.location.file}:${item.location.line}:${item.location.column}`
        : "ui/e2e";
      errors.push(`${loc}: ${item?.message ?? "Playwright collection error"}`);
    }
  }

  const realRepoRoot = fs.realpathSync(repoRoot);
  const rootDir = parsed?.config?.rootDir
    ? path.resolve(parsed.config.rootDir)
    : path.join(repoRoot, "ui", "e2e");
  const byFile = new Map();

  const walkSuite = (suite) => {
    for (const spec of suite?.specs ?? []) {
      const candidateAbs = path.resolve(rootDir, spec.file);
      const realSpecPath = fs.existsSync(candidateAbs)
        ? fs.realpathSync(candidateAbs)
        : candidateAbs;
      const relFile = path
        .relative(realRepoRoot, realSpecPath)
        .split(path.sep)
        .join("/");
      let fileMap = byFile.get(relFile);
      if (!fileMap) {
        fileMap = new Map();
        byFile.set(relFile, fileMap);
      }
      const key = `${spec.line}:${spec.column}`;
      if (!fileMap.has(key)) {
        fileMap.set(key, spec.title);
      }
    }
    for (const child of suite?.suites ?? []) {
      walkSuite(child);
    }
  };

  for (const topSuite of parsed?.suites ?? []) {
    walkSuite(topSuite);
  }

  return { ok: errors.length === 0, byFile, errors };
}

export function validateRepoE2eAttribution(repoRoot) {
  const normalizedRepoRoot = fs.existsSync(repoRoot)
    ? fs.realpathSync(repoRoot)
    : path.resolve(repoRoot);
  const specIndex = loadOpenSpecIndex(normalizedRepoRoot);
  const e2eDir = path.join(normalizedRepoRoot, "ui", "e2e");
  const files = listE2eFiles(e2eDir);
  if (files.length === 0) {
    return {
      ok: false,
      testCount: 0,
      errors: ["ui/e2e: expected at least one *.e2e.ts Playwright suite"],
    };
  }
  const allErrors = [];
  const astSitesByFile = new Map();
  let totalTests = 0;

  for (const absFile of files) {
    const relFile = path
      .relative(normalizedRepoRoot, absFile)
      .split(path.sep)
      .join("/");
    const text = fs.readFileSync(absFile, "utf8");
    const { testCount, errors, callSites } = validateSourceSpecAttribution(
      relFile,
      text,
      specIndex,
      absFile,
    );
    astSitesByFile.set(relFile, callSites);
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

  // Independent oracle: cross-check AST declaration locations against Playwright's own test collector (`--list --reporter=json`).
  const listed = collectPlaywrightListedSites(normalizedRepoRoot);
  allErrors.push(...listed.errors);
  if (listed.ok) {
    for (const relFile of astSitesByFile.keys()) {
      const collectedSites = listed.byFile.get(relFile);
      if (!collectedSites || collectedSites.size === 0) {
        if (!allErrors.some((e) => e.startsWith(`${relFile}:`))) {
          allErrors.push(
            `${relFile}: Playwright '--list' collected zero test declarations`,
          );
        }
      }
    }

    for (const [collectedFile, collectedSites] of listed.byFile.entries()) {
      const astSites = astSitesByFile.get(collectedFile);
      if (!astSites) {
        for (const [siteKey, title] of collectedSites.entries()) {
          allErrors.push(
            `${collectedFile}:${siteKey}: Playwright collected test '${title}' outside supported ui/e2e/**/*.e2e.ts suite files`,
          );
        }
        continue;
      }
      for (const [siteKey, title] of collectedSites.entries()) {
        if (!astSites.has(siteKey)) {
          allErrors.push(
            `${collectedFile}:${siteKey}: Playwright collected test '${title}' via an unrecognized or unattributed declaration form`,
          );
        }
      }
    }
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

  const pwModuleAbs = path.resolve(
    path.dirname(fileURLToPath(import.meta.url)),
    "..",
    "node_modules",
    "@playwright",
    "test",
  );

  // Full repo + Playwright `--list` oracle self-tests covering:
  // 1) per-file zero-declaration gate,
  // 2) expression-body declarations (`["boat", "rover"].forEach(name => test(name, async () => {}))`) failing uncited then passing cited,
  // 3) default-imported test (`import scenario from ...`) + `scenario.skip(title, body)` with named values failing uncited then passing cited,
  // 4) unrecognized indirect declaration caught by Playwright `--list` location oracle.
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
    const tmpE2eDir = path.join(tmpRepo, "ui", "e2e");
    fs.mkdirSync(tmpE2eDir, { recursive: true });
    fs.writeFileSync(
      path.join(tmpE2eDir, "local-default.ts"),
      `import pwTest from ${JSON.stringify(pwModuleAbs)};\nexport default pwTest;\n`,
      "utf8",
    );

    // Case A: empty *.e2e.ts alongside a valid *.e2e.ts fails closed.
    fs.writeFileSync(
      path.join(tmpE2eDir, "valid.e2e.ts"),
      `import { test } from ${JSON.stringify(pwModuleAbs)};\n// spec: cop-ui / Full-stack browser E2E requirement\ntest("valid", async () => {});\n`,
      "utf8",
    );
    fs.writeFileSync(
      path.join(tmpE2eDir, "empty.e2e.ts"),
      `export const helperOnly = 42;\n`,
      "utf8",
    );
    const emptyRepoRes = validateRepoE2eAttribution(tmpRepo);
    assert.equal(emptyRepoRes.ok, false);
    assert.equal(
      emptyRepoRes.errors.some((e) =>
        e.includes("ui/e2e/empty.e2e.ts: zero Playwright test() declarations"),
      ),
      true,
    );
    fs.rmSync(path.join(tmpE2eDir, "empty.e2e.ts"));

    // Case B: mixed file with cited baseline + uncited expression-body forEach declaration fails closed, then passes once cited.
    fs.writeFileSync(
      path.join(tmpE2eDir, "foreach.e2e.ts"),
      [
        `import { test } from ${JSON.stringify(pwModuleAbs)};`,
        `// spec: cop-ui / Full-stack browser E2E requirement`,
        `test("cited baseline", async () => {});`,
        `["boat", "rover"].forEach((name) => test(name, async () => {}));`,
      ].join("\n") + "\n",
      "utf8",
    );
    const uncitedForEachListed = collectPlaywrightListedSites(tmpRepo);
    assert.equal(uncitedForEachListed.ok, true);
    assert.equal(
      uncitedForEachListed.byFile.get("ui/e2e/foreach.e2e.ts")?.size,
      2,
    );
    const uncitedForEachRes = validateRepoE2eAttribution(tmpRepo);
    assert.equal(uncitedForEachRes.ok, false);
    assert.equal(
      uncitedForEachRes.errors.some(
        (e) =>
          e.includes("ui/e2e/foreach.e2e.ts:4:") &&
          e.includes("missing attached"),
      ),
      true,
    );

    fs.writeFileSync(
      path.join(tmpE2eDir, "foreach.e2e.ts"),
      [
        `import { test } from ${JSON.stringify(pwModuleAbs)};`,
        `// spec: cop-ui / Full-stack browser E2E requirement`,
        `test("cited baseline", async () => {});`,
        `// spec: cop-ui / Full-stack browser E2E requirement`,
        `["boat", "rover"].forEach((name) => test(name, async () => {}));`,
      ].join("\n") + "\n",
      "utf8",
    );
    const citedForEachRes = validateRepoE2eAttribution(tmpRepo);
    assert.equal(citedForEachRes.ok, true, citedForEachRes.errors.join("\n"));

    // Case C: default-imported test (both package default and relative module default) + test.skip(title, body) with named values fails uncited, then passes cited.
    fs.writeFileSync(
      path.join(tmpE2eDir, "default-named.e2e.ts"),
      [
        `import pkgDefaultTest from ${JSON.stringify(pwModuleAbs)};`,
        `import localDefaultTest from "./local-default";`,
        `const skippedTitlePkg = "named package skip title";`,
        `const skippedTitleLocal = "named relative skip title";`,
        `const skippedBody = async () => {};`,
        `// spec: cop-ui / Full-stack browser E2E requirement`,
        `pkgDefaultTest("cited default baseline", async () => {});`,
        `pkgDefaultTest.skip(skippedTitlePkg, skippedBody);`,
        `localDefaultTest.skip(skippedTitleLocal, skippedBody);`,
      ].join("\n") + "\n",
      "utf8",
    );
    const uncitedDefaultRes = validateRepoE2eAttribution(tmpRepo);
    assert.equal(uncitedDefaultRes.ok, false);
    assert.equal(
      uncitedDefaultRes.errors.filter((e) =>
        e.includes("ui/e2e/default-named.e2e.ts"),
      ).length,
      2,
    );

    fs.writeFileSync(
      path.join(tmpE2eDir, "default-named.e2e.ts"),
      [
        `import pkgDefaultTest from ${JSON.stringify(pwModuleAbs)};`,
        `import localDefaultTest from "./local-default";`,
        `const skippedTitlePkg = "named package skip title";`,
        `const skippedTitleLocal = "named relative skip title";`,
        `const skippedBody = async () => {};`,
        `// spec: cop-ui / Full-stack browser E2E requirement`,
        `pkgDefaultTest("cited default baseline", async () => {});`,
        `//aip:verifies LiveBrowserSync`,
        `pkgDefaultTest.skip(skippedTitlePkg, skippedBody);`,
        `// spec: cop-ui / Full-stack browser E2E requirement`,
        `localDefaultTest.skip(skippedTitleLocal, skippedBody);`,
      ].join("\n") + "\n",
      "utf8",
    );
    const citedDefaultRes = validateRepoE2eAttribution(tmpRepo);
    assert.equal(citedDefaultRes.ok, true, citedDefaultRes.errors.join("\n"));

    // Case D: dynamic/unsupported wrapper hiding test() from static AST still fails closed via Playwright's `--list` location oracle.
    fs.writeFileSync(
      path.join(tmpE2eDir, "dynamic.e2e.ts"),
      [
        `import { test } from ${JSON.stringify(pwModuleAbs)};`,
        `const bag = { run: test };`,
        `// spec: cop-ui / Full-stack browser E2E requirement`,
        `test("cited baseline", async () => {});`,
        `bag.run("hidden dynamic declaration", async () => {});`,
      ].join("\n") + "\n",
      "utf8",
    );
    const dynamicRes = validateRepoE2eAttribution(tmpRepo);
    assert.equal(dynamicRes.ok, false);
    assert.equal(
      dynamicRes.errors.some(
        (e) =>
          e.includes("ui/e2e/dynamic.e2e.ts:5:") &&
          e.includes("unrecognized or unattributed declaration form"),
      ),
      true,
    );
    fs.rmSync(path.join(tmpE2eDir, "dynamic.e2e.ts"));

    // Case E: caller PLAYWRIGHT_JSON_OUTPUT_FILE / PLAYWRIGHT_JSON_OUTPUT_DIR / PLAYWRIGHT_JSON_OUTPUT_NAME stay byte-identical while collection succeeds.
    const callerReportDir = path.join(tmpRepo, "caller-reports");
    fs.mkdirSync(callerReportDir, { recursive: true });
    const callerReportFile = path.join(callerReportDir, "caller-report.json");
    const sentinelBytes = Buffer.from(
      '{"sentinel":"caller-report-must-stay-byte-identical"}\n',
      "utf8",
    );
    fs.writeFileSync(callerReportFile, sentinelBytes);

    const savedEnv = {
      PLAYWRIGHT_JSON_OUTPUT_FILE: process.env.PLAYWRIGHT_JSON_OUTPUT_FILE,
      PLAYWRIGHT_JSON_OUTPUT_DIR: process.env.PLAYWRIGHT_JSON_OUTPUT_DIR,
      PLAYWRIGHT_JSON_OUTPUT_NAME: process.env.PLAYWRIGHT_JSON_OUTPUT_NAME,
    };
    process.env.PLAYWRIGHT_JSON_OUTPUT_FILE = callerReportFile;
    process.env.PLAYWRIGHT_JSON_OUTPUT_DIR = callerReportDir;
    process.env.PLAYWRIGHT_JSON_OUTPUT_NAME = "caller-report.json";
    try {
      const realCheckoutRoot = path.resolve(
        path.dirname(fileURLToPath(import.meta.url)),
        "..",
        "..",
      );
      const isolatedTmpRes = validateRepoE2eAttribution(tmpRepo);
      assert.equal(isolatedTmpRes.ok, true, isolatedTmpRes.errors.join("\n"));
      const isolatedRealRes = validateRepoE2eAttribution(realCheckoutRoot);
      assert.equal(isolatedRealRes.ok, true, isolatedRealRes.errors.join("\n"));
      assert.equal(
        Buffer.compare(fs.readFileSync(callerReportFile), sentinelBytes),
        0,
      );
    } finally {
      for (const [key, val] of Object.entries(savedEnv)) {
        if (val === undefined) {
          delete process.env[key];
        } else {
          process.env[key] = val;
        }
      }
    }

    // Case F: helper file outside *.e2e.ts registering an uncited test is rejected with its file path and line.
    fs.writeFileSync(
      path.join(tmpE2eDir, "cases.ts"),
      [
        `import { test } from ${JSON.stringify(pwModuleAbs)};`,
        `export function registerCases() {`,
        `  test("uncited helper test", async () => {});`,
        `}`,
      ].join("\n") + "\n",
      "utf8",
    );
    fs.writeFileSync(
      path.join(tmpE2eDir, "helper-call.e2e.ts"),
      [
        `import { test } from ${JSON.stringify(pwModuleAbs)};`,
        `import { registerCases } from "./cases";`,
        `// spec: cop-ui / Full-stack browser E2E requirement`,
        `test("cited baseline", async () => {});`,
        `registerCases();`,
      ].join("\n") + "\n",
      "utf8",
    );
    const helperFileRes = validateRepoE2eAttribution(tmpRepo);
    assert.equal(helperFileRes.ok, false);
    assert.equal(
      helperFileRes.errors.some(
        (e) =>
          e.includes("ui/e2e/cases.ts:3:") &&
          e.includes("outside supported ui/e2e/**/*.e2e.ts suite files"),
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
