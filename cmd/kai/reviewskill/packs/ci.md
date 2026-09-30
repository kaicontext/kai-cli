## CI workflows and shell scripts

- **Script injection:** a `${{ … }}` expression inside a `run:` script is pasted into the script text before the shell parses it. An input, branch name, title or other value someone can set that is spliced in this way runs as shell. Pass it through `env:` and quote the variable instead. The same applies to any value interpolated into `sh -c`, `eval` or a generated script.
- **Untrusted code with secrets:** `pull_request_target` or `workflow_run` that checks out and runs the pull request's code while secrets or a write token are available.
- **Permissions:** a `permissions:` block, or a token, broader than what the job uses.
- **Concurrency:** jobs in different concurrency groups, or without one, that read, change and write the same shared thing: an issue or comment body, a branch, a release, a cache key, a file in the repository. Two runs overlap and the later write drops the earlier one.
- **Failure handling:**
  - `set -e` does not stop on a failure inside a pipeline without `pipefail`, in a command substitution assigned with `local`, or in a condition.
  - A step that should fail the job but ends in `|| true`, `continue-on-error`, or a command whose exit status is lost.
  - A re-run of the same job that fails because its first run already created the branch, tag or release.
- **Quoting:** an unquoted variable split on spaces or expanded as a glob; a heredoc whose delimiter or indentation changes what it contains.
- **Secrets:** a secret echoed to the log, written to an artifact, or passed on a command line other processes can read.
- **Triggers and conditions:** an `if:` that is always true or always false (a string compared with a boolean, a missing `${{ }}`), a path filter that skips the files the job exists for, a schedule that never runs on the default branch.
