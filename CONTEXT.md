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

**Pro**:
The fourth public tier (`tiers.TierPro`, `"pro"`), above Premium: `anonymous < free < premium < pro`, with the derived `operator` above it and inheriting from it (ADR-0011). Granted by the Stripe Price id through `billing.PriceTiers` (an unknown price id grants `premium`, never `pro`) or by `cmd/grant-tier -tier pro`. API bodies call it a **plan** (`plan`, `target`: `premium | pro`); the stored column is still `tier`.

**Pending plan**:
A plan change that has been scheduled but has not happened yet: the Pro-to-Premium downgrade, a Stripe Subscription Schedule that switches the price at `current_period_end`. Exposed as `pendingPlan {to, at}` on `GET /api/billing/status`; released by `DELETE /api/billing/change-plan` ("Keep Pro"). The tier stays `pro` until the switch; the webhook then reads the Premium price and grants `premium`.
_Avoid_: "downgrade pending" as a tier or a stored column; nothing about it is stored on our side.

**Naming: the Motion editor**:
"Motion" is the user-facing brand for the SVG + animation editor (ADR-0012, design `docs/design/MOTION_SVG_ANIMATOR.md` §1). Every backend identifier is `vector`: routes `/api/vector/*`, tables `vector_projects`/`vector_exports`, package `internal/vector`, handler `VectorHandler`, tier fields `Vector*` (`vectorExportsPerDay`, `vectorFormats`, `vectorProjects`, `vectorVideo` in `/api/tiers`), env `TIER_*_VECTOR_*` and `VECTOR_*`, metric `vector_export`, ledger kind `vector_project`. Mirrors the Darkroom/`studio` split; the brand is cosmetic and swappable. Not related to the `motion` npm package.
_Avoid_: "motion" in route paths, table names or Go identifiers.

**VectorDoc**:
The persisted Motion document (v1): canvas, a layer tree, `defs` (gradients, masks, clipPaths, filters, symbols) and keyframe `animations` tracks. Zod model in the UI (`lib/vector/vectorTypes.ts`); `internal/vector.Sanitize` is the Go mirror that validates and canonicalises it before storage. Additive-only: unknown fields round-trip, an unknown `version` is refused. The shared fixtures in `internal/vector/testdata/` are the contract.

**Counted Motion export**:
One export or import conversion spent against the `vector_export` daily allowance by `POST /api/vector/exports` (or `/api/vector/vectorize`), recorded as one `vector_exports` row in the same transaction. Static SVG export is never counted. A unit is spent before the browser runs the exporter and is not refunded on cancel. Each one also emits `server_vector_export_counted {format, tier}` (MM49) when the caller sent analytics identity; `vector_exports` stays the ground truth.

**Pro Motion row** (MM49):
Pro's own `Vector*` limits (`TIER_PRO_VECTOR_*`): 150 exports/day, Premium's formats plus `png_sequence`, `mp4`, `webm` (unioned, so Pro never loses a Premium format), unlimited projects, raster video 2160p / 60 fps / 60 s. Operator still derives from Pro.

**Billing currency** (MM69):
`usd` or `eur` (`billing.CurrencyUSD` / `CurrencyEUR`), sold at the same numerals. A Checkout Session always carries one: the request's, or the Stripe Customer's locked currency, which takes precedence (a mismatch answers 409 `currency_locked`). `mm_users.billing_currency` mirrors `Subscription.currency`, and `mm_user_settings.currency` stores the user's choice. The geographic default (`BILLING_EUR_COUNTRIES`) is display-only and is never used to charge.
_Avoid_: converting amounts, adding USD and EUR together, or reading the currency from the item price, which is only the multi-currency Price's default.

**Video summary** (MM74, ADR-0014):
A staged job (tool `video_summary`, mode `summarize`) that turns a video or audio file into `summary.json` + `summary.md`: title, 3-6 sentence summary, timestamped chapters, topics, on-screen text, visual and speech paragraphs, language, caveats. Stages `probe`, `audio` (Whisper) ∥ `frames` (scene-aware sampling), `vision` (VLM notes per batch of four frames with that window's transcript), `synthesis` (one text-model call, schema-validated, one repair retry), `render`. Routes `/api/summarize/*`; one `ai_ops`; sign-in required (anonymous frame budget 0). Distinct from the internal **analysis** (`analysis_queue.go`, `analysis.json`), which shares the frame/VLM helpers in `vlm.go` but is product intelligence, never shown to users.
_Avoid_: calling a plain transcript a summary; "analysis" for this feature.

**Deep synthesis** (MM74):
`depth: deep` on a video summary: the synthesis step (only) runs on `OLLAMA_SUMMARY_DEEP_MODEL` (the 27B) served by the third, unpinned Ollama daemon at `OLLAMA_SUMMARY_DEEP_URL` (:11436). Pro only (`tiers.Limits.SummaryDeep`), and only when both env vars are set, which waits on Mitch's benchmark (>= 12 tokens/s at 100% GPU). Otherwise a `deep` request silently runs as `standard` and the job's `options.depth` records the effective depth. The frame pass never uses the deep model.
_Avoid_: "deep mode" for anything but the synthesis model choice.

**Voices** (MM77, ADR-0015):
The feature: clone a voice from the user's own 5-60 s clip, design one from a text description, speak text in a saved or preset voice, convert a recording to a voice, and shift pitch/formant. Routes `/api/voices/*`, jobs `voice_tts` | `voice_convert` | `voice_shift` (the `mode` column carries the voice id or preset name), tier fields `voices*`, env `VOICES_*` and `TIER_*_VOICES_*`.
_Avoid_: "TTS" or "clone" as the feature name in identifiers; "voiceover" (that is Darkroom's recorded narration, MM79 builds on voices for it).

**mm_voices**:
The table of saved voices: always owned by a signed-in account (anonymous visitors cannot save one), `kind` cloned | designed | preset, a consent record (`consent_at`, `consent_text_version`) required for a cloned voice, soft delete then sweep. Ledger kind `voice`; its reference clip and preview are `s3_object` rows under `voices/<userId>/<voiceId>/` with the voice id as parent.

**mm-voices**:
The resident FastAPI service on the GPU host (`scripts/server/voices_service.py`, unit `deploy/mm-voices.service`, `127.0.0.1:8791`, GPU 0) that holds Chatterbox Multilingual and Qwen3-TTS and answers the API's `internal/services/voices_client.go`. Files travel by path under the shared `VOICES_SCRATCH_DIR`. Not a per-job subprocess, unlike every other AI tool here.
_Avoid_: calling it "the voices API" (that is `/api/voices` in this repo).
