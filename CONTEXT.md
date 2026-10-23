# Backend API (media_manipulator_api)

The primary Go API for media-manipulator.com: media-processing tools, uploads, the shared job system, and the Content Studio (Darkroom) render pipeline.

## Language

**Content Studio**:
The internal/engineering name for the browser-based multi-track video editor. Everything in this repo keeps this name: `/api/studio/*` routes, `studio_projects`/`studio_assets` tables, `Studio*` models, `studio_*.go` services. The user-facing product name is **Darkroom** (see below) — do not rename backend identifiers to match.

**Darkroom still images**:
Assets with `media_kind = 'image'` (JPG/PNG/WebP/GIF/BMP/TIFF/HEIC/HEIF/AVIF/SVG). Ingest runs ImageMagick (libheif) + `rsvg-convert` on the workstation to make a 1920px display proxy and 320px thumbnail (stored in `thumbnail_sprite_url`). See ADR-0008.

**Darkroom**:
The user-facing product name for Content Studio, used in the web UI, admin UI, and CreaTV. It appears **nowhere** in this backend by design — the rename is cosmetic-only in the UIs. Any "Darkroom" reference arriving from a UI or ticket means Content Studio.
_Avoid_: introducing "darkroom" into route paths, table names, or Go identifiers.

**EDL** (Edit Decision List):
The persisted editor document — tracks, clips, effects, captions, audio config. Go structs in `internal/models/studio.go`, mirrored field-for-field (camelCase JSON) by the frontend Zod schema. Sanitized on save and before export.

**Studio Project / Studio Asset**:
A saved editor document (EDL + metadata), and an ingested media source (video/audio/LUT) with server-generated proxy, sprite, and peaks. Owned by an `mm_users` id or an anonymous `X-MM-Session-ID`.
