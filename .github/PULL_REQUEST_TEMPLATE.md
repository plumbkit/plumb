<!--
Thanks for contributing to Plumb! Please fill out the sections below.
See CONTRIBUTING.md for the full guide.
-->

## What does this change?

<!-- A short description of the change and the motivation (the "why", not just the "what"). -->

## Related issue

<!-- e.g. Closes #123. For non-trivial changes, link the issue where the approach was agreed. -->

## Checklist

- [ ] `make verify` is green (build + test + lint)
- [ ] `CHANGELOG.md` entry added
- [ ] Prose is in Australian English (-ise/-isation, behaviour, …); spec identifiers left canonical
- [ ] New/changed behaviour is covered by tests (integration tests gated with `//go:build integration`)
- [ ] No file exceeds ~600 lines; no function exceeds gocyclo 15
- [ ] Tool `Execute()` stays a thin orchestrator (parse/validate → logic → presentation)
- [ ] Conventional-commit message(s)

## Notes for reviewers

<!-- Anything tricky, trade-offs made, or areas you'd like extra scrutiny on. -->
