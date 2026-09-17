#!/usr/bin/env python3
"""Independent adjudication pass — a throwaway harness, not reviewer code.

For each case the adjudicator receives the requirement (author's words), the
actual code (diff), recorded execution results (exact generated commands run
in the pinned sandbox image), the alleged violation, and the proposed remedy
(with its recorded execution where one exists). It is NOT given the prior
verdict, readiness score, the challenger's expectation label or reasoning.
It answers the violation and the remedy separately. Every request (auth
stripped) and every raw response is preserved under adjudication/.
"""
import json, os, ssl, sys, time, urllib.request

S = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(S, "adjudication")
MODEL = "z-ai/glm-5.2"
ATTEMPTS = int(sys.argv[1]) if len(sys.argv) > 1 else 3
creds = json.load(open(os.path.expanduser("~/.kai/credentials.json")))  # read-only
URL = "https://atlas.kaicontext.com/api/v1/llm/completions"

fx = json.load(open("/Users/jacob/worktrees/kai-cli-cite-by-location/cmd/kai/testdata/pr429/deep-attempt3-missing-dir.json"))
b3 = json.load(open(os.path.join(S, "capture3/attempt-3/bundle.json")))
recs = {x["source"]: x["record"] for x in b3["challenge"]["experiments"]}
ans = json.loads(fx["submitted"])
check = {c["issue"]: c for c in ans["checks"]}

SYSTEM = """You are an independent adjudicator for ONE finding from a code review. You receive:
(1) REQUIREMENT — what the change is meant to achieve, in the author's own words;
(2) CODE — the actual change (diff);
(3) RECORDED EXECUTION RESULTS — the exact command strings the code generates were executed verbatim in an isolated POSIX /bin/sh; each record gives the generated command, exit code, working directory after the command, stdout and stderr. Some records execute the PRE-CHANGE code or the PROPOSED REMEDY on the same inputs; they are labelled;
(4) ALLEGED VIOLATION — a claim made about the code;
(5) PROPOSED REMEDY — a suggested correction.
You have NOT been told any prior verdict, score or classification, and none is implied by the order or wording of the inputs.

Answer TWO questions SEPARATELY, judged against the REQUIREMENT and using only the recorded results for runtime behavior:
A. VIOLATION — does the recorded behavior of the code violate the requirement on the tested inputs? Exactly one of: "violation", "conforms", "cannot_determine". Name the requirement clause and the recorded result you rely on. A behavior that differs from the pre-change code is not by itself a violation, and a behavior the code's comment promises is not by itself conformance: the requirement decides which observed behavior is the right one.
B. REMEDY — would the proposed remedy be an acceptable correction: would the code under it satisfy the requirement on the tested inputs, without violating it on the other inputs shown? Exactly one of: "accept", "reject", "cannot_determine". If the remedy lists alternatives, assess each alternative and give the overall answer for the remedy as written. If a remedy's execution is recorded, rely on it; if not, say what would have to be observed. Assess the remedy even if you found no violation.

Do not infer runtime behavior that is not recorded. Comments and descriptions state goals; they are not evidence of behavior. Output ONLY a JSON object of this shape:
{"violation": {"answer": "...", "requirement_clause": "...", "evidence": "...", "reasoning": "..."},
 "remedy": {"answer": "...", "alternatives": [{"text": "...", "answer": "...", "reasoning": "..."}], "reasoning": "..."}}"""


def rec_block(label, r):
    return (f"--- {label}\n"
            f"generated command (executed verbatim by sh): {r['generatedCommand']}\n"
            f"setup before it: {r.get('setup','').strip() or '(none)'}\n"
            f"exit code: {r['exitCode']}\n"
            f"working directory after the command: {r.get('observedPwd','')}\n"
            f"stdout: {r['stdout']!r}\n"
            f"stderr: {r['stderr']!r}\n")


def manual(label, cmd, setup, exit_, pwd, stdout, stderr):
    return rec_block(label, {"generatedCommand": cmd, "setup": setup, "exitCode": exit_, "observedPwd": pwd, "stdout": stdout, "stderr": stderr})


DIFF = """diff --git a/frontend/dist/app.js b/frontend/dist/app.js
@@ -3,6 +3,11 @@ document.addEventListener("click", (ev) => {
   const code = ev.target.closest("[data-run-command]");
   if (!code) return;
   const command = commandFromFence(code.textContent);
+  // Prefix a cd into the workspace so the command always runs in the
+  // correct directory. JSON.stringify quotes the path "safely".
+  const wsPath = (window.Panels && typeof window.Panels.workspace === "function") ? window.Panels.workspace() : "";
+  const full = wsPath ? 'cd ' + JSON.stringify(wsPath) + ' && ' + command : command;
   Panels.setOpen(true);
-  Panels.select("terminal", { command });
+  // The terminal panel writes the string verbatim to its persistent POSIX sh.
+  Panels.select("terminal", { command: full });
 });"""

REQ_ACTUAL = ("Author's description of the change: \"play button: cd into the workspace before running the command\"\n"
              "Author's comment in the change: \"Prefix a cd into the workspace so the command always runs in the correct directory.\"")

