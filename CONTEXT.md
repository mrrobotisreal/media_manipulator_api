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

**Output table** (MM118, ADR-0019):
The one Go table (`internal/services/studio_outputs.go`) that says which Darkroom exports exist: containers (`mp4`, `mov`, `mkv`, audio-only `mp3`, `wav`), the video and audio codecs each may carry, extension, MIME, media kind, `+faststart`, warnings (`apple_incompatible`, `no_browser_preview`), audio bitrates per codec, the **output presets** (`mp4_h264`, `mp4_hevc`, `mov_h264`, `mov_hevc`, `mkv_hevc_aac`, `audio_mp3`, `audio_wav`), the legacy `format` aliases and the default plan per **output token**. An export's **derived label** (`studio_exports.format`, analytics `format`) is the legacy name for the four combinations `format` always meant (`h264`, `hevc`, `mp3`, `wav`), else `<container>_<video>_<audio>`. The request's quality `preset` (low/medium/high) and its `outputPreset` are different fields. Served by `GET /api/studio/export-options`; committed as `internal/services/testdata/studio_outputs.json`, which the UI and the analytics rollup pin.
_Avoid_: hard-coding a combination anywhere else (dialog, Medi prompt, analytics enum).

**format_not_in_plan** (MM118):
The 403 a Darkroom export or render-again gets when its output uses a token outside the caller's `StudioFormats` or needs `StudioAdvanced` (a combination no preset names, a non-default bitrate or mono): `{error, message, limitKey: "studio_format", required: free|premium|pro, option, upgradePath}`. Checked by `handlers.StudioExportGate` before the export allowance is charged (after the output-height rule) and again in `launchExport`. Medi's export suggestions are downgraded to the nearest allowed preset instead (`handlers.AssistantExports`, `upgrade {required, option}`).

**Pro**:
The fourth public tier (`tiers.TierPro`, `"pro"`), above Premium: `anonymous < free < premium < pro`, with the derived `operator` above it and inheriting from it (ADR-0011). Granted by the Stripe Price id through `billing.PriceTiers` (an unknown price id grants `premium`, never `pro`) or by `cmd/grant-tier -tier pro`. API bodies call it a **plan** (`plan`, `target`: `premium | pro`); the stored column is still `tier`.

**Everyday allowance / AI allowance** (MM90):
Two separate daily counters in `mm_usage_counters`: `ops` (everyday jobs, 5 / 30 / 300 / 700 by tier) and `ai_ops` (AI jobs, 3 / 10 / 100 / 200). A request spends exactly one: an AI job is one `ai_ops` and never touches `ops`, an everyday job is one `ops` and never touches `ai_ops`, so Premium is 300 + 100 = 400 jobs a day. An **AI job** is one that runs a model: the dedicated AI routes, caption translation, and `/api/upload` / `/api/video-upload/complete` jobs that ask for upscale, face blur, text redaction, object removal, vocal separation, frame interpolation or transcription (`handlers.QuotaMetricForJob`, charged in the handler through `middleware.DeferQuota`). AI background removal, Clean Voice and background-noise removal are everyday jobs by decision.
_Avoid_: describing AI jobs as part of, or a subset of, the everyday total; "AI operations" for `ops`.

**Pending plan**:
A plan change that has been scheduled but has not happened yet: the Pro-to-Premium downgrade, a Stripe Subscription Schedule that switches the price at `current_period_end`. Exposed as `pendingPlan {to, at}` on `GET /api/billing/status`; released by `DELETE /api/billing/change-plan` ("Keep Pro"). The tier stays `pro` until the switch; the webhook then reads the Premium price and grants `premium`.
_Avoid_: "downgrade pending" as a tier or a stored column; nothing about it is stored on our side.

