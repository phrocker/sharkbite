#ifndef SHOAL_TYPES_H
#define SHOAL_TYPES_H

#include <stddef.h>
#include <stdint.h>

#if defined(_WIN32)
#define SHOAL_CALL __cdecl
#if defined(SHOAL_STATIC)
#define SHOAL_API
#elif defined(SHOAL_BUILDING_LIBRARY)
#define SHOAL_API __declspec(dllexport)
#else
#define SHOAL_API __declspec(dllimport)
#endif
#else
#define SHOAL_CALL
#define SHOAL_API __attribute__((visibility("default")))
#endif

/*
 * SHOAL_ABI_VERSION is the stable compatibility-major used by the original
 * shoal_abi_version() query and existing callers.
 */
#define SHOAL_ABI_VERSION 1u
#define SHOAL_ABI_VERSION_MAJOR 1u
#define SHOAL_ABI_VERSION_MINOR 19u
#define SHOAL_ABI_VERSION_PATCH 0u
#define SHOAL_ABI_PACK_VERSION(major, minor, patch)                           \
  ((((uint32_t)(major) & 0xffu) << 16) |                                     \
   (((uint32_t)(minor) & 0xffu) << 8) | ((uint32_t)(patch) & 0xffu))
#define SHOAL_ABI_VERSION_PACKED                                             \
  SHOAL_ABI_PACK_VERSION(SHOAL_ABI_VERSION_MAJOR,                            \
                         SHOAL_ABI_VERSION_MINOR,                            \
                         SHOAL_ABI_VERSION_PATCH)

typedef uint32_t shoal_abi_capability_id;
typedef uint64_t shoal_abi_capability_bits;

/*
 * Capability identifiers are append-only. Existing numeric assignments and the
 * meaning of each reported bit never change.
 */
enum {
  SHOAL_ABI_CAPABILITY_CONNECTOR = 0,
  SHOAL_ABI_CAPABILITY_BOOTSTRAP = 1,
  SHOAL_ABI_CAPABILITY_ERROR = 2,
  SHOAL_ABI_CAPABILITY_SCANNER = 3,
  SHOAL_ABI_CAPABILITY_BATCH_SCANNER = 4,
  SHOAL_ABI_CAPABILITY_OWNED_SCAN_RESULT = 5,
  SHOAL_ABI_CAPABILITY_MUTATION = 6,
  SHOAL_ABI_CAPABILITY_BATCH_WRITER = 7,
  SHOAL_ABI_CAPABILITY_STRUCTURED_WRITE_FAILURE = 8,
  SHOAL_ABI_CAPABILITY_TABLE_ADMIN = 9,
  SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN = 10,
  SHOAL_ABI_CAPABILITY_SECURITY_ADMIN = 11,
  SHOAL_ABI_CAPABILITY_TABLE_SPLITS = 12,
  SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY = 13,
  SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS = 14,
  SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY = 15,
  SHOAL_ABI_CAPABILITY_RFILE = 16,
  SHOAL_ABI_CAPABILITY_DATA_VALUES = 17,
  SHOAL_ABI_CAPABILITY_BUFFERED_WRITER = 18,
  SHOAL_ABI_CAPABILITY_TABLE_MAINTENANCE = 19,
  SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL = 20,
  SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT = 21,
  SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER = 22,
  SHOAL_ABI_CAPABILITY_COMPATIBILITY_ERRORS = 23,
  SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR = 24,
  SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY = 25,
  SHOAL_ABI_CAPABILITY_OWNED_KEY = 26,
  SHOAL_ABI_CAPABILITY_HDFS = 27,
  SHOAL_ABI_CAPABILITY_RFILE_LOCALITY_GROUPS = 28,
  SHOAL_ABI_CAPABILITY_CLIENT_PARITY_CONTROLS = 29,
  SHOAL_ABI_CAPABILITY_STORAGE_ERROR_PARITY = 30,
  SHOAL_ABI_CAPABILITY_ZOOKEEPER_INSTANCE = 31
};

#define SHOAL_ABI_CAPABILITY_COUNT 32u
#define SHOAL_ABI_CAPABILITY_WORD_BITS 64u
#define SHOAL_ABI_CAPABILITY_WORD_INDEX(capability_id)                       \
  ((uint32_t)(capability_id) / SHOAL_ABI_CAPABILITY_WORD_BITS)
