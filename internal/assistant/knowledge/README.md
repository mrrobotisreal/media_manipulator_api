# Medi knowledge (generated)

This directory is embedded into the API (`go:embed`, `internal/assistant/retrieve.go`).
It is **generated** by `npm run export:medi` in `media-manipulator-ui/` (MM105) and
must never be edited by hand: fix the exporter instead.

Expected contents after MM105: `tools/<slug>.md`, `pages/<route>.md`,
`editors/{studio,vector,photo}/*.md`, `routes.json`, the locale name map.
MM104 ships this README only, so the retriever is a stub that finds nothing.
