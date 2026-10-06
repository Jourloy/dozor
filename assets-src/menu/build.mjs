// Builds the Dozor menu: the shared Sidebar of @jourloy/00 (React, source-only) as a vendor bundle for the vanilla
// web UI. Outputs internal/dozor/web/vendor/a00-menu.{js,css} and a00-menu-LICENSES.txt; they are committed, so
// `go build` never needs Node. Run through `make menu`.
//
//   A00_DIR   directory of the @jourloy/00 package
//             (default: ../monorepo-frontend/packages/00 next to the dozor checkout). It is read-only here.
//
// Resolution:
//   - `@jourloy/00/...` follows the `exports` map of the package at A00_DIR (no file: dependency, no copy).
//   - Every other bare import made by the package (react, clsx, tailwind-merge, lucide-react) is resolved from
//     assets-src/menu/node_modules, never from the monorepo's: exactly one React, and the versions are the ones
//     of package-lock.json.
//   - `next/link` is replaced by src/stubs/next-link.tsx. Any other `next/*` import fails the build.
//   - The Sidebar markup uses Tailwind utility classes (the package contract: the host scans packages/00/src), so
//     the Tailwind CLI generates exactly the utilities of the three Sidebar components; the rest is CSS Modules.
//   - Fonts and the --a00-* tokens are not bundled: the page has them (tokens.css, fonts/).
import {build, transform} from "esbuild";
import {execFileSync} from "node:child_process";
import {createHash} from "node:crypto";
import {existsSync, mkdirSync, readFileSync, readdirSync, realpathSync, rmSync, statSync, writeFileSync} from "node:fs";
import {dirname, join, relative, resolve, sep} from "node:path";
import {fileURLToPath} from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "../..");
const outDir = join(repoRoot, "internal/dozor/web/vendor");
const buildDir = join(here, ".build");
const a00Requested = resolve(process.env.A00_DIR || join(repoRoot, "../monorepo-frontend/packages/00"));

if (!existsSync(join(a00Requested, "package.json"))) {
  throw new Error(`@jourloy/00 not found at ${a00Requested}. Set A00_DIR to the packages/00 directory of monorepo-frontend.`);
}
// esbuild reports importers by their real path. With a symlinked A00_DIR the "is this file the package's?" test in
// the resolver below would never match, and react would silently be resolved from the monorepo: a second copy.
const a00Dir = realpathSync(a00Requested);
const a00Package = JSON.parse(readFileSync(join(a00Dir, "package.json"), "utf8"));
if (a00Package.name !== "@jourloy/00") throw new Error(`${a00Dir} is not @jourloy/00 (name: ${a00Package.name}).`);

// ---------------------------------------------------------------- provenance for the banner
function git(args, cwd) {
  try {
    return execFileSync("git", args, {cwd, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"]}).trim();
  } catch {
    return null;
  }
}
// "-dirty" means that a file which went into the bundle differs from the commit (or is untracked); changes elsewhere in
// packages/00 do not matter to the bundle.
function revision(files) {
  const commit = git(["rev-parse", "--short", "HEAD"], a00Dir);
  if (commit === null) return "unknown revision";
  const changed = git(["status", "--porcelain", "--", ...files], a00Dir);
  return commit + (changed === "" ? "" : "-dirty");
}
const reactVersion = JSON.parse(readFileSync(join(here, "node_modules/react/package.json"), "utf8")).version;

// ---------------------------------------------------------------- labels
// app.js passes every label the Sidebar renders, so that a change of defaultSidebarLabels upstream cannot silently
// change the Russian text of Dozor. Fail the build when the package has a label that src/index.tsx does not pass.
function checkLabels() {
  const upstreamSource = readFileSync(join(a00Dir, "src/components/sidebar-navigation.ts"), "utf8");
  const block = /defaultSidebarLabels[^=]*=\s*\{([^}]*)\}/.exec(upstreamSource);
  const ours = /SIDEBAR_LABEL_KEYS\s*=\s*\[([^\]]*)\]/.exec(readFileSync(join(here, "src/index.tsx"), "utf8"));
  if (!block || !ours) throw new Error("Cannot read defaultSidebarLabels of the package or SIDEBAR_LABEL_KEYS of src/index.tsx.");
  const upstream = [...block[1].matchAll(/^\s*(\w+)\s*:/gm)].map(match => match[1]).sort();
  const passed = [...ours[1].matchAll(/"(\w+)"/g)].map(match => match[1]).sort();
  if (upstream.join() !== passed.join()) {
    throw new Error(
      `Sidebar labels differ. Package: ${upstream.join(", ")}. src/index.tsx (SIDEBAR_LABEL_KEYS): ${passed.join(", ")}. ` +
        "Add the new label to SIDEBAR_LABEL_KEYS and to the labels in app.js."
    );
  }
}