#define SHOAL_ABI_CAPABILITY_BIT_INDEX(capability_id)                        \
  ((uint32_t)(capability_id) % SHOAL_ABI_CAPABILITY_WORD_BITS)
#define SHOAL_ABI_CAPABILITY_MASK(capability_id)                             \
  (((shoal_abi_capability_bits)UINT64_C(1))                                  \
   << SHOAL_ABI_CAPABILITY_BIT_INDEX(capability_id))
#define SHOAL_ABI_CAPABILITY_WORD_COUNT 1u
#define SHOAL_ABI_CAPABILITY_CONNECTOR_MASK                                  \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_CONNECTOR)
#define SHOAL_ABI_CAPABILITY_BOOTSTRAP_MASK                                  \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_BOOTSTRAP)
#define SHOAL_ABI_CAPABILITY_ERROR_MASK                                      \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_ERROR)
#define SHOAL_ABI_CAPABILITY_SCANNER_MASK                                    \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_SCANNER)
#define SHOAL_ABI_CAPABILITY_BATCH_SCANNER_MASK                              \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_BATCH_SCANNER)
#define SHOAL_ABI_CAPABILITY_OWNED_SCAN_RESULT_MASK                          \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_OWNED_SCAN_RESULT)
#define SHOAL_ABI_CAPABILITY_MUTATION_MASK                                   \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_MUTATION)
#define SHOAL_ABI_CAPABILITY_BATCH_WRITER_MASK                               \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_BATCH_WRITER)
#define SHOAL_ABI_CAPABILITY_STRUCTURED_WRITE_FAILURE_MASK                   \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_STRUCTURED_WRITE_FAILURE)
#define SHOAL_ABI_CAPABILITY_TABLE_ADMIN_MASK                                \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_TABLE_ADMIN)
#define SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN_MASK                            \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN)
#define SHOAL_ABI_CAPABILITY_SECURITY_ADMIN_MASK                             \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_SECURITY_ADMIN)
#define SHOAL_ABI_CAPABILITY_TABLE_SPLITS_MASK                               \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_TABLE_SPLITS)
#define SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY_MASK                         \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY)
#define SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS_MASK                           \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS)
#define SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY_MASK                     \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY)
#define SHOAL_ABI_CAPABILITY_RFILE_MASK                                      \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_RFILE)
#define SHOAL_ABI_CAPABILITY_DATA_VALUES_MASK                                \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_DATA_VALUES)
#define SHOAL_ABI_CAPABILITY_BUFFERED_WRITER_MASK                            \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_BUFFERED_WRITER)
#define SHOAL_ABI_CAPABILITY_TABLE_MAINTENANCE_MASK                          \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_TABLE_MAINTENANCE)
#define SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL_MASK                          \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL)
#define SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT_MASK                          \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT)
#define SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER_MASK                         \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER)
#define SHOAL_ABI_CAPABILITY_COMPATIBILITY_ERRORS_MASK                       \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_COMPATIBILITY_ERRORS)
#define SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR_MASK                      \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR)
#define SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY_MASK                          \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY)
#define SHOAL_ABI_CAPABILITY_OWNED_KEY_MASK                                  \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_OWNED_KEY)
#define SHOAL_ABI_CAPABILITY_HDFS_MASK                                       \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_HDFS)
#define SHOAL_ABI_CAPABILITY_RFILE_LOCALITY_GROUPS_MASK                      \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_RFILE_LOCALITY_GROUPS)
#define SHOAL_ABI_CAPABILITY_CLIENT_PARITY_CONTROLS_MASK                     \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_CLIENT_PARITY_CONTROLS)
#define SHOAL_ABI_CAPABILITY_STORAGE_ERROR_PARITY_MASK                       \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_STORAGE_ERROR_PARITY)
#define SHOAL_ABI_CAPABILITY_ZOOKEEPER_INSTANCE_MASK                         \
  SHOAL_ABI_CAPABILITY_MASK(SHOAL_ABI_CAPABILITY_ZOOKEEPER_INSTANCE)
