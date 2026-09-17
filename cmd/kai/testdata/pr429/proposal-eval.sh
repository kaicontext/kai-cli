#!/bin/sh
# Mechanical checks of the proposal, executed on the captured case's inputs.
# Constructions come from the diff (old: `Panels.select({command})` = bare
# command; new: 'cd ' + JSON.stringify(wsPath) + ' && ' + command) and from
# the two remedies as written (`cd ... ; <command>`; single-quote escaper).
mkdir -p '/tmp/ws$dir' '/tmp/ws`back`'
gen() { # $1 = kind (new|old|semi|squote) $2 = wsPath $3 = command
  node -e '
    const [kind, wsPath, command] = process.argv.slice(1);
    const sq = "\x27" + wsPath.replace(/\x27/g, "\x27\\\x27\x27") + "\x27";
    const out = { new: "cd " + JSON.stringify(wsPath) + " && " + command,
                  old: command,
                  semi: "cd " + JSON.stringify(wsPath) + " ; " + command,
                  squote: "cd " + sq + " && " + command }[kind];
    process.stdout.write(out);' "$1" "$2" "$3"
}
run() { # $1 label, $2 generated command
  printf '%s\n' "$2" > /tmp/__run.sh
  printf '\n__KAI_EXIT__=$?\necho "__EXIT__=$__KAI_EXIT__"\necho "__PWD__=$(pwd)"\n' >> /tmp/__run.sh
  cd /tmp && out=$(sh /tmp/__run.sh 2>/tmp/__err)
  printf '  %-7s cmd=%-46s -> %s stderr=%s\n' "$1" "$2" "$(printf '%s' "$out" | tr '\n' '|')" "$(tr '\n' '|' < /tmp/__err)"
}
echo "## missing-directory allegation, input /bad/missing/path, command 'echo COMMAND_RAN'"
for k in new old semi; do run $k "$(gen $k /bad/missing/path 'echo COMMAND_RAN')"; done
echo "## quoting allegation, input '/tmp/ws\$dir', command pwd"
for k in new old squote; do run $k "$(gen $k '/tmp/ws$dir' pwd)"; done
echo "## quoting allegation, input '/tmp/ws\`back\`', command pwd"
for k in new old squote; do run $k "$(gen $k '/tmp/ws`back`' pwd)"; done
echo "## missing-directory: does the single-quote remedy change the stop-on-failure behavior? (it should not)"
run squote "$(gen squote /bad/missing/path 'echo COMMAND_RAN')"
