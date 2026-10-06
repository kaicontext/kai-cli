## Authentication and authorization (this change touches access control)

- **Every new route, endpoint, job and message handler** needs the authentication and ownership checks its siblings have. Compare the new path with an existing one line by line.
- **Scope and default:** a check that tests a broader scope than the operation needs, or that defaults to allow when a lookup fails.
- **Object references:** a resource fetched by an id from the request without checking that it belongs to the caller (an insecure direct object reference).
- **Roles:** a role or permission the caller can change for themselves.
- **Tokens and sessions:** not checked for expiry, audience or revocation; a secret compared with `==`; a token written to logs or returned in a response.
- **Permission sets:** a permission list or role map that gains a new entry some consumer does not handle, and a cache of permissions that is not invalidated when they change.