// ---------------------------------------------------------------- @jourloy/00 exports map
function resolveA00(subpath) {
  const exportsMap = a00Package.exports ?? {};
  let target = typeof exportsMap[subpath] === "string" ? exportsMap[subpath] : null;
  if (target === null) {
    for (const [key, value] of Object.entries(exportsMap)) {
      const star = key.indexOf("*");
      if (star < 0 || typeof value !== "string") continue;
      const prefix = key.slice(0, star);
      const suffix = key.slice(star + 1);
      if (subpath.length >= prefix.length + suffix.length && subpath.startsWith(prefix) && subpath.endsWith(suffix)) {
        target = value.replace("*", subpath.slice(prefix.length, subpath.length - suffix.length));
        break;
      }
    }
  }
  return target === null ? null : resolve(a00Dir, target);
}

const stubs = {"next/link": join(here, "src/stubs/next-link.tsx")};

const resolvePlugin = {
  name: "a00-resolve",
  setup(b) {
    b.onResolve({filter: /^@jourloy\/00(\/|$)/}, args => {
      const file = resolveA00("." + args.path.slice("@jourloy/00".length));
      if (file === null || !existsSync(file)) {
        return {errors: [{text: `${args.path} is not in the exports map of @jourloy/00 at ${a00Dir}`}]};
      }
      return {path: file};
    });

    b.onResolve({filter: /^next(\/|$)/}, args => {
      const stub = stubs[args.path];
      if (stub) return {path: stub};
      return {
        errors: [
          {
            text: `${args.path} is imported by ${relative(a00Dir, args.importer)}. Next is not bundled: add a stub for it to src/stubs and to \`stubs\` in build.mjs.`,
          },
        ],
      };
    });

    // The package imports its third-party modules by bare name, which node resolution would find in the
    // monorepo's node_modules. Pin them to this project's.
    b.onResolve({filter: /^[^./]/}, async args => {
      if (args.pluginData?.pinned || !args.importer.startsWith(a00Dir + sep)) return undefined;
      const result = await b.resolve(args.path, {kind: args.kind, resolveDir: here, pluginData: {pinned: true}});
      if (result.errors.length > 0) {
        return {
          errors: [
            {
              text: `${args.path} (imported by ${relative(a00Dir, args.importer)}) is not installed in assets-src/menu. Add it to package.json with the version of packages/00 and run npm install.`,
            },
          ],
        };
      }
      return {path: result.path, sideEffects: result.sideEffects};
    });

    // The Tailwind output of the step below.
    b.onResolve({filter: /^a00-tailwind$/}, () => ({path: join(buildDir, "tailwind.css")}));
  },
};

// ---------------------------------------------------------------- Tailwind: the utilities of the Sidebar components
function generateTailwind() {
  rmSync(buildDir, {recursive: true, force: true});
  mkdirSync(buildDir, {recursive: true});
  const sources = ["SidebarShell", "SidebarDock", "SidebarMobileNav"].map(
    name => `@source ${JSON.stringify(join(a00Dir, "src/components", name, "index.tsx"))};`
  );
  const input = join(buildDir, "tailwind-input.css");
  // No preflight (the page has its own reset) and no automatic source detection: only the three components.
  writeFileSync(
    input,
    [
      '@import "tailwindcss/theme.css" layer(theme);',
      '@import "tailwindcss/utilities.css" layer(utilities) source(none);',
      ...sources,
      "",
    ].join("\n")
  );
  const cli = join(here, "node_modules/@tailwindcss/cli/dist/index.mjs");
  execFileSync(process.execPath, [cli, "--input", input, "--output", join(buildDir, "tailwind.css"), "--minify"], {
    cwd: here,
    stdio: ["ignore", "inherit", "inherit"],
  });
}

// ---------------------------------------------------------------- licenses of the bundled third-party packages
function licenseTexts(inputs) {
  const names = new Set();
  for (const input of Object.keys(inputs)) {
    const match = /(?:^|\/)node_modules\/((?:@[^/]+\/)?[^/]+)\//.exec(input);
    if (match) names.add(match[1]);
  }
  names.add("tailwindcss"); // its utilities are in the CSS
  const parts = [
    "Third-party software in a00-menu.js and a00-menu.css, generated by assets-src/menu/build.mjs.\n" +
      "@jourloy/00 (the Sidebar components) is part of the same project and has no separate notice.\n",
  ];
  for (const name of [...names].sort()) {
    const dir = join(here, "node_modules", name);
    const version = JSON.parse(readFileSync(join(dir, "package.json"), "utf8")).version;
    const file = readdirSync(dir)
      .sort()
      .find(entry => /^(licen[sc]e|copying)/i.test(entry));
    if (!file) throw new Error(`No license file in node_modules/${name}`);
    parts.push(`\n${"=".repeat(72)}\n${name} ${version}\n${"=".repeat(72)}\n\n${readFileSync(join(dir, file), "utf8").trim()}\n`);
  }
  return parts.join("");
}