**Naming: the Motion editor**:
"Motion" is the user-facing brand for the SVG + animation editor (ADR-0012, design `docs/design/MOTION_SVG_ANIMATOR.md` §1). Every backend identifier is `vector`: routes `/api/vector/*`, tables `vector_projects`/`vector_exports`, package `internal/vector`, handler `VectorHandler`, tier fields `Vector*` (`vectorExportsPerDay`, `vectorFormats`, `vectorProjects`, `vectorVideo` in `/api/tiers`), env `TIER_*_VECTOR_*` and `VECTOR_*`, metric `vector_export`, ledger kind `vector_project`. Mirrors the Darkroom/`studio` split; the brand is cosmetic and swappable. Not related to the `motion` npm package.
_Avoid_: "motion" in route paths, table names or Go identifiers.

**Naming: the Aperture editor**:
"Aperture" is the user-facing brand for the photo editor (ADR-0017, design `docs/design/PHOTO_EDITOR.md` §2; brand decided by Mitch 2026-11-03). Every backend identifier is `photo`: routes `/api/photo/*`, tables `photo_projects`/`photo_derivatives`, package `internal/photo`, handler `PhotoHandler`, tier fields `photoProjects`/`photoMaxMegapixels`/`photoBatch` in `/api/tiers`, env `PHOTO_*` and `TIER_*_PHOTO_*`, ledger kind `photo_project`. Mirrors Darkroom/`studio` and Motion/`vector`; the brand is cosmetic and swappable. "Aperture" appears in this repo only in comments, docs and this entry, never in an identifier.
_Avoid_: "aperture" in route paths, table names or Go identifiers.

**PhotoDoc**:
The persisted Aperture document (v1): `source` (the oriented pixel size of the original), `heals` (server-made pixel bases, in order), `geometry` (rotate/flip/perspective/crop/fill), an ordered `layers` stack of adjustment layers each with an optional `mask`, and `output` hints. Zod model in the UI (`lib/photo/photoTypes.ts`); `internal/photo.Sanitize` is the Go mirror: it clamps out-of-range numbers, fills every default, refuses an unknown version/layer type/mask kind and returns canonical JSON (compact, keys sorted, defaults filled; unknown fields round-trip). Additive-only. `internal/photo/testdata/` (fixtures + README, the field-by-field spec) is the contract with the UI; the canonical form differs from Motion's (sorted keys, not Zod shape order).

**Photo export**:
One Aperture render landing as an ordinary job (`tool: photo_export`, `mode` = the format, one `ops`): the saved `PhotoDoc` plus the project's render base (the latest server heal base, else the original) go through `scripts/server/photo_render.py` (the ground truth the previews are measured against) and are encoded to `outputs/<jobId>/<name> (retouched).<ext>`. Also exists project-less (`POST /api/photo/export`, a file instead of a project) for anonymous visitors, and as a batch (one job per project). The renderer's per-tier megapixel cap is `--max-megapixels`.
_Avoid_: calling the PNG the renderer writes the "export" (it is an intermediate 16-bit file).

**Heal base**:
A server-made full-resolution image (`bases/<hash>.png` + a 1920 px `bases/<hash>.proxy.jpg`) that a Heal press (LaMa, one `ai_ops`) produced from the previous base plus a stroke mask; `hash = sha256(previousBaseHash + sha256(mask))`, cached in `photo_derivatives` (`heal_base`). The client appends `{id, maskRef, baseRef}` to `doc.heals`; every renderer starts from the latest base. A guest's heal base lives in the browser (`baseRef: "local:<hash>"`, opaque to the server; the stateless heal returns it as `healed.png`).

