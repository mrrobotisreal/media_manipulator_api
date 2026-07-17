-- 20260717001_seed_odin_agentic_harness_doc.up.sql
--
-- Seed "The ODIN Agentic Harness" design document into the Double Raven
-- document store, copying the established seed pattern (document insert +
-- revision-1 snapshot; see 20260703001 / 20260711001 / 20260712002).
--
-- Content was authored from the Notion source ("The ODIN Agentic Harness",
-- Double Raven workspace, last updated July 16, 2026) and converted to
-- dr-blocks/v1 by hand. NOTE: unlike the other seeded documents, this one was
-- generated directly as a migration without a canonical TS twin in
-- media-manipulator-ui/content/dr-docs/ — if this document is ever re-seeded
-- or programmatically edited, add the canonical file and bring it under the
-- dr-editor-roundtrip discipline first.
--
-- The two Mermaid diagrams from the source are preserved as language-tagged
-- code blocks (dr-blocks has no diagram type); they render as monospace code,
-- which keeps the content faithful and copy-pasteable into any Mermaid tool.
--
-- Ownership + sharing: created_by/updated_by are 'mwintrow@creatv.io' (the
-- owner gets edit/delete + the sharing toggle via drCanDelete), and
-- allow_partner_edits is seeded FALSE — this is a locked design reference the
-- partner reads and comments on; the owner can flip the "Partner can edit"
-- switch in the viewer at any time.

BEGIN;

