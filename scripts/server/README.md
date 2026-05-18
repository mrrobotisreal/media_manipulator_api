# Server reference scripts

These files are reference copies of helper scripts that live on the GPU host at
`/opt/media-manipulator-ai/scripts/`. The Go API does **not** load them from
this repo — it shells out to whatever path is configured per script. They are
checked in so the runtime version is reviewable and so updates can be diffed.

## `face_privacy.py`

Runtime path is configured by `AI_FACE_PRIVACY_SCRIPT` and defaults to
`/opt/media-manipulator-ai/scripts/face_privacy.py`. The Go API invokes it via
`AI_VISION_PYTHON` (default
`/opt/media-manipulator-ai/venvs/vision-privacy/bin/python`).

Two entry points are used:

- **Detect-only preview** (`POST /api/ai/faces/detect`):
  `python face_privacy.py --input <img> --detect-only --json-out <path>`
- **Final conversion with selection** (`POST /api/upload`):
  `python face_privacy.py --input <img> --output <out> --mode blur \
     --selection-json <selection.json>` where the JSON contains the stored
  face boxes plus the user's `selectionMode` / `selectedFaceIds`.

To deploy on the server:

```bash
sudo cp scripts/server/face_privacy.py \
  /opt/media-manipulator-ai/scripts/face_privacy.py
```
