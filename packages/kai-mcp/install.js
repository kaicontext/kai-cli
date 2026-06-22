#!/usr/bin/env node
"use strict";

const { execSync } = require("child_process");
const fs = require("fs");
const path = require("path");
const https = require("https");
const zlib = require("zlib");

const REPO = "kaicontext/kai-cli";
const VERSION = require("./package.json").version;

function getPlatform() {
  const platform = process.platform;
  if (platform === "darwin") return "darwin";
  if (platform === "linux") return "linux";
  throw new Error(`Unsupported platform: ${platform}`);
}

function getArch() {
  const arch = process.arch;
  if (arch === "x64") return "amd64";
  if (arch === "arm64") return "arm64";
  throw new Error(`Unsupported architecture: ${arch}`);
}

function download(url) {
  return new Promise((resolve, reject) => {
    https.get(url, (res) => {
      if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
        return download(res.headers.location).then(resolve, reject);
      }
      if (res.statusCode !== 200) {
        return reject(new Error(`Download failed: HTTP ${res.statusCode} from ${url}`));
      }
      const chunks = [];
      res.on("data", (chunk) => chunks.push(chunk));
      res.on("end", () => resolve(Buffer.concat(chunks)));
      res.on("error", reject);
    }).on("error", reject);
  });
}

async function main() {
  const os = getPlatform();
  const arch = getArch();
  const asset = `kai-${os}-${arch}.gz`;
  const url = `https://github.com/${REPO}/releases/download/v${VERSION}/${asset}`;

  console.log(`Downloading kai v${VERSION} (${os}/${arch})...`);

  const gzData = await download(url);
  const binary = zlib.gunzipSync(gzData);

  const binDir = path.join(__dirname, "bin");
  fs.mkdirSync(binDir, { recursive: true });

  const binPath = path.join(binDir, "kai");
  fs.writeFileSync(binPath, binary);
  fs.chmodSync(binPath, 0o755);

  console.log(`Installed kai to ${binPath}`);
}

main().catch((err) => {
  console.error(`Failed to install kai binary: ${err.message}`);
  console.error("You can install manually: curl -sSL https://get.kaicontext.com | sh");
  process.exit(1);
});
