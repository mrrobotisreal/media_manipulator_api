# vector testdata

These fixtures are the **shared contract** between this repo
(`internal/vector`, the Go sanitizer) and the UI's Zod model
(`media-manipulator-ui/lib/vector/fixtures/`). Both sides must accept every
file here and re-emit it byte-identically.

- The fixture list is the contract: adding a document here means adding it to
  the UI fixtures directory, in the same change.
- Fixtures are copied **byte-identical** between the repos. Never regenerate
  one on only one side; edit by hand (or regenerate in one place and copy).
- Canonical byte format: `JSON.stringify(doc, null, 2) + "\n"` (2-space indent,
  `"key": value`, `[]` / `{}` for empties, single trailing newline), keys in
  document order (version, canvas, duration, fps, layers, defs, animations,
  then additive keys). Numbers use ES6 `Number#toString`; strings are escaped
  as JS does (no HTML escaping). The compact form (stored in Postgres) is
  `JSON.stringify(doc)`.
- Known limitation: JS orders integer-like object keys ("1", "42") first; docs
  never use such keys and the Go emitter does not emulate it.

Files: blank, shapes, gradients, masks-clip, use-symbol, nested-groups,
raw-unsupported, tracks (`.json`). `tracks.json` carries the additive top-level
`loop` key and a nested unknown layer field (`futureField`) to prove passthrough.

Provenance (MM43, 2026-10-25): MM42 had not landed when these were written, so
they were authored from design §3.2 here and are the **source** copy. MM42
copies them into `lib/vector/fixtures/` unchanged and makes its Zod model
accept them; any later schema change updates both directories in one go.
