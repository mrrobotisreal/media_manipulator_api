package models

import (
	"encoding/json"
	"time"
)

// Double Raven partner portal documents. Content is an opaque, validated-at-seed
// JSONB block array (format "dr-blocks/v1") that the Go layer never models — it
// passes the JSON through untouched so the future DR+MM Doc Editor IDE can evolve
// the block shapes without a Go change per block type. The canonical block + DTO
// contract lives in media-manipulator-ui/schemas/drDocs.ts; these structs mirror
// its camelCase JSON keys. Timestamps serialize as RFC 3339 UTC (the pgx pool
// pins the session timezone to UTC — see internal/db).

// DrDocStatus is the publication state of a portal document. Only "published"
// rows are listed; the column is CHECK-constrained to these values in the
// migration.
type DrDocStatus string

const (
	DrDocStatusDraft     DrDocStatus = "draft"
	DrDocStatusPublished DrDocStatus = "published"
	DrDocStatusArchived  DrDocStatus = "archived"
)

// DrDocSummary is the list-view projection: metadata ONLY, never content. The
// list endpoint selects exactly these columns so a document's body is never
// shipped in a listing. Summary is a pointer so a NULL column serializes as
// JSON null rather than an empty string.
type DrDocSummary struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Summary   *string   `json:"summary"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// DrDoc is the full single-document payload: the summary metadata plus the
// opaque content. Content is json.RawMessage so the stored block JSON
// round-trips byte-for-byte without the API parsing it. DrDocSummary is embedded
// anonymously so its fields flatten into the same JSON object as contentFormat +
// content.
type DrDoc struct {
	DrDocSummary
	ContentFormat string          `json:"contentFormat"`
	Content       json.RawMessage `json:"content"`
}
