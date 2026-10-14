package metadata

// System table IDs. From core/.../metadata/SystemTables.java.
const (
	RootTableID     = "+r" // SystemTables.ROOT
	MetadataTableID = "!0" // SystemTables.METADATA
)

// Column-family + qualifier names from core/.../metadata/schema/MetadataSchema.java.
const (
	CFFile            = "file" // DataFileColumnFamily.STR_NAME
	CFFileEmbedding   = "file.embedding"
	CFCurrentLocation = "loc"    // CurrentLocationColumnFamily.STR_NAME
	CFFutureLocation  = "future" // FutureLocationColumnFamily.STR_NAME
	CFLog             = "log"    // LogColumnFamily.STR_NAME
	CFServer          = "srv"    // ServerColumnFamily.STR_NAME
	CFTabletSection   = "~tab"   // TabletColumnFamily — holds prev-row etc.
	CQPrevRow         = "~pr"    // PrevRowColumn qualifier under ~tab
	CQDirectory       = "dir"    // ServerColumnFamily.DIRECTORY_QUAL
	CQTime            = "time"   // ServerColumnFamily.TIME_QUAL
	CQLock            = "lock"   // ServerColumnFamily.LOCK_QUAL
	CQFlushID         = "flush"  // ServerColumnFamily.FLUSH_COLUMN qualifier
)
