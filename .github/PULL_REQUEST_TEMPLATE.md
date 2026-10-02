<!--
Explain the why. The diff already says what.
Changes under apis/ need `make generate` committed with them. Identifiers that
reach SQL go through ValidateIdent then QuoteIdent (CONTRIBUTING.md).
-->

## What this changes



## Why



## How it was verified

<!--
Tests are the usual answer. A bug in plan construction needs a unit test; a
bug about what PostgreSQL accepts needs an integration test (-tags
integration); a bug in the reconcile contract or CRD schema needs an envtest.
-->



---

- [ ] I have signed the CLA (see CLA.md)

Closes #
