# RFCs

Design documents for changes too large for a pull request description. Each
one is numbered, added by a pull request, discussed in a GitHub Discussions
thread linked from the document, and carries one of three statuses:

- **Draft**: under discussion; nothing in the code depends on it.
- **Accepted**: the plan the maintainers intend to follow; milestones may
  reference it.
- **Implemented** or **Superseded**: kept for history, with a pointer to what
  replaced it.

`docs/` describes what the code does today; `rfcs/` describes what it should
do next. When the two disagree, `docs/` wins until the RFC is implemented.

A gate an RFC defines produces a record when it passes, kept beside the RFC
as `NNNN-<gate>-decision.md` with the measurements and the decisions taken;
the RFC's next version carries the consequences and links the record.

| Number | Title | Status |
| --- | --- | --- |
| [0001](0001-redundanet-3.md) | RedundaNet 3: one binary on Windows, macOS and Linux, with a native storage engine | Draft, version 4; [G0 passed](0001-g0-decision.md) |