#define SHOAL_ABI_CAPABILITY_WORD0                                           \
  (SHOAL_ABI_CAPABILITY_CONNECTOR_MASK | SHOAL_ABI_CAPABILITY_BOOTSTRAP_MASK | \
   SHOAL_ABI_CAPABILITY_ERROR_MASK | SHOAL_ABI_CAPABILITY_SCANNER_MASK |     \
   SHOAL_ABI_CAPABILITY_BATCH_SCANNER_MASK |                                 \
   SHOAL_ABI_CAPABILITY_OWNED_SCAN_RESULT_MASK |                             \
   SHOAL_ABI_CAPABILITY_MUTATION_MASK |                                      \
   SHOAL_ABI_CAPABILITY_BATCH_WRITER_MASK |                                  \
   SHOAL_ABI_CAPABILITY_STRUCTURED_WRITE_FAILURE_MASK |                      \
   SHOAL_ABI_CAPABILITY_TABLE_ADMIN_MASK |                                   \
   SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN_MASK |                               \
   SHOAL_ABI_CAPABILITY_SECURITY_ADMIN_MASK |                                \
   SHOAL_ABI_CAPABILITY_TABLE_SPLITS_MASK |                                  \
   SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY_MASK |                            \
   SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS_MASK |                              \
   SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY_MASK |                        \
   SHOAL_ABI_CAPABILITY_RFILE_MASK |                                         \
   SHOAL_ABI_CAPABILITY_DATA_VALUES_MASK |                                  \
   SHOAL_ABI_CAPABILITY_BUFFERED_WRITER_MASK |                              \
   SHOAL_ABI_CAPABILITY_TABLE_MAINTENANCE_MASK |                             \
   SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL_MASK |                             \
   SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT_MASK |                             \
   SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER_MASK |                            \
   SHOAL_ABI_CAPABILITY_COMPATIBILITY_ERRORS_MASK |                          \
   SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR_MASK |                         \
   SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY_MASK |                             \
   SHOAL_ABI_CAPABILITY_OWNED_KEY_MASK |                                     \
   SHOAL_ABI_CAPABILITY_HDFS_MASK |                                          \
   SHOAL_ABI_CAPABILITY_RFILE_LOCALITY_GROUPS_MASK |                         \
   SHOAL_ABI_CAPABILITY_CLIENT_PARITY_CONTROLS_MASK |                        \
   SHOAL_ABI_CAPABILITY_STORAGE_ERROR_PARITY_MASK |                         \
   SHOAL_ABI_CAPABILITY_ZOOKEEPER_INSTANCE_MASK)

typedef int32_t shoal_log_level;

enum {
  SHOAL_LOG_LEVEL_OFF = 0,
  SHOAL_LOG_LEVEL_DEBUG = 1,
  SHOAL_LOG_LEVEL_TRACE = 2
};

typedef void(SHOAL_CALL *shoal_log_callback)(
    shoal_log_level level, const char *event_name,
    const char *attributes_json, void *context);

typedef int32_t shoal_status;

enum {
  SHOAL_STATUS_OK = 0,
  SHOAL_STATUS_INVALID_ARGUMENT = 1,
  SHOAL_STATUS_INVALID_HANDLE = 2,
  SHOAL_STATUS_OUT_OF_MEMORY = 3,
  SHOAL_STATUS_UNSUPPORTED = 4,
  SHOAL_STATUS_BOOTSTRAP_FAILED = 5,
  SHOAL_STATUS_CLOSED = 6,
  SHOAL_STATUS_CANCELLED = 7,
  SHOAL_STATUS_DEADLINE_EXCEEDED = 8,
  SHOAL_STATUS_NOT_FOUND = 9,
  SHOAL_STATUS_PERMISSION_DENIED = 10,
  SHOAL_STATUS_DISCOVERY_UNAVAILABLE = 11,
  SHOAL_STATUS_TABLET_UNAVAILABLE = 12,
  SHOAL_STATUS_RANGE_SPANS_TABLETS = 13,
  SHOAL_STATUS_CLEANUP_FAILED = 14,
  SHOAL_STATUS_OPERATION_FAILED = 15,
  SHOAL_STATUS_RETRY_EXHAUSTED = 16,
  SHOAL_STATUS_MUTATION_REJECTED = 17,
  SHOAL_STATUS_AMBIGUOUS_WRITE = 18,
  SHOAL_STATUS_ALREADY_EXISTS = 19,
  SHOAL_STATUS_UNAVAILABLE = 20,
  SHOAL_STATUS_NAMESPACE_NOT_EMPTY = 21,
  SHOAL_STATUS_TABLE_OFFLINE = 22,
  SHOAL_STATUS_USER_NOT_FOUND = 23,
  SHOAL_STATUS_BAD_CREDENTIALS = 24,
  SHOAL_STATUS_SECURITY_UNAVAILABLE = 25,
  SHOAL_STATUS_INCOMPLETE = 26,
  SHOAL_STATUS_INTERNAL = 255
};

