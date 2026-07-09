-- 20260711001_seed_cloud_ai_model_access_doc.up.sql
--
-- Seed the "Cloud AI Model Access: Direct APIs vs. OpenRouter" Infrastructure
-- Strategy Doc into the Double Raven document store, copying the
-- 20260703001 seed pattern verbatim (document insert + revision-1 snapshot).
--
-- The content JSONB below is generated from (and MUST stay byte-for-byte
-- identical to) media-manipulator-ui/content/dr-docs/cloud-ai-model-access.ts,
-- which is the single source of truth for this document — the same discipline
-- as the first seeded document.
--
-- ONE deliberate difference from the first seed: created_by/updated_by are
-- 'mwintrow@creatv.io' (not 'seed:migration') so drCanDelete's
-- EqualFold(created_by, callerEmail) check grants that owner edit/delete
-- rights over this document; the revision's created_by matches. Both portal
-- users can read and comment on it like any published document.

BEGIN;

INSERT INTO dr_documents (slug, title, summary, status, content_format, content, created_by, updated_by)
VALUES (
  'cloud-ai-model-access',
  'Cloud AI Model Access: Direct APIs vs. OpenRouter',
  'Infrastructure Strategy Doc: direct provider API keys vs. routing through OpenRouter — routing architecture, pricing mechanics, cost tracking, privacy controls, and the recommended hybrid strategy for Double Raven.',
  'published',
  'dr-blocks/v1',
  $dr_doc$
{
  "format": "dr-blocks/v1",
  "blocks": [
    {
      "type": "blockquote",
      "lines": [
        [
          {
            "text": "Document Type:",
            "bold": true
          },
          {
            "text": " Infrastructure Strategy Doc"
          }
        ],
        [
          {
            "text": "Project:",
            "bold": true
          },
          {
            "text": " Double Raven"
          }
        ],
        [
          {
            "text": "Status:",
            "bold": true
          },
          {
            "text": " Draft"
          }
        ],
        [
          {
            "text": "Last Updated:",
            "bold": true
          },
          {
            "text": " July 2026"
          }
        ]
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "Table of Contents",
      "id": "table-of-contents"
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "Executive Summary",
            "link": "#1-executive-summary"
          }
        ],
        [
          {
            "text": "Two Fundamental Approaches to Cloud AI Access",
            "link": "#2-two-fundamental-approaches-to-cloud-ai-access"
          }
        ],
        [
          {
            "text": "Direct Provider API Keys — Deep Dive",
            "link": "#3-direct-provider-api-keys-deep-dive"
          }
        ],
        [
          {
            "text": "Current Direct Pricing Snapshot (Anthropic & OpenAI)",
            "link": "#4-current-direct-pricing-snapshot-anthropic-openai"
          }
        ],
        [
          {
            "text": "OpenRouter — Comprehensive Deep Dive",
            "link": "#5-openrouter-comprehensive-deep-dive"
          }
        ],
        [
          {
            "text": "Head-to-Head: Direct API Keys vs. OpenRouter",
            "link": "#6-head-to-head-direct-api-keys-vs-openrouter"
          }
        ],
        [
          {
            "text": "Other Notable Aggregators & Alternatives",
            "link": "#7-other-notable-aggregators-alternatives"
          }
        ],
        [
          {
            "text": "Recommended Strategy for Double Raven",
            "link": "#8-recommended-strategy-for-double-raven"
          }
        ],
        [
          {
            "text": "Final Decision Matrix",
            "link": "#9-final-decision-matrix"
          }
        ]
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "1. Executive Summary",
      "id": "1-executive-summary"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This document is a companion to the "
        },
        {
          "text": "Backend & AI Infrastructure",
          "bold": true
        },
        {
          "text": " doc and focuses specifically on "
        },
        {
          "text": "cloud-based",
          "italic": true
        },
        {
          "text": " model access strategy — not on-premises hardware. The central question:"
        }
      ]
    },
    {
      "type": "blockquote",
      "lines": [
        [
          {
            "text": "Should Double Raven's AI backend call model providers (Anthropic, OpenAI, Google, etc.) directly with individual API keys — or should it route all calls through an aggregator like OpenRouter, which provides one API key and lets you select from hundreds of models across dozens of providers?",
            "italic": true
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The short answer, expanded throughout this document: "
        },
        {
          "text": "these are not mutually exclusive",
          "bold": true
        },
        {
          "text": ", and the strongest architecture for most teams (including a solo-developer project like Double Raven) is a "
        },
        {
          "text": "hybrid approach",
          "bold": true
        },
        {
          "text": " — OpenRouter as the default routing layer for flexibility and resilience, with direct provider keys reserved for cases where provider-specific features (prompt caching, batch APIs, bleeding-edge model access) or absolute-lowest-latency matter enough to justify managing a second integration."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This document goes deep on "
        },
        {
          "text": "how OpenRouter actually works",
          "bold": true
        },
        {
          "text": " — its routing architecture, provider/infra selection, pricing mechanics, cost and token tracking, and privacy model — since that's the direction most likely to be adopted for Double Raven's AI pipeline in the near term."
        }
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "2. Two Fundamental Approaches to Cloud AI Access",
      "id": "2-two-fundamental-approaches-to-cloud-ai-access"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.1 Approach A: Direct Provider API Keys",
      "id": "2-1-approach-a-direct-provider-api-keys"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "You sign up directly with each AI lab — Anthropic, OpenAI, Google, etc. — and get a separate API key, billing relationship, rate limit pool, and SDK for each one. Your application code talks directly to "
        },
        {
          "text": "api.anthropic.com",
          "code": true
        },
        {
          "text": ", "
        },
        {
          "text": "api.openai.com",
          "code": true
        },
        {
          "text": ", and so on."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.2 Approach B: Aggregators / Routers (e.g., OpenRouter)",
      "id": "2-2-approach-b-aggregators-routers-e-g-openrouter"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "You sign up with a single aggregator service that sits between your application and the underlying model providers. You get "
        },
        {
          "text": "one API key",
          "bold": true
        },
        {
          "text": ", "
        },
        {
          "text": "one bill",
          "bold": true
        },
        {
          "text": ", and "
        },
        {
          "text": "one OpenAI-compatible endpoint",
          "bold": true
        },
        {
          "text": ", and you select which model to use per-request via a model string (e.g., "
        },
        {
          "text": "anthropic/claude-sonnet-4.6",
          "code": true
        },
        {
          "text": " or "
        },
        {
          "text": "openai/gpt-5.4",
          "code": true
        },
        {
          "text": "). The aggregator handles authentication with the underlying providers on your behalf, picks which literal infrastructure/provider actually serves the request, and can automatically fail over to a backup if your first choice is down or rate-limited."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.3 Why This Distinction Matters for Double Raven",
      "id": "2-3-why-this-distinction-matters-for-double-raven"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Double Raven's AI pipeline (per the Backend & AI Infrastructure doc) spans OCR, HTR, NLP, entity extraction, and semantic analysis. Some of that work is well-suited to smaller, cheaper, open-weight models; some of it (complex identity resolution, cross-document reasoning) benefits from frontier models like Claude. A router that lets you cheaply swap models per-task — without rewriting integration code every time a better or cheaper model ships — has real architectural value for a project like this, especially while the pipeline design is still evolving."
        }
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "3. Direct Provider API Keys — Deep Dive",
      "id": "3-direct-provider-api-keys-deep-dive"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "3.1 How It Works",
      "id": "3-1-how-it-works"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Each provider (Anthropic, OpenAI, Google, etc.) issues you an API key tied to your account with them. You integrate their specific SDK or REST API format, manage your own rate limits and quotas with that provider, and receive a separate invoice/billing dashboard from each one you use."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "3.2 Pros of Direct API Keys",
      "id": "3-2-pros-of-direct-api-keys"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Lowest possible latency.",
            "bold": true
          },
          {
            "text": " No intermediary hop. Direct Anthropic API access has been benchmarked at roughly 2x faster on p50 latency versus routing through an aggregator, since there's no additional network hop or routing-layer processing."
          }
        ],
        [
          {
            "text": "Day-one access to new features.",
            "bold": true
          },
          {
            "text": " When Anthropic or OpenAI ship a new capability (e.g., extended thinking, computer use, a new tool-use format), it's available immediately via direct API. Aggregators need to build support for it, which can lag by days to weeks."
          }
        ],
        [
          {
            "text": "Full access to provider-specific cost optimizations.",
            "bold": true
          },
          {
            "text": " This is significant:"
          }
        ]
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Anthropic prompt caching",
            "bold": true
          },
          {
            "text": ": up to 90% off cached input tokens, with fine-grained control over cache breakpoints ("
          },
          {
            "text": "cache_control",
            "code": true
          },
          {
            "text": "), 5-minute or 1-hour TTLs."
          }
        ],
        [
          {
            "text": "Anthropic Batch API",
            "bold": true
          },
          {
            "text": ": flat 50% off all token costs for asynchronous processing (results within 24 hours), plus support for larger max-output-token limits per request."
          }
        ],
        [
          {
            "text": "OpenAI Batch API",
            "bold": true
          },
          {
            "text": ": flat 50% off, similar 24-hour turnaround."
          }
        ],
        [
          {
            "text": "OpenAI automatic caching",
            "bold": true
          },
          {
            "text": ": cached input billed at a fraction of standard rate, applied automatically with no code changes."
          }
        ],
        [
          {
            "text": "Aggregators generally pass these discounts through, but with caveats (see Section 5.7) — caching benefits depend on being routed to the "
          },
          {
            "text": "same",
            "italic": true
          },
          {
            "text": " underlying provider endpoint consistently, which isn't always guaranteed unless you pin the provider."
          }
        ],
        [
          {
            "text": "Fine-tuning",
            "bold": true
          },
          {
            "text": " (where still offered), "
          },
          {
            "text": "Assistants/Responses API",
            "bold": true
          },
          {
            "text": ", "
          },
          {
            "text": "Realtime API",
            "bold": true
          },
          {
            "text": ", and other provider-specific product surfaces are direct-API-only."
          }
        ]
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Enterprise agreements & negotiated pricing.",
            "bold": true
          },
          {
            "text": " At high volume, providers will negotiate custom rates, dedicated capacity, and priority service tiers that aggregators can't always match or pass through."
          }
        ],
        [
          {
            "text": "Simpler compliance story.",
            "bold": true
          },
          {
            "text": " Your data crosses one administrative boundary (you → provider) instead of two (you → aggregator → provider). For regulated data, this can matter."
          }
        ],
        [
          {
            "text": "Direct support relationship.",
            "bold": true
          },
          {
            "text": " Enterprise-tier direct accounts get dedicated support channels and (where offered) contractual SLA terms straight from the provider."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "3.3 Cons of Direct API Keys",
      "id": "3-3-cons-of-direct-api-keys"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Fragmented management.",
            "bold": true
          },
          {
            "text": " A separate API key, environment variable, billing dashboard, and rate-limit pool for every provider you use. This gets tedious fast once you're using 2–3+ providers."
          }
        ],
        [
          {
            "text": "No built-in failover.",
            "bold": true
          },
          {
            "text": " If Anthropic has an outage or rate-limits you mid-traffic, your application needs its own retry/backoff/failover logic, or it simply fails. You have to build and maintain this yourself."
          }
        ],
        [
          {
            "text": "Model-switching requires code changes.",
            "bold": true
          },
          {
            "text": " Swapping from one model/provider to another means touching your integration code, not just a config string."
          }
        ],
        [
          {
            "text": "No unified cost visibility.",
            "bold": true
          },
          {
            "text": " If you're using multiple providers, you're reconciling spend across multiple dashboards and billing formats — there's no single place to see \"total AI spend this month across everything.\""
          }
        ]
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "4. Current Direct Pricing Snapshot (Anthropic & OpenAI)",
      "id": "4-current-direct-pricing-snapshot-anthropic-openai"
    },
    {
      "type": "callout",
      "variant": "warning",
      "spans": [
        {
          "text": "Pricing changes frequently.",
          "bold": true
        },
        {
          "text": " These figures were verified against official pricing pages as of early July 2026. Always check current rates at claude.com/pricing and platform.openai.com before budgeting."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.1 Anthropic (Claude) — Direct API Pricing",
      "id": "4-1-anthropic-claude-direct-api-pricing"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Model"
            }
          ],
          [
            {
              "text": "Input $/MTok"
            }
          ],
          [
            {
              "text": "Output $/MTok"
            }
          ],
          [
            {
              "text": "Notes"
            }
          ]
        ],
        [
          [
            {
              "text": "Claude Haiku 4.5"
            }
          ],
          [
            {
              "text": "$1.00"
            }
          ],
          [
            {
              "text": "$5.00"
            }
          ],
          [
            {
              "text": "Cheapest current-gen model; fast, high-volume workloads"
            }
          ]
        ],
        [
          [
            {
              "text": "Claude Sonnet 5"
            }
          ],
          [
            {
              "text": "$2.00 (intro, through Aug 31 2026) → $3.00 standard"
            }
          ],
          [
            {
              "text": "$10.00 (intro) → $15.00 standard"
            }
          ],
          [
            {
              "text": "Best price-to-performance; 1M context at flat rate"
            }
          ]
        ],
        [
          [
            {
              "text": "Claude Opus 4.8"
            }
          ],
          [
            {
              "text": "$5.00"
            }
          ],
          [
            {
              "text": "$25.00"
            }
          ],
          [
            {
              "text": "Flagship; adaptive thinking, effort controls; Fast Mode at $10/$50"
            }
          ]
        ],
        [
          [
            {
              "text": "Claude Fable 5 / Mythos 5"
            }
          ],
          [
            {
              "text": "$10.00"
            }
          ],
          [
            {
              "text": "$50.00"
            }
          ],
          [
            {
              "text": "Mythos-tier; Fable 5 has additional safety classifiers for bio/cyber/LLM R&D"
            }
          ]
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Cost levers on Anthropic:",
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
            "text": "Prompt caching:",
            "bold": true
          },
          {
            "text": " cache hits cost 0.1x base input price (90% off). Cache writes cost 1.25x base input for a 5-minute TTL, or 2x for a 1-hour TTL."
          }
        ],
        [
          {
            "text": "Batch API:",
            "bold": true
          },
          {
            "text": " flat 50% off both input and output, for asynchronous jobs completed within 24 hours."
          }
        ],
        [
          {
            "text": "US-only inference:",
            "bold": true
          },
          {
            "text": " applying "
          },
          {
            "text": "inference_geo: \"us\"",
            "code": true
          },
          {
            "text": " on Opus/Sonnet-tier models adds a 1.1x pricing multiplier."
          }
        ],
        [
          {
            "text": "Stacking caching + batch can push effective costs to a small fraction of headline rates for well-architected, high-repeat-context pipelines — highly relevant for a document-heavy pipeline like Double Raven's, where the same system prompt/schema instructions get reused across many documents."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.2 OpenAI (GPT) — Direct API Pricing",
      "id": "4-2-openai-gpt-direct-api-pricing"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Model"
            }
          ],
          [
            {
              "text": "Input $/MTok"
            }
          ],
          [
            {
              "text": "Output $/MTok"
            }
          ],
          [
            {
              "text": "Notes"
            }
          ]
        ],
        [
          [
            {
              "text": "GPT-5.4 nano-tier"
            }
          ],
          [
            {
              "text": "~$0.20"
            }
          ],
          [
            {
              "text": "~$1.25"
            }
          ],
          [
            {
              "text": "Cheapest tier; classification, extraction"
            }
          ]
        ],
        [
          [
            {
              "text": "GPT-5.4 mini"
            }
          ],
          [
            {
              "text": "$0.75"
            }
          ],
          [
            {
              "text": "$4.50"
            }
          ],
          [
            {
              "text": "General production tasks"
            }
          ]
        ],
        [
          [
            {
              "text": "GPT-5.2"
            }
          ],
          [
            {
              "text": "$1.75"
            }
          ],
          [
            {
              "text": "$14.00"
            }
          ],
          [
            {
              "text": "Mid-tier reasoning"
            }
          ]
        ],
        [
          [
            {
              "text": "GPT-5.4"
            }
          ],
          [
            {
              "text": "$2.50"
            }
          ],
          [
            {
              "text": "$15.00"
            }
          ],
          [
            {
              "text": "Lower-cost frontier option"
            }
          ]
        ],
        [
          [
            {
              "text": "GPT-5.5"
            }
          ],
          [
            {
              "text": "$5.00"
            }
          ],
          [
            {
              "text": "$30.00"
            }
          ],
          [
            {
              "text": "Current flagship; 1M+ context"
            }
          ]
        ],
        [
          [
            {
              "text": "GPT-5.5 Pro"
            }
          ],
          [
            {
              "text": "$30.00"
            }
          ],
          [
            {
              "text": "$180.00"
            }
          ],
          [
            {
              "text": "Highest-capability tier"
            }
          ]
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Cost levers on OpenAI:",
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
            "text": "Cached input:",
            "bold": true
          },
          {
            "text": " automatic, no code changes required; typically ~90% off on newer model families."
          }
        ],
        [
          {
            "text": "Batch API:",
            "bold": true
          },
          {
            "text": " flat 50% off, 24-hour turnaround."
          }
        ],
        [
          {
            "text": "Flex processing:",
            "bold": true
          },
          {
            "text": " also 50% off, with variable (sometimes faster than Batch) latency."
          }
        ],
        [
          {
            "text": "Priority processing:",
            "bold": true
          },
          {
            "text": " a premium tier that costs more (roughly 2.5x standard) in exchange for guaranteed low latency."
          }
        ],
        [
          {
            "text": "Data residency / regional processing:",
            "bold": true
          },
          {
            "text": " adds a 10% uplift for models released after March 5, 2026."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.3 Takeaway",
      "id": "4-3-takeaway"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Direct pricing between the two labs is broadly comparable at each capability tier, with Anthropic's Haiku/Sonnet/Opus ladder being simpler to reason about (output is consistently 5x input) and OpenAI's ladder having more tiers and more variable output-to-input ratios. "
        },
        {
          "text": "Neither is a clear universal winner",
          "bold": true
        },
        {
          "text": " — the right choice depends on task fit, and this is exactly the kind of decision an aggregator like OpenRouter makes easy to test empirically without separate integrations."
        }
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "5. OpenRouter — Comprehensive Deep Dive",
      "id": "5-openrouter-comprehensive-deep-dive"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.1 What OpenRouter Actually Is",
      "id": "5-1-what-openrouter-actually-is"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "OpenRouter is "
        },
        {
          "text": "not",
          "bold": true
        },
        {
          "text": " a model — it's a routing and access layer. Founded in 2023 by Alex Atallah (former CTO/co-founder of OpenSea), it has grown into the default aggregation layer for developers who want to access many models without separately integrating each provider's API. By mid-2026, OpenRouter reports roughly 8 million users and approximately 100 trillion tokens processed per month, with annualized inference spend through the platform growing from $10M (Oct 2024) to over $100M (May 2025)."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Strip away the marketing, and OpenRouter is three things combined:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "A unified API.",
            "bold": true
          },
          {
            "text": " One endpoint ("
          },
          {
            "text": "https://openrouter.ai/api/v1",
            "code": true
          },
          {
            "text": "), OpenAI-compatible request/response format. You select any participating provider's model via a single model string. Existing OpenAI SDK code becomes drop-in compatible just by swapping the "
          },
          {
            "text": "base_url",
            "code": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "A routing layer.",
            "bold": true
          },
          {
            "text": " When the provider behind your chosen model has an outage or hits a rate limit, OpenRouter automatically falls back to another provider serving the same model, without your application ever seeing the failure."
          }
        ],
        [
          {
            "text": "A billing/analytics layer.",
            "bold": true
          },
          {
            "text": " One credit balance, one activity dashboard, cost and token tracking across every model and provider you use."
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "OpenRouter currently offers access to "
        },
        {
          "text": "400+ models from 60–70+ providers",
          "bold": true
        },
        {
          "text": " (OpenAI, Anthropic, Google, Meta, Mistral, DeepSeek, and dozens of open-weight hosts), all through a single OpenAI-compatible endpoint and a single API key (which starts with the prefix "
        },
        {
          "text": "sk-or-",
          "code": true
        },
        {
          "text": ", distinguishing it from a raw OpenAI key)."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.2 The Two-Layer Routing Model",
      "id": "5-2-the-two-layer-routing-model"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This is the conceptual core of how OpenRouter works, and it's worth understanding precisely because it explains almost every configuration option OpenRouter exposes. "
        },
        {
          "text": "Routing happens on two independent layers:",
          "bold": true
        }
      ]
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "Model routing",
            "bold": true
          },
          {
            "text": " — "
          },
          {
            "text": "which model",
            "italic": true
          },
          {
            "text": " answers your request (e.g., did you ask for Claude Sonnet, or did you let OpenRouter's Auto Router pick for you?)."
          }
        ],
        [
          {
            "text": "Provider routing",
            "bold": true
          },
          {
            "text": " — "
          },
          {
            "text": "which infrastructure/provider",
            "italic": true
          },
          {
            "text": " actually serves that model (e.g., if you asked for Claude Sonnet, does the request go to Anthropic directly, or to Amazon Bedrock, or to Google Vertex AI, all of which can serve the same underlying Claude model)?"
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This second layer is the one most people don't realize exists, and it's the answer to your specific question about \"choosing what infra it runs on.\""
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.3 How Provider/Infra Selection Actually Works",
      "id": "5-3-how-provider-infra-selection-actually-works"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Many models on OpenRouter — especially widely-used ones — are served by "
        },
        {
          "text": "multiple providers simultaneously",
          "bold": true
        },
        {
          "text": ". For example, Claude models can be served via Anthropic directly, Amazon Bedrock, or Google Vertex AI. An open-weight model like Llama or DeepSeek might be hosted by half a dozen different inference companies (Together AI, Fireworks, DeepInfra, Groq, etc.), each at a different price and speed."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Default behavior (no configuration needed):",
          "bold": true
        },
        {
          "text": " When you don't specify a provider preference, OpenRouter runs a 3-step load-balancing strategy on every request:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "Prioritize stability.",
            "bold": true
          },
          {
            "text": " Providers that have had significant outages in the last 30 seconds are deprioritized (not removed — just pushed to the back of the line)."
          }
        ],
        [
          {
            "text": "Weight by price.",
            "bold": true
          },
          {
            "text": " Among the stable providers, OpenRouter picks one weighted by the "
          },
          {
            "text": "inverse square",
            "italic": true
          },
          {
            "text": " of price — meaning cheaper providers are favored disproportionately, not just slightly."
          }
        ],
        [
          {
            "text": "Everything else becomes fallback.",
            "bold": true
          },
          {
            "text": " If your chosen provider fails mid-request, OpenRouter automatically retries against the next-best provider in the list."
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Worked example from OpenRouter's own documentation:",
          "bold": true
        },
        {
          "text": " if a model is served by Provider A ($1/M tokens), Provider B ($2/M), and Provider C ($3/M), and Provider B has had recent outages, OpenRouter's default logic favors Provider A heavily (due to price-weighting), keeps Provider C as a stable fallback, and pushes Provider B toward the back of the queue due to its instability — all without you writing any routing logic."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Manual overrides — the ",
          "bold": true
        },
        {
          "text": "provider",
          "bold": true,
          "code": true
        },
        {
          "text": " object:",
          "bold": true
        },
        {
          "text": " You can attach a "
        },
        {
          "text": "provider",
          "code": true
        },
        {
          "text": " object to any request to take direct control:"
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
              "text": "Field"
            }
          ],
          [
            {
              "text": "What It Does"
            }
          ]
        ],
        [
          [
            {
              "text": "order",
              "code": true
            }
          ],
          [
            {
              "text": "A prioritized list of providers to try, in order (e.g., force Anthropic first, Bedrock second)"
            }
          ]
        ],
        [
          [
            {
              "text": "only",
              "code": true
            }
          ],
          [
            {
              "text": "Restrict routing to "
            },
            {
              "text": "only",
              "italic": true
            },
            {
              "text": " the listed providers — nothing else will ever serve the request"
            }
          ]
        ],
        [
          [
            {
              "text": "ignore",
              "code": true
            }
          ],
          [
            {
              "text": "Exclude specific providers entirely"
            }
          ]
        ],
        [
          [
            {
              "text": "sort",
              "code": true
            }
          ],
          [
            {
              "text": "Override default price-weighting; e.g., "
            },
            {
              "text": "sort: \"throughput\"",
              "code": true
            },
            {
              "text": " to prioritize the fastest provider instead"
            }
          ]
        ],
        [
          [
            {
              "text": "max_price",
              "code": true
            }
          ],
          [
            {
              "text": "A hard price ceiling — the request will not run at all if no provider meets it (unlike latency/throughput preferences, which are best-effort)"
            }
          ]
        ],
        [
          [
            {
              "text": "allow_fallbacks",
              "code": true
            }
          ],
          [
            {
              "text": "Set to "
            },
            {
              "text": "false",
              "code": true
            },
            {
              "text": " to guarantee the request is served "
            },
            {
              "text": "only",
              "italic": true
            },
            {
              "text": " by your top-priority provider, or fails outright"
            }
          ]
        ],
        [
          [
            {
              "text": "data_collection",
              "code": true
            }
          ],
          [
            {
              "text": "Set to "
            },
            {
              "text": "\"deny\"",
              "code": true
            },
            {
              "text": " to exclude any provider that stores or trains on your inputs"
            }
          ]
        ],
        [
          [
            {
              "text": "zdr",
              "code": true
            }
          ],
          [
            {
              "text": "Set to "
            },
            {
              "text": "true",
              "code": true
            },
            {
              "text": " to restrict routing to Zero Data Retention endpoints only"
            }
          ]
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Shorthand \"variant\" suffixes",
          "bold": true
        },
        {
          "text": " on the model string give quick access to common routing preferences without building a full "
        },
        {
          "text": "provider",
          "code": true
        },
        {
          "text": " object:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": ":nitro",
            "code": true
          },
          {
            "text": " — routes to the fastest available provider for that model"
          }
        ],
        [
          {
            "text": ":floor",
            "code": true
          },
          {
            "text": " — forces the absolute cheapest provider, bypassing the default inverse-square weighting"
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Special case — tool-calling requests:",
          "bold": true
        },
        {
          "text": " Standard requests use the price-weighted default described above, but requests that include tool/function definitions route through a separate mechanism called "
        },
        {
          "text": "Auto Exacto",
          "bold": true
        },
        {
          "text": ", which tiers providers by tool-call "
        },
        {
          "text": "quality",
          "italic": true
        },
        {
          "text": " signals first (since a cheap provider that fumbles tool calls isn't actually cheaper once you account for retries), and only applies price-ordering within each quality tier."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.4 Model Selection Methods",
      "id": "5-4-model-selection-methods"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "OpenRouter gives you several distinct ways to choose "
        },
        {
          "text": "which model",
          "italic": true
        },
        {
          "text": " answers a request:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "Manual / explicit.",
            "bold": true
          },
          {
            "text": " You specify the exact model string (e.g., "
          },
          {
            "text": "anthropic/claude-sonnet-4.6",
            "code": true
          },
          {
            "text": "). Full control, fully deterministic, easiest to reproduce and audit."
          }
        ],
        [
          {
            "text": "Auto Router.",
            "bold": true
          },
          {
            "text": " You let OpenRouter's routing layer pick the model itself, based on the content and complexity of your request. This trades control for convenience — useful for prototyping, less useful when you need reproducibility, since the model actually used can vary between otherwise-identical calls. OpenRouter does return which model was used with every response, so you retain visibility even when you don't retain control."
          }
        ],
        [
          {
            "text": "Fallback arrays.",
            "bold": true
          },
          {
            "text": " You specify a primary model plus one or more backup models. If the primary fails or errors, OpenRouter tries the next one in the list automatically."
          }
        ],
        [
          {
            "text": "Presets.",
            "bold": true
          },
          {
            "text": " A named, server-side configuration bundling a fallback model array, provider rules (e.g., "
          },
          {
            "text": "zdr: true",
            "code": true
          },
          {
            "text": "), request parameters, and even a system prompt — all under one slug (e.g., "
          },
          {
            "text": "@preset/customer-support",
            "code": true
          },
          {
            "text": "). This means a routing/compliance decision is made once, centrally, and every service or team member referencing that preset picks up the change automatically without a code redeploy. Presets are versioned, so a bad change can be rolled back to a previous version."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.5 Pricing & Fee Structure",
      "id": "5-5-pricing-fee-structure"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This is the part people most often get wrong, because the marketing headline (\"no markup on inference\") is true but incomplete. OpenRouter's billing has "
        },
        {
          "text": "three distinct layers",
          "bold": true
        },
        {
          "text": ":"
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Layer 1 — Token/inference pass-through (genuinely no markup).",
          "bold": true
        },
        {
          "text": " OpenRouter charges you the "
        },
        {
          "text": "exact",
          "italic": true
        },
        {
          "text": " per-token rate the underlying provider charges. If Claude Sonnet costs $3/$15 per MTok directly from Anthropic, it costs the same $3/$15 on OpenRouter. There is no per-token markup layered on top of provider pricing — this has been true since OpenRouter simplified its fee structure in mid-2025 (previously it added a small per-token markup)."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Layer 2 — Credit purchase fee (this is where OpenRouter makes money).",
          "bold": true
        },
        {
          "text": " When you add credits to your account (the standard way to pay), OpenRouter charges a "
        },
        {
          "text": "5.5% fee on credit card purchases",
          "bold": true
        },
        {
          "text": " (5.0% flat on cryptocurrency payments), with a "
        },
        {
          "text": "$0.80 minimum fee per transaction",
          "bold": true
        },
        {
          "text": ". This minimum matters more than it looks: a $5 top-up effectively pays a 16% fee; a $10 top-up pays 8%; the fee doesn't normalize down to the advertised 5.5% until you're topping up roughly $15+ at a time. "
        },
        {
          "text": "Practical implication: load credits in larger, less-frequent amounts rather than many small top-ups.",
          "bold": true
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Layer 3 — BYOK fee (only if you bring your own provider keys).",
          "bold": true
        },
        {
          "text": " Covered in detail in Section 5.9 below — 5% of the equivalent OpenRouter-rate cost, after a free monthly allowance."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Pricing Tiers:",
          "bold": true
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
              "text": "Tier"
            }
          ],
          [
            {
              "text": "What You Get"
            }
          ],
          [
            {
              "text": "Cost"
            }
          ]
        ],
        [
          [
            {
              "text": "Free",
              "bold": true
            }
          ],
          [
            {
              "text": "25+ free models, 20 requests/minute, 50 free-model requests/day (rising to 1,000/day once you've purchased $10+ in lifetime credits)"
            }
          ],
          [
            {
              "text": "$0"
            }
          ]
        ],
        [
          [
            {
              "text": "Pay-as-you-go",
              "bold": true
            }
          ],
          [
            {
              "text": "Full 400+ model catalog, no minimums, no lock-in, no monthly fee"
            }
          ],
          [
            {
              "text": "Pass-through token pricing + 5.5% credit purchase fee"
            }
          ]
        ],
        [
          [
            {
              "text": "Enterprise",
              "bold": true
            }
          ],
          [
            {
              "text": "SSO/SAML, negotiated/lower platform fee (reportedly 4–5% range, negotiated), contractual SLA terms, invoicing/PO support, dedicated support, EU in-region routing, BYOC (apply existing AWS/GCP/Azure credits)"
            }
          ],
          [
            {
              "text": "Custom, volume-based"
            }
          ]
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Other billing notes:",
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
            "text": "No monthly fees, no minimum spend, no lock-in on Pay-as-you-go."
          }
        ],
        [
          {
            "text": "Minimum credit purchase is $5."
          }
        ],
        [
          {
            "text": "Credits are generally non-refundable after a short window (roughly 24 hours) following purchase; the platform fee portion is never refundable."
          }
        ],
        [
          {
            "text": "Purchased credits may expire after 12 months of account inactivity; promotional/free credits can expire much sooner (as short as 30 days)."
          }
        ],
        [
          {
            "text": "If routing/fallback is enabled and your first-choice provider fails, you are billed only for the successful model run — not for failed attempts (with one documented edge case: if a failing provider had already billed partial tokens before erroring out, that partial charge can still appear, though OpenRouter states this affects well under 0.5% of typical monthly spend)."
          }
        ],
        [
          {
            "text": "OpenRouter does not currently offer standard volume discounts on Pay-as-you-go; large-scale users are directed to contact sales for Enterprise terms."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.6 Cost Tracking & Token Tracking",
      "id": "5-6-cost-tracking-token-tracking"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This is one of OpenRouter's strongest practical features, and directly relevant to a solo-developer project where you want visibility without building your own observability stack."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Per-response usage accounting (built into every API call).",
          "bold": true
        },
        {
          "text": " Every response — streaming or not — includes a detailed "
        },
        {
          "text": "usage",
          "code": true
        },
        {
          "text": " object automatically, with no extra parameters required. It includes fields like "
        },
        {
          "text": "completion_tokens",
          "code": true
        },
        {
          "text": ", "
        },
        {
          "text": "prompt_tokens",
          "code": true
        },
        {
          "text": ", "
        },
        {
          "text": "total_tokens",
          "code": true
        },
        {
          "text": ", "
        },
        {
          "text": "cost",
          "code": true
        },
        {
          "text": ", "
        },
        {
          "text": "cached_tokens",
          "code": true
        },
        {
          "text": ", "
        },
        {
          "text": "cache_write_tokens",
          "code": true
        },
        {
          "text": ", and "
        },
        {
          "text": "reasoning_tokens",
          "code": true
        },
        {
          "text": " (for models with extended thinking)."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This gives you prompt/completion token counts (using each model's "
        },
        {
          "text": "own",
          "italic": true
        },
        {
          "text": " native tokenizer — important, since different models tokenize differently and a token isn't a token isn't a token across providers), cached-token counts, reasoning-token counts, and the actual dollar cost of that specific request — all without a separate API call. You can also retrieve this after the fact using the "
        },
        {
          "text": "generation_id",
          "code": true
        },
        {
          "text": " returned with each response, useful for async auditing."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The Activity page (dashboard).",
          "bold": true
        },
        {
          "text": " A real-time usage dashboard in the OpenRouter web UI. You can filter by time period (1 hour / 1 day / 1 month / 1 year) and group by "
        },
        {
          "text": "Model",
          "bold": true
        },
        {
          "text": ", "
        },
        {
          "text": "API Key",
          "bold": true
        },
        {
          "text": ", or "
        },
        {
          "text": "Creator",
          "bold": true
        },
        {
          "text": " (organization member). This is where you'd go to answer \"what did I spend last month\" or \"which model is burning the most credits.\""
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Activity Export.",
          "bold": true
        },
        {
          "text": " Export aggregated usage data as "
        },
        {
          "text": "CSV or PDF",
          "bold": true
        },
        {
          "text": " directly from the Activity page, pre-filtered and grouped the same way as the dashboard view. Useful for moving spend data into a spreadsheet or expense-tracking workflow."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Analytics API (beta).",
          "bold": true
        },
        {
          "text": " A more powerful, queryable API for cost analysis — requires a separate "
        },
        {
          "text": "Management Key",
          "bold": true
        },
        {
          "text": " (not your regular inference key; regular keys get a 403 if you try). Lets you build custom recipes like \"which models are burning the most,\" \"which API keys are driving cost for a given model,\" or \"did my prompt-caching hit rate change month over month.\" OpenRouter has also published a reusable \"analytics\" skill designed to be handed to a coding agent (like Claude Code) so the agent can run a full cost review of your account autonomously."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Credits API.",
          "bold": true
        },
        {
          "text": " A lightweight endpoint for checking your live account balance and remaining credits programmatically — useful for building your own low-balance alerts."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Practical recommendation for Double Raven:",
          "bold": true
        },
        {
          "text": " Set a "
        },
        {
          "text": "budget alert / spending limit per API key",
          "bold": true
        },
        {
          "text": " (create separate keys per environment — dev vs. prod — or per feature area, e.g., one key for OCR pipeline calls, one for NLP/entity resolution calls). This gives you Activity-page cost breakdowns by feature area for free, without any custom instrumentation."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.7 Privacy & Data Retention",
      "id": "5-7-privacy-data-retention"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This deserves careful attention given Double Raven's private, access-controlled nature."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The two-hop reality.",
          "bold": true
        },
        {
          "text": " Every OpenRouter request crosses "
        },
        {
          "text": "two administrative boundaries",
          "bold": true
        },
        {
          "text": " instead of one: your client → OpenRouter ("
        },
        {
          "text": "api.openrouter.ai",
          "code": true
        },
        {
          "text": "), then OpenRouter → the downstream provider you selected (or that was auto-selected for you). OpenRouter sees your prompt in order to route and log usage; the downstream provider sees it in order to generate the response. "
        },
        {
          "text": "The effective retention/training guarantee you get is the union of both parties' policies",
          "bold": true
        },
        {
          "text": " — you're only as protected as the weaker of the two."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "OpenRouter's own default policy.",
          "bold": true
        },
        {
          "text": " OpenRouter states it does not train any foundation models on your data, and does not log or store your prompts/completions by default — prompt logging is opt-in, not opt-on-by-default. This applies to what "
        },
        {
          "text": "OpenRouter itself",
          "italic": true
        },
        {
          "text": " does with your data; it does not change what the "
        },
        {
          "text": "downstream provider",
          "italic": true
        },
        {
          "text": " does with it once forwarded."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Account-wide and per-request data policy controls.",
          "bold": true
        },
        {
          "text": " In your OpenRouter privacy settings (or per-request via the "
        },
        {
          "text": "provider",
          "code": true
        },
        {
          "text": " object):"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "data_collection: \"deny\"",
            "code": true
          },
          {
            "text": " — excludes any provider from routing that stores inputs for training purposes."
          }
        ],
        [
          {
            "text": "zdr: true",
            "code": true
          },
          {
            "text": " — restricts routing to endpoints with a "
          },
          {
            "text": "Zero Data Retention",
            "bold": true
          },
          {
            "text": " policy specifically (a stronger, more specific guarantee than just \"doesn't train on it\" — ZDR means the provider doesn't retain the prompt/completion at all beyond the request). Whether ZDR is actually available depends on both your OpenRouter setting "
          },
          {
            "text": "and",
            "italic": true
          },
          {
            "text": " the selected downstream provider supporting ZDR on that specific model/endpoint — not every provider offers it for every model. As one concrete example: one of Anthropic's newest model tiers is documented as "
          },
          {
            "text": "not",
            "bold": true
          },
          {
            "text": " supporting zero data retention, so a "
          },
          {
            "text": "zdr: true",
            "code": true
          },
          {
            "text": " rule will correctly skip that model/provider combination and fall through to the next qualifying option in your fallback list."
          }
        ],
        [
          {
            "text": "These settings can be set "
          },
          {
            "text": "account-wide",
            "bold": true
          },
          {
            "text": " (so every request obeys them by default) or "
          },
          {
            "text": "per-request",
            "bold": true
          },
          {
            "text": " (for finer-grained control if some workloads need ZDR and others don't)."
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Geographic / data residency controls.",
          "bold": true
        },
        {
          "text": " For Enterprise accounts, OpenRouter offers "
        },
        {
          "text": "EU in-region routing",
          "bold": true
        },
        {
          "text": ": requests are decrypted and processed entirely within the EU via a dedicated endpoint ("
        },
        {
          "text": "eu.openrouter.ai",
          "code": true
        },
        {
          "text": "), routing only to EU-eligible providers. For non-Enterprise accounts, you can still manually restrict routing to EU-headquartered providers (e.g., Mistral) combined with "
        },
        {
          "text": "data_collection: deny",
          "code": true
        },
        {
          "text": ", though OpenRouter notes that independent confirmation of a provider's actual data-center location should come from that provider directly, not from OpenRouter."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Failure mode when compliance rules can't be satisfied.",
          "bold": true
        },
        {
          "text": " If you set "
        },
        {
          "text": "allow_fallbacks: false",
          "code": true
        },
        {
          "text": " alongside your data-policy restrictions, and no provider in your allowed list can satisfy the request, OpenRouter returns an error rather than silently routing to a non-compliant provider. This is the correct behavior for regulated workloads — fail loudly rather than fail privately."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Practical read for Double Raven:",
          "bold": true
        },
        {
          "text": " Given that Double Raven handles private, two-user partner data, the recommended default is to set "
        },
        {
          "text": "data_collection: \"deny\"",
          "code": true
        },
        {
          "text": " and "
        },
        {
          "text": "zdr: true",
          "code": true
        },
        {
          "text": " as "
        },
        {
          "text": "account-wide defaults",
          "bold": true
        },
        {
          "text": " in OpenRouter's privacy settings from day one, and treat any model/provider that gets excluded by those rules as simply unavailable for this project — rather than loosening the policy to gain access to a specific model. This costs you nothing in typical usage (most frontier providers support ZDR) and closes the two-hop exposure gap by default rather than by remembering to set it per-request."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.8 Reliability & Failover",
      "id": "5-8-reliability-failover"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The practical value proposition, in plain terms: if Anthropic rate-limits you or has a brief outage mid-traffic, a direct integration returns an error and your application (or your AI agent, if it's mid-task) has to handle that failure itself. Through OpenRouter, that same failure is invisible — the routing layer detects the failed provider and automatically retries against the next one serving the same model, and your application just receives a normal successful completion. For a "
        },
        {
          "text": "multi-step agentic pipeline",
          "bold": true
        },
        {
          "text": " (e.g., an ingest-document-then-OCR-then-extract-entities pipeline), this matters more than it would for a single one-off request, because a failure partway through a multi-call chain can leave the pipeline in an inconsistent state that's much harder to recover from than a single retried call."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "OpenRouter's own reported operating scale (roughly 100 trillion tokens/month as of mid-2026) means this routing logic runs continuously against real production traffic across 70+ providers — it's a mature, battle-tested layer rather than an edge-case feature."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.9 BYOK — Bring Your Own Key",
      "id": "5-9-byok-bring-your-own-key"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "For cases where you already have (or want) a direct relationship with a specific provider — e.g., to preserve provider-side volume discounts, or to keep that provider as your formal data processor for compliance reasons — OpenRouter supports "
        },
        {
          "text": "BYOK",
          "bold": true
        },
        {
          "text": ": you supply your own Anthropic/OpenAI/etc. API key, and OpenRouter routes through it while still giving you the unified interface, unified activity dashboard, and fallback logic."
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Your provider keys are encrypted at rest."
          }
        ],
        [
          {
            "text": "The first 1,000,000 BYOK requests per month are free",
            "bold": true
          },
          {
            "text": " (5,000,000/month on Enterprise)."
          }
        ],
        [
          {
            "text": "Beyond that threshold, OpenRouter charges "
          },
          {
            "text": "5% of what the same model/provider call would have cost on standard OpenRouter credits",
            "bold": true
          },
          {
            "text": ", deducted from your OpenRouter credit balance."
          }
        ],
        [
          {
            "text": "You can configure priority/fallback behavior per key — e.g., prefer your own OpenAI key, but fall back to OpenRouter's pooled Anthropic access if you don't have your own Anthropic key configured."
          }
        ],
        [
          {
            "text": "BYOK keys are automatically prioritized in routing when configured, and a "
          },
          {
            "text": "partition: \"none\"",
            "code": true
          },
          {
            "text": " setting lets that prioritization work "
          },
          {
            "text": "across",
            "italic": true
          },
          {
            "text": " model boundaries (e.g., routing to a different model entirely if that's the one you have a BYOK key for), not just across providers of the same model."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.10 Getting Started (Practical Steps)",
      "id": "5-10-getting-started-practical-steps"
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "Create an account at openrouter.ai (no credit card required to start with free models)."
          }
        ],
        [
          {
            "text": "Go to "
          },
          {
            "text": "Settings → Keys",
            "bold": true
          },
          {
            "text": " and create an API key (starts with "
          },
          {
            "text": "sk-or-...",
            "code": true
          },
          {
            "text": "). Recommended: create separate keys per project/environment so you get per-key cost breakdowns for free."
          }
        ],
        [
          {
            "text": "Set your "
          },
          {
            "text": "privacy settings",
            "bold": true
          },
          {
            "text": " ("
          },
          {
            "text": "data_collection: deny",
            "code": true
          },
          {
            "text": ", "
          },
          {
            "text": "zdr: true",
            "code": true
          },
          {
            "text": " if desired) account-wide before sending any real traffic."
          }
        ],
        [
          {
            "text": "Add credits via credit card, crypto (USDC), or bank transfer (Enterprise supports invoicing/PO)."
          }
        ],
        [
          {
            "text": "Point your existing OpenAI-SDK-based code at "
          },
          {
            "text": "base_url: \"https://openrouter.ai/api/v1\"",
            "code": true
          },
          {
            "text": " with your new key — this is a two-line change if you're already using an OpenAI-compatible client."
          }
        ],
        [
          {
            "text": "Pick a model string in "
          },
          {
            "text": "provider/model",
            "code": true
          },
          {
            "text": " format from the catalog at openrouter.ai/models (e.g., "
          },
          {
            "text": "anthropic/claude-sonnet-4.6",
            "code": true
          },
          {
            "text": ")."
          }
        ],
        [
          {
            "text": "Monitor spend via the "
          },
          {
            "text": "Activity",
            "bold": true
          },
          {
            "text": " page as you go, and set per-key limits before scaling up usage."
          }
        ]
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "6. Head-to-Head: Direct API Keys vs. OpenRouter",
      "id": "6-head-to-head-direct-api-keys-vs-openrouter"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Factor"
            }
          ],
          [
            {
              "text": "Direct API Keys"
            }
          ],
          [
            {
              "text": "OpenRouter"
            }
          ]
        ],
        [
          [
            {
              "text": "Setup complexity",
              "bold": true
            }
          ],
          [
            {
              "text": "Separate integration per provider"
            }
          ],
          [
            {
              "text": "One integration, OpenAI-compatible"
            }
          ]
        ],
        [
          [
            {
              "text": "Model switching",
              "bold": true
            }
          ],
          [
            {
              "text": "Requires code changes"
            }
          ],
          [
            {
              "text": "Change a model string"
            }
          ]
        ],
        [
          [
            {
              "text": "Latency overhead",
              "bold": true
            }
          ],
          [
            {
              "text": "None (baseline)"
            }
          ],
          [
            {
              "text": "+50–150ms typically (network hop + routing layer)"
            }
          ]
        ],
        [
          [
            {
              "text": "Failover on outage",
              "bold": true
            }
          ],
          [
            {
              "text": "You build it yourself"
            }
          ],
          [
            {
              "text": "Automatic, built-in"
            }
          ]
        ],
        [
          [
            {
              "text": "Pricing on inference",
              "bold": true
            }
          ],
          [
            {
              "text": "Provider's list price"
            }
          ],
          [
            {
              "text": "Same list price, no per-token markup"
            }
          ]
        ],
        [
          [
            {
              "text": "Extra fees",
              "bold": true
            }
          ],
          [
            {
              "text": "None (beyond provider's own rates)"
            }
          ],
          [
            {
              "text": "5.5% fee on credit purchases (or 5% on BYOK usage beyond free tier)"
            }
          ]
        ],
        [
          [
            {
              "text": "Provider-specific features (prompt caching, batch, fine-tuning)",
              "bold": true
            }
          ],
          [
            {
              "text": "Full access, day one"
            }
          ],
          [
            {
              "text": "Mostly supported, but can lag; caching benefits depend on sticky routing to one provider"
            }
          ]
        ],
        [
          [
            {
              "text": "New model availability",
              "bold": true
            }
          ],
          [
            {
              "text": "Immediate"
            }
          ],
          [
            {
              "text": "Usually within days, not always day one"
            }
          ]
        ],
        [
          [
            {
              "text": "Unified billing/cost visibility",
              "bold": true
            }
          ],
          [
            {
              "text": "No — per-provider dashboards"
            }
          ],
          [
            {
              "text": "Yes — one Activity dashboard across all models/providers"
            }
          ]
        ],
        [
          [
            {
              "text": "Data exposure",
              "bold": true
            }
          ],
          [
            {
              "text": "One administrative hop (you → provider)"
            }
          ],
          [
            {
              "text": "Two hops (you → OpenRouter → provider), mitigated by ZDR settings"
            }
          ]
        ],
        [
          [
            {
              "text": "Enterprise agreements / negotiated rates",
              "bold": true
            }
          ],
          [
            {
              "text": "Directly available at scale"
            }
          ],
          [
            {
              "text": "Only via OpenRouter's own Enterprise tier"
            }
          ]
        ],
        [
          [
            {
              "text": "Best for",
              "bold": true
            }
          ],
          [
            {
              "text": "Single-provider production systems, latency-critical apps, workloads needing bleeding-edge or provider-exclusive features"
            }
          ],
          [
            {
              "text": "Multi-model systems, rapid prototyping, resilience-critical pipelines, cost experimentation"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "6.1 What the Evidence Actually Shows on Cost",
      "id": "6-1-what-the-evidence-actually-shows-on-cost"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "A detailed cost study on running Claude Opus through OpenRouter found that, for a single-provider-dominant model like Opus (where OpenRouter routes nearly all traffic to one underlying provider, e.g., Amazon Bedrock, rather than genuinely shopping across competing providers), "
        },
        {
          "text": "the token price itself was identical to going direct",
          "bold": true
        },
        {
          "text": " — the \"shop around for the best price\" benefit didn't materialize because there wasn't real price competition behind that specific model. The money that "
        },
        {
          "text": "was",
          "italic": true
        },
        {
          "text": " left on the table came from "
        },
        {
          "text": "prompt-caching hit rate",
          "bold": true
        },
        {
          "text": ", not from provider markup: in that study, roughly 61% of prompt tokens across a real agentic workflow were billed at full price because consecutive calls weren't structured to land cache hits reliably — a problem that exists on both direct and routed integrations equally, since cache hit rate is determined by how you structure your calls, not by who bills you."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Practical takeaway:",
          "bold": true
        },
        {
          "text": " the biggest cost lever for Double Raven's AI pipeline won't be \"direct vs. OpenRouter\" — it will be "
        },
        {
          "text": "prompt caching discipline",
          "bold": true
        },
        {
          "text": " (structuring calls so repeated context, like system prompts and schema instructions, actually lands cache hits) and "
        },
        {
          "text": "model selection",
          "bold": true
        },
        {
          "text": " (routing routine, well-defined tasks to cheap models and reserving frontier models for genuinely hard reasoning). Both of these matter identically regardless of which path you choose."
        }
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "7. Other Notable Aggregators & Alternatives",
      "id": "7-other-notable-aggregators-alternatives"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "For completeness, OpenRouter is the most established option but not the only one:"
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
              "text": "Option"
            }
          ],
          [
            {
              "text": "What It Is"
            }
          ],
          [
            {
              "text": "Notable For"
            }
          ]
        ],
        [
          [
            {
              "text": "Vercel AI Gateway",
              "bold": true
            }
          ],
          [
            {
              "text": "A competing aggregator, similar model to OpenRouter"
            }
          ],
          [
            {
              "text": "Recently expanded Zero Data Retention coverage to OpenAI, Anthropic, and Google; charges per-request for team-wide ZDR enforcement ($0.10/1,000 requests) rather than folding it into a credit fee"
            }
          ]
        ],
        [
          [
            {
              "text": "LiteLLM",
              "bold": true
            }
          ],
          [
            {
              "text": "An open-source, "
            },
            {
              "text": "self-hosted",
              "bold": true
            },
            {
              "text": " routing library rather than a managed service"
            }
          ],
          [
            {
              "text": "You run the routing logic on your own infrastructure; more control, more operational overhead. Natural \"graduate to\" option if OpenRouter's managed model ever becomes limiting"
            }
          ]
        ],
        [
          [
            {
              "text": "ofox.ai",
              "bold": true
            }
          ],
          [
            {
              "text": "A discount-focused aggregator"
            }
          ],
          [
            {
              "text": "Publishes per-model pricing with automatic volume-tier discounts (3–7% off at $1K/$5K/$10K/$20K monthly spend tiers); no credit-purchase fee, billed on actual usage instead"
            }
          ]
        ],
        [
          [
            {
              "text": "Together AI, Fireworks, Groq, DeepInfra",
              "bold": true
            }
          ],
          [
            {
              "text": "Specialized inference providers"
            }
          ],
          [
            {
              "text": "These are some of the "
            },
            {
              "text": "underlying providers",
              "italic": true
            },
            {
              "text": " that OpenRouter itself routes to for open-weight models (Llama, DeepSeek, Qwen, etc.) — you can also use them directly if you specifically want one open-weight model at maximum speed/cost-efficiency"
            }
          ]
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "For a solo project like Double Raven, OpenRouter remains the most practical default: broadest model catalog, most mature tooling (Activity dashboard, Analytics API, presets), and an ecosystem large enough that documentation and community troubleshooting are easy to find."
        }
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "8. Recommended Strategy for Double Raven",
      "id": "8-recommended-strategy-for-double-raven"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "Phase 1 — Start with OpenRouter as the Default Router",
      "id": "phase-1-start-with-openrouter-as-the-default-router"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Sign up, set "
          },
          {
            "text": "data_collection: deny",
            "code": true
          },
          {
            "text": " and "
          },
          {
            "text": "zdr: true",
            "code": true
          },
          {
            "text": " as account-wide privacy defaults immediately."
          }
        ],
        [
          {
            "text": "Use separate API keys per feature area (OCR pipeline, NLP/entity resolution, communication/feedback workspace features) so cost breakdowns fall out of the Activity dashboard for free."
          }
        ],
        [
          {
            "text": "Start with manual model selection (not Auto Router) for anything production-facing, so behavior stays reproducible and auditable — log the "
          },
          {
            "text": "generation_id",
            "code": true
          },
          {
            "text": " and model actually used with every call for traceability."
          }
        ],
        [
          {
            "text": "Build a small internal escalation pattern: cheap/fast model first (e.g., Haiku-tier) for routine extraction and classification tasks, escalate to a frontier model (Sonnet/Opus-tier) only for genuinely ambiguous entity resolution or cross-document reasoning."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "Phase 2 — Add Direct Keys Selectively, Not Wholesale",
      "id": "phase-2-add-direct-keys-selectively-not-wholesale"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "If a specific workload becomes high-volume and dominated by a single provider (e.g., heavy reliance on Claude for entity resolution), evaluate a "
          },
          {
            "text": "direct Anthropic key with aggressive prompt caching",
            "bold": true
          },
          {
            "text": " for that specific workload — that's where the real savings live, not in avoiding OpenRouter's fee."
          }
        ],
        [
          {
            "text": "Keep OpenRouter as the fallback/experimentation layer even after adding direct keys — use BYOK to route your own Anthropic key "
          },
          {
            "text": "through",
            "italic": true
          },
          {
            "text": " OpenRouter, which gives you provider-side rate limits and caching plus OpenRouter's unified dashboard and failover, without paying the 5.5% credit fee on that traffic (BYOK usage is 5% only after 1M free requests/month)."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "Phase 3 — Reassess as the Pipeline Matures",
      "id": "phase-3-reassess-as-the-pipeline-matures"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Once document volumes and model choices stabilize (see the Backend & AI Infrastructure doc's Phase 1–3 rollout), revisit whether specific high-frequency workloads (e.g., embedding generation) are cheaper to run on owned GPU hardware entirely, with cloud/OpenRouter reserved for burst capacity and frontier reasoning tasks."
          }
        ]
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "9. Final Decision Matrix",
      "id": "9-final-decision-matrix"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Workload"
            }
          ],
          [
            {
              "text": "Recommended Path"
            }
          ]
        ],
        [
          [
            {
              "text": "Prototyping / testing new models"
            }
          ],
          [
            {
              "text": "OpenRouter (free tier or light pay-as-you-go)"
            }
          ]
        ],
        [
          [
            {
              "text": "Routine OCR/HTR post-processing, classification, extraction"
            }
          ],
          [
            {
              "text": "OpenRouter, routed to cheap/fast models (Haiku-tier or open-weight)"
            }
          ]
        ],
        [
          [
            {
              "text": "Complex entity resolution / cross-document reasoning"
            }
          ],
          [
            {
              "text": "OpenRouter routed to Sonnet/Opus-tier, or direct Anthropic + prompt caching once volume justifies it"
            }
          ]
        ],
        [
          [
            {
              "text": "High-frequency, cache-friendly repeated-context calls"
            }
          ],
          [
            {
              "text": "Direct Anthropic API (or OpenRouter BYOK with provider pinned) to guarantee cache hit consistency"
            }
          ]
        ],
        [
          [
            {
              "text": "Burst/overflow capacity"
            }
          ],
          [
            {
              "text": "OpenRouter (broadest fallback options)"
            }
          ]
        ],
        [
          [
            {
              "text": "Anything touching private Double Raven partner data"
            }
          ],
          [
            {
              "text": "OpenRouter with "
            },
            {
              "text": "zdr: true",
              "code": true
            },
            {
              "text": " • "
            },
            {
              "text": "data_collection: deny",
              "code": true
            },
            {
              "text": " enforced account-wide, or direct API with the provider's own ZDR/no-training terms confirmed"
            }
          ]
        ]
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "blockquote",
      "lines": [
        [
          {
            "text": "Document Status:",
            "bold": true
          },
          {
            "text": " Living document — revisit pricing figures and OpenRouter feature set periodically, as both provider pricing and OpenRouter's platform capabilities have historically changed every few months."
          }
        ],
        [
          {
            "text": "Next Steps:",
            "bold": true
          }
        ],
        [
          {
            "text": "1. Create OpenRouter account and configure account-wide privacy defaults"
          }
        ],
        [
          {
            "text": "2. Prototype the OCR/NLP pipeline routing logic against 2–3 candidate models via OpenRouter before committing to a default"
          }
        ],
        [
          {
            "text": "3. Instrument cost tracking early — separate API keys per feature area from day one"
          }
        ],
        [
          {
            "text": "4. Revisit this doc once real document volumes are known, to model actual monthly spend under both direct and OpenRouter paths"
          }
        ]
      ]
    }
  ]
}
$dr_doc$::jsonb,
  'mwintrow@creatv.io',
  'mwintrow@creatv.io'
);

INSERT INTO dr_document_revisions (document_id, revision_number, title, content_format, content, created_by)
SELECT id, 1, title, content_format, content, 'mwintrow@creatv.io'
FROM dr_documents WHERE slug = 'cloud-ai-model-access';

COMMIT;
