# CLAUDE.md — media_manipulator_api

Domain glossary: `CONTEXT.md`. Operations and deploy: `RUNBOOK.md`.

## Code navigation: graphify knowledge graph

`graphify-out/graph.json` (gitignored, local) is an AST + docs knowledge graph of this repo: every Go package, type, function and call/import edge, the migration tables (FKs and indexes), the Python GPU-host scripts, and concepts from `CONTEXT.md`, `RUNBOOK.md`, `README*.md` and `docs/`, grouped into named communities. Use it instead of chains of grep:

- Before the first grep/search/source read for a codebase question, run `graphify query "<question>"` (add `--budget 2000` to cap output). Use `graphify explain "<Symbol or file.go>"` for one node and its neighbours and `graphify path "<A>" "<B>"` for how two things connect. Read raw files only for the lines the graph pointed to. Put this rule in every subagent prompt that explores code.
- `graphify-out/GRAPH_REPORT.md` is the architecture overview (god nodes, communities, cohesion); read it only for broad review.
- After changing code, run `graphify update .` (AST only, no LLM) so the graph stays current. Doc changes (`*.md`) need `/graphify --update` to re-extract their concepts.
- If `graphify-out/` is missing (fresh clone or worktree), build it with `/graphify .`. Exclusions live in `.graphifyignore` (testdata, fonts, generated metadata catalog, email locale JSON). SQL migrations need the `graphifyy[sql]` extra (`uv tool install 'graphifyy[sql]'`).