typedef int32_t shoal_error_source_class;

enum {
  SHOAL_ERROR_SOURCE_RUNTIME = 0,
  SHOAL_ERROR_SOURCE_CLIENT_EXCEPTION = 1,
  SHOAL_ERROR_SOURCE_ILLEGAL_STATE_EXCEPTION = 2,
  SHOAL_ERROR_SOURCE_ITERATION_INTERRUPTED_EXCEPTION = 3,
  SHOAL_ERROR_SOURCE_VISIBILITY_PARSE_EXCEPTION = 4,
  SHOAL_ERROR_SOURCE_HDFS_EXCEPTION = 5,
  SHOAL_ERROR_SOURCE_ILLEGAL_ARGUMENT_EXCEPTION = 6
};

typedef int32_t shoal_error_compatibility_class;

enum {
  SHOAL_ERROR_COMPATIBILITY_RUNTIME_ERROR = 0,
  SHOAL_ERROR_COMPATIBILITY_CLIENT_EXCEPTION = 1
};

typedef int32_t shoal_bootstrap;

enum {
  SHOAL_BOOTSTRAP_UNSPECIFIED = 0,
  SHOAL_BOOTSTRAP_STATIC = 1,
  SHOAL_BOOTSTRAP_ZOOKEEPER = 2
};

typedef struct shoal_connector shoal_connector;
typedef struct shoal_client shoal_client;
typedef struct shoal_cancellation shoal_cancellation;
typedef struct shoal_scanner shoal_scanner;
typedef struct shoal_batch_scanner shoal_batch_scanner;
typedef struct shoal_scan_result shoal_scan_result;
typedef struct shoal_scan_cursor shoal_scan_cursor;
typedef struct shoal_table_list_result shoal_table_list_result;
typedef struct shoal_mutation shoal_mutation;
typedef struct shoal_batch_writer shoal_batch_writer;
typedef struct shoal_write_failure shoal_write_failure;
typedef struct shoal_table_properties_result shoal_table_properties_result;
typedef struct shoal_namespace_list_result shoal_namespace_list_result;
typedef struct shoal_namespace_properties_result shoal_namespace_properties_result;
typedef struct shoal_versioned_properties_result shoal_versioned_properties_result;
typedef struct shoal_bytes_list_result shoal_bytes_list_result;
typedef struct shoal_connector_identity_result shoal_connector_identity_result;
typedef struct shoal_range_result shoal_range_result;
typedef struct shoal_iterator_setting_result shoal_iterator_setting_result;
typedef struct shoal_configuration shoal_configuration;
typedef struct shoal_bytes_result shoal_bytes_result;
typedef struct shoal_string_list_result shoal_string_list_result;
typedef struct shoal_server_list_result shoal_server_list_result;
typedef struct shoal_rfile_reader shoal_rfile_reader;
typedef struct shoal_rfile_writer shoal_rfile_writer;
typedef struct shoal_rfile_seekable shoal_rfile_seekable;
typedef struct shoal_rfile_entry_result shoal_rfile_entry_result;
typedef struct shoal_hdfs_client shoal_hdfs_client;
typedef struct shoal_hdfs_input_stream shoal_hdfs_input_stream;
typedef struct shoal_hdfs_output_stream shoal_hdfs_output_stream;
typedef struct shoal_hdfs_dir_entry_result shoal_hdfs_dir_entry_result;
typedef struct shoal_hdfs_dir_list_result shoal_hdfs_dir_list_result;
typedef struct shoal_authorizations shoal_authorizations;
typedef struct shoal_column_visibility shoal_column_visibility;
typedef struct shoal_owned_key shoal_owned_key;
typedef struct shoal_visibility_node shoal_visibility_node;
typedef struct shoal_node_expression shoal_node_expression;
typedef struct shoal_visibility_evaluator shoal_visibility_evaluator;
typedef struct shoal_key_value_result shoal_key_value_result;
typedef struct shoal_accumulo_writer shoal_accumulo_writer;
typedef struct shoal_table_constraint_list_result
    shoal_table_constraint_list_result;
typedef struct shoal_error shoal_error;

