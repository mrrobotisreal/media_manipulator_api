# Medi op schemas (copied from the UI)

`<surface>.json` (photo, studio, vector) is the JSON Schema of one editor's op
list, compiled from the Zod schemas in `media-manipulator-ui/lib/assistant/ops/`
and byte-copied here (MM108–MM110; the photo-fixtures pattern, with a sync check
in both repos). Until a surface's file exists, `apply_edits` is not offered to
the model on that surface.

Each file validates the `ops` array of one `apply_edits` call.
