# Go-side fixtures for `VectorDoc` v1

These files are **byte-identical copies** of
`media-manipulator-ui/lib/vector/fixtures/` (MM42, UI commit f6b2e91). They are
the cross-side contract with the UI's Zod model and are **never regenerated
here**. Any change lands in both repos in one change, or in neither.

- `*.json` (11 canonical documents): must sanitize and re-serialize to the same bytes.
- `inputs/*.json`: non-canonical documents that must sanitize (defaults filled); `inputs/defaults.json` must produce exactly `defaults.json`.
- `invalid/*.json`: documents that must be rejected.

Canonical rules: `JSON.stringify(doc, null, 2)` plus `\n`; known keys in Zod
shape order, unknown (additive) keys after them in input order; optional
fields absent when unset, never `null`; `canvas.viewBox` always present;
`defs` always has all five arrays; numbers as JavaScript prints them.
The `.svg` files and `fixtures.ts` stay UI-only.
