#!/usr/bin/env node
"use strict";

// Thin launcher: resolve the platform package installed by npm, then run the
// static Go binary with inherited stdio, forwarded signals, and ds-tools'
// 0-4 exit codes preserved.

const os = require("node:os");
const path = require("node:path");
const { spawn } = require("node:child_process");

const PACKAGES = {
  "darwin arm64": "ds-tools-darwin-arm64",
  "darwin x64": "ds-tools-darwin-x64",
  "linux arm64": "ds-tools-linux-arm64",
  "linux x64": "ds-tools-linux-x64",
};

const platform = `${process.platform} ${process.arch}`;
const pkg = PACKAGES[platform];

function fail(message) {
  process.stderr.write(`ds-tools: ${message}\n`);
  process.exit(1);
}

if (!pkg) {
  fail(
    `不支持当前平台 ${platform}。` +
      "请从 https://github.com/Ming3z/ds-tools/releases 下载对应二进制。"
  );
}

let binary;
try {
  const pkgDir = path.dirname(require.resolve(`${pkg}/package.json`));
  binary = path.join(pkgDir, "bin", "ds-tools");
} catch {
  fail(`缺少平台包 ${pkg}，请重新安装：npm install -g ds-tools`);
}

const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });

for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(signal, () => {
    try {
      child.kill(signal);
    } catch {
      // Child already exited.
    }
  });
}

child.on("error", (error) => {
  fail(`无法启动 ${binary}：${error.message}`);
});

child.on("exit", (code, signal) => {
  if (signal) {
    process.exit(128 + (os.constants.signals[signal] ?? 0));
  }
  process.exit(code === null ? 1 : code);
});
