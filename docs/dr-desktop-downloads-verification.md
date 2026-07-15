# DR Portal desktop app downloads — runtime verification

Runtime checks for the "Double Raven Portal desktop app" download section on the
portal home (`/dr`) and its API endpoint
`GET /api/dr/desktop/download-url?platform=mac-arm64|mac-intel|windows`.
Static verification (`go build/vet/test`, `tsc`, `lint`) already passed at
development time; everything below runs on the Ubuntu server / against the
deployed UI.

## 1. Environment + assets

The endpoint presigns GETs against `S3_BUCKET` (default `media-manipulator`)
using three env-overridable keys (RUNBOOK §4.10). Defaults — note the **literal
spaces**; the SDK presigner handles encoding:

| Var | Default |
| --- | --- |
| `DR_DESKTOP_MAC_ARM64_KEY` | `double-raven/desktop/mac/apple/Double Raven Portal-0.1.0-arm64.dmg` |
| `DR_DESKTOP_MAC_INTEL_KEY` | `double-raven/desktop/mac/intel/Double Raven Portal-0.1.0.dmg` |
| `DR_DESKTOP_WINDOWS_KEY` | `double-raven/desktop/windows/Double Raven Portal Setup 0.1.0.exe` |

Before testing:

1. **Upload the three installers** to exactly those keys (the API never uploads
   them). Confirm:

   ```bash
   aws s3 ls 's3://media-manipulator/double-raven/desktop/' --recursive
   ```

   All three artifacts must appear, filenames (including spaces) intact.

2. **Drop the platform logos** into the UI repo's `public/`:
   `dr-apple-logo.svg` and `dr-windows-logo.svg` (official Brandfetch assets).
   Missing files just render as broken images on the buttons — no build failure.

## 2. API checks (curl)

Get a Firebase ID token for an allowlisted DR account (same way as in
`dr-feedback-verification.md`) and export it as `$TOKEN`. `$API` is the API
origin (e.g. `https://api.media-manipulator.com`).

Each platform returns a URL + the correct `fileName`:

```bash
for p in mac-arm64 mac-intel windows; do
  curl -s -H "Authorization: Bearer $TOKEN" \
    "$API/api/dr/desktop/download-url?platform=$p" | jq '{platform, fileName, url: (.url | .[0:80])}'
done
```

Expected `fileName` per platform:

- `mac-arm64` → `Double Raven Portal-0.1.0-arm64.dmg`
- `mac-intel` → `Double Raven Portal-0.1.0.dmg`
- `windows` → `Double Raven Portal Setup 0.1.0.exe`

Error paths:

```bash
# Unknown platform → 400 {"error": "Unknown platform"}
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" \
  "$API/api/dr/desktop/download-url?platform=linux"

# Unauthenticated → 401
curl -s -o /dev/null -w '%{http_code}\n' \
  "$API/api/dr/desktop/download-url?platform=windows"
```

The presigned URL itself works and expires:

```bash
URL=$(curl -s -H "Authorization: Bearer $TOKEN" \
  "$API/api/dr/desktop/download-url?platform=mac-arm64" | jq -r .url)

# Immediately → 200
curl -s -o /dev/null -w '%{http_code}\n' "$URL"

# TTL is 5 minutes — after ~6 minutes the SAME URL → 403
sleep 360 && curl -s -o /dev/null -w '%{http_code}\n' "$URL"
```

## 3. Browser checks

On `/dr` (signed in as an allowlisted account):

1. The **"Double Raven Portal desktop app"** section renders **below** the three
   nav cards (Documentation / Demos / Communication-Feedback), at both desktop
   and mobile widths (the three buttons wrap on narrow screens). The nav cards'
   layout is unchanged.
2. Each button shows its platform logo (Apple logo on both Mac buttons, Windows
   logo on the Windows button) plus its label.
3. Clicking each button downloads the right file with the right filename —
   spaces intact (e.g. `Double Raven Portal-0.1.0-arm64.dmg`, not a
   URL-encoded name).
4. Double-clicking a button does **not** double-fire: while a URL fetch is in
   flight the clicked button shows a spinner and all three are disabled.
5. Failure toast: temporarily misconfigure S3 (e.g. point
   `DR_DESKTOP_WINDOWS_KEY` at the server side to an empty value is not enough —
   instead stop/deny S3, or unset AWS credentials and restart so presigning
   fails). Clicking a button shows the sonner toast
   *"Couldn't start the download — try again."* and the pending spinner clears.
   Restore the config afterwards.

## 4. Version-bump drill (no code change)

1. Upload a hypothetical new build, e.g.
   `s3://media-manipulator/double-raven/desktop/windows/Double Raven Portal Setup 0.2.0.exe`.
2. On the server set
   `DR_DESKTOP_WINDOWS_KEY="double-raven/desktop/windows/Double Raven Portal Setup 0.2.0.exe"`
   and restart the API.
3. Click **Windows** on `/dr` — the `0.2.0` installer downloads under its new
   filename. No code change, no UI deploy.
