## Go

- **Goroutines:** a goroutine with no path that ends it (no context, no closed channel), or one that writes to a variable the caller reads without synchronization.
- **Loop variables:** captured by a closure or a goroutine in code built for Go before 1.22, where every iteration shares one variable.
- **Maps and slices:**
  - Writing to a nil map panics.
  - A slice kept after an `append` that may have reallocated it, or two slices sharing one backing array.
- **Errors:**
  - An `err` shadowed by `:=` in an inner scope, so the outer check sees nil.
  - An error ignored with `_`.
  - `errors.Is` or `As` used against a value that was wrapped without `%w`.
- **`defer`:**
  - A `defer` inside a loop runs only when the function returns.
  - A `defer` placed after an early return never runs.
- **Nil interfaces:** an interface holding a typed nil pointer is not `== nil`.
- **Contexts:** a context that is not passed down, so cancellation and deadlines stop at this call.
- **External commands:** `exec` with no deadline, or with a deadline and no `WaitDelay`.
- **Tickers and timers:** a `time.Ticker` that is never stopped.
- **JSON:** struct tags that no longer match the fields a client sends, and `omitempty` dropping a meaningful zero value.
