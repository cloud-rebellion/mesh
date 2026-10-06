# Guards (turning gotchas into enforcement)

A gotcha you wrote down is only worth something if it stops the mistake from coming
back. Mesh knows your gotchas, so it can propose the pre-commit checks that enforce
them. This closes the loop from "we learned this" to "the repo refuses to let it
happen again".

## How it works

```
mesh guards list       # authored troubleshooting notes and their evidence
mesh guards suggest    # candidate checks; review before enabling
```

`suggest` reads each high-confidence troubleshooting note's authored sections and
evidence. A guard needs an explicit current rule, a concrete detection example and
enough context to bound where the rule applies. A symptom, incident cause, title or
quoted example does not establish a prohibition. Incomplete or truncated authored
content is not used to propose enforcement; resolve the source's gaps first.

For a supported rule, your own LLM proposes a grep-style regex, narrow file globs, a
failure message and a severity. Evidence limits and legitimate counterexamples matter:
judgment, architecture, ordering and runtime behavior usually cannot be checked with a
simple regex and are marked as not applicable. Historical notes retain their original
field meanings when read; new notes use the purpose-specific template.

The applicable ones are emitted as a paste-ready bash block you can drop into your
pre-commit hook. For example, the gotcha "use bun, not npm" becomes a check that flags
`npm install` or a stray `package-lock.json`.

## You are the gate

Mesh proposes; you review the generated checks before enabling the ones that fit.
This review concerns executable enforcement, not approval of ordinary notes. The
generated script is a starting point: patterns that need
lookahead (which `grep -E` cannot run) are skipped with a note rather than emitted
broken. A guard that fires on legitimate code is worse than no guard, so review first.

## Why it matters

The same knowledge that an agent reads to avoid a mistake can now refuse the mistake at
commit time. Developer-laptop guards (the pre-commit hook) plus a server-side deploy
gate is the belt-and-suspenders pattern: the hook catches it early, the gate enforces
it. Mesh just makes writing the hook a review step instead of a from-scratch task.