INSERT INTO dr_documents (slug, title, summary, status, content_format, content, created_by, updated_by, allow_partner_edits)
VALUES (
  'odin-agentic-harness',
  'The ODIN Agentic Harness',
  'Design reference for the ODIN Agentic Harness: the event-driven background-analysis system in odin-api — constitutional rules, event flow, v1 workflows, the three memories, the feedback/learning loop, and the roadmap.',
  'published',
  'dr-blocks/v1',
  $dr_doc$
{
  "format": "dr-blocks/v1",
  "blocks": [
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Double Raven Solutions LLC — Internal documentation. Last updated July 16, 2026. Status: design locked, implementation prompt (Prompt 4) ready for Claude Code.",
          "italic": true
        }
      ]
    },
    {
      "type": "heading",
      "level": 1,
      "text": "What It Is",
      "id": "what-it-is"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The "
        },
        {
          "text": "ODIN Agentic Harness",
          "bold": true
        },
        {
          "text": " is an event-driven background-analysis system inside the "
        },
        {
          "text": "odin-api",
          "code": true
        },
        {
          "text": " Go backend. It watches case data change, automatically runs analysis workflows in the background, and produces "
        },
        {
          "text": "proposals",
          "bold": true
        },
        {
          "text": " — transcripts, entity-relationship graphs, to-do lists, and (in future phases) timelines, pattern analyses, hypotheses, and report drafts — that detectives review, rate, keep, edit, or discard."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The inspiration is the “AI harness that learns as you use it” concept: the more data you add to a case, the more the harness knows, and the smarter its next pass gets. It runs on the infrastructure ODIN already has (PostgreSQL, Redis, RabbitMQ, S3, OpenRouter) plus local GPU transcription on our own hardware."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "The Three Constitutional Rules",
      "id": "the-three-constitutional-rules"
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "Nothing merges silently.",
            "bold": true
          },
          {
            "text": " Every harness output lands as a provenance-tagged, confidence-scored "
          },
          {
            "text": "proposal",
            "italic": true
          },
          {
            "text": ". A detective must explicitly keep, edit, or discard it before it touches case data. Every resolution is audit-logged. In a criminal-investigations product, a hallucinated relationship or invented timeline event is not an “oops” — so the human is always the gate."
          }
        ],
        [
          {
            "text": "The planner is code; the LLM is the analyst.",
            "bold": true
          },
          {
            "text": " Workflows are deterministic Go-defined DAGs. Each node is either pure Go logic or exactly one LLM call with a strict, schema-validated output contract. The model never decides what runs next — which keeps the system debuggable, auditable, and cheap."
          }
        ],
        [
          {
            "text": "Learning means memory + routing, not model magic.",
            "bold": true
          },
          {
            "text": " The harness does not fine-tune models. It gets better over time by accumulating three memories (below) and by tracking which models perform best at which tasks."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 1,
      "text": "How It Works",
      "id": "how-it-works"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "The event flow",
      "id": "the-event-flow"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Every meaningful change to a case (person added, evidence uploaded, OCR completed, narrative edited…) emits a "
        },
        {
          "text": "domain event",
          "bold": true
        },
        {
          "text": " using the transactional-outbox pattern: the event row is written in the "
        },
        {
          "text": "same database transaction",
          "italic": true
        },
        {
          "text": " as the change itself, then relayed to a RabbitMQ topic exchange ("
        },
        {
          "text": "odin.events",
          "code": true
        },
        {
          "text": "). This guarantees no change is ever missed, and gives us a replayable history of everything that happened to a case."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "A "
        },
        {
          "text": "scheduler",
          "bold": true
        },
        {
          "text": " listens to those events and decides what analysis work they imply — but it never reacts instantly to every keystroke. Three gates stand between an event and an actual AI task:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Debounce",
            "bold": true
          },
          {
            "text": " — the case must be quiet for ~2 minutes, so a burst of edits coalesces into one analysis pass instead of twenty."
          }
        ],
        [
          {
            "text": "Input hashing",
            "bold": true
          },
          {
            "text": " — each task type hashes its inputs; if nothing relevant changed since the last successful pass, the task is skipped entirely. (Same hash-gating pattern as the DR Portal’s nightly memory updates.)"
          }
        ],
        [
          {
            "text": "Budget",
            "bold": true
          },
          {
            "text": " — the harness has its own monthly spending cap (its own “department” in the usage system, seeded at $25/mo). If the cap is reached, tasks are recorded as skipped with a reason instead of silently draining money."
          }
        ]
      ]
    },
    {
      "type": "code",
      "language": "mermaid",
      "code": "flowchart TD\n    A[Detective edits case\\nperson added, evidence uploaded, OCR done] --> B[(events table\\nwritten in same DB transaction)]\n    B --> C[Relay goroutine] --> D{{RabbitMQ topic exchange\\nodin.events}}\n    D --> E[Scheduler\\nmarks task types dirty per case]\n    E --> F{Quiet for 120s?\\nInput hash changed?\\nBudget headroom?}\n    F -- no --> G[Skip / wait]\n    F -- yes --> H[(agent_tasks row\\n+ atomic budget reservation)]\n    H --> I{{agent.tasks queue}}\n    I --> J[Worker runs the DAG workflow]\n    J --> K[(agent_proposals\\nstatus: proposed)]\n    K --> L[SSE push to UI\\nAgent Activity panel]\n    L --> M{Detective reviews}\n    M -- keep/edit --> N[Merged into case data\\n+ audit log]\n    M -- discard --> O[Recorded — becomes\\nlearning signal]"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "The v1 workflows",
      "id": "the-v1-workflows"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Three workflows are live in the first build; four more are registered but disabled (their prompts are seeded, their logic comes later)."
        }
      ]
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Workflow"
            }
          ],
          [
            {
              "text": "Type"
            }
          ],
          [
            {
              "text": "What it does"
            }
          ]
        ],
        [
          [
            {
              "text": "Transcribe",
              "bold": true
            }
          ],
          [
            {
              "text": "Local GPU"
            }
          ],
          [
            {
              "text": "Any audio/video evidence or document is automatically transcribed on our own server — ffmpeg extracts audio, then "
            },
            {
              "text": "whisper-ctranslate2",
              "code": true
            },
            {
              "text": " runs the "
            },
            {
              "text": "whisper-large-v3",
              "bold": true
            },
            {
              "text": " model across the two RTX 5080s. Zero cloud cost, and sensitive interview audio never leaves our hardware."
            }
          ]
        ],
        [
          [
            {
              "text": "Entity Graph",
              "bold": true
            }
          ],
          [
            {
              "text": "LLM (OpenRouter)"
            }
          ],
          [
            {
              "text": "Gathers persons, narrative, kept transcripts, and OCR text; an LLM extracts entities and relationships into a strict JSON schema; a deterministic diff annotates what’s new/changed vs. the existing graph. Output is d3-ready "
            },
            {
              "text": "{nodes, links}",
              "code": true
            },
            {
              "text": " the Link Chart renders directly."
            }
          ]
        ],
        [
          [
            {
              "text": "To-Do List",
              "bold": true
            }
          ],
          [
            {
              "text": "Pure Go — no AI"
            }
          ],
          [
            {
              "text": "A deterministic checklist generated from case completeness, grouped into the Case Orchestrator’s five phases (Details, Subjects, Evidence, OSINT, Reports): people missing details, evidence without custody entries, media without transcripts, addresses not geocoded, documents never OCR’d, and so on. Each new list supersedes the previous one."
            }
          ]
        ],
        [
          [
            {
              "text": "Timeline Analysis"
            }
          ],
          [
            {
              "text": "LLM — "
            },
            {
              "text": "disabled in v1",
              "italic": true
            }
          ],
          [
            {
              "text": "Event chronology extraction and ordering."
            }
          ]
        ],
        [
          [
            {
              "text": "Pattern Diff"
            }
          ],
          [
            {
              "text": "LLM — "
            },
            {
              "text": "disabled in v1",
              "italic": true
            }
          ],
          [
            {
              "text": "Pattern analysis, diffed against the harness’s own prior pass."
            }
          ]
        ],
        [
          [
            {
              "text": "Hypotheses"
            }
          ],
          [
            {
              "text": "LLM — "
            },
            {
              "text": "disabled in v1",
              "italic": true
            }
          ],
          [
            {
              "text": "Multiple plausible investigative hypotheses, generated as proposals to save/edit/discard."
            }
          ]
        ],
        [
          [
            {
              "text": "Report Draft"
            }
          ],
          [
            {
              "text": "LLM — "
            },
            {
              "text": "disabled in v1",
              "italic": true
            }
          ],
          [
            {
              "text": "Report generation from real case data (replaces the simulated AI Report Builder)."
            }
          ]
        ]
      ]
    },
    {
      "type": "code",
      "language": "mermaid",
      "code": "flowchart LR\n    subgraph Transcribe workflow\n        F1[fetch\\nS3 download] --> F2[extract-audio\\nffmpeg → 16kHz WAV] --> F3[asr\\nwhisper-large-v3\\non 2× RTX 5080] --> F4[normalize\\nsegments + language] --> P1[proposal: transcript]\n    end\n    subgraph Entity Graph workflow\n        G1[gather\\npersons + narrative +\\ntranscripts + OCR text] --> G2[extract\\nLLM, strict JSON schema\\nretry → fallback model] --> G3[diff\\nvs existing graph] --> P2[proposal: entity_graph\\nd3-ready nodes+links]\n    end"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "How It Learns and Adapts",
      "id": "how-it-learns-and-adapts"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "The three memories",
      "id": "the-three-memories"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Case memory (",
            "bold": true
          },
          {
            "text": "case_facts",
            "bold": true,
            "code": true
          },
          {
            "text": ")",
            "bold": true
          },
          {
            "text": " — structured facts with provenance and vector embeddings (pgvector). Each analysis pass can include what previous passes established, so pass N+1 is genuinely smarter about "
          },
          {
            "text": "this case",
            "italic": true
          },
          {
            "text": " than pass N. Facts are deduplicated by hash and can be superseded, never silently overwritten."
          }
        ],
        [
          {
            "text": "Routing memory (",
            "bold": true
          },
          {
            "text": "model_routing",
            "bold": true,
            "code": true
          },
          {
            "text": ")",
            "bold": true
          },
          {
            "text": " — which model handles which task type, with a fallback and per-task parameters. Seeded from our DR Chat Lab research (e.g., Gemini 3.x strength on vision/handwriting; GLM 5.2 excluded from multimodal). Admin-editable via the API today; updated automatically by the A/B layer in the future."
          }
        ],
        [
          {
            "text": "Prompt memory (",
            "bold": true
          },
          {
            "text": "prompt_versions",
            "bold": true,
            "code": true
          },
          {
            "text": ")",
            "bold": true
          },
          {
            "text": " — every analyst prompt is versioned with its JSON output schema. Every run records exactly which prompt version produced it, so prompt improvements are measurable, and regressions are traceable."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "The feedback system",
      "id": "the-feedback-system"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Every AI result — any proposal, any individual model run, any task — can be "
        },
        {
          "text": "rated (1–5 stars) and commented on by any user, at any time, and changed at any time",
          "bold": true
        },
        {
          "text": ". The current rating is upserted; every revision is preserved in an append-only history. Combined with the keep/edit/discard disposition on proposals, this gives us the highest-quality learning signal there is: "
        },
        {
          "text": "what the detective actually did with the output",
          "italic": true
        },
        {
          "text": ". No judge model can match that."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "What feeds the learning loop",
      "id": "what-feeds-the-learning-loop"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Every model invocation is logged ("
        },
        {
          "text": "agent_runs",
          "code": true
        },
        {
          "text": "): model, prompt version, tokens, cost, latency, and whether the output passed schema validation. A nightly aggregator (4 AM Mountain, DST-safe — same scheduler pattern as the DR Portal) rolls these up with proposal dispositions and feedback ratings into "
        },
        {
          "text": "model_stats",
          "code": true
        },
        {
          "text": " per task type, model, and month. In v1 this is a "
        },
        {
          "text": "read-only learning substrate",
          "bold": true
        },
        {
          "text": " — it observes and reports, and we make routing changes by hand. The A/B layer (roadmap) will act on it."
        }
      ]
    },
    {
      "type": "heading",
      "level": 1,
      "text": "Where It Excels — and Where It Doesn’t",
      "id": "where-it-excels-and-where-it-doesnt"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Excels:",
          "bold": true
        },
        {
          "text": " everything shaped like "
        },
        {
          "text": "“data in, schema-validated JSON out, human reviews before merge”",
          "italic": true
        },
        {
          "text": " — entity extraction, transcription, chronology, checklists, drafts, and hypotheses-as-proposals. It’s cheap and idempotent by construction (debounce + hash-gating), it can never spend beyond its cap, and it never breaks a user-facing request (failed tasks fail soft, release their budget reservation, and log loudly)."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Doesn’t (by design or by reality):",
          "bold": true
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "It does not make models smarter — it makes "
          },
          {
            "text": "our use of them",
            "italic": true
          },
          {
            "text": " smarter (memory + routing)."
          }
        ],
        [
          {
            "text": "It does not auto-merge anything into case data, ever."
          }
        ],
        [
          {
            "text": "It cannot automate OSINT collection yet — the OSINT tools are still simulated, and there’s nothing real to orchestrate. Real SpiderFoot REST integration is a prerequisite. Autonomous social-media scraping stays off the table for legal/admissibility reasons; human-initiated OSINT with harness-assisted analysis is the line."
          }
        ],
        [
          {
            "text": "Speaker diarization (who said what) is not in v1 — it requires whisperX/pyannote-style tooling on top of our local ASR. Roadmapped."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 1,
      "text": "Roadmap",
      "id": "roadmap"
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "UI: Agent Activity & review panel",
            "bold": true
          },
          {
            "text": " (next UI prompt) — live SSE feed of task/proposal lifecycle per case, the keep/edit/discard review surface, and star-rating + comment controls on every AI result."
          }
        ],
        [
          {
            "text": "Enable the disabled workflows",
            "bold": true
          },
          {
            "text": " in value order: Timeline Analysis → Hypotheses → Pattern Diff → Report Draft (retiring the simulated AI Report Builder)."
          }
        ],
        [
          {
            "text": "A/B testing layer",
            "bold": true
          },
          {
            "text": " — shadow-run a challenger model on a sampled 10–20% of jobs; evaluate via layered signals (schema validity → human disposition/ratings → sampled pairwise LLM judging with position-swapping, never self-judging); epsilon-greedy routing (~90% exploit / ~10% explore) that updates "
          },
          {
            "text": "model_routing",
            "code": true
          },
          {
            "text": " from "
          },
          {
            "text": "model_stats",
            "code": true
          },
          {
            "text": " automatically. The schema for all of this already exists — no migration needed."
          }
        ],
        [
          {
            "text": "Speaker diarization",
            "bold": true
          },
          {
            "text": " on the local ASR path."
          }
        ],
        [
          {
            "text": "Retrieval endpoints over case memory",
            "bold": true
          },
          {
            "text": " — semantic search across "
          },
          {
            "text": "case_facts",
            "code": true
          },
          {
            "text": " embeddings, powering “ask the case” style queries and smarter analyst-prompt context selection."
          }
        ],
        [
          {
            "text": "Real OSINT orchestration",
            "bold": true
          },
          {
            "text": " once SpiderFoot’s REST API is integrated."
          }
        ],
        [
          {
            "text": "Notifications",
            "bold": true
          },
          {
            "text": " — harness lifecycle events surfaced through Smart Notifications (making another simulated tool real)."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 1,
      "text": "Technical Reference (quick)",
      "id": "technical-reference-quick"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "New tables (migration 0004):",
            "bold": true
          },
          {
            "text": " "
          },
          {
            "text": "events",
            "code": true
          },
          {
            "text": " (outbox), "
          },
          {
            "text": "agent_tasks",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "agent_runs",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "agent_proposals",
            "code": true
          },
          {
            "text": " (unified — hypotheses are a proposal kind), "
          },
          {
            "text": "agent_feedback",
            "code": true
          },
          {
            "text": " + "
          },
          {
            "text": "agent_feedback_history",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "case_facts",
            "code": true
          },
          {
            "text": " (pgvector, 1536-dim), "
          },
          {
            "text": "prompt_versions",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "model_routing",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "model_stats",
            "code": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "New API surface:",
            "bold": true
          },
          {
            "text": " "
          },
          {
            "text": "/api/agent/tasks",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "/api/agent/proposals",
            "code": true
          },
          {
            "text": " (+ "
          },
          {
            "text": "/resolve",
            "code": true
          },
          {
            "text": "), "
          },
          {
            "text": "/api/agent/feedback",
            "code": true
          },
          {
            "text": " (upsert, editable forever), "
          },
          {
            "text": "/api/agent/routing",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "/api/agent/transcribe",
            "code": true
          },
          {
            "text": " (manual trigger), "
          },
          {
            "text": "/api/agent/stream",
            "code": true
          },
          {
            "text": " (SSE)."
          }
        ],
        [
          {
            "text": "Infra:",
            "bold": true
          },
          {
            "text": " RabbitMQ topic exchange "
          },
          {
            "text": "odin.events",
            "code": true
          },
          {
            "text": "; queues "
          },
          {
            "text": "agent.scheduler",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "agent.tasks",
            "code": true
          },
          {
            "text": " (+ DLQ); Redis for dirty-flags, debounce, and SSE pub/sub; budget department "
          },
          {
            "text": "agent",
            "code": true
          },
          {
            "text": " ($25/mo seed)."
          }
        ],
        [
          {
            "text": "Local ASR:",
            "bold": true
          },
          {
            "text": " "
          },
          {
            "text": "/opt/creatv/whisper-ct2/bin/whisper-ctranslate2 --model large-v3 --device cuda --compute_type float16",
            "code": true
          },
          {
            "text": ", jobs spread across both GPUs via "
          },
          {
            "text": "--device_index",
            "code": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "Proposal statuses:",
            "bold": true
          },
          {
            "text": " "
          },
          {
            "text": "proposed → kept | edited | discarded | superseded",
            "code": true
          },
          {
            "text": ". Kept/edited entity graphs merge into the case Link Chart; kept transcripts attach to their source media and seed case memory."
          }
        ]
      ]
    }
  ]
}
$dr_doc$::jsonb,
  'mwintrow@creatv.io',
  'mwintrow@creatv.io',
  false
);

INSERT INTO dr_document_revisions (document_id, revision_number, title, content_format, content, created_by)
SELECT id, 1, title, content_format, content, 'mwintrow@creatv.io'
FROM dr_documents WHERE slug = 'odin-agentic-harness';

COMMIT;