typedef struct shoal_connector_identity_view {
  uint32_t struct_size;
  const char *instance_name;
  const char *instance_id;
  const char *principal;
} shoal_connector_identity_view;

#define SHOAL_CONNECTOR_IDENTITY_VIEW_V1_SIZE                                \
  ((uint32_t)(offsetof(shoal_connector_identity_view, principal) +           \
              sizeof(((shoal_connector_identity_view *)0)->principal)))

typedef struct shoal_bytes {
  const uint8_t *data;
  size_t length;
} shoal_bytes;

typedef int32_t shoal_visibility_node_type;

enum {
  SHOAL_VISIBILITY_EMPTY = 0,
  SHOAL_VISIBILITY_TERM = 1,
  SHOAL_VISIBILITY_OR = 2,
  SHOAL_VISIBILITY_AND = 3
};

typedef struct shoal_visibility_node_view {
  uint32_t struct_size;
  shoal_visibility_node_type node_type;
  size_t child_count;
  size_t span_length;
  size_t term_start;
  size_t term_end;
  uint8_t empty;
} shoal_visibility_node_view;

#define SHOAL_VISIBILITY_NODE_VIEW_V1_SIZE                                   \
  ((uint32_t)(offsetof(shoal_visibility_node_view, empty) +                  \
              sizeof(((shoal_visibility_node_view *)0)->empty)))

typedef struct shoal_visibility_parse_error_view {
  uint32_t struct_size;
  shoal_bytes terms;
  const char *reason;
  size_t offset;
} shoal_visibility_parse_error_view;

#define SHOAL_VISIBILITY_PARSE_ERROR_VIEW_V1_SIZE                            \
  ((uint32_t)(offsetof(shoal_visibility_parse_error_view, offset) +          \
              sizeof(((shoal_visibility_parse_error_view *)0)->offset)))

typedef struct shoal_server_view {
  uint32_t struct_size;
  shoal_bytes kind;
  shoal_bytes group;
  shoal_bytes host;
  uint16_t port;
} shoal_server_view;

#define SHOAL_SERVER_VIEW_V1_SIZE                                            \
  ((uint32_t)(offsetof(shoal_server_view, port) +                            \
              sizeof(((shoal_server_view *)0)->port)))

typedef struct shoal_column {
  shoal_bytes family;
  shoal_bytes qualifier;
  uint8_t has_qualifier;
} shoal_column;

typedef struct shoal_iterator_option {
  const char *key;
  const char *value;
} shoal_iterator_option;

typedef struct shoal_iterator_setting {
  const char *name;
  const char *class_name;
  int32_t priority;
  const shoal_iterator_option *options;
  size_t option_count;
} shoal_iterator_setting;

typedef struct shoal_iterator_setting_view {
  uint32_t struct_size;
  const char *name;
  const char *class_name;
  int32_t priority;
  const shoal_iterator_option *options;
  size_t option_count;
} shoal_iterator_setting_view;

#define SHOAL_ITERATOR_SETTING_VIEW_V1_SIZE                                 \
  ((uint32_t)(offsetof(shoal_iterator_setting_view, option_count) +          \
              sizeof(((shoal_iterator_setting_view *)0)->option_count)))

typedef struct shoal_key {
  shoal_bytes row;
  shoal_bytes column_family;
  shoal_bytes column_qualifier;
  shoal_bytes column_visibility;
  int64_t timestamp;
} shoal_key;

typedef struct shoal_key_value {
  uint32_t struct_size;
  shoal_key key;
  shoal_bytes value;
} shoal_key_value;

#define SHOAL_KEY_VALUE_V1_SIZE                                              \
  ((uint32_t)(offsetof(shoal_key_value, value) +                             \
              sizeof(((shoal_key_value *)0)->value)))

typedef struct shoal_rfile_writer_config {
  uint32_t struct_size;
  const char *codec;
  int64_t block_size;
} shoal_rfile_writer_config;

#define SHOAL_RFILE_WRITER_CONFIG_V1_SIZE                                    \
  ((uint32_t)(offsetof(shoal_rfile_writer_config, block_size) +              \
              sizeof(((shoal_rfile_writer_config *)0)->block_size)))

typedef struct shoal_rfile_merge_config {
  uint32_t struct_size;
  int32_t versions;
  uint8_t apply_deletes;
  uint8_t propagate;
  int64_t min_timestamp;
} shoal_rfile_merge_config;

