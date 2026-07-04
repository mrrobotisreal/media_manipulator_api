-- 20260704001_update_dr_doc_toc_links.up.sql
--
-- DR portal v2, Part A: make the seeded "Backend & AI Infrastructure" Table of
-- Contents entries clickable in-page anchor links. Only the ToC list block
-- changes; every other block is byte-for-byte identical to the v1 seed. The JSON
-- below is generated from (and MUST stay identical to)
-- media-manipulator-ui/content/dr-docs/backend-ai-infrastructure.ts.
--
-- Snapshots the post-update content into dr_document_revisions
-- (revision_number = current max + 1) so document history stays complete.

BEGIN;

UPDATE dr_documents
SET content = $dr_doc$
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
            "text": " Architecture Design Doc (ADD)"
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
            "text": " June 2026"
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
            "text": "Why Consumer Hardware Is Insufficient",
            "link": "#2-why-consumer-hardware-is-insufficient"
          }
        ],
        [
          {
            "text": "The Case for Dedicated GPU Infrastructure",
            "link": "#3-the-case-for-dedicated-gpu-infrastructure"
          }
        ],
        [
          {
            "text": "AI Pipeline Breakdown by Workload",
            "link": "#4-ai-pipeline-breakdown-by-workload"
          }
        ],
        [
          {
            "text": "Recommended On-Premises Hardware Specification",
            "link": "#5-recommended-on-premises-hardware-specification"
          }
        ],
        [
          {
            "text": "Total Cost of Ownership — On-Premises Build",
            "link": "#6-total-cost-of-ownership-on-premises-build"
          }
        ],
        [
          {
            "text": "Cloud Infrastructure Alternative (AWS + Anthropic APIs)",
            "link": "#7-cloud-infrastructure-alternative"
          }
        ],
        [
          {
            "text": "On-Premises vs. Cloud — Head-to-Head Comparison",
            "link": "#8-on-premises-vs-cloud-head-to-head-comparison"
          }
        ],
        [
          {
            "text": "Hybrid Architecture Strategy",
            "link": "#9-hybrid-architecture-strategy"
          }
        ],
        [
          {
            "text": "Final Recommendation",
            "link": "#10-final-recommendation"
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
          "text": "Double Raven requires a suite of AI-powered backend capabilities spanning "
        },
        {
          "text": "Optical Character Recognition (OCR)",
          "bold": true
        },
        {
          "text": ", "
        },
        {
          "text": "Handwriting Detection & Text Extraction (HTR)",
          "bold": true
        },
        {
          "text": ", "
        },
        {
          "text": "Natural Language Processing (NLP)",
          "bold": true
        },
        {
          "text": ", "
        },
        {
          "text": "Semantic Analysis",
          "bold": true
        },
        {
          "text": ", "
        },
        {
          "text": "Entity Resolution",
          "bold": true
        },
        {
          "text": ", "
        },
        {
          "text": "Identity Disambiguation",
          "bold": true
        },
        {
          "text": ", and more. These are not lightweight tasks — they involve running large neural network models (transformer-based, CNN-based, and ensemble architectures) against potentially high volumes of documents and data."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The core architectural question this document addresses is:"
        }
      ]
    },
    {
      "type": "blockquote",
      "lines": [
        [
          {
            "text": "Should Double Raven run AI inference workloads on owned, on-premises hardware — or should it offload those workloads to cloud providers (AWS GPU instances) and third-party AI APIs (Anthropic Claude, AWS Textract, etc.)?",
            "italic": true
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This document explains the technical justifications, hardware requirements, cost modeling, and trade-offs of both approaches, and concludes with a recommended strategy."
        }
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "2. Why Consumer Hardware Is Insufficient",
      "id": "2-why-consumer-hardware-is-insufficient"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.1 The Fundamental Mismatch: What AI Models Actually Need",
      "id": "2-1-the-fundamental-mismatch-what-ai-models-actually-need"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Modern AI models — even \"small\" production-ready ones — are fundamentally different in their computational profile compared to typical software. A Node.js web server, a Go microservice, or even a PostgreSQL database are largely "
        },
        {
          "text": "CPU-bound and I/O-bound",
          "bold": true
        },
        {
          "text": " workloads. They process work sequentially or with modest parallelism, and a modern laptop or desktop can handle them comfortably."
        }
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "AI inference (running a trained model to get a prediction or output) is a "
        },
        {
          "text": "massively parallel linear algebra problem",
          "bold": true
        },
        {
          "text": ". Every forward pass through a neural network involves thousands to millions of matrix multiplications happening simultaneously. Consumer hardware was never designed to do this at production scale."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.2 Why Your CPU Can't Keep Up",
      "id": "2-2-why-your-cpu-cant-keep-up"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "A high-end consumer CPU — say, an Intel Core i9-14900K or AMD Ryzen 9 9950X — has 24–32 cores. This sounds like a lot. But AI inference requires a fundamentally different kind of parallelism:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "A typical transformer model (e.g., a mid-size OCR or NLP model) performs "
          },
          {
            "text": "billions of floating-point operations per inference pass",
            "bold": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "CPU cores are optimized for complex, branchy, low-latency single-threaded work — not for the simple-but-massively-parallel matrix math AI requires."
          }
        ],
        [
          {
            "text": "Even with AVX-512 SIMD instructions, a top-of-the-line CPU might deliver 1–5 TFLOPS of FP32 performance."
          }
        ],
        [
          {
            "text": "An NVIDIA RTX 4090 GPU delivers ~82 TFLOPS FP32. An H100 delivers ~67 TFLOPS FP32 and ~1,979 TFLOPS with Tensor Core FP8."
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "That's a "
        },
        {
          "text": "16x to 400x performance gap",
          "bold": true
        },
        {
          "text": " just from the core compute unit. On a laptop or consumer desktop, you're adding thermal throttling, power limits, and shared memory bandwidth on top of that gap."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.3 The VRAM Problem",
      "id": "2-3-the-vram-problem"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "This is arguably the single biggest blocker for running AI models on consumer hardware:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Most production-quality AI models require their weights to live "
          },
          {
            "text": "entirely in GPU VRAM",
            "bold": true
          },
          {
            "text": " during inference for acceptable performance."
          }
        ],
        [
          {
            "text": "A state-of-the-art OCR model (e.g., Surya, TrOCR-Large, Nougat) requires "
          },
          {
            "text": "4–12 GB of VRAM",
            "bold": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "A capable NLP model (e.g., fine-tuned LLaMA 3 8B at FP16) requires "
          },
          {
            "text": "~16 GB of VRAM",
            "bold": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "A serious entity resolution + identity disambiguation pipeline running multiple models simultaneously requires "
          },
          {
            "text": "24–80+ GB of VRAM",
            "bold": true
          },
          {
            "text": " in aggregate."
          }
        ],
        [
          {
            "text": "Consumer GPUs: RTX 4090 = "
          },
          {
            "text": "24 GB VRAM",
            "bold": true
          },
          {
            "text": ". RTX 4080 = "
          },
          {
            "text": "16 GB VRAM",
            "bold": true
          },
          {
            "text": ". A laptop GPU (RTX 4070 Mobile) = "
          },
          {
            "text": "8 GB VRAM",
            "bold": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "When a model doesn't fit in VRAM, it falls back to system RAM — and inference speed can drop by "
          },
          {
            "text": "10x–50x",
            "bold": true
          },
          {
            "text": ", making real-time or near-real-time processing completely infeasible."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.4 Thermal and Power Constraints",
      "id": "2-4-thermal-and-power-constraints"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Consumer laptops operate under strict TDP (thermal design power) limits:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "A laptop CPU typically runs at "
          },
          {
            "text": "28–55W TDP",
            "bold": true
          },
          {
            "text": " under sustained load."
          }
        ],
        [
          {
            "text": "A desktop \"high performance\" CPU might hit "
          },
          {
            "text": "125–253W TDP",
            "bold": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "An NVIDIA H100 SXM5 GPU alone has a "
          },
          {
            "text": "700W TDP",
            "bold": true
          },
          {
            "text": ". An RTX Pro 5000 has a "
          },
          {
            "text": "250W TDP",
            "bold": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "Sustained AI inference workloads — especially document pipelines that may run for hours — will cause consumer hardware to "
          },
          {
            "text": "thermally throttle within minutes",
            "bold": true
          },
          {
            "text": ", degrading performance by 30–60% and risking hardware instability."
          }
        ],
        [
          {
            "text": "Consumer hardware is not designed for 24/7 sustained high-utilization workloads. Server hardware is."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.5 Memory Bandwidth Bottleneck",
      "id": "2-5-memory-bandwidth-bottleneck"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Even if a consumer CPU could perform the raw math, memory bandwidth becomes the ceiling:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "AI inference is often "
          },
          {
            "text": "memory-bandwidth-bound",
            "bold": true
          },
          {
            "text": ", not just compute-bound. Moving model weights and activations between memory and compute units is frequently the limiting step."
          }
        ],
        [
          {
            "text": "Consumer DDR5 RAM has ~50–90 GB/s bandwidth per channel."
          }
        ],
        [
          {
            "text": "An NVIDIA H100 GPU's HBM3 memory delivers "
          },
          {
            "text": "~3.35 TB/s",
            "bold": true
          },
          {
            "text": " of memory bandwidth — over "
          },
          {
            "text": "37x faster",
            "bold": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "An RTX Pro 5000's GDDR7 memory delivers "
          },
          {
            "text": "~576 GB/s",
            "bold": true
          },
          {
            "text": "."
          }
        ],
        [
          {
            "text": "Running large models on CPU system RAM introduces a crippling bottleneck that no amount of faster CPUs can fix."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "2.6 No Path to Scale",
      "id": "2-6-no-path-to-scale"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Beyond raw performance, consumer hardware fails on operational requirements:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "No ECC RAM",
            "bold": true
          },
          {
            "text": " — Consumer RAM does not have Error Correcting Code memory. Silent bit-flip errors can corrupt model outputs or crash long-running inference jobs without any warning."
          }
        ],
        [
          {
            "text": "No hot-swap storage",
            "bold": true
          },
          {
            "text": " — A failed NVMe or drive on a desktop means downtime. Servers support hot-swap bays and RAID."
          }
        ],
        [
          {
            "text": "No redundant power supplies",
            "bold": true
          },
          {
            "text": " — A power supply failure on a consumer machine brings everything down."
          }
        ],
        [
          {
            "text": "No remote management (IPMI/BMC)",
            "bold": true
          },
          {
            "text": " — Server hardware includes out-of-band management controllers (like iDRAC or iLO) so you can reboot, diagnose, or reinstall OS without physical access."
          }
        ],
        [
          {
            "text": "Consumer parts are not rated for sustained 24/7 operation",
            "bold": true
          },
          {
            "text": " — Warranties reflect this: consumer desktop parts typically carry 1–3 year limited warranties vs. server hardware at 3–5 years with pro support SLAs."
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
      "text": "3. The Case for Dedicated GPU Infrastructure",
      "id": "3-the-case-for-dedicated-gpu-infrastructure"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "3.1 What a GPU Actually Is (and Why It Matters)",
      "id": "3-1-what-a-gpu-actually-is-and-why-it-matters"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "A GPU (Graphics Processing Unit) was originally designed to render thousands of pixels simultaneously — a massively parallel task. It turns out this same architecture is perfect for AI inference:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "GPUs contain thousands of small, simple processing cores (CUDA cores on NVIDIA) designed to execute the same operation on many data points simultaneously."
          }
        ],
        [
          {
            "text": "NVIDIA H100: "
          },
          {
            "text": "16,896 CUDA cores",
            "bold": true
          },
          {
            "text": " + "
          },
          {
            "text": "528 Tensor Cores",
            "bold": true
          }
        ],
        [
          {
            "text": "NVIDIA RTX Pro 5000: "
          },
          {
            "text": "4,096 CUDA cores",
            "bold": true
          },
          {
            "text": " + "
          },
          {
            "text": "128 Tensor Cores",
            "bold": true
          }
        ],
        [
          {
            "text": "Tensor Cores are specialized hardware units that can perform entire matrix multiplications in a single clock cycle — the fundamental operation in neural networks."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "3.2 The CUDA Ecosystem Lock-In (And Why It's Worth It)",
      "id": "3-2-the-cuda-ecosystem-lock-in-and-why-its-worth-it"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Virtually the entire modern AI/ML stack is built on "
        },
        {
          "text": "NVIDIA CUDA",
          "bold": true
        },
        {
          "text": " — NVIDIA's proprietary GPU programming platform:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "PyTorch, TensorFlow, JAX, Hugging Face Transformers, ONNX Runtime, TensorRT — all are built around CUDA as the primary accelerated compute target."
          }
        ],
        [
          {
            "text": "AMD ROCm is a competing alternative, but library support, model compatibility, and community tooling significantly lag behind CUDA."
          }
        ],
        [
          {
            "text": "Intel Gaudi and other accelerators exist, but ecosystem maturity is far behind NVIDIA for production inference workloads."
          }
        ],
        [
          {
            "text": "Conclusion:",
            "bold": true
          },
          {
            "text": " For a production AI backend today, NVIDIA GPUs with CUDA are the pragmatic choice, even accounting for higher cost."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "3.3 Multi-GPU and NVLink Considerations",
      "id": "3-3-multi-gpu-and-nvlink-considerations"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "For running multiple large models simultaneously (e.g., OCR + NLP + entity resolution all at once, or serving many concurrent requests):"
        }
      ]
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Multiple GPUs can be used in a single server, either independently (separate models on each GPU) or cooperatively via "
          },
          {
            "text": "NVLink",
            "bold": true
          },
          {
            "text": " (models too large for one GPU split across multiple)."
          }
        ],
        [
          {
            "text": "The RTX Pro 5000 does not support NVLink — each GPU runs independently. This is fine for running separate model pipelines concurrently."
          }
        ],
        [
          {
            "text": "For truly massive models requiring NVLink, you'd need the H100 or A100 tier — but that's likely overkill for Double Raven's scope."
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
      "text": "4. AI Pipeline Breakdown by Workload",
      "id": "4-ai-pipeline-breakdown-by-workload"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.1 OCR — Optical Character Recognition",
      "id": "4-1-ocr-optical-character-recognition"
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
              "text": "Detail"
            }
          ]
        ],
        [
          [
            {
              "text": "Representative Models"
            }
          ],
          [
            {
              "text": "Surya, PaddleOCR, EasyOCR, Tesseract (CPU-only, legacy), TrOCR"
            }
          ]
        ],
        [
          [
            {
              "text": "VRAM Required"
            }
          ],
          [
            {
              "text": "4–12 GB (model-dependent)"
            }
          ]
        ],
        [
          [
            {
              "text": "GPU Benefit"
            }
          ],
          [
            {
              "text": "10–50x faster than CPU inference for CNN/transformer OCR models"
            }
          ]
        ],
        [
          [
            {
              "text": "Throughput Need"
            }
          ],
          [
            {
              "text": "For document pipelines: 10–100+ pages/minute"
            }
          ]
        ],
        [
          [
            {
              "text": "CPU-Only Feasibility"
            }
          ],
          [
            {
              "text": "Possible for <1 page/minute; completely impractical for production volumes"
            }
          ]
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "OCR on modern documents (PDFs, scanned images) uses deep learning models that process images as tensors through multiple CNN and transformer layers. Running this on a consumer CPU means processing one page every 5–30 seconds. With a GPU, you can batch-process dozens of pages per second."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.2 HTR — Handwriting Text Recognition",
      "id": "4-2-htr-handwriting-text-recognition"
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
              "text": "Detail"
            }
          ]
        ],
        [
          [
            {
              "text": "Representative Models"
            }
          ],
          [
            {
              "text": "TrOCR-large-handwritten, Flor, custom fine-tuned Vision Transformers"
            }
          ]
        ],
        [
          [
            {
              "text": "VRAM Required"
            }
          ],
          [
            {
              "text": "8–16 GB"
            }
          ]
        ],
        [
          [
            {
              "text": "GPU Benefit"
            }
          ],
          [
            {
              "text": "Critical — HTR models are significantly larger and more complex than print OCR"
            }
          ]
        ],
        [
          [
            {
              "text": "Throughput Need"
            }
          ],
          [
            {
              "text": "Highly variable; handwritten documents are often complex and require more processing"
            }
          ]
        ],
        [
          [
            {
              "text": "CPU-Only Feasibility"
            }
          ],
          [
            {
              "text": "No — latency becomes 30–120 seconds per page; completely unusable for any real pipeline"
            }
          ]
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Handwriting recognition is harder than print OCR by an order of magnitude. The models are larger, the pre-processing pipelines are more intensive (deskewing, binarization, line segmentation), and confidence scoring requires multiple inference passes. GPU inference is not optional here — it's a prerequisite."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.3 NLP — Natural Language Processing",
      "id": "4-3-nlp-natural-language-processing"
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
              "text": "Detail"
            }
          ]
        ],
        [
          [
            {
              "text": "Representative Models"
            }
          ],
          [
            {
              "text": "spaCy (transformer pipelines), fine-tuned BERT/RoBERTa, LLaMA 3 8B for complex analysis"
            }
          ]
        ],
        [
          [
            {
              "text": "VRAM Required"
            }
          ],
          [
            {
              "text": "2–16 GB depending on model size and batch size"
            }
          ]
        ],
        [
          [
            {
              "text": "GPU Benefit"
            }
          ],
          [
            {
              "text": "Transformers scale exceptionally well on GPU; BERT-large inference is ~40x faster on GPU"
            }
          ]
        ],
        [
          [
            {
              "text": "Throughput Need"
            }
          ],
          [
            {
              "text": "Depends on document volume; for near-real-time NLP, GPU is required"
            }
          ]
        ],
        [
          [
            {
              "text": "CPU-Only Feasibility"
            }
          ],
          [
            {
              "text": "Small spaCy models are CPU-viable; transformer-based NLP at scale requires GPU"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.4 Entity Extraction & Identity Resolution",
      "id": "4-4-entity-extraction-identity-resolution"
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
              "text": "Detail"
            }
          ]
        ],
        [
          [
            {
              "text": "Representative Models"
            }
          ],
          [
            {
              "text": "Custom NER (Named Entity Recognition) models, GLiNER, fine-tuned BERT-NER"
            }
          ]
        ],
        [
          [
            {
              "text": "VRAM Required"
            }
          ],
          [
            {
              "text": "4–8 GB per model"
            }
          ]
        ],
        [
          [
            {
              "text": "GPU Benefit"
            }
          ],
          [
            {
              "text": "High — running NER across large document corpora requires batched GPU inference"
            }
          ]
        ],
        [
          [
            {
              "text": "Unique Challenge"
            }
          ],
          [
            {
              "text": "Identity resolution also involves "
            },
            {
              "text": "graph algorithms",
              "bold": true
            },
            {
              "text": " and "
            },
            {
              "text": "fuzzy matching",
              "bold": true
            },
            {
              "text": " — compute-intensive even without deep learning"
            }
          ]
        ],
        [
          [
            {
              "text": "CPU-Only Feasibility"
            }
          ],
          [
            {
              "text": "Marginal for small document sets; GPU required for production throughput"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.5 Semantic Analysis & Vector Embeddings",
      "id": "4-5-semantic-analysis-vector-embeddings"
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
              "text": "Detail"
            }
          ]
        ],
        [
          [
            {
              "text": "Representative Models"
            }
          ],
          [
            {
              "text": "sentence-transformers (all-MiniLM, all-mpnet), E5-Large, OpenAI-compatible embedding models"
            }
          ]
        ],
        [
          [
            {
              "text": "VRAM Required"
            }
          ],
          [
            {
              "text": "1–4 GB per model"
            }
          ]
        ],
        [
          [
            {
              "text": "GPU Benefit"
            }
          ],
          [
            {
              "text": "Embedding generation throughput: ~200 sentences/second on CPU vs. ~10,000/second on GPU"
            }
          ]
        ],
        [
          [
            {
              "text": "Throughput Need"
            }
          ],
          [
            {
              "text": "High — embeddings are generated for every document, chunk, and entity"
            }
          ]
        ],
        [
          [
            {
              "text": "CPU-Only Feasibility"
            }
          ],
          [
            {
              "text": "Technically possible but ~50x slower; unacceptable for production"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "4.6 Summary: Why All of These Together Demand a GPU Server",
      "id": "4-6-summary-why-all-of-these-together-demand-a-gpu-server"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "In a real Double Raven backend pipeline, these workloads don't run sequentially one-at-a-time — they run "
        },
        {
          "text": "concurrently",
          "bold": true
        },
        {
          "text": ". A document ingestion event might simultaneously trigger:"
        }
      ]
    },
    {
      "type": "list",
      "ordered": true,
      "items": [
        [
          {
            "text": "OCR pipeline (GPU 1 or GPU partition)"
          }
        ],
        [
          {
            "text": "HTR pipeline (GPU 1 or GPU partition)"
          }
        ],
        [
          {
            "text": "NLP preprocessing + entity extraction (GPU 2)"
          }
        ],
        [
          {
            "text": "Embedding generation for semantic indexing (GPU 2)"
          }
        ],
        [
          {
            "text": "Identity resolution graph computation (CPU-bound, multi-core)"
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The VRAM footprint of this concurrent pipeline alone is "
        },
        {
          "text": "20–40 GB VRAM minimum",
          "bold": true
        },
        {
          "text": ", exceeding any single consumer GPU. The CPU cores needed for the non-GPU workloads (identity resolution, data transformation, API serving) require a high-core-count server CPU — not a consumer desktop chip."
        }
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "5. Recommended On-Premises Hardware Specification",
      "id": "5-recommended-on-premises-hardware-specification"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The following is a recommended server build for running Double Raven's AI backend on-premises. All specs are chosen for professional/workstation-class reliability and performance sufficient for the described AI pipeline."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.1 CPU — Processor",
      "id": "5-1-cpu-processor"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Recommended Spec"
            }
          ]
        ],
        [
          [
            {
              "text": "Model",
              "bold": true
            }
          ],
          [
            {
              "text": "AMD Threadripper PRO 7965WX (or 7985WX for more headroom)"
            }
          ]
        ],
        [
          [
            {
              "text": "Cores / Threads",
              "bold": true
            }
          ],
          [
            {
              "text": "24C/48T (7965WX) or 64C/128T (7985WX)"
            }
          ]
        ],
        [
          [
            {
              "text": "Base / Boost Clock",
              "bold": true
            }
          ],
          [
            {
              "text": "4.2 GHz / 5.3 GHz"
            }
          ]
        ],
        [
          [
            {
              "text": "TDP",
              "bold": true
            }
          ],
          [
            {
              "text": "350W"
            }
          ]
        ],
        [
          [
            {
              "text": "PCIe Lanes",
              "bold": true
            }
          ],
          [
            {
              "text": "128 PCIe Gen 5 lanes — critical for multi-GPU and NVMe bandwidth"
            }
          ]
        ],
        [
          [
            {
              "text": "Platform",
              "bold": true
            }
          ],
          [
            {
              "text": "WRX90 (Threadripper PRO)"
            }
          ]
        ],
        [
          [
            {
              "text": "Why This CPU",
              "bold": true
            }
          ],
          [
            {
              "text": "Threadripper PRO gives you server-class PCIe lane counts, ECC memory support, RDIMM support, and high core counts for parallel CPU workloads like identity resolution and data orchestration. Consumer Ryzen has only 24–28 PCIe lanes, which is completely insufficient for multiple GPUs + NVMe drives running at full bandwidth."
            }
          ]
        ],
        [
          [
            {
              "text": "Estimated Cost",
              "bold": true
            }
          ],
          [
            {
              "text": "$2,500 – $5,500 (CPU only)"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.2 Motherboard",
      "id": "5-2-motherboard"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Recommended Spec"
            }
          ]
        ],
        [
          [
            {
              "text": "Model",
              "bold": true
            }
          ],
          [
            {
              "text": "ASUS Pro WS WRX90E-SAGE SE"
            }
          ]
        ],
        [
          [
            {
              "text": "Socket",
              "bold": true
            }
          ],
          [
            {
              "text": "sWRX9"
            }
          ]
        ],
        [
          [
            {
              "text": "Memory Slots",
              "bold": true
            }
          ],
          [
            {
              "text": "8x DDR5 RDIMM (up to 2 TB RAM)"
            }
          ]
        ],
        [
          [
            {
              "text": "PCIe Slots",
              "bold": true
            }
          ],
          [
            {
              "text": "7x PCIe 5.0 x16 slots"
            }
          ]
        ],
        [
          [
            {
              "text": "Networking",
              "bold": true
            }
          ],
          [
            {
              "text": "Dual 10GbE onboard"
            }
          ]
        ],
        [
          [
            {
              "text": "IPMI/BMC",
              "bold": true
            }
          ],
          [
            {
              "text": "Yes (AST2600 BMC for out-of-band management)"
            }
          ]
        ],
        [
          [
            {
              "text": "Estimated Cost",
              "bold": true
            }
          ],
          [
            {
              "text": "$1,500 – $2,000"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.3 RAM — System Memory",
      "id": "5-3-ram-system-memory"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Recommended Spec"
            }
          ]
        ],
        [
          [
            {
              "text": "Type",
              "bold": true
            }
          ],
          [
            {
              "text": "DDR5 RDIMM (Registered ECC)"
            }
          ]
        ],
        [
          [
            {
              "text": "Speed",
              "bold": true
            }
          ],
          [
            {
              "text": "DDR5-4800 or DDR5-5600"
            }
          ]
        ],
        [
          [
            {
              "text": "Capacity",
              "bold": true
            }
          ],
          [
            {
              "text": "192 GB minimum (6x 32 GB RDIMMs) — 384 GB recommended (6x 64 GB RDIMMs)"
            }
          ]
        ],
        [
          [
            {
              "text": "ECC",
              "bold": true
            }
          ],
          [
            {
              "text": "Required",
              "bold": true
            },
            {
              "text": " — ECC (Error Correcting Code) detects and corrects single-bit memory errors silently. Without ECC, a bit flip during a long AI inference job can produce corrupted outputs or crash processes with no warning. This is non-negotiable for production AI workloads."
            }
          ]
        ],
        [
          [
            {
              "text": "Why RDIMM",
              "bold": true
            }
          ],
          [
            {
              "text": "Registered DIMMs have a buffer chip that reduces electrical load on the memory controller, allowing more DIMMs per channel and higher capacity. Required for WRX90 platform."
            }
          ]
        ],
        [
          [
            {
              "text": "Estimated Cost",
              "bold": true
            }
          ],
          [
            {
              "text": "$800 – $2,500 (depending on capacity)"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.4 Primary Storage — NVMe SSD",
      "id": "5-4-primary-storage-nvme-ssd"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Recommended Spec"
            }
          ]
        ],
        [
          [
            {
              "text": "Type",
              "bold": true
            }
          ],
          [
            {
              "text": "PCIe Gen 5.0 x4 NVMe M.2"
            }
          ]
        ],
        [
          [
            {
              "text": "Model Options",
              "bold": true
            }
          ],
          [
            {
              "text": "Samsung 990 Pro (PCIe 4), Crucial T705, Seagate FireCuda 540 (Gen 5)"
            }
          ]
        ],
        [
          [
            {
              "text": "Capacity",
              "bold": true
            }
          ],
          [
            {
              "text": "4 TB (OS + active models + hot data)"
            }
          ]
        ],
        [
          [
            {
              "text": "Sequential Read",
              "bold": true
            }
          ],
          [
            {
              "text": "12,000–14,000 MB/s (Gen 5)"
            }
          ]
        ],
        [
          [
            {
              "text": "Why Gen 5 NVMe",
              "bold": true
            }
          ],
          [
            {
              "text": "AI models are loaded from disk into GPU VRAM at startup and when swapping models. Gen 5 NVMe dramatically reduces model cold-start time — loading a 10 GB model takes ~1 second on Gen 5 NVMe vs. ~10–15 seconds on SATA SSD."
            }
          ]
        ],
        [
          [
            {
              "text": "Estimated Cost",
              "bold": true
            }
          ],
          [
            {
              "text": "$400 – $700 per drive"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.5 Bulk Storage — HDD",
      "id": "5-5-bulk-storage-hdd"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Recommended Spec"
            }
          ]
        ],
        [
          [
            {
              "text": "Type",
              "bold": true
            }
          ],
          [
            {
              "text": "3.5\" 7200RPM enterprise SATA HDD"
            }
          ]
        ],
        [
          [
            {
              "text": "Model Options",
              "bold": true
            }
          ],
          [
            {
              "text": "Seagate Exos X20, WD Gold"
            }
          ]
        ],
        [
          [
            {
              "text": "Capacity",
              "bold": true
            }
          ],
          [
            {
              "text": "4x 20 TB = 80 TB raw (configure as RAID 6 for ~60 TB usable)"
            }
          ]
        ],
        [
          [
            {
              "text": "Purpose",
              "bold": true
            }
          ],
          [
            {
              "text": "Raw document storage, archive data, training data sets, database backups, audit logs"
            }
          ]
        ],
        [
          [
            {
              "text": "Estimated Cost",
              "bold": true
            }
          ],
          [
            {
              "text": "$350 – $500 per drive"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.6 GPU(s) — The Core AI Accelerator",
      "id": "5-6-gpus-the-core-ai-accelerator"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Recommended Spec"
            }
          ]
        ],
        [
          [
            {
              "text": "Model",
              "bold": true
            }
          ],
          [
            {
              "text": "NVIDIA RTX Pro 5000 (Blackwell Architecture)"
            }
          ]
        ],
        [
          [
            {
              "text": "VRAM",
              "bold": true
            }
          ],
          [
            {
              "text": "48 GB GDDR7"
            }
          ]
        ],
        [
          [
            {
              "text": "Memory Bandwidth",
              "bold": true
            }
          ],
          [
            {
              "text": "~576 GB/s"
            }
          ]
        ],
        [
          [
            {
              "text": "CUDA Cores",
              "bold": true
            }
          ],
          [
            {
              "text": "4,096"
            }
          ]
        ],
        [
          [
            {
              "text": "Tensor Cores",
              "bold": true
            }
          ],
          [
            {
              "text": "128 (5th Gen)"
            }
          ]
        ],
        [
          [
            {
              "text": "TDP",
              "bold": true
            }
          ],
          [
            {
              "text": "250W"
            }
          ]
        ],
        [
          [
            {
              "text": "Quantity",
              "bold": true
            }
          ],
          [
            {
              "text": "2x minimum (96 GB VRAM total) — 4x recommended (192 GB VRAM total)"
            }
          ]
        ],
        [
          [
            {
              "text": "Form Factor",
              "bold": true
            }
          ],
          [
            {
              "text": "Full-height PCIe (requires adequate PCIe lanes from Threadripper PRO)"
            }
          ]
        ],
        [
          [
            {
              "text": "Why This GPU",
              "bold": true
            }
          ],
          [
            {
              "text": "The RTX Pro 5000 is the professional/workstation Blackwell-generation card. 48 GB VRAM per card means you can load multiple large models simultaneously without swapping. Two cards give you 96 GB VRAM — enough to run OCR, HTR, NLP, and embedding models concurrently. Professional drivers offer better stability and longer support cycles than consumer GeForce drivers."
            }
          ]
        ],
        [
          [
            {
              "text": "Estimated Cost",
              "bold": true
            }
          ],
          [
            {
              "text": "$4,500 – $6,000 per GPU"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.7 Cooling, Power, Chassis",
      "id": "5-7-cooling-power-chassis"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Recommended Spec"
            }
          ],
          [
            {
              "text": "Est. Cost"
            }
          ]
        ],
        [
          [
            {
              "text": "Server Chassis",
              "bold": true
            }
          ],
          [
            {
              "text": "4U Rackmount (e.g., SuperMicro SYS-421GE-TNRT or similar WRX90-compatible)"
            }
          ],
          [
            {
              "text": "$800 – $1,500"
            }
          ]
        ],
        [
          [
            {
              "text": "PSU",
              "bold": true
            }
          ],
          [
            {
              "text": "Dual redundant 2000W 80+ Platinum PSU"
            }
          ],
          [
            {
              "text": "$400 – $800"
            }
          ]
        ],
        [
          [
            {
              "text": "CPU Cooler",
              "bold": true
            }
          ],
          [
            {
              "text": "High-performance tower cooler or custom liquid cooling loop"
            }
          ],
          [
            {
              "text": "$200 – $600"
            }
          ]
        ],
        [
          [
            {
              "text": "Case Fans",
              "bold": true
            }
          ],
          [
            {
              "text": "High-static-pressure 120mm or 140mm fans (8–12 required for airflow)"
            }
          ],
          [
            {
              "text": "$150 – $300"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "5.8 Networking",
      "id": "5-8-networking"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Recommended Spec"
            }
          ],
          [
            {
              "text": "Est. Cost"
            }
          ]
        ],
        [
          [
            {
              "text": "NIC",
              "bold": true
            }
          ],
          [
            {
              "text": "Dual 10GbE (onboard on WRX90 board)"
            }
          ],
          [
            {
              "text": "Included"
            }
          ]
        ],
        [
          [
            {
              "text": "Switch",
              "bold": true
            }
          ],
          [
            {
              "text": "10GbE managed switch (e.g., Ubiquiti USW-Pro-Aggregation or similar)"
            }
          ],
          [
            {
              "text": "$500 – $2,000"
            }
          ]
        ],
        [
          [
            {
              "text": "Internet Uplink",
              "bold": true
            }
          ],
          [
            {
              "text": "Minimum 1 Gbps symmetric business fiber for API access"
            }
          ],
          [
            {
              "text": "Ongoing OPEX"
            }
          ]
        ]
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "6. Total Cost of Ownership — On-Premises Build",
      "id": "6-total-cost-of-ownership-on-premises-build"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "6.1 Upfront Capital Expenditure (CapEx)",
      "id": "6-1-upfront-capital-expenditure-capex"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Component"
            }
          ],
          [
            {
              "text": "Quantity"
            }
          ],
          [
            {
              "text": "Unit Cost (Est.)"
            }
          ],
          [
            {
              "text": "Total (Est.)"
            }
          ]
        ],
        [
          [
            {
              "text": "AMD Threadripper PRO 7965WX"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$3,000"
            }
          ],
          [
            {
              "text": "$3,000"
            }
          ]
        ],
        [
          [
            {
              "text": "ASUS Pro WS WRX90E-SAGE SE Motherboard"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$1,700"
            }
          ],
          [
            {
              "text": "$1,700"
            }
          ]
        ],
        [
          [
            {
              "text": "192 GB DDR5 RDIMM ECC (6x 32 GB)"
            }
          ],
          [
            {
              "text": "1 kit"
            }
          ],
          [
            {
              "text": "$1,200"
            }
          ],
          [
            {
              "text": "$1,200"
            }
          ]
        ],
        [
          [
            {
              "text": "PCIe Gen 5 NVMe 4 TB SSD"
            }
          ],
          [
            {
              "text": "2"
            }
          ],
          [
            {
              "text": "$550"
            }
          ],
          [
            {
              "text": "$1,100"
            }
          ]
        ],
        [
          [
            {
              "text": "20 TB Enterprise HDD (Seagate Exos)"
            }
          ],
          [
            {
              "text": "4"
            }
          ],
          [
            {
              "text": "$450"
            }
          ],
          [
            {
              "text": "$1,800"
            }
          ]
        ],
        [
          [
            {
              "text": "NVIDIA RTX Pro 5000 48 GB GPU"
            }
          ],
          [
            {
              "text": "2"
            }
          ],
          [
            {
              "text": "$5,200"
            }
          ],
          [
            {
              "text": "$10,400"
            }
          ]
        ],
        [
          [
            {
              "text": "4U Server Chassis"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$1,200"
            }
          ],
          [
            {
              "text": "$1,200"
            }
          ]
        ],
        [
          [
            {
              "text": "Dual Redundant 2000W PSU"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$600"
            }
          ],
          [
            {
              "text": "$600"
            }
          ]
        ],
        [
          [
            {
              "text": "CPU Liquid Cooling"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$400"
            }
          ],
          [
            {
              "text": "$400"
            }
          ]
        ],
        [
          [
            {
              "text": "Case Fans + Misc Cables"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$250"
            }
          ],
          [
            {
              "text": "$250"
            }
          ]
        ],
        [
          [
            {
              "text": "10GbE Managed Switch"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$1,000"
            }
          ],
          [
            {
              "text": "$1,000"
            }
          ]
        ],
        [
          [
            {
              "text": "UPS (Uninterruptible Power Supply)"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$800"
            }
          ],
          [
            {
              "text": "$800"
            }
          ]
        ],
        [
          [
            {
              "text": "OS + Software Licenses (Ubuntu Server Pro)"
            }
          ],
          [
            {
              "text": "1"
            }
          ],
          [
            {
              "text": "$500"
            }
          ],
          [
            {
              "text": "$500"
            }
          ]
        ],
        [
          [
            {
              "text": "Total Estimated CapEx",
              "bold": true
            }
          ],
          [],
          [],
          [
            {
              "text": "~$23,950",
              "bold": true
            }
          ]
        ]
      ]
    },
    {
      "type": "callout",
      "variant": "warning",
      "spans": [
        {
          "text": "Note:",
          "bold": true
        },
        {
          "text": " Pricing is estimated as of mid-2026. RTX Pro 5000 pricing is particularly variable and dependent on availability. Actual costs may vary ±20%. A 4-GPU configuration would add ~$10,400 to bring VRAM to 192 GB total, raising CapEx to ~$34,350."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "6.2 Ongoing Operating Expenditure (OpEx)",
      "id": "6-2-ongoing-operating-expenditure-opex"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Cost Category"
            }
          ],
          [
            {
              "text": "Monthly Estimate"
            }
          ]
        ],
        [
          [
            {
              "text": "Electricity (2x GPUs at 250W + CPU/system at ~400W = ~900W average draw, 24/7)"
            }
          ],
          [
            {
              "text": "~$80 – $130/month"
            }
          ]
        ],
        [
          [
            {
              "text": "Internet uplink (business fiber, 1 Gbps symmetric)"
            }
          ],
          [
            {
              "text": "~$80 – $200/month"
            }
          ]
        ],
        [
          [
            {
              "text": "Hardware maintenance / replacement reserve (3% of CapEx annually)"
            }
          ],
          [
            {
              "text": "~$60/month"
            }
          ]
        ],
        [
          [
            {
              "text": "Backup storage / offsite backup service"
            }
          ],
          [
            {
              "text": "~$20 – $50/month"
            }
          ]
        ],
        [
          [
            {
              "text": "Total Monthly OpEx (approx.)",
              "bold": true
            }
          ],
          [
            {
              "text": "~$240 – $440/month",
              "bold": true
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "6.3 Time to Break-Even vs. Cloud",
      "id": "6-3-time-to-break-even-vs-cloud"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "At full utilization (GPU instances running 24/7), the equivalent cloud cost (AWS p3.2xlarge at ~$3.00/hr = ~$2,160/month, or p4d at ~$32/hr) would exceed the on-premises CapEx payback in "
        },
        {
          "text": "6–18 months",
          "bold": true
        },
        {
          "text": " depending on actual utilization. At lighter utilization (a few hours/day), cloud can be cheaper for a longer period."
        }
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "7. Cloud Infrastructure Alternative",
      "id": "7-cloud-infrastructure-alternative"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "7.1 AWS GPU Instances",
      "id": "7-1-aws-gpu-instances"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Instance Type"
            }
          ],
          [
            {
              "text": "GPU"
            }
          ],
          [
            {
              "text": "VRAM"
            }
          ],
          [
            {
              "text": "vCPUs"
            }
          ],
          [
            {
              "text": "RAM"
            }
          ],
          [
            {
              "text": "On-Demand $/hr"
            }
          ],
          [
            {
              "text": "Monthly (24/7)"
            }
          ]
        ],
        [
          [
            {
              "text": "g4dn.xlarge"
            }
          ],
          [
            {
              "text": "NVIDIA T4"
            }
          ],
          [
            {
              "text": "16 GB"
            }
          ],
          [
            {
              "text": "4"
            }
          ],
          [
            {
              "text": "16 GB"
            }
          ],
          [
            {
              "text": "~$0.53"
            }
          ],
          [
            {
              "text": "~$382"
            }
          ]
        ],
        [
          [
            {
              "text": "g5.2xlarge"
            }
          ],
          [
            {
              "text": "NVIDIA A10G"
            }
          ],
          [
            {
              "text": "24 GB"
            }
          ],
          [
            {
              "text": "8"
            }
          ],
          [
            {
              "text": "32 GB"
            }
          ],
          [
            {
              "text": "~$1.21"
            }
          ],
          [
            {
              "text": "~$871"
            }
          ]
        ],
        [
          [
            {
              "text": "p3.2xlarge"
            }
          ],
          [
            {
              "text": "NVIDIA V100"
            }
          ],
          [
            {
              "text": "16 GB"
            }
          ],
          [
            {
              "text": "8"
            }
          ],
          [
            {
              "text": "61 GB"
            }
          ],
          [
            {
              "text": "~$3.06"
            }
          ],
          [
            {
              "text": "~$2,203"
            }
          ]
        ],
        [
          [
            {
              "text": "p4d.24xlarge"
            }
          ],
          [
            {
              "text": "8x A100"
            }
          ],
          [
            {
              "text": "320 GB total"
            }
          ],
          [
            {
              "text": "96"
            }
          ],
          [
            {
              "text": "1.1 TB"
            }
          ],
          [
            {
              "text": "~$32.77"
            }
          ],
          [
            {
              "text": "~$23,595"
            }
          ]
        ],
        [
          [
            {
              "text": "p5.48xlarge"
            }
          ],
          [
            {
              "text": "8x H100"
            }
          ],
          [
            {
              "text": "640 GB total"
            }
          ],
          [
            {
              "text": "192"
            }
          ],
          [
            {
              "text": "2 TB"
            }
          ],
          [
            {
              "text": "~$98.32"
            }
          ],
          [
            {
              "text": "~$70,790"
            }
          ]
        ],
        [
          [
            {
              "text": "g6.2xlarge"
            }
          ],
          [
            {
              "text": "NVIDIA L4"
            }
          ],
          [
            {
              "text": "24 GB"
            }
          ],
          [
            {
              "text": "8"
            }
          ],
          [
            {
              "text": "32 GB"
            }
          ],
          [
            {
              "text": "~$0.97"
            }
          ],
          [
            {
              "text": "~$698"
            }
          ]
        ]
      ]
    },
    {
      "type": "callout",
      "variant": "info",
      "spans": [
        {
          "text": "AWS Reserved Instances (1-year, no upfront)",
          "bold": true
        },
        {
          "text": " offer ~30–40% savings vs. on-demand pricing. "
        },
        {
          "text": "Spot Instances",
          "bold": true
        },
        {
          "text": " for non-latency-critical batch workloads can reduce costs by up to 70–90% but carry interruption risk."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "7.2 Managed AI Services on AWS",
      "id": "7-2-managed-ai-services-on-aws"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Service"
            }
          ],
          [
            {
              "text": "Use Case"
            }
          ],
          [
            {
              "text": "Pricing Model"
            }
          ]
        ],
        [
          [
            {
              "text": "AWS Textract",
              "bold": true
            }
          ],
          [
            {
              "text": "OCR on printed documents and forms"
            }
          ],
          [
            {
              "text": "Per-page: $0.0015/page (detect text) – $0.065/page (forms/tables)"
            }
          ]
        ],
        [
          [
            {
              "text": "AWS Rekognition",
              "bold": true
            }
          ],
          [
            {
              "text": "Image analysis, some document capabilities"
            }
          ],
          [
            {
              "text": "Per-image or per-minute of video"
            }
          ]
        ],
        [
          [
            {
              "text": "Amazon Comprehend",
              "bold": true
            }
          ],
          [
            {
              "text": "NLP: entity detection, sentiment, syntax"
            }
          ],
          [
            {
              "text": "Per unit (100 characters = 1 unit), ~$0.0001/unit"
            }
          ]
        ],
        [
          [
            {
              "text": "Amazon Bedrock",
              "bold": true
            }
          ],
          [
            {
              "text": "Hosted LLM inference (Claude, Titan, etc.)"
            }
          ],
          [
            {
              "text": "Per token (input/output)"
            }
          ]
        ],
        [
          [
            {
              "text": "SageMaker Endpoints",
              "bold": true
            }
          ],
          [
            {
              "text": "Host custom models on managed GPU instances"
            }
          ],
          [
            {
              "text": "Per hour based on instance type"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "7.3 Anthropic / Claude APIs",
      "id": "7-3-anthropic-claude-apis"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The Anthropic API is particularly relevant for Double Raven for workloads that benefit from frontier-level language understanding — complex entity resolution, reasoning across documents, and high-quality text analysis where a smaller fine-tuned model may fall short."
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
              "text": "Best For"
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
              "text": "~$0.80"
            }
          ],
          [
            {
              "text": "~$4.00"
            }
          ],
          [
            {
              "text": "High-volume, fast, cost-efficient NLP tasks"
            }
          ]
        ],
        [
          [
            {
              "text": "Claude Sonnet 4.6"
            }
          ],
          [
            {
              "text": "~$3.00"
            }
          ],
          [
            {
              "text": "~$15.00"
            }
          ],
          [
            {
              "text": "Balanced: document analysis, entity resolution, summarization"
            }
          ]
        ],
        [
          [
            {
              "text": "Claude Opus 4.x"
            }
          ],
          [
            {
              "text": "~$15.00"
            }
          ],
          [
            {
              "text": "~$75.00"
            }
          ],
          [
            {
              "text": "Complex reasoning, long-context document synthesis"
            }
          ]
        ]
      ]
    },
    {
      "type": "blockquote",
      "lines": [
        [
          {
            "text": "Note:",
            "bold": true
          },
          {
            "text": " API pricing is subject to change. Always verify current pricing at "
          },
          {
            "text": "https://www.anthropic.com/pricing",
            "link": "https://www.anthropic.com/pricing"
          },
          {
            "text": ". Volume discounts and batch API pricing options are available."
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "When to use the Claude API vs. a self-hosted model:",
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
            "text": "Use Claude API when the task requires "
          },
          {
            "text": "frontier reasoning",
            "bold": true
          },
          {
            "text": " (complex entity disambiguation, cross-document inference, nuanced identity resolution)."
          }
        ],
        [
          {
            "text": "Use a self-hosted model when the task is "
          },
          {
            "text": "high-volume and well-defined",
            "bold": true
          },
          {
            "text": " (batch OCR post-processing, standard NER, embedding generation) — the per-token cost of Claude would be prohibitive at scale."
          }
        ],
        [
          {
            "text": "A "
          },
          {
            "text": "hybrid approach",
            "bold": true
          },
          {
            "text": " (see Section 9) is strongly recommended."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "7.4 Other Relevant Third-Party AI APIs",
      "id": "7-4-other-relevant-third-party-ai-apis"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Provider"
            }
          ],
          [
            {
              "text": "Service"
            }
          ],
          [
            {
              "text": "Relevant For"
            }
          ]
        ],
        [
          [
            {
              "text": "Google Cloud Vision AI"
            }
          ],
          [
            {
              "text": "OCR, document understanding"
            }
          ],
          [
            {
              "text": "Printed text OCR at scale"
            }
          ]
        ],
        [
          [
            {
              "text": "Azure Cognitive Services"
            }
          ],
          [
            {
              "text": "OCR, Form Recognizer, Language"
            }
          ],
          [
            {
              "text": "Alternative to AWS Textract"
            }
          ]
        ],
        [
          [
            {
              "text": "Hugging Face Inference API"
            }
          ],
          [
            {
              "text": "Hosted open-source model inference"
            }
          ],
          [
            {
              "text": "Cheaper alternative for standard NLP models"
            }
          ]
        ],
        [
          [
            {
              "text": "Pinecone / Weaviate / Qdrant"
            }
          ],
          [
            {
              "text": "Managed vector databases"
            }
          ],
          [
            {
              "text": "Semantic search, embedding storage"
            }
          ]
        ]
      ]
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "8. On-Premises vs. Cloud — Head-to-Head Comparison",
      "id": "8-on-premises-vs-cloud-head-to-head-comparison"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "8.1 Cost Comparison",
      "id": "8-1-cost-comparison"
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
              "text": "On-Premises"
            }
          ],
          [
            {
              "text": "Cloud (AWS)"
            }
          ]
        ],
        [
          [
            {
              "text": "Upfront Cost"
            }
          ],
          [
            {
              "text": "~$24,000 – $35,000 CapEx"
            }
          ],
          [
            {
              "text": "$0 – low (pay-as-you-go)"
            }
          ]
        ],
        [
          [
            {
              "text": "Monthly OpEx at idle"
            }
          ],
          [
            {
              "text": "~$240 – $440 (electricity + internet)"
            }
          ],
          [
            {
              "text": "$0 (no instances running)"
            }
          ]
        ],
        [
          [
            {
              "text": "Monthly OpEx at full utilization"
            }
          ],
          [
            {
              "text": "~$240 – $440"
            }
          ],
          [
            {
              "text": "$700 – $2,200+ per GPU instance"
            }
          ]
        ],
        [
          [
            {
              "text": "Break-even point (full utilization)"
            }
          ],
          [
            {
              "text": "~6–18 months"
            }
          ],
          [
            {
              "text": "N/A (always higher long-term)"
            }
          ]
        ],
        [
          [
            {
              "text": "Break-even point (light utilization)"
            }
          ],
          [
            {
              "text": "24–48+ months"
            }
          ],
          [
            {
              "text": "May never break even"
            }
          ]
        ],
        [
          [
            {
              "text": "Scaling cost"
            }
          ],
          [
            {
              "text": "High (buy new hardware)"
            }
          ],
          [
            {
              "text": "Low (spin up more instances)"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "8.2 Capability Comparison",
      "id": "8-2-capability-comparison"
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
              "text": "On-Premises"
            }
          ],
          [
            {
              "text": "Cloud (AWS + APIs)"
            }
          ]
        ],
        [
          [
            {
              "text": "Max VRAM per GPU"
            }
          ],
          [
            {
              "text": "48 GB (RTX Pro 5000) or 80 GB (A100)"
            }
          ],
          [
            {
              "text": "Up to 80 GB (A100) per GPU instance"
            }
          ]
        ],
        [
          [
            {
              "text": "Multi-GPU scaling"
            }
          ],
          [
            {
              "text": "Up to 4 GPUs in single server"
            }
          ],
          [
            {
              "text": "Effectively unlimited (p4d = 8x A100s)"
            }
          ]
        ],
        [
          [
            {
              "text": "Model customization / fine-tuning"
            }
          ],
          [
            {
              "text": "Full control"
            }
          ],
          [
            {
              "text": "Possible via SageMaker, but complex"
            }
          ]
        ],
        [
          [
            {
              "text": "Model privacy"
            }
          ],
          [
            {
              "text": "Complete — data never leaves your infrastructure"
            }
          ],
          [
            {
              "text": "Data processed by third party"
            }
          ]
        ],
        [
          [
            {
              "text": "Cold start time"
            }
          ],
          [
            {
              "text": "Seconds (model already loaded)"
            }
          ],
          [
            {
              "text": "30 seconds – 5 minutes (instance startup)"
            }
          ]
        ],
        [
          [
            {
              "text": "Inference latency"
            }
          ],
          [
            {
              "text": "Low (local network)"
            }
          ],
          [
            {
              "text": "Higher (network round-trip + API overhead)"
            }
          ]
        ],
        [
          [
            {
              "text": "Custom hardware (e.g., specific GPU)"
            }
          ],
          [
            {
              "text": "Full choice"
            }
          ],
          [
            {
              "text": "Limited to what AWS offers"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "8.3 Operational Comparison",
      "id": "8-3-operational-comparison"
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
              "text": "On-Premises"
            }
          ],
          [
            {
              "text": "Cloud (AWS + APIs)"
            }
          ]
        ],
        [
          [
            {
              "text": "Hardware maintenance"
            }
          ],
          [
            {
              "text": "You are responsible"
            }
          ],
          [
            {
              "text": "Managed by cloud provider"
            }
          ]
        ],
        [
          [
            {
              "text": "Uptime / SLA"
            }
          ],
          [
            {
              "text": "You are responsible"
            }
          ],
          [
            {
              "text": "AWS SLA: 99.9%–99.99%"
            }
          ]
        ],
        [
          [
            {
              "text": "Disaster recovery"
            }
          ],
          [
            {
              "text": "Must architect and manage yourself"
            }
          ],
          [
            {
              "text": "Built-in multi-AZ replication options"
            }
          ]
        ],
        [
          [
            {
              "text": "Scaling response time"
            }
          ],
          [
            {
              "text": "Days to weeks (order/ship hardware)"
            }
          ],
          [
            {
              "text": "Minutes (API call)"
            }
          ]
        ],
        [
          [
            {
              "text": "Security"
            }
          ],
          [
            {
              "text": "Full control; your responsibility"
            }
          ],
          [
            {
              "text": "Shared responsibility model"
            }
          ]
        ],
        [
          [
            {
              "text": "Data sovereignty"
            }
          ],
          [
            {
              "text": "Complete"
            }
          ],
          [
            {
              "text": "Depends on region/compliance config"
            }
          ]
        ],
        [
          [
            {
              "text": "DevOps complexity"
            }
          ],
          [
            {
              "text": "High (CUDA drivers, BIOS, hardware monitoring)"
            }
          ],
          [
            {
              "text": "Medium (managed services reduce overhead)"
            }
          ]
        ],
        [
          [
            {
              "text": "Vendor lock-in"
            }
          ],
          [
            {
              "text": "Low (your hardware, open source tools)"
            }
          ],
          [
            {
              "text": "High (AWS-specific services)"
            }
          ]
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "8.4 Pros and Cons Summary",
      "id": "8-4-pros-and-cons-summary"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "On-Premises — Pros:",
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
            "text": "Complete data privacy and sovereignty — no data leaves your infrastructure"
          }
        ],
        [
          {
            "text": "Lowest long-term cost at sustained high utilization"
          }
        ],
        [
          {
            "text": "No per-token or per-API-call costs — inference is \"free\" once hardware is paid for"
          }
        ],
        [
          {
            "text": "Full control over hardware, drivers, models, and software stack"
          }
        ],
        [
          {
            "text": "Low and consistent latency for inference requests"
          }
        ],
        [
          {
            "text": "Customizable — can run any model, any framework, any configuration"
          }
        ],
        [
          {
            "text": "No cold start delays for pre-loaded models"
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "On-Premises — Cons:",
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
            "text": "Very high upfront capital expenditure ($24k–$35k+)"
          }
        ],
        [
          {
            "text": "You bear all hardware failure risk"
          }
        ],
        [
          {
            "text": "Physical maintenance responsibility (cooling, drives, GPUs, cables)"
          }
        ],
        [
          {
            "text": "Hardware becomes outdated — GPU generations move fast"
          }
        ],
        [
          {
            "text": "No automatic scaling — you plan for peak capacity even if it's unused most of the time"
          }
        ],
        [
          {
            "text": "Requires network/infrastructure knowledge beyond pure software development"
          }
        ],
        [
          {
            "text": "Power, cooling, and physical space requirements"
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Cloud (AWS + APIs) — Pros:",
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
            "text": "Zero upfront cost — start immediately"
          }
        ],
        [
          {
            "text": "Pay only for what you use (especially valuable in early development)"
          }
        ],
        [
          {
            "text": "Infinitely scalable on demand"
          }
        ],
        [
          {
            "text": "Provider manages hardware, maintenance, and physical infrastructure"
          }
        ],
        [
          {
            "text": "Managed services (Textract, Comprehend, Bedrock) can dramatically reduce implementation complexity"
          }
        ],
        [
          {
            "text": "Anthropic API provides frontier-quality AI without needing to host large models yourself"
          }
        ],
        [
          {
            "text": "Geographic distribution for low-latency global deployments"
          }
        ]
      ]
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "Cloud (AWS + APIs) — Cons:",
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
            "text": "At full sustained utilization, cost is dramatically higher than on-premises (5x–20x over 3 years)"
          }
        ],
        [
          {
            "text": "Per-token API costs can balloon quickly with high document volumes"
          }
        ],
        [
          {
            "text": "Data leaves your infrastructure — relevant for sensitive or private data"
          }
        ],
        [
          {
            "text": "Vendor lock-in risk — pricing and API terms can change"
          }
        ],
        [
          {
            "text": "Network latency adds overhead vs. local inference"
          }
        ],
        [
          {
            "text": "Dependent on provider uptime (though SLAs are very strong)"
          }
        ],
        [
          {
            "text": "Less control over model versions, fine-tuning, and behavior for managed APIs"
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
      "text": "9. Hybrid Architecture Strategy",
      "id": "9-hybrid-architecture-strategy"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "The recommended approach is not a binary choice — it is a "
        },
        {
          "text": "hybrid architecture",
          "bold": true
        },
        {
          "text": " that combines on-premises GPU infrastructure with cloud APIs and services, using each where it provides the most leverage."
        }
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "9.1 On-Premises for:",
      "id": "9-1-on-premises-for"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "High-volume, high-frequency inference",
            "bold": true
          },
          {
            "text": " — OCR, HTR, embedding generation, NER. These run on every document ingested and the per-call economics of cloud APIs become prohibitive at scale."
          }
        ],
        [
          {
            "text": "Sensitive data processing",
            "bold": true
          },
          {
            "text": " — Any pipeline handling private or proprietary data that must not leave the local environment."
          }
        ],
        [
          {
            "text": "Custom fine-tuned models",
            "bold": true
          },
          {
            "text": " — Models fine-tuned specifically for Double Raven's domain (e.g., domain-specific entity recognition models trained on the project's specific data types)."
          }
        ],
        [
          {
            "text": "Low-latency real-time features",
            "bold": true
          },
          {
            "text": " — User-facing features where sub-second inference is required."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "9.2 Cloud / Anthropic API for:",
      "id": "9-2-cloud-anthropic-api-for"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Complex reasoning and synthesis",
            "bold": true
          },
          {
            "text": " — Use Claude Sonnet or Opus for tasks requiring multi-step reasoning, complex entity disambiguation, or cross-document inference that smaller local models can't handle reliably."
          }
        ],
        [
          {
            "text": "Burst and overflow capacity",
            "bold": true
          },
          {
            "text": " — If on-premises GPUs are saturated during peak load, overflow to cloud GPU instances (Auto Scaling + Spot Instances)."
          }
        ],
        [
          {
            "text": "Development and prototyping",
            "bold": true
          },
          {
            "text": " — Early-stage development before committing hardware investment. Use cloud APIs to validate pipelines and model choices before building on-premises infrastructure."
          }
        ],
        [
          {
            "text": "Managed document services",
            "bold": true
          },
          {
            "text": " — AWS Textract as a fallback or for specific document types (e.g., structured forms, IRS documents) where managed services outperform custom models."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "9.3 Architecture Diagram (Conceptual)",
      "id": "9-3-architecture-diagram-conceptual"
    },
    {
      "type": "code",
      "language": null,
      "code": "┌─────────────────────────────────────────────────────────┐\n│                  Double Raven Backend                   │\n│                                                         │\n│  ┌─────────────────┐    ┌──────────────────────────┐   │\n│  │ Document Ingest  │───▶│    Task Queue (Kafka)    │   │\n│  │  API (Go/gRPC)   │    └──────────────────────────┘   │\n│  └─────────────────┘                  │                 │\n│                                       ▼                 │\n│       ┌────────────────────────────────────────────┐    │\n│       │        On-Premises GPU Server              │    │\n│       │  ┌───────────┐  ┌───────────────────────┐  │    │\n│       │  │  OCR/HTR  │  │  NLP / NER / Embeds   │  │    │\n│       │  │  Pipeline │  │       Pipeline        │  │    │\n│       │  │ (GPU 1)   │  │       (GPU 2)         │  │    │\n│       │  └───────────┘  └───────────────────────┘  │    │\n│       │  ┌─────────────────────────────────────┐   │    │\n│       │  │   Identity Resolution (CPU / RAM)   │   │    │\n│       │  └─────────────────────────────────────┘   │    │\n│       └────────────────────────────────────────────┘    │\n│                           │                             │\n│              ┌────────────┼────────────┐                │\n│              ▼            ▼            ▼                │\n│       ┌──────────┐  ┌──────────┐  ┌───────────────┐    │\n│       │ MySQL /  │  │ MongoDB  │  │  Vector DB    │    │\n│       │Structured│  │Document  │  │ (Embeddings)  │    │\n│       │  Store   │  │  Store   │  │   (Qdrant)    │    │\n│       └──────────┘  └──────────┘  └───────────────┘    │\n│                                                         │\n│   Cloud / API Layer (Overflow + Advanced Reasoning)     │\n│  ┌─────────────┐  ┌───────────────┐  ┌──────────────┐  │\n│  │  Anthropic  │  │   AWS GPU     │  │ AWS Textract │  │\n│  │ Claude API  │  │  Spot (Burst) │  │  (Fallback)  │  │\n│  └─────────────┘  └───────────────┘  └──────────────┘  │\n└─────────────────────────────────────────────────────────┘"
    },
    {
      "type": "divider"
    },
    {
      "type": "heading",
      "level": 1,
      "text": "10. Final Recommendation",
      "id": "10-final-recommendation"
    },
    {
      "type": "heading",
      "level": 2,
      "text": "Phase 1 — Development & Validation (Months 1–6)",
      "id": "phase-1-development-validation-months-1-6"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "→ Use Cloud APIs + Small Cloud GPU Instances",
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
            "text": "Use "
          },
          {
            "text": "Anthropic Claude API",
            "bold": true
          },
          {
            "text": " (Haiku/Sonnet) for all NLP, analysis, and entity resolution during development. Fast to iterate, no infrastructure overhead."
          }
        ],
        [
          {
            "text": "Use "
          },
          {
            "text": "AWS Textract",
            "bold": true
          },
          {
            "text": " to validate OCR pipeline requirements without building OCR infrastructure."
          }
        ],
        [
          {
            "text": "Use "
          },
          {
            "text": "AWS g5.2xlarge",
            "bold": true
          },
          {
            "text": " (A10G, 24 GB VRAM) spot instances for running open-source model experiments."
          }
        ],
        [
          {
            "text": "Validate document volumes, throughput requirements, and model quality before committing to hardware."
          }
        ],
        [
          {
            "text": "Estimated monthly cost:",
            "bold": true
          },
          {
            "text": " $200 – $800/month (API calls + occasional GPU instances)."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "Phase 2 — Initial On-Premises Build (Month 6–9)",
      "id": "phase-2-initial-on-premises-build-month-6-9"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "→ Build 2x GPU Server After Validating Requirements",
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
            "text": "Once pipeline architecture is validated, purchase the 2x GPU server (2x RTX Pro 5000, Threadripper PRO, 192 GB ECC RAM)."
          }
        ],
        [
          {
            "text": "Deploy self-hosted models for high-frequency workloads (OCR, embeddings, NER)."
          }
        ],
        [
          {
            "text": "Retain Claude API for advanced reasoning tasks."
          }
        ],
        [
          {
            "text": "Estimated CapEx:",
            "bold": true
          },
          {
            "text": " ~$24,000."
          }
        ],
        [
          {
            "text": "Break-even vs. cloud:",
            "bold": true
          },
          {
            "text": " ~8–12 months at expected utilization."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "Phase 3 — Scale On-Premises (Month 12+)",
      "id": "phase-3-scale-on-premises-month-12"
    },
    {
      "type": "paragraph",
      "spans": [
        {
          "text": "→ Add GPU Capacity and Storage as Demand Grows",
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
            "text": "Add 2 more RTX Pro 5000 GPUs if compute demand warrants (~$10,400)."
          }
        ],
        [
          {
            "text": "Expand HDD storage for growing document archive."
          }
        ],
        [
          {
            "text": "Fine-tune domain-specific models using accumulated data."
          }
        ],
        [
          {
            "text": "Continue using Claude API for frontier reasoning tasks where the quality delta vs. local models is meaningful."
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 2,
      "text": "Summary Decision Matrix",
      "id": "summary-decision-matrix"
    },
    {
      "type": "table",
      "headerRow": true,
      "rows": [
        [
          [
            {
              "text": "Consideration"
            }
          ],
          [
            {
              "text": "Short-Term (Dev)"
            }
          ],
          [
            {
              "text": "Long-Term (Prod)"
            }
          ]
        ],
        [
          [
            {
              "text": "OCR / HTR"
            }
          ],
          [
            {
              "text": "Cloud API"
            }
          ],
          [
            {
              "text": "On-Premises GPU"
            }
          ]
        ],
        [
          [
            {
              "text": "NLP / NER"
            }
          ],
          [
            {
              "text": "Cloud API"
            }
          ],
          [
            {
              "text": "On-Premises GPU"
            }
          ]
        ],
        [
          [
            {
              "text": "Embedding Generation"
            }
          ],
          [
            {
              "text": "Cloud API"
            }
          ],
          [
            {
              "text": "On-Premises GPU"
            }
          ]
        ],
        [
          [
            {
              "text": "Complex Reasoning / Analysis"
            }
          ],
          [
            {
              "text": "Claude API"
            }
          ],
          [
            {
              "text": "Claude API (retained)"
            }
          ]
        ],
        [
          [
            {
              "text": "Identity Resolution"
            }
          ],
          [
            {
              "text": "CPU (cloud or local)"
            }
          ],
          [
            {
              "text": "On-Premises CPU"
            }
          ]
        ],
        [
          [
            {
              "text": "Burst / Peak Overflow"
            }
          ],
          [
            {
              "text": "N/A"
            }
          ],
          [
            {
              "text": "AWS Spot GPU"
            }
          ]
        ],
        [
          [
            {
              "text": "Data Storage"
            }
          ],
          [
            {
              "text": "AWS S3"
            }
          ],
          [
            {
              "text": "On-Premises + S3 backup"
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
            "text": " Living document — update as hardware is purchased, cloud spend is measured, and pipeline architecture evolves."
          }
        ],
        [],
        [
          {
            "text": "Next Steps:",
            "bold": true
          }
        ],
        [
          {
            "text": "1. Finalize document volume estimates and throughput requirements"
          }
        ],
        [
          {
            "text": "2. Benchmark candidate OCR/HTR models on AWS spot instances"
          }
        ],
        [
          {
            "text": "3. Get formal quotes for Threadripper PRO system from system integrators (Puget Systems, BoXX Technologies, or custom build)"
          }
        ],
        [
          {
            "text": "4. Establish AWS cost budget alerts and monthly cloud spend baseline"
          }
        ]
      ]
    }
  ]
}
$dr_doc$::jsonb,
    updated_at = now(),
    updated_by = 'migration:toc-links'
WHERE slug = 'backend-ai-infrastructure';

INSERT INTO dr_document_revisions (document_id, revision_number, title, content_format, content, created_by)
SELECT d.id, COALESCE(MAX(r.revision_number), 0) + 1, d.title, d.content_format, d.content, 'migration:toc-links'
FROM dr_documents d
LEFT JOIN dr_document_revisions r ON r.document_id = d.id
WHERE d.slug = 'backend-ai-infrastructure'
GROUP BY d.id, d.title, d.content_format, d.content;

COMMIT;
