## Python

- **Mutable default arguments:** a default list or dict shared across calls.
- **Late binding:** closures in a loop that bind the loop variable late.
- **Truthiness:** `if value:` treating a valid `0`, `""` or `[]` like `None`.
- **Broad exceptions:** a broad `except` (or `except Exception`) that hides a real failure or catches control-flow exceptions.
- **Blocking in async:** blocking I/O (`requests`, `time.sleep`, sync database calls) inside `async def`.
- **Datetimes:** naive and timezone-aware datetimes compared or mixed.
- **ORM:**
  - A queryset evaluated in a loop (N+1).
  - `.update()` or bulk writes that skip validation and signals the code relies on.
  - `get()` raising when a row may be absent.
- **Injection:** SQL or shell commands built with f-strings or `.format()`, and `subprocess` with `shell=True` around untrusted values.
- **Dictionaries:** `dict.get` with a default that hides a missing required key, and key-type mismatches (a string key read with an int).
- **Generators:** a generator or iterator consumed twice.