#define SHOAL_RFILE_MERGE_CONFIG_V1_SIZE                                     \
  ((uint32_t)(offsetof(shoal_rfile_merge_config, min_timestamp) +            \
              sizeof(((shoal_rfile_merge_config *)0)->min_timestamp)))

typedef struct shoal_rfile_entry {
  uint32_t struct_size;
  shoal_key key;
  shoal_bytes value;
  uint8_t deleted;
} shoal_rfile_entry;

#define SHOAL_RFILE_ENTRY_V1_SIZE                                            \
  ((uint32_t)(offsetof(shoal_rfile_entry, deleted) +                         \
              sizeof(((shoal_rfile_entry *)0)->deleted)))

typedef struct shoal_rfile_entry_view {
  uint32_t struct_size;
  shoal_key key;
  shoal_bytes value;
  uint8_t deleted;
} shoal_rfile_entry_view;

#define SHOAL_RFILE_ENTRY_VIEW_V1_SIZE                                       \
  ((uint32_t)(offsetof(shoal_rfile_entry_view, deleted) +                    \
              sizeof(((shoal_rfile_entry_view *)0)->deleted)))

typedef struct shoal_hdfs_dir_entry_view {
  uint32_t struct_size;
  const char *name;
  const char *owner;
  const char *group;
  int64_t size;
  int64_t modification_time_ms;
  uint32_t mode;
  uint8_t is_directory;
} shoal_hdfs_dir_entry_view;

#define SHOAL_HDFS_DIR_ENTRY_VIEW_V1_SIZE                                    \
  ((uint32_t)(offsetof(shoal_hdfs_dir_entry_view, is_directory) +            \
              sizeof(((shoal_hdfs_dir_entry_view *)0)->is_directory)))

typedef int32_t shoal_range_bound_kind;

enum {
  SHOAL_RANGE_BOUND_UNBOUNDED = 0,
  SHOAL_RANGE_BOUND_ROW = 1,
  SHOAL_RANGE_BOUND_KEY = 2
};

typedef struct shoal_range_bound {
  shoal_range_bound_kind kind;
  shoal_bytes row;
  shoal_key key;
} shoal_range_bound;

/*
 * All pointer fields are borrowed for the duration of
 * shoal_connector_create. The library copies every value it retains.
 *
 * shoal_connector_config_init initializes only the V1 prefix and sets
 * struct_size to SHOAL_CONNECTOR_CONFIG_V1_SIZE. Callers using appended fields
 * must initialize their full structure and set struct_size = sizeof(*config).
 * Future ABI versions may append fields; version 1 readers ignore bytes beyond
 * the known structure.
 */
typedef struct shoal_connector_config {
  uint32_t struct_size;
  shoal_bootstrap bootstrap;
  const char *instance_name;
  const char *instance_id;
  const char *zookeeper_servers;
  const char *principal;
  const uint8_t *password;
  size_t password_length;
  const char *accumulo_version;
  int64_t zookeeper_session_timeout_ms;
  int64_t bootstrap_timeout_ms;
  const char *instance_secret;
  int64_t dial_timeout_ms;
} shoal_connector_config;

#define SHOAL_CONNECTOR_CONFIG_V1_SIZE                                      \
  ((uint32_t)(offsetof(shoal_connector_config, dial_timeout_ms) +            \
              sizeof(((shoal_connector_config *)0)->dial_timeout_ms)))

/*
 * High-level client defaults mirror Sharkbite's facade: thread_count is 10,
 * table_name is optional until a scanner or writer is created, and an empty
 * authorization array means no labels. All retained values are copied.
 */
typedef struct shoal_client_config {
  uint32_t struct_size;
  const shoal_connector_config *connector;
  const char *table_name;
  const shoal_bytes *authorizations;
  size_t authorization_count;
  int32_t thread_count;
} shoal_client_config;

#define SHOAL_CLIENT_CONFIG_V1_SIZE                                          \
  ((uint32_t)(offsetof(shoal_client_config, thread_count) +                   \
              sizeof(((shoal_client_config *)0)->thread_count)))

/*
 * shoal_scanner_config_init initializes only the V1 prefix and sets struct_size
 * to SHOAL_SCANNER_CONFIG_V1_SIZE. Callers using appended fields must
 * initialize their full structure and set struct_size = sizeof(*config).
 * Future ABI versions may append fields; version 1 readers ignore bytes beyond
 * the known structure.
 *
 * Exactly one of table_name and table_id must be non-NULL and non-empty.
 * Arrays and all memory they reference are borrowed only during scanner
 * creation. Shoal copies every retained value.
 */