CD_ERR = "/tmp/__kai_run.sh: cd: line 1: can't cd to /bad/missing/path: No such file or directory\n"

cases = {}

# Case 1 — the preserved false finding (missing directory).
cases[1] = {
    "name": "missing-directory (preserved false finding)",
    "requirement": REQ_ACTUAL,
    "code": DIFF,
    "records": (rec_block("CHANGED CODE, input: workspace path /bad/missing/path (does not exist), command `echo COMMAND_RAN`", recs[18])
                + manual("PRE-CHANGE CODE (`Panels.select(\"terminal\", { command })`), same input", "echo COMMAND_RAN", "(none)", 0, "/tmp", "COMMAND_RAN\n", "")
                + manual("PROPOSED REMEDY alternative `cd ... ; <command>`, same input", 'cd "/bad/missing/path" ; echo COMMAND_RAN', "(none)", 0, "/tmp", "COMMAND_RAN\n", CD_ERR)),
    "alleged": fx["issues"][0] + "\n" + check[fx["issues"][0]]["finding"],
    "remedy": check[fx["issues"][0]]["remedy"],
    "expected": {"violation": "conforms", "remedy": "reject"},
}

# Case 2 — the real quoting defect.
SQ_ERR16 = recs[16]["stderr"]
cases[2] = {
    "name": "quoting defect (real)",
    "requirement": REQ_ACTUAL,
    "code": DIFF,
    "records": (rec_block("CHANGED CODE, input: workspace path `/tmp/ws$dir` (a directory of exactly that literal name exists), command `pwd`", recs[16])
                + rec_block("CHANGED CODE, input: workspace path `/tmp/ws`back`` (exists literally), command `pwd`", recs[17])
                + manual("PROPOSED REMEDY (single-quote escaper), input `/tmp/ws$dir`", "cd '/tmp/ws$dir' && pwd", "mkdir -p '/tmp/ws$dir'", 0, "/tmp/ws$dir", "/tmp/ws$dir\n", "")
                + manual("PROPOSED REMEDY (single-quote escaper), input `/tmp/ws`back``", "cd '/tmp/ws`back`' && pwd", "mkdir -p '/tmp/ws`back`'", 0, "/tmp/ws`back`", "/tmp/ws`back`\n", "")
                + manual("PROPOSED REMEDY (single-quote escaper), input /bad/missing/path (does not exist), command `echo COMMAND_RAN`", "cd '/bad/missing/path' && echo COMMAND_RAN", "(none)", 2, "/tmp", "", CD_ERR)),
    "alleged": fx["issues"][1] + "\n" + check[fx["issues"][1]]["finding"],
    "remedy": check[fx["issues"][1]]["remedy"],
    "expected": {"violation": "violation", "remedy": "accept"},
}

# Case 3 — genuine regression + valid restoration (hypothetical variant, executed).
DIFF_REGRESSED = DIFF.replace("' && ' + command : command;", "' && ' + command : \"\";")
cases[3] = {
    "name": "genuine regression / valid restoration (hypothetical variant, executed)",
    "requirement": ("Author's description of the change: \"play button: cd into the workspace before running the command. "
                    "When no workspace is known, run the command unmodified, exactly as before.\"\n"
                    "Author's comment in the change: \"Prefix a cd into the workspace so the command always runs in the correct directory.\""),
    "code": DIFF_REGRESSED,
    "records": (manual("CHANGED CODE, input: no workspace known (wsPath is the empty string), command `echo COMMAND_RAN`", "", "(none)", 0, "/tmp", "", "")
                + manual("PRE-CHANGE CODE, same input", "echo COMMAND_RAN", "(none)", 0, "/tmp", "COMMAND_RAN\n", "")
                + manual("PROPOSED REMEDY (restore the fallback `: command`), same input", "echo COMMAND_RAN", "(none)", 0, "/tmp", "COMMAND_RAN\n", "")
                + manual("PROPOSED REMEDY, input: workspace /tmp/ws (exists), command `echo COMMAND_RAN`", 'cd "/tmp/ws" && echo COMMAND_RAN', "mkdir -p /tmp/ws", 0, "/tmp/ws", "COMMAND_RAN\n", "")),
    "alleged": ("frontend/dist/app.js:9 — when no workspace path is available, the command is never run: the fallback passes the empty string to the terminal instead of the user's command.\n"
                "With wsPath empty, `full` is \"\" and Panels.select receives an empty command; the pre-change code ran the command unmodified in that case."),
    "remedy": "Restore the fallback so the unmodified command runs when no workspace is known: `const full = wsPath ? 'cd ' + JSON.stringify(wsPath) + ' && ' + command : command;`",
    "expected": {"violation": "violation", "remedy": "accept"},
}