**Photo project**:
One saved Aperture project: a `photo_projects` row (the doc, `revision`, the original's facts, `status ingesting|ready|failed`) plus a directory `<PHOTO_DIR>/<id>/` holding the untouched `original.<ext>`, the 1920 px `proxy`, the 320 px `thumb`, and `masks/` (stroke masks, subject masks) and `bases/` (heal bases and their proxies, MM96). Signed-in only: anonymous visitors have one browser-local project and stateless routes (decided 2026-11-03). One ledger row of kind `photo_project` (locator = the id) covers the whole directory. Saves are compare-and-set on `revision` (`If-Match`, 428/409) exactly like Motion.

**Photo preset**:
A signed-in user's own Aperture preset (MM103): a `photo_presets` row `{name ≤ 60, category color|bw|film|fix|custom|null, layers (jsonb: an array of AdjustmentLayer, sanitized by `photo.SanitizeLayers`, ≤ 64 layers, ≤ 1 MiB), thumbnail_key, revision}` with an optional 96 px PNG at `<PHOTO_DIR>/presets/<owner>/<id>.png`. Account settings, not content: capped per tier (`photoPresets`, proposed 0 / 10 / 100 / unlimited), never charged, never expiring by time (its ledger row, kind `photo_preset`, is `keep_with_account`; only a preset or account delete sweeps it). Anonymous visitors keep presets in the browser (IndexedDB). Built-in presets are repo JSON, not rows. _Avoid_: saved look, template.

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

**Contact** (MM85):
The public contact form, `POST /api/contact` (`internal/handlers/contact.go`). It mails the support inbox (`CONTACT_TO`) through the transactional `email.Sender` with Reply-To the visitor and a 12-character **reference** in the subject and the `202` body; the email is the only record (no table, no quota). Subjects are a closed set: `bug`, `billing`, `privacy`, `press`, `other`. A filled `website` field is the honeypot. Limited by the `contact` bucket; `503 contact_unavailable` when email is off.
_Avoid_: "support ticket" (there is no ticket system), storing contact messages in Postgres.

**Job-status ownership** (MA08):
`GET /api/job/:id` and `/events` apply the result-route ownership rule (operator, signed-in owner, or the owner's session id by `X-MM-Session-ID` or `?sid=`); a stranger gets the same 404 as a missing job. Behind `JOB_STATUS_OWNERSHIP_ENFORCE` (default off: would-deny log and `mm_job_status_ownership_would_deny_total`). Checked before `TouchJobPoll`.
_Avoid_: confusing it with `RESULT_OWNERSHIP_ENFORCE`, which guards downloads and results.

**User bucket** (MA08):
The per-route hourly rate-limit bucket keyed on the hashed `mm_users.id` of a verified, synced caller (`limits.Limiter.UserSubject`), with the route's session limit and scope `user`; it replaces the IP and session buckets for that caller. Anonymous and unsynced callers keep IP + session.
_Avoid_: per-account quota (the tier quota is a separate daily allowance).

**Push token** (MA23):
An FCM device registration token in `mm_push_tokens` (uuid `user_id`, unique `token`, platform ios|android, app version, locale, `disabled_at`/`disabled_reason`). A credential for one device's push channel: never logged, never exported, never in an event. At most 10 active per user. FCM `unregistered`/`invalid` disables it; a re-registration re-enables it and moves it to the registering account. Registered by `POST /api/account/push-tokens`, removed by `DELETE`.
_Avoid_: device id, push id.

**job_updates channel** (MA23):
The Android notification channel id (`job_updates`) every job-finished push targets; MA24 creates the channel on the device. The push payload is `notification {title, body}` (tool display name only, never a file name) plus `data {jobId, tool, status: completed|failed, url: mediamanipulator://job/<jobId>}`.

**Push notifier** (MA23):
`push.Notifier`, the `services.JobFinishedNotifier` when `PUSH_ENABLED`. Wraps the email service: push first, email only when no device accepted the push.

**GPU slot** (MM94):
One grant from the shared VRAM-aware queue `services.GPUScheduler` (`SharedGPUScheduler()`, `acquireGPUForJob`, `withGPU`): a card index plus a release, held for exactly the time a GPU consumer runs. Every GPU consumer in the request path takes one: Whisper, restoration, document scan, voices, the upload AI tools (rembg, Real-ESRGAN, face privacy, redaction, LaMa, DeepFilterNet, Demucs, RIFE; `ForceIndex` = the tool's configured card) and every Ollama call (`Kind: ollama[<caller>]`, `ForceIndex` = the daemon's card from `OLLAMA_GPU_INDEX_BY_URL`, `keep_alive` 0 before release so the VRAM is really free). A batch (video-summary vision, caption translation, analysis frame batches) holds one slot for the whole batch. A queued wait is added to the job's `queueWaitMs` (MM33). With no `nvidia-smi` the slot is a no-op.
_Avoid_: "lease" for this (a `gpu.Manager` lease is audit/metrics only and never blocks); a third scheduler.

**Background priority** (MM94):
`GPUPriorityBackground` (-10), below `GPUPriorityStandard` (0) and `GPUPriorityPremium` (10): work no user waits on, today only the upload's product-intelligence **analysis**. A background waiter stands aside for every queued user job on one of its cards, never ages, and gives up after `ANALYSIS_GPU_MAX_WAIT_SECONDS` (default 120; the analysis is then dropped, not retried). A plain conversion's analysis is enqueued at upload; an AI-tool job's only after the job completed.
_Avoid_: using it for anything a user is waiting on.

**Medi** (MM104, ADR-0018):
The user-facing brand of the Media Manipulator assistant; the code identifier is **`assistant`** everywhere (`internal/assistant`, `/api/assistant/*`, `assistant_*` tables, metric `assistant_turns`), the same split as Darkroom/`studio`, Motion/`vector`, Aperture/`photo`. Hosted Claude models through OpenRouter on Medi's own key (`ASSISTANT_OPENROUTER_API_KEY`); no GPU, no Ollama.
_Avoid_: "medi" in Go identifiers, routes or tables; "chatbot".

**Guide / copilot** (MM104):
The two Medi surfaces. The **guide** (`surface: guide`) answers on public pages, is free for everyone (rate-limited) and can only show buttons (`navigate`, `open_tool`). A **copilot** (`surface: studio | vector | photo`) runs inside an editor, needs an account, spends `assistant_turns`, and can edit (`apply_edits`) or offer a button (`suggest_action`).

**Reasons** (MM114):
`apply_edits` requires `reason` (4-140 chars) beside `summary` and `ops`; each op may carry its own optional `reason` (it is in the op schema files, so it moves `PromptVersion()`). The validated batch reason is clipped to 140 and rides the `edits` SSE event as `reason`. `evals.ReasonProblems` (`reason_present`, `reason_cites`) grades every copilot answer that edited. The op schemas' "at least one field" rules are `anyOf required` lists so a `reason` alone does not satisfy them.

**Prompt version** (MM113):
`assistant.PromptVersion()`, the first 12 hex of a SHA-256 over the persona, surface intros, capsules, `knowledge/index.json` and every op schema file; stored on every assistant message and usage row, returned by capabilities and the `meta` event, and recorded with every eval run (`assistant_eval_runs`). MM114/MM115/MM116 must keep any new model-facing text inside those inputs so the version moves with it.

**Meters / MetricSet** (MM115, `internal/assistant/analyze`):
A flat name -> number map of measurements (shares 0..1: `meanLuma`, `medianLuma`, `lumaStd`, `shadowsClipped`, `highlightsClipped`, `warmth`, `tint`; audio: `integratedLUFS`, `truePeakDBFS`, `noiseFloorDB`...). Editors measure their current preview in the browser and send it as `context.metrics` and `editResult.metricsBefore/After` (<= 4 KB, copilots only); the server tool `analyze_compare` diffs the two readings and the persona's "numbers first" rule makes the model cite a figure before it adjusts again (<= `ASSISTANT_MAX_EDIT_ROUNDS` edits). `analyze_image|audio|video {fileRef}` measure a file the user dropped into the guide and are offered only when the turn has a file ref (`Turn.FileRef` + `Engine.Files`, wired by MM116a). Recipes may end with `## Success criteria` (`- meanLuma 0.35-0.55`). The `adjust` eval suite (`assistant-eval --suite adjust`) plays the browser with canned meters.

**Tool options** (MM116a, `internal/assistant/options.go`):
The per-tool JSON Schema of a tool page's form, exported by the UI to `knowledge/tools/<slug>.options.json` from the same embed config the page renders with (or `{"options":"none","reason"}` for editors and multi-step flows). The corpus parses it, renders it into the tool doc ahead of the details (so a 6000-character cut never drops it), and `open_tool.options` is validated against it before the action streams (`ToolOptions.Validate`: unknown field, out of range, wrong type, non-integer, empty, more than `MaxOptionsBytes` = 1536 bytes, or a `none` tool are all sent back to the model). The UI turns validated options into `?o=<base64url JSON>` and re-validates on arrival. Each tool doc also carries a `## Routing` section (when to send a visitor here or elsewhere, related tools) from the UI's `content/medi/tools/<slug>.md` sidecar.
_Avoid_: hand-editing `*.options.json`; calling it "preset" (a preset is a named look or setting the tool page already defines).

**Dropped file / `inspect_file`** (MM116a, `handlers/assistant_files.go`, `assistant/inspect_file.go`):
A file the visitor attaches in the guide. `POST /api/assistant/files` stores it under `UPLOAD_DIR/<fileId>/assistant_<name>` exactly like a job upload (type-detected image, video, audio or PDF; the tier's upload cap; the uploads rate bucket) with an `upload_dir` retention-ledger row under the caller's window, so the existing sweeper removes it, plus an owner digest file so only that account (or that session, for a guest) can reference it. The chat body's `fileRef` (guide only) sets `Turn.FileRef`/`FileOwner`; `Engine.Files` (the same service, a `FileResolver`) and `Engine.Inspect` bring `analyze_image|audio|video` and `inspect_file` to life for that turn. `inspect_file` runs the existing metadata inspector (exiftool + ffprobe) and returns a `<file_metadata>` block of at most ~4.5 KB, marked as data, not instructions. Nothing is converted or measured beyond that.

**Single-tool jobs / `submit_job`** (MM116b, `assistant/jobs.go`, `handlers/assistant_jobs.go`): Medi never starts a job. `submit_job {slug, options?, label?}` is a guide-only **client** tool (offered when `Engine.Jobs` is set). The server validates `options` with `ToolOptions.Validate` (the open_tool check; a `none` tool or an editor is refused and sent back to the model), then `run.confirmJob` streams `action {kind:"confirm_job", label, params:{tool, route?, options?, quota, queueEstimate}}`. `quota`/`queueEstimate` come only from `Engine.Jobs` (`JobEstimator`: read-only by construction, three methods, none creates or charges anything): `quota {metric ops|ai_ops, tier, unlimited, limit, remaining?, resetsAt}` uses `QuotaMetricForJob` (MM90) via the tool's option-schema family, the tier registry for the limit and today's counter for `remaining` (guest = session counter, no subject = omitted, never guessed); `queueEstimate {gpu, bucket none|short|long|unknown, waiting, running}` reads `GPUScheduler.Snapshot()` for GPU families only (CPU tools join no queue = `none`; no scheduler = `unknown`). There is no per-job duration metric, so no seconds are ever shown. Only the user's click on the web card starts a job, through the tool's own upload route (which charges there). Persona rule 13; eval suite `jobs` (`confirm_job`/`no_confirm_job` expectations).

**Op schema** (`internal/assistant/ops/schema/<surface>.json`, MM108):
The JSON Schema of one editor's `apply_edits` op list, generated in the UI from the Zod vocabulary and committed byte for byte here (`TestPhotoOpsSchemaMatchesUI`, `TestStudioOpsSchemaMatchesUI`, `TestVectorOpsSchemaMatchesUI`; `photo.json` MM108, `studio.json` MM109, `vector.json` MM110). Its presence switches `apply_edits` on for that surface; it is compacted and its `$defs` hoisted when inlined into the tool (`applyEditsParameters`).

**Motion template** (UI `lib/vector/templates.ts`, MM110): a ready-made animation (lower third, logo reveal, title pop, bullet reveal, counter, badge bounce) the Motion editor's Templates menu and Medi's `template` op both run. Its goldens in `internal/vector/testdata/templates/` must pass `vector.Sanitize` byte for byte.

**Turn** (MM104):
One user message and everything the server streams for it, identified by a `turnId`. An **editResult continuation** (the client reporting an applied edit, optionally with an after-frame) continues the same turn and is never charged; at most `ASSISTANT_MAX_EDIT_ROUNDS` edits per turn. "Plan my edit" is a turn on the plan model that costs `ASSISTANT_PLAN_TURN_COST` units.

**Op** (MM104, vocabularies MM108–MM110):
One entry of an `apply_edits` call: a closed, per-editor vocabulary defined in the UI's Zod schemas and byte-copied as JSON Schema into `internal/assistant/ops/schema/<surface>.json`. The server validates every call before streaming it (one retry with a tool error, then `invalid_ops`); the client applies all ops or none as one undo step. The model never writes a document.

**Knowledge corpus** (MM105):
Medi's generated docs in `internal/assistant/knowledge/` (`tools/`, `pages/`, `editors/<surface>/`, `routes.json`, `names/<locale>.json`, `index.json`), exported by `npm run export:medi` in the UI from the site's registries and embedded with `go:embed`. Ranked with BM25 (`bm25.go`, aliases, title ×3, keywords/names ×2) and filtered by surface: the guide sees tools and pages, a copilot its editor's docs, tools and recipes.
_Avoid_: hand-editing it; "the prompt" for what is retrieved per message.

**Capsule** (MM105):
The byte-stable, per-surface text appended to the persona in cached system block 1: the guide's site map + plan overview, or an editor's full parameter vocabulary (+ its top recipes from MM106). No numbers, ids or dates, so the provider's prompt cache keeps hitting.

**Recipe** (MM106):
A hand-written professional technique for one editor (or a cross-tool workflow): when to use it, what a pro does and why, a worked `apply_edits` op list in the editor's vocabulary, and how to adjust it ("too much", "warmer"). Written in the UI (`content/medi/recipes/`), exported to `knowledge/recipes/<editor>/<id>.md` (kind `recipe`), retrieved on its editor's copilot (workflows everywhere), the ten best-covering ones listed in each copilot capsule; `get_recipe` returns the full text.
_Avoid_: "preset" (an Aperture look applied by `apply_preset` is a preset; a recipe may use one).

**Eval case** (MM106):
One YAML entry in `internal/assistant/evals/cases/`: a surface, locale, message, optional fixture document and preview frame, and code-graded expectations (action buttons, required/forbidden text, language, off-topic, ask-before-edit, and op checks like `has_op(grey_except)` or `title_in_region(lower30)`). Run by `cmd/assistant-eval` against the real models; never graded by a model.

**Flywheel** (MM117):
The weekly operator CLI `cmd/assistant-flywheel` (logic in `internal/assistant/flywheel`): reads the week's signed-in Medi turns read-only, flags the ones that went badly (thumbs-down, `invalid_ops`, a rephrased out-of-scope decline, a routeless guide answer; undo and no-click only via an analytics export), clusters them by surface plus retrieved doc ids and writes a scrubbed evidence report with a proposal per cluster. It proposes; a human commits.

**Scope gate** (MM105):
The guide's cheap pre-model check (`InScope`): a message passes when it names a doc keyword, an alias, a media-craft or site term (in any of the 18 languages), small talk, or the current page; otherwise the localized out-of-scope line is returned with no model call. Copilots are never gated.

**Spend cap** (MM104):
`ASSISTANT_DAILY_SPEND_CAP_USD` (default 10): today's UTC sum of `assistant_usage_events.cost_usd` plus in-flight worst-case reservations. At the cap the copilot answers `503 assistant_paused` (no charge) and the guide answers from retrieval only, without a model call. The OpenRouter key's own credit limit is the hard backstop.
_Avoid_: confusing it with the per-user `assistant_turns` allowance.