typedef struct shoal_scanner_config {
  uint32_t struct_size;
  const char *table_name;
  const char *table_id;
  int32_t batch_size;
  const shoal_bytes *authorizations;
  size_t authorization_count;
  const shoal_column *columns;
  size_t column_count;
  const shoal_iterator_setting *iterators;
  size_t iterator_count;
  int32_t parallelism;
  uint8_t use_multi_scan;
} shoal_scanner_config;

#define SHOAL_SCANNER_CONFIG_V1_SIZE                                        \
  ((uint32_t)(offsetof(shoal_scanner_config, use_multi_scan) +               \
              sizeof(((shoal_scanner_config *)0)->use_multi_scan)))

/*
 * shoal_range_init initializes only the V1 prefix and sets struct_size to
 * SHOAL_RANGE_V1_SIZE. Callers using appended fields must initialize their
 * full structure and set struct_size = sizeof(*range). Future ABI versions may
 * append fields; version 1 readers ignore bytes beyond the known structure.
 *
 * ROW bounds include/exclude an entire row. KEY bounds use full Accumulo key
 * ordering. A range may not mix ROW and KEY bound kinds, though either side
 * may be UNBOUNDED. Pointer fields are borrowed only during a scan call.
 */
typedef struct shoal_range {
  uint32_t struct_size;
  shoal_range_bound start;
  shoal_range_bound end;
  uint8_t start_inclusive;
  uint8_t end_inclusive;
} shoal_range;

#define SHOAL_RANGE_V1_SIZE                                                  \
  ((uint32_t)(offsetof(shoal_range, end_inclusive) +                         \
              sizeof(((shoal_range *)0)->end_inclusive)))

typedef struct shoal_range_view {
  uint32_t struct_size;
  shoal_range_bound_kind start_kind;
  uint8_t has_start_key;
  shoal_key start_key;
  shoal_range_bound_kind end_kind;
  uint8_t has_end_key;
  shoal_key end_key;
  uint8_t start_inclusive;
  uint8_t end_inclusive;
} shoal_range_view;

#define SHOAL_RANGE_VIEW_V1_SIZE                                             \
  ((uint32_t)(offsetof(shoal_range_view, end_inclusive) +                    \
              sizeof(((shoal_range_view *)0)->end_inclusive)))

/*
 * Every pointer in a key/value view is borrowed from its shoal_scan_result
 * and remains valid until that result is freed.
 */
typedef struct shoal_key_value_view {
  shoal_bytes row;
  shoal_bytes column_family;
  shoal_bytes column_qualifier;
  shoal_bytes column_visibility;
  int64_t timestamp;
  shoal_bytes value;
} shoal_key_value_view;

typedef struct shoal_table_view {
  const char *name;
  const char *id;
} shoal_table_view;

typedef struct shoal_table_property_view {
  const char *key;
  const char *value;
} shoal_table_property_view;

typedef struct shoal_table_constraint_view {
  uint32_t struct_size;
  int32_t number;
  const char *class_name;
} shoal_table_constraint_view;

#define SHOAL_TABLE_CONSTRAINT_VIEW_V1_SIZE                                  \
  ((uint32_t)(offsetof(shoal_table_constraint_view, class_name) +            \
              sizeof(((shoal_table_constraint_view *)0)->class_name)))

typedef struct shoal_namespace_view {
  const char *name;
  const char *id;
} shoal_namespace_view;

typedef int8_t shoal_system_permission;
enum {
  SHOAL_SYSTEM_PERMISSION_GRANT = 0,
  SHOAL_SYSTEM_PERMISSION_CREATE_TABLE = 1,
  SHOAL_SYSTEM_PERMISSION_DROP_TABLE = 2,
  SHOAL_SYSTEM_PERMISSION_ALTER_TABLE = 3,
  SHOAL_SYSTEM_PERMISSION_CREATE_USER = 4,
  SHOAL_SYSTEM_PERMISSION_DROP_USER = 5,
  SHOAL_SYSTEM_PERMISSION_ALTER_USER = 6,
  SHOAL_SYSTEM_PERMISSION_SYSTEM = 7,
  SHOAL_SYSTEM_PERMISSION_CREATE_NAMESPACE = 8,
  SHOAL_SYSTEM_PERMISSION_DROP_NAMESPACE = 9,
  SHOAL_SYSTEM_PERMISSION_ALTER_NAMESPACE = 10,
  SHOAL_SYSTEM_PERMISSION_OBTAIN_DELEGATION_TOKEN = 11
};

