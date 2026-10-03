## Java and Kotlin

- **Equality:**
  - `==` on strings or boxed numbers where the value is meant.
  - `equals` overridden without `hashCode`, or changed so that collection lookups break.
- **Null:** a getter result or map lookup dereferenced when it can be null, and `Optional.get()` called without a presence check.
- **Exceptions:**
  - A checked exception caught and swallowed.
  - A resource not closed on every path (use try-with-resources).
- **Transactions:** `@Transactional` bypassed by a self-invocation, work that must be atomic split across two transactions, or lazy-loaded entities read after the session closed.
- **Concurrency:** shared mutable state in a singleton bean or a static field, and a non-thread-safe collection shared across threads.
- **Permissions and ownership:** a new service or endpoint method that skips the permission or ownership check its sibling methods perform, and a check that tests a broader scope than the operation needs.
- **Streams:** stream pipelines with side effects, and `Collectors.toMap` failing on duplicate keys.
