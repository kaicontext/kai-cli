## TypeScript and JavaScript

- **Promises:**
  - A promise not awaited (a floating promise), so its errors are lost and its result is read too early.
  - `async` work inside `forEach`.
- **Equality and types:** `==` where `===` is meant, and a value typed `any` or cast with `as` that hides a shape mismatch.
- **Optional chaining:** `?.` hiding a required value that should fail loudly.
- **React:**
  - A `useEffect` or `useCallback` with missing dependencies (a stale closure).
  - State mutated in place.
  - Index keys on reorderable lists.
- **Server and client:** server-only code or secrets reaching the client bundle, and a client-supplied value trusted by a server action.
- **Validation:** input from a request, a file or another service used without validating its shape.
- **Queries:** a Prisma or SQL query missing a `where` condition its siblings apply, or `findUnique` on a field that is not unique.
- **Numbers and time:** dates compared across time zones, and money handled in floating point.
- **Untrusted input:** `JSON.parse` on untrusted input without a guard.
