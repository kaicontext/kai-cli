## Ruby and Rails

- **Nil:** `nil` reaching a method call (`undefined method for nil`), and `&.` hiding a value that must be present.
- **Hash keys:** symbol keys and string keys mixed in a hash that is not indifferent-access.
- **Queries:** an N+1 query in a loop or view (missing `includes`), and `update_column` or `update_all` skipping validations and callbacks the code relies on.
- **Output and parameters:**
  - `html_safe` or `raw` on user content (XSS).
  - Mass assignment without strong parameters.
- **Callbacks:** callbacks with side effects that now run on a new path, or no longer run.
- **Exceptions:** `rescue => e` swallowing an error the caller needs, and `find` raising where the row may be absent.
- **Frozen strings:** a string literal mutated under `frozen_string_literal: true`.
