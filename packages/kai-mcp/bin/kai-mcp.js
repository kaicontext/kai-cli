#!/usr/bin/env node
"use strict";

const { execFileSync } = require("child_process");
const path = require("path");

const kai = path.join(__dirname, "kai");

try {
  execFileSync(kai, ["mcp", "serve"], { stdio: "inherit" });
} catch (err) {
  if (err.status != null) process.exit(err.status);
  console.error("Failed to start kai MCP server:", err.message);
  process.exit(1);
}
