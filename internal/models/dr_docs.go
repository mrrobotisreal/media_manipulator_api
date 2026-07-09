package models

import (
	"encoding/json"
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
//
// CanDelete + HasEditSession are SERVER-COMPUTED per request, never trusted from
// the client: CanDelete is case-insensitive equality of created_by and the
// caller's verified email (creator-only delete); HasEditSession reflects whether
// an edit-session row exists (drives the "Resume editing" label + restore
// confirmation UX).
type DrDocSummary struct {
	ID             string  `json:"id"`
	Slug           string  `json:"slug"`
	Title          string  `json:"title"`
	Summary        *string `json:"summary"`
	Status         string  `json:"status"`
	CreatedBy      string  `json:"createdBy"`
	CanDelete      bool    `json:"canDelete"`
	HasEditSession bool    `json:"hasEditSession"`
	// FolderID places the document in the docs-explorer tree; nil = root
	// (additive — populated by ListDocs/GetDoc; other summary producers leave
	// it nil and the client reconciles via the list).
	FolderID  *string `json:"folderId"`
	CreatedAt UTCTime `json:"createdAt"`
	UpdatedAt UTCTime `json:"updatedAt"`
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

// ---------------------------------------------------------------------------
// Documentation filesystem (dr_doc_folders.go). Folders are COLLABORATIVE:
// both allowlisted users create/rename/move/delete them (like chat-lab
// projects); document delete keeps its creator-only rule.
// ---------------------------------------------------------------------------

// DrDocFolder is one folder row. ParentID nil = a root-level folder.
type DrDocFolder struct {
	ID             string  `json:"id"`
	ParentID       *string `json:"parentId"`
	Name           string  `json:"name"`
	CreatedByEmail string  `json:"createdByEmail"`
	CreatedAt      UTCTime `json:"createdAt"`
	UpdatedAt      UTCTime `json:"updatedAt"`
}

// DrDocFoldersResponse is GET /doc-folders: the FLAT list (cap 500) — the
// client assembles the tree (flat is simpler, cheaper, and immune to
// deep-nesting JSON recursion).
type DrDocFoldersResponse struct {
	Folders []DrDocFolder `json:"folders"`
}

// DrCreateDocFolderRequest creates a folder. ParentID nil/omitted = root.
type DrCreateDocFolderRequest struct {
	Name     string  `json:"name"`
	ParentID *string `json:"parentId"`
}

// DrUpdateDocFolderRequest partially updates a folder: rename and/or move in
// one call. ParentID is raw JSON so ABSENT (no move) is distinguishable from
// NULL (move to root) — a *string cannot express that difference.
type DrUpdateDocFolderRequest struct {
	Name     *string         `json:"name"`
	ParentID json.RawMessage `json:"parentId"`
}

// DrMoveDocRequest is PUT /docs/:slug/move. FolderID nil = move to root.
type DrMoveDocRequest struct {
	FolderID *string `json:"folderId"`
}

// DrRenameDocRequest is PUT /docs/:slug/rename — title ONLY, never the slug
// (slugs are stable identifiers baked into URLs and the comments/revisions
// chain).
type DrRenameDocRequest struct {
	Title string `json:"title"`
}

// ---------------------------------------------------------------------------
// "Create Doc" editor request/response contracts (see internal/handlers/
// dr_docs.go). Authorship (created_by/updated_by/author_uid) is always taken
// from the verified Firebase claims in the gin context — never from these
// bodies. camelCase JSON keys mirror media-manipulator-ui/schemas/drDocs.ts.
// ---------------------------------------------------------------------------

// DrCreateDocRequest starts a new draft. Title is optional (defaults to
// "Untitled"); everything else is filled in by autosave/publish.
type DrCreateDocRequest struct {
	Title string `json:"title"`
}

// DrUpdateDocRequest is the autosave payload. Summary is a pointer so the
// client can distinguish "leave summary unset" (nil) from "clear it" (""). The
// content is opaque json.RawMessage validated structurally by
// validateDrBlocksJSON (full schema validation stays in the UI's Zod layer).
type DrUpdateDocRequest struct {
	Title   string          `json:"title"`
	Summary *string         `json:"summary"`
	Content json.RawMessage `json:"content"`
}

// DrUpdateDocResponse is the autosave ack. Kept as an explicit struct (rather
// than an ad-hoc gin.H) so the client can Zod-model it.
type DrUpdateDocResponse struct {
	OK bool `json:"ok"`
}

// DrPresignAssetRequest requests an S3 upload URL for one document asset. Kind
// and ContentType are cross-checked by docAssetExt; Width/Height are optional
// image dimensions captured client-side.
type DrPresignAssetRequest struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Kind        string `json:"kind"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
}

// DrPresignAssetResponse returns the created asset id and the presigned PUT URL
// the client uploads to directly.
type DrPresignAssetResponse struct {
	AssetID   string `json:"assetId"`
	UploadURL string `json:"uploadUrl"`
}

// ---------------------------------------------------------------------------
// Edit sessions, publish-changes, soft delete, and version history contracts
// (see internal/handlers/dr_docs_editor.go). Same authorship rule as above.
// ---------------------------------------------------------------------------

// DrEditSession is the staged, in-progress edit of a published document. Content
// is the canonical dr-blocks/v1 JSON hydrated to presigned URLs at read time
// (the stored session content stays canonical).
type DrEditSession struct {
	DocumentID string          `json:"documentId"`
	Title      string          `json:"title"`
	Summary    *string         `json:"summary"`
	Content    json.RawMessage `json:"content"`
	CreatedBy  string          `json:"createdBy"`
	UpdatedBy  string          `json:"updatedBy"`
	CreatedAt  UTCTime         `json:"createdAt"`
	UpdatedAt  UTCTime         `json:"updatedAt"`
}

// DrStartEditRequest opens/resumes/replaces the document's edit session. Both
// fields optional: no FromRevision + Replace=false → resume-or-create from the
// current published content; FromRevision set → seed from that revision;
// Replace=true → discard any existing session first.
type DrStartEditRequest struct {
	FromRevision *int `json:"fromRevision"`
	Replace      bool `json:"replace"`
}

// DrUpdateEditRequest is the edit-session autosave payload — identical shape to
// the draft autosave (title/summary/content).
type DrUpdateEditRequest = DrUpdateDocRequest

// DrRevisionSummary is one version-history row (metadata only, never content).
// CreatedBy is a pointer so a NULL author column serializes as JSON null.
type DrRevisionSummary struct {
	RevisionNumber int     `json:"revisionNumber"`
	Title          string  `json:"title"`
	CreatedBy      *string `json:"createdBy"`
	CreatedAt      UTCTime `json:"createdAt"`
	IsCurrent      bool    `json:"isCurrent"`
}

// DrRevision is a full version snapshot: the summary plus the (hydrated) content.
type DrRevision struct {
	DrRevisionSummary
	ContentFormat string          `json:"contentFormat"`
	Content       json.RawMessage `json:"content"`
}

// DrRevisionsListResponse is the version-history listing.
type DrRevisionsListResponse struct {
	Revisions []DrRevisionSummary `json:"revisions"`
}
