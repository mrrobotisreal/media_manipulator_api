// Package metadata builds the canonical, group-keyed MetadataDocument that the
// metadata inspector (MM-M2) consumes: exiftool runs for every file kind, ffprobe
// is folded in as synthetic groups for video and audio, and every entry is
// classified into a category and privacy class from a table generated from the
// research catalog.
//
// Design: docs/design/METADATA_INSPECTOR.md, ADR-0007, ticket MM07.
//
// # Entry ids
//
// An entry id is "<group>:<tag>". The group is exiftool's family-1 group
// (IFD0, ExifIFD, GPS, XMP-xmp, QuickTime, Keys, ...) or one of the synthetic
// groups below. When the same (group, tag) pair appears more than once the
// first keeps the plain id and later ones get "#2", "#3", ... in output order,
// never random and never map ordered. IFD0:ModifyDate and XMP-xmp:ModifyDate
// are therefore distinct entries.
//
// # Synthetic groups (ffprobe)
//
//	Format          one entry per ffprobe format field (except filename, which is
//	                a server temp path) and one per format.tags key:
//	                Format:duration, Format:major_brand
//	Stream:<n>      one entry per stream field and per stream tag, plus the
//	                synthetic Stream:<n>:StreamType holding codec_type
//	                (group "Stream:<n>", tag "StreamType"). <n> is the ffprobe
//	                stream index.
//	Chapter:<n>     start, end, start_time, end_time, time_base, id and one entry
//	                per chapter tag (title, ...). <n> is the chapter's position.
//	SideData        one entry per side-data item. group "SideData", tag
//	                "<stream>:<side_data_type>", for example
//	                "SideData:0:Display Matrix" or "SideData:0:Mastering display
//	                metadata". The value is the item's fields as an ordered
//	                object (rotation, displaymatrix, red_x, ...).
//
// Synthetic group names contain a colon or are not exiftool family-1 names, so
// they can never collide with exiftool entries, and an exiftool entry is never
// overwritten.
//
// # Warnings
//
// Tool failure never fails an inspect. See the warning* constants in
// inspect.go for the exact strings.
//
//go:generate go run ../../scripts/gen-metadata-catalog -catalog ../../../docs/design/research/METADATA_TAG_CATALOG.md -out .
package metadata