typedef int8_t shoal_table_permission;
enum {
  SHOAL_TABLE_PERMISSION_READ = 2,
  SHOAL_TABLE_PERMISSION_WRITE = 3,
  SHOAL_TABLE_PERMISSION_BULK_IMPORT = 4,
  SHOAL_TABLE_PERMISSION_ALTER_TABLE = 5,
  SHOAL_TABLE_PERMISSION_GRANT = 6,
  SHOAL_TABLE_PERMISSION_DROP_TABLE = 7,
  SHOAL_TABLE_PERMISSION_GET_SUMMARIES = 8
};

typedef int8_t shoal_namespace_permission;
enum {
  SHOAL_NAMESPACE_PERMISSION_READ = 0,
  SHOAL_NAMESPACE_PERMISSION_WRITE = 1,
  SHOAL_NAMESPACE_PERMISSION_ALTER_NAMESPACE = 2,
  SHOAL_NAMESPACE_PERMISSION_GRANT = 3,
  SHOAL_NAMESPACE_PERMISSION_ALTER_TABLE = 4,
  SHOAL_NAMESPACE_PERMISSION_CREATE_TABLE = 5,
  SHOAL_NAMESPACE_PERMISSION_DROP_TABLE = 6,
  SHOAL_NAMESPACE_PERMISSION_BULK_IMPORT = 7,
  SHOAL_NAMESPACE_PERMISSION_DROP_NAMESPACE = 8
};

typedef int32_t shoal_durability;

enum {
  SHOAL_DURABILITY_DEFAULT = 0,
  SHOAL_DURABILITY_SYNC = 1,
  SHOAL_DURABILITY_FLUSH = 2,
  SHOAL_DURABILITY_LOG = 3,
  SHOAL_DURABILITY_NONE = 4
};

/*
 * shoal_batch_writer_config_init initializes only the V1 prefix and sets
 * struct_size to SHOAL_BATCH_WRITER_CONFIG_V1_SIZE. Callers using appended
 * fields must initialize their full structure and set
 * struct_size = sizeof(*config). Future ABI versions may append fields;
 * version 1 readers ignore bytes beyond the known structure.
 */
typedef struct shoal_batch_writer_config {
  uint32_t struct_size;
  const char *table_name;
  const char *table_id;
  int64_t max_memory_bytes;
  int64_t max_batch_bytes;
  int64_t max_latency_ms;
  int32_t max_write_threads;
  int32_t max_retries;
  int64_t retry_backoff_ms;
  shoal_durability durability;
} shoal_batch_writer_config;

#define SHOAL_BATCH_WRITER_CONFIG_V1_SIZE                                   \
  ((uint32_t)(offsetof(shoal_batch_writer_config, durability) +              \
              sizeof(((shoal_batch_writer_config *)0)->durability)))

typedef uint32_t shoal_write_failure_flags;

enum {
  SHOAL_WRITE_FAILURE_AMBIGUOUS_COMMIT = 1u << 0,
  SHOAL_WRITE_FAILURE_RETRY_EXHAUSTED = 1u << 1,
  SHOAL_WRITE_FAILURE_AUTOMATIC_FLUSH = 1u << 2
};

typedef struct shoal_failed_extent_view {
  const char *server;
  const char *table_id;
  shoal_bytes prev_row;
  shoal_bytes end_row;
  uint8_t has_prev_row;
  uint8_t has_end_row;
  size_t submitted;
  int64_t committed;
} shoal_failed_extent_view;

typedef struct shoal_constraint_violation_view {
  const char *server;
  const char *constraint_class;
  int16_t violation_code;
  const char *description;
  int64_t violating_mutation_count;
} shoal_constraint_violation_view;

typedef struct shoal_authorization_failure_view {
  const char *server;
  const char *table_id;
  shoal_bytes prev_row;
  shoal_bytes end_row;
  uint8_t has_prev_row;
  uint8_t has_end_row;
  const char *code;
} shoal_authorization_failure_view;

typedef struct shoal_cleanup_failure_view {
  const char *server;
  const char *message;
} shoal_cleanup_failure_view;

#endif