// ---------------------------------------------------------------- build
// Floor of the browsers: the Tailwind output uses color-mix() and @property, the dark skin :has().
const target = ["es2020", "chrome111", "safari16.4", "firefox113"];
checkLabels();
generateTailwind();

const result = await build({
  entryPoints: [join(here, "src/index.tsx")],
  outfile: join(outDir, "a00-menu.js"),
  absWorkingDir: here,
  write: false, // nothing reaches web/vendor until the checks below have passed
  bundle: true,
  format: "iife",
  platform: "browser",
  target,
  // Identifiers stay readable in this pass: minified CSS Module names would be single letters (.t, .a) in the
  // page's global namespace, one future host class away from a collision. The JS is mangled in a second pass below.
  minifyWhitespace: true,
  minifySyntax: true,
  minifyIdentifiers: false,
  legalComments: "none",
  jsx: "automatic",
  tsconfig: join(here, "tsconfig.json"),
  define: {"process.env.NODE_ENV": '"production"'},
  loader: {".module.css": "local-css", ".css": "css"},
  plugins: [resolvePlugin],
  metafile: true,
  logLevel: "warning",
});
const outputs = Object.fromEntries(result.outputFiles.map(file => [file.path.split(sep).pop(), file.text]));
if (typeof outputs["a00-menu.js"] !== "string" || typeof outputs["a00-menu.css"] !== "string") {
  throw new Error(`esbuild did not produce a00-menu.js and a00-menu.css (got: ${Object.keys(outputs).join(", ")}).`);
}

// ---------------------------------------------------------------- checks on what went in
const inputs = Object.keys(result.metafile.inputs);

// Every npm package exactly once, and react in particular: two copies of React break hooks and double the size.
const roots = new Map();
for (const input of inputs) {
  const match = /^(.*node_modules\/((?:@[^/]+\/)?[^/]+))\//.exec(input);
  if (match) roots.set(match[2], (roots.get(match[2]) ?? new Set()).add(match[1]));
}
for (const [name, dirs] of roots) {
  if (dirs.size > 1) throw new Error(`More than one copy of ${name} was bundled: ${[...dirs].join(", ")}`);
}
if (!roots.has("react") || !roots.has("react-dom")) throw new Error("react and react-dom were not bundled.");
if ([...roots.get("react")][0] !== "node_modules/react") {
  throw new Error(`react was bundled from ${[...roots.get("react")][0]}, not from assets-src/menu/node_modules.`);
}

// The @jourloy/00 files that went in: hashed for the banner (the commit alone does not say what a dirty tree
// contained) and checked against git.
const a00Files = inputs
  .map(input => resolve(here, input))
  .filter(file => file.startsWith(a00Dir + sep) && statSync(file).isFile())
  .sort();
if (a00Files.length === 0) throw new Error(`No file of @jourloy/00 (${a00Dir}) went into the bundle.`);
const hash = createHash("sha256");
for (const file of a00Files) {
  hash.update(relative(a00Dir, file) + "\0");
  hash.update(readFileSync(file));
  hash.update("\0");
}
const source = revision(a00Files);

// ---------------------------------------------------------------- write
const banner =
  `Dozor menu: the Sidebar of @jourloy/00 (SidebarShell, SidebarDock, SidebarMobileNav, SidebarDarkTheme).\n` +
  ` * Source: monorepo-frontend packages/00 at ${source}, ${a00Files.length} files, sha256 ${hash.digest("hex").slice(0, 16)}; react ${reactVersion}.\n` +
  ` * Generated by assets-src/menu/build.mjs (make menu), do not edit. Notices of the bundled packages: a00-menu-LICENSES.txt.`;
// Mangle the JS now that the class names are fixed: they are string literals, so renaming the locals cannot touch
// them. This halves the file (React alone is most of it).
const mangled = await transform(outputs["a00-menu.js"], {loader: "js", minify: true, target});
mkdirSync(outDir, {recursive: true});
writeFileSync(join(outDir, "a00-menu.js"), `/*! ${banner} */\n${mangled.code}`);
writeFileSync(join(outDir, "a00-menu.css"), `/*! ${banner} */\n${outputs["a00-menu.css"]}`);
writeFileSync(join(outDir, "a00-menu-LICENSES.txt"), licenseTexts(result.metafile.inputs));
rmSync(buildDir, {recursive: true, force: true});

for (const name of ["a00-menu.js", "a00-menu.css"]) {
  console.log(`${name}: ${statSync(join(outDir, name)).size} bytes`);
}
console.log(`source: packages/00 at ${source}, ${a00Files.length} files (${a00Dir})`);
