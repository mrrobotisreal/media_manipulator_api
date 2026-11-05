# Medi op schemas (copied from the UI)

`<surface>.json` (photo, studio, vector) is the JSON Schema of one editor's op
list, compiled from the Zod vocabularies in `media-manipulator-ui/lib/assistant/ops/`
by `npm run export:ops` (which writes the UI copy and this one) and committed
byte for byte in both repos (the photo-fixtures pattern). Never edit a file here
by hand: change the Zod vocabulary and re-export. `TestPhotoOpsSchemaMatchesUI`
(here) and `ops/schemaFiles.test.ts` (UI) fail when the copies differ.

Each file validates the `ops` array of one `apply_edits` call. Until a
surface's file exists, `apply_edits` is not offered to the model on that
surface; when it is inlined into the tool, its `$defs` move to the tool
parameters' root (`applyEditsParameters`).

| File | Ticket | Ops |
|---|---|---|
| `photo.json` | MM108 | `add_layer`, `update_layer`, `remove_layer`, `move_layer`, `grey_except`, `set_mask`, `apply_preset`, `auto_tone`, `set_geometry` |
| `studio.json` | MM109 | — |
| `vector.json` | MM110 | — |
