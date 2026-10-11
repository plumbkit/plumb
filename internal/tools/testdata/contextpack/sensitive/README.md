# contextpack fixture: sensitive

Synthetic workspace for PLAN-462 `context_for_task` tests. `vault/secrets.go`
matches the default `[history] sensitive_globs` (`secrets.*`) and holds a marker
that must never reach a response when the walk reaches it from `app`. Nothing here
is built or run.
