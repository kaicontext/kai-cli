#!/bin/sh
# Hypothetical regressed change: the fallback drops the command when no workspace is known.
gen() { node -e '
  const [kind, wsPath, command] = process.argv.slice(1);
  const out = { regressed: wsPath ? "cd " + JSON.stringify(wsPath) + " && " + command : "",
                fix:       wsPath ? "cd " + JSON.stringify(wsPath) + " && " + command : command,
                old:       command }[kind];
  process.stdout.write(out);' "$1" "$2" "$3"; }
run() { printf '%s\n' "$2" > /tmp/__run.sh; printf '\n__KAI_EXIT__=$?\necho "__EXIT__=$__KAI_EXIT__"\necho "__PWD__=$(pwd)"\n' >> /tmp/__run.sh
  cd /tmp && out=$(sh /tmp/__run.sh 2>/tmp/__err); printf '%s|generated=%s|%s|stderr=%s\n' "$1" "$2" "$(printf '%s' "$out" | tr '\n' '|')" "$(tr '\n' '|' < /tmp/__err)"; }
run regressed "$(gen regressed '' 'echo COMMAND_RAN')"
run fix "$(gen fix '' 'echo COMMAND_RAN')"
run old "$(gen old '' 'echo COMMAND_RAN')"
mkdir -p /tmp/ws; run fix-with-ws "$(gen fix /tmp/ws 'echo COMMAND_RAN')"