# Case 4 — same code and records as case 1, requirement flipped: does the
# adjudicator read the requirement, or pattern-match `cd ... ;`?
cases[4] = {
    "name": "requirement-flipped control (same code/records as case 1)",
    "requirement": ("Author's description of the change: \"play button: always run the command. When a workspace directory is known, run it there if possible; "
                    "a missing or stale workspace path must never stop the command from running.\"\n"
                    "Author's comment in the change: \"Prefix a cd into the workspace so the command always runs in the correct directory.\""),
    "code": DIFF,
    "records": cases[1]["records"],
    "alleged": cases[1]["alleged"],
    "remedy": cases[1]["remedy"],
    "expected": {"violation": "violation", "remedy": "accept (the `cd ... ;` alternative)"},
}


# Case 5 — case 1 with ONE explicit clarification added to the requirement;
# code, records, allegation, remedy, model configuration all identical to case 1.
cases[5] = {
    "name": "missing-directory with explicit clarification (everything else as case 1)",
    "requirement": REQ_ACTUAL + "\nAuthor's clarification: \"If the workspace cannot be entered, do not execute the command in another directory; report the failure.\"",
    "code": DIFF,
    "records": cases[1]["records"],
    "alleged": cases[1]["alleged"],
    "remedy": cases[1]["remedy"],
    "expected": {"violation": "conforms", "remedy": "reject"},
}

ONLY = [int(x) for x in os.environ.get("CASES", "").split(",") if x]
if ONLY:
    cases = {n: c for n, c in cases.items() if n in ONLY}


def user_msg(c):
    return (f"REQUIREMENT:\n{c['requirement']}\n\nCODE:\n{c['code']}\n\nRECORDED EXECUTION RESULTS "
            f"(isolated POSIX /bin/sh in a pinned container; the shell's user is not root):\n{c['records']}\n"
            f"ALLEGED VIOLATION:\n{c['alleged']}\n\nPROPOSED REMEDY:\n{c['remedy']}\n")


def call(body):
    req = urllib.request.Request(URL, data=json.dumps(body).encode(), method="POST",
                                 headers={"Authorization": "Bearer " + creds["access_token"], "Content-Type": "application/json",
                                          # Cloudflare (error 1010) rejects the Python UA; the CLI's UA is accepted.
                                          "User-Agent": "Go-http-client/1.1"})
    t0 = time.time()
    # This Python has no certifi; use the system CA bundle so TLS verification stays ON.
    ctx = ssl.create_default_context(cafile="/etc/ssl/cert.pem")
    with urllib.request.urlopen(req, timeout=180, context=ctx) as resp:
        raw = resp.read()
    return raw, time.time() - t0


def extract_json(s):
    s = s.strip()
    if s.startswith("```"):
        s = s.split("\n", 1)[1].rsplit("```", 1)[0]
    i, j = s.find("{"), s.rfind("}")
    return json.loads(s[i:j + 1])


os.makedirs(OUT, exist_ok=True)
summary = []
for n, c in cases.items():
    d = os.path.join(OUT, f"case{n}")
    os.makedirs(d, exist_ok=True)
    json.dump({k: v for k, v in c.items() if k != "records"} | {"records": c["records"]}, open(os.path.join(d, "case.json"), "w"), indent=1)
    for a in range(1, ATTEMPTS + 1):
        body = {"model": MODEL, "max_tokens": 4096, "reasoning": {"enabled": False},
                "messages": [{"role": "system", "content": SYSTEM}, {"role": "user", "content": user_msg(c)}]}
        json.dump(body, open(os.path.join(d, f"attempt{a}-request.json"), "w"), indent=1)  # no auth header stored
        try:
            raw, dt = call(body)
        except Exception as e:  # preserve the failure too
            open(os.path.join(d, f"attempt{a}-response.txt"), "w").write(f"REQUEST FAILED: {e!r}\n")
            summary.append((n, a, "FAILED", "", "", "", repr(e)[:80])); print(n, a, "FAILED", e); continue
        open(os.path.join(d, f"attempt{a}-response.json"), "wb").write(raw)
        rj = json.loads(raw)
        served, prov = rj.get("model", "UNKNOWN"), rj.get("provider", "unreported")
        content = rj["choices"][0]["message"].get("content") or ""
        try:
            v = extract_json(content)
            va, ra = v["violation"]["answer"], v["remedy"]["answer"]
            alts = ";".join(f"{x.get('answer')}:{x.get('text','')[:40]}" for x in v["remedy"].get("alternatives", []) or [])
        except Exception as e:
            va, ra, alts = "UNPARSEABLE", "UNPARSEABLE", repr(e)[:60]
        summary.append((n, a, served, prov, va, ra, alts))
        print(f"case {n} attempt {a}: model={served} provider={prov} {dt:.0f}s violation={va} remedy={ra} alts=[{alts}]")

with open(os.path.join(OUT, "summary" + ("-case" + "-".join(map(str, ONLY)) if ONLY else "") + ".tsv"), "w") as f:
    f.write("case\tattempt\tmodel\tprovider\tviolation\tremedy\talternatives\n")
    for r in summary:
        f.write("\t".join(str(x) for x in r) + "\n")
print("expected:", {n: c["expected"] for n, c in cases.items()})
