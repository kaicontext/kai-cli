## Calls to other services (this change talks to the network)

- **Timeouts:** every outbound call needs one. A call without a timeout can hang the request or the worker holding it.
- **Retries:** retries on a call that is not idempotent, retries without backoff, and a retried error that can never succeed.
- **Failures:** a failed response treated as success.
- **Credentials:** credentials or tokens that expire, with no path that renews them before use.
- **Webhooks:** received without a signature check or replay protection, or processed twice.
- **URLs:** a URL taken from user input and fetched (server-side request forgery).
- **Undocumented behaviour:** a claim about what the external service does or requires that is not established by its documentation or source. Verify it before you rely on it.
