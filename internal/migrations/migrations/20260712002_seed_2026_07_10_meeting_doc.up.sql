-- 20260712002_seed_2026_07_10_meeting_doc.up.sql
--
-- Seed the "2026-07-10 Meeting" notes into the Double Raven document store,
-- copying the established seed pattern (document insert + revision-1
-- snapshot).
--
-- The content JSONB below is generated from (and MUST stay byte-for-byte
-- identical to) media-manipulator-ui/content/dr-docs/meeting-2026-07-10.ts,
-- the single source of truth for this document.
--
-- Ownership + sharing: created_by/updated_by are 'mwintrow@creatv.io' (the
-- owner gets edit/delete + the sharing toggle via drCanDelete), and
-- allow_partner_edits is seeded TRUE — these are shared meeting notes both
-- users maintain; the creator can flip it off in the UI later. The
-- allow_partner_edits column exists because this build's 20260712001
-- migration runs first.

BEGIN;

INSERT INTO dr_documents (slug, title, summary, status, content_format, content, created_by, updated_by, allow_partner_edits)
VALUES (
  '2026-07-10-meeting',
  '2026-07-10 Meeting',
  'Meeting notes for the July 10, 2026 Double Raven sync: portal/tool access, DNS + api.doubleraven.net setup, and the OpenRouter financial/billing structure for departments.',
  'published',
  'dr-blocks/v1',
  $dr_doc$
{
  "format": "dr-blocks/v1",
  "blocks": [
    {
      "type": "heading",
      "level": 1,
      "text": "Topics For Discussion",
      "id": "topics-for-discussion"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "I need access to the Double Raven website and especially the DR/ODIN app tool itself so I can begin getting familiar with everything and start creating and testing the APIs"
          }
        ],
        [
          {
            "text": "Note: for the DR/ODIN tool in particular I'm going to need my own account that I can use for testing so that I don't screw up any real accounts or data with testing data"
          }
        ],
        [
          {
            "text": "Note: for the DR website itself I just need to know how exactly you'd like it to be laid out, anything to be added or removed, and especially all of the copy (text) and resources and links"
          }
        ],
        [
          {
            "text": "Note: I need to know who is/where you're managing your domain and DNS, because I need to (or you can do it for me if you'd rather do it that way, that's fine too) make sure some CNAME records get added to your DNS. Specifically I need to create an \"api\" CNAME record that I can point at and connect to the Cloudflare tunnel I have set up to my server so that we can use https://api.doubleraven.net as the base API url for all API calls and ensure it's encrypted with TLS/SSL and only accessible via access to the Cloudflare tunnel."
          }
        ],
        [
          {
            "text": "If going this route with using OpenRouter so we can use the right models for the right jobs (which I'm highly recommending), then we need to figure out the financial structure and marketing of how exactly this is going to work when departments/detectives begin actually using it."
          }
        ],
        [
          {
            "text": "E.g. do they pay up front to top-up how many credits they have for usage? Or do we handle that for them and then bill them? etc…"
          }
        ]
      ]
    },
    {
      "type": "heading",
      "level": 1,
      "text": "Questions",
      "id": "questions"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Question 1"
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
      "text": "Action Items",
      "id": "action-items"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "☐ Item 1"
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
      "text": "References | Resources | Links",
      "id": "references-resources-links"
    },
    {
      "type": "list",
      "ordered": false,
      "items": [
        [
          {
            "text": "Double Raven x Media Manipulator portal",
            "link": "https://www.media-manipulator.com/dr/auth"
          }
        ]
      ]
    }
  ]
}
$dr_doc$::jsonb,
  'mwintrow@creatv.io',
  'mwintrow@creatv.io',
  true
);

INSERT INTO dr_document_revisions (document_id, revision_number, title, content_format, content, created_by)
SELECT id, 1, title, content_format, content, 'mwintrow@creatv.io'
FROM dr_documents WHERE slug = '2026-07-10-meeting';

COMMIT;
