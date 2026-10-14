#ifndef SHOAL_CAPI_BRIDGE_H
#define SHOAL_CAPI_BRIDGE_H

#include "shoal_types.h"

struct shoal_connector {
  uint64_t id;
};

struct shoal_client {
  uint64_t id;
};

struct shoal_cancellation {
  uint64_t id;
};

struct shoal_scanner {
  uint64_t id;
};

struct shoal_batch_scanner {
  uint64_t id;
};

struct shoal_mutation {
  uint64_t id;
};

struct shoal_batch_writer {
  uint64_t id;
};

struct shoal_configuration {
  uint64_t id;
};

struct shoal_rfile_reader {
  uint64_t id;
};

struct shoal_rfile_writer {
  uint64_t id;
};

struct shoal_rfile_seekable {
  uint64_t id;
};

struct shoal_hdfs_client {
  uint64_t id;
};

struct shoal_hdfs_input_stream {
  uint64_t id;
};

struct shoal_hdfs_output_stream {
  uint64_t id;
};

struct shoal_authorizations {
  uint64_t id;
};

struct shoal_column_visibility {
  uint64_t id;
};

struct shoal_owned_key {
  uint64_t id;
};

struct shoal_visibility_node {
  uint64_t id;
};

struct shoal_node_expression {
  uint64_t id;
};

struct shoal_visibility_evaluator {
  uint64_t id;
};

struct shoal_accumulo_writer {
  uint64_t id;
};

struct shoal_error {
  shoal_status code;
  shoal_error_source_class source;
  shoal_error_compatibility_class compatibility;
  int16_t compatibility_code;
  char *message;
  char *security_user;
  char *security_code;
  uint8_t *visibility_terms;
  size_t visibility_terms_length;
  char *visibility_reason;
  size_t visibility_offset;
  uint8_t has_visibility_parse;
};

typedef struct shoal_bridge_scan_entry {
  uint8_t *row;
  size_t row_length;
  uint8_t *column_family;
  size_t column_family_length;
  uint8_t *column_qualifier;
  size_t column_qualifier_length;
  uint8_t *column_visibility;
  size_t column_visibility_length;
  int64_t timestamp;
  uint8_t *value;
  size_t value_length;
} shoal_bridge_scan_entry;

struct shoal_scan_result {
  size_t count;
  shoal_bridge_scan_entry *entries;
};

struct shoal_scan_cursor {
  uint64_t id;
};

typedef struct shoal_bridge_table_entry {
  char *name;
  char *id;
} shoal_bridge_table_entry;

struct shoal_table_list_result {
  size_t count;
  shoal_bridge_table_entry *entries;
};

typedef struct shoal_bridge_table_constraint_entry {
  int32_t number;
  char *class_name;
} shoal_bridge_table_constraint_entry;

struct shoal_table_constraint_list_result {
  size_t count;
  shoal_bridge_table_constraint_entry *entries;
};

typedef struct shoal_bridge_extent {
  char *server;
  char *table_id;
  uint8_t *prev_row;
  size_t prev_row_length;
  uint8_t *end_row;
  size_t end_row_length;
  uint8_t has_prev_row;
  uint8_t has_end_row;
} shoal_bridge_extent;

typedef struct shoal_bridge_failed_extent {
  shoal_bridge_extent extent;
  size_t submitted;
  int64_t committed;
} shoal_bridge_failed_extent;

typedef struct shoal_bridge_constraint_violation {
  char *server;
  char *constraint_class;
  int16_t violation_code;
  char *description;
  int64_t violating_mutation_count;
} shoal_bridge_constraint_violation;

typedef struct shoal_bridge_authorization_failure {
  shoal_bridge_extent extent;
  char *code;
} shoal_bridge_authorization_failure;

typedef struct shoal_bridge_cleanup_failure {
  char *server;
  char *message;
} shoal_bridge_cleanup_failure;

struct shoal_write_failure {
  shoal_write_failure_flags flags;
  size_t failed_extent_count;
  shoal_bridge_failed_extent *failed_extents;
  size_t constraint_count;
  shoal_bridge_constraint_violation *constraints;
  size_t authorization_count;
  shoal_bridge_authorization_failure *authorizations;
  size_t cleanup_count;
  shoal_bridge_cleanup_failure *cleanups;
};

typedef struct shoal_bridge_table_property_entry {
  char *key;
  char *value;
} shoal_bridge_table_property_entry;

struct shoal_table_properties_result {
  size_t count;
  shoal_bridge_table_property_entry *entries;
};

struct shoal_namespace_list_result {
  size_t count;
  shoal_bridge_table_entry *entries;
};

struct shoal_namespace_properties_result {
  size_t count;
  shoal_bridge_table_property_entry *entries;
};

struct shoal_versioned_properties_result {
  int64_t version;
  size_t count;
  shoal_bridge_table_property_entry *entries;
};

typedef struct shoal_bridge_bytes_entry {
  uint8_t *data;
  size_t length;
} shoal_bridge_bytes_entry;

struct shoal_bytes_list_result {
  size_t count;
  shoal_bridge_bytes_entry *entries;
};

struct shoal_connector_identity_result {
  char *instance_name;
  char *instance_id;
  char *principal;
};

struct shoal_range_result {
  shoal_range_bound_kind start_kind;
  uint8_t has_start_key;
  shoal_bridge_scan_entry start_key;
  shoal_range_bound_kind end_kind;
  uint8_t has_end_key;
  shoal_bridge_scan_entry end_key;
  uint8_t start_inclusive;
  uint8_t end_inclusive;
};

struct shoal_iterator_setting_result {
  char *name;
  char *class_name;
  int32_t priority;
  size_t option_count;
  shoal_iterator_option *options;
};

struct shoal_bytes_result {
  uint8_t *data;
  size_t length;
};

struct shoal_string_list_result {
  size_t count;
  shoal_bridge_bytes_entry *entries;
};

typedef struct shoal_bridge_server_entry {
  uint8_t *kind;
  size_t kind_length;
  uint8_t *group;
  size_t group_length;
  uint8_t *host;
  size_t host_length;
  uint16_t port;
} shoal_bridge_server_entry;

struct shoal_server_list_result {
  size_t count;
  shoal_bridge_server_entry *entries;
};

struct shoal_rfile_entry_result {
  shoal_bridge_scan_entry entry;
  uint8_t deleted;
};

struct shoal_key_value_result {
  shoal_bridge_scan_entry entry;
};

typedef struct shoal_bridge_hdfs_dir_entry {
  char *name;
  char *owner;
  char *group;
  int64_t size;
  int64_t modification_time_ms;
  uint32_t mode;
  uint8_t is_directory;
} shoal_bridge_hdfs_dir_entry;

struct shoal_hdfs_dir_entry_result {
  shoal_bridge_hdfs_dir_entry entry;
};

struct shoal_hdfs_dir_list_result {
  size_t count;
  shoal_bridge_hdfs_dir_entry *entries;
};

shoal_connector *shoal_bridge_connector_alloc(uint64_t id);
uint64_t shoal_bridge_connector_id(const shoal_connector *connector);
void shoal_bridge_connector_free(shoal_connector *connector);
shoal_client *shoal_bridge_client_alloc(uint64_t id);
uint64_t shoal_bridge_client_id(const shoal_client *client);
void shoal_bridge_client_free(shoal_client *client);
shoal_cancellation *shoal_bridge_cancellation_alloc(uint64_t id);
uint64_t
shoal_bridge_cancellation_id(const shoal_cancellation *cancellation);
void shoal_bridge_cancellation_free(shoal_cancellation *cancellation);

shoal_scanner *shoal_bridge_scanner_alloc(uint64_t id);
uint64_t shoal_bridge_scanner_id(const shoal_scanner *scanner);
void shoal_bridge_scanner_free(shoal_scanner *scanner);

shoal_batch_scanner *shoal_bridge_batch_scanner_alloc(uint64_t id);
uint64_t shoal_bridge_batch_scanner_id(const shoal_batch_scanner *scanner);
void shoal_bridge_batch_scanner_free(shoal_batch_scanner *scanner);

shoal_mutation *shoal_bridge_mutation_alloc(uint64_t id);
uint64_t shoal_bridge_mutation_id(const shoal_mutation *mutation);
void shoal_bridge_mutation_free(shoal_mutation *mutation);

shoal_batch_writer *shoal_bridge_batch_writer_alloc(uint64_t id);
uint64_t shoal_bridge_batch_writer_id(const shoal_batch_writer *writer);
void shoal_bridge_batch_writer_free(shoal_batch_writer *writer);

shoal_configuration *shoal_bridge_configuration_alloc(uint64_t id);
uint64_t shoal_bridge_configuration_id(const shoal_configuration *configuration);
void shoal_bridge_configuration_free(shoal_configuration *configuration);
shoal_rfile_reader *shoal_bridge_rfile_reader_alloc(uint64_t id);
uint64_t shoal_bridge_rfile_reader_id(const shoal_rfile_reader *reader);
void shoal_bridge_rfile_reader_free(shoal_rfile_reader *reader);
shoal_rfile_writer *shoal_bridge_rfile_writer_alloc(uint64_t id);
uint64_t shoal_bridge_rfile_writer_id(const shoal_rfile_writer *writer);
void shoal_bridge_rfile_writer_free(shoal_rfile_writer *writer);
shoal_rfile_seekable *shoal_bridge_rfile_seekable_alloc(uint64_t id);
uint64_t shoal_bridge_rfile_seekable_id(const shoal_rfile_seekable *seekable);
void shoal_bridge_rfile_seekable_free(shoal_rfile_seekable *seekable);
shoal_hdfs_client *shoal_bridge_hdfs_client_alloc(uint64_t id);
uint64_t shoal_bridge_hdfs_client_id(const shoal_hdfs_client *client);
void shoal_bridge_hdfs_client_free(shoal_hdfs_client *client);
shoal_hdfs_input_stream *
shoal_bridge_hdfs_input_stream_alloc(uint64_t id);
uint64_t
shoal_bridge_hdfs_input_stream_id(const shoal_hdfs_input_stream *stream);
void shoal_bridge_hdfs_input_stream_free(shoal_hdfs_input_stream *stream);
shoal_hdfs_output_stream *
shoal_bridge_hdfs_output_stream_alloc(uint64_t id);
uint64_t
shoal_bridge_hdfs_output_stream_id(const shoal_hdfs_output_stream *stream);
void shoal_bridge_hdfs_output_stream_free(shoal_hdfs_output_stream *stream);
shoal_authorizations *shoal_bridge_authorizations_alloc(uint64_t id);
uint64_t
shoal_bridge_authorizations_id(const shoal_authorizations *authorizations);
void shoal_bridge_authorizations_free(shoal_authorizations *authorizations);
shoal_column_visibility *shoal_bridge_column_visibility_alloc(uint64_t id);
uint64_t shoal_bridge_column_visibility_id(const shoal_column_visibility *value);
void shoal_bridge_column_visibility_free(shoal_column_visibility *value);
shoal_owned_key *shoal_bridge_owned_key_alloc(uint64_t id);
uint64_t shoal_bridge_owned_key_id(const shoal_owned_key *value);
void shoal_bridge_owned_key_free(shoal_owned_key *value);
shoal_visibility_node *shoal_bridge_visibility_node_alloc(uint64_t id);
uint64_t shoal_bridge_visibility_node_id(const shoal_visibility_node *value);
void shoal_bridge_visibility_node_free(shoal_visibility_node *value);
shoal_node_expression *shoal_bridge_node_expression_alloc(uint64_t id);
uint64_t shoal_bridge_node_expression_id(const shoal_node_expression *value);
void shoal_bridge_node_expression_free(shoal_node_expression *value);
shoal_visibility_evaluator *
shoal_bridge_visibility_evaluator_alloc(uint64_t id);
uint64_t
shoal_bridge_visibility_evaluator_id(const shoal_visibility_evaluator *value);
void shoal_bridge_visibility_evaluator_free(shoal_visibility_evaluator *value);
shoal_accumulo_writer *shoal_bridge_accumulo_writer_alloc(uint64_t id);
uint64_t
shoal_bridge_accumulo_writer_id(const shoal_accumulo_writer *writer);
void shoal_bridge_accumulo_writer_free(shoal_accumulo_writer *writer);

char *shoal_bridge_string_alloc(const char *value, size_t length);
void shoal_bridge_string_free(char *value);
shoal_bytes_result *shoal_bridge_bytes_result_alloc(const uint8_t *value,
                                                    size_t length);
shoal_bytes shoal_bridge_bytes_result_get(const shoal_bytes_result *result);
void shoal_bridge_bytes_result_free(shoal_bytes_result *result);
shoal_string_list_result *shoal_bridge_string_list_alloc(size_t count);
int shoal_bridge_string_list_set(shoal_string_list_result *result, size_t index,
                                 const uint8_t *value, size_t length);
size_t shoal_bridge_string_list_count(const shoal_string_list_result *result);
int shoal_bridge_string_list_get(const shoal_string_list_result *result,
                                 size_t index, shoal_bytes *out_value);
void shoal_bridge_string_list_free(shoal_string_list_result *result);
shoal_hdfs_dir_entry_result *shoal_bridge_hdfs_dir_entry_alloc(
    const char *name, const char *owner, const char *group, int64_t size,
    int64_t modification_time_ms, uint32_t mode, uint8_t is_directory);
int shoal_bridge_hdfs_dir_entry_get(const shoal_hdfs_dir_entry_result *result,
                                    shoal_hdfs_dir_entry_view *out_entry);
void shoal_bridge_hdfs_dir_entry_free(shoal_hdfs_dir_entry_result *result);
shoal_hdfs_dir_list_result *shoal_bridge_hdfs_dir_list_alloc(size_t count);
int shoal_bridge_hdfs_dir_list_set(
    shoal_hdfs_dir_list_result *result, size_t index, const char *name,
    const char *owner, const char *group, int64_t size,
    int64_t modification_time_ms, uint32_t mode, uint8_t is_directory);
size_t
shoal_bridge_hdfs_dir_list_count(const shoal_hdfs_dir_list_result *result);
int shoal_bridge_hdfs_dir_list_get(const shoal_hdfs_dir_list_result *result,
                                   size_t index,
                                   shoal_hdfs_dir_entry_view *out_entry);
void shoal_bridge_hdfs_dir_list_free(shoal_hdfs_dir_list_result *result);
shoal_server_list_result *shoal_bridge_server_list_alloc(size_t count);
int shoal_bridge_server_list_set(shoal_server_list_result *result, size_t index,
                                 const uint8_t *kind, size_t kind_length,
                                 const uint8_t *group, size_t group_length,
                                 const uint8_t *host, size_t host_length,
                                 uint16_t port);
size_t shoal_bridge_server_list_count(const shoal_server_list_result *result);
int shoal_bridge_server_list_get(const shoal_server_list_result *result,
                                 size_t index, shoal_server_view *out_server);
void shoal_bridge_server_list_free(shoal_server_list_result *result);
shoal_rfile_entry_result *shoal_bridge_rfile_entry_alloc(
    const uint8_t *row, size_t row_length, const uint8_t *column_family,
    size_t column_family_length, const uint8_t *column_qualifier,
    size_t column_qualifier_length, const uint8_t *column_visibility,
    size_t column_visibility_length, int64_t timestamp, const uint8_t *value,
    size_t value_length, uint8_t deleted);
int shoal_bridge_rfile_entry_get(const shoal_rfile_entry_result *result,
                                 shoal_rfile_entry_view *out_entry);
void shoal_bridge_rfile_entry_free(shoal_rfile_entry_result *result);
shoal_key_value_result *shoal_bridge_key_value_alloc(
    const uint8_t *row, size_t row_length, const uint8_t *column_family,
    size_t column_family_length, const uint8_t *column_qualifier,
    size_t column_qualifier_length, const uint8_t *column_visibility,
    size_t column_visibility_length, int64_t timestamp, const uint8_t *value,
    size_t value_length);
int shoal_bridge_key_value_get(const shoal_key_value_result *result,
                               shoal_key_value_view *out_value);
void shoal_bridge_key_value_free(shoal_key_value_result *result);
#ifdef SHOAL_CAPI_TEST
void shoal_bridge_test_string_alloc_fail_after(size_t successful_allocations);
void shoal_bridge_test_string_alloc_reset(void);
void shoal_bridge_test_result_alloc_fail_after(size_t successful_allocations);
void shoal_bridge_test_result_alloc_reset(void);
void shoal_bridge_test_error_alloc_fail_after(size_t successful_allocations);
void shoal_bridge_test_error_alloc_reset(void);
void shoal_bridge_test_error_message_alloc_fail_after(
    size_t successful_allocations);
void shoal_bridge_test_error_message_alloc_reset(void);
#endif

shoal_scan_result *shoal_bridge_scan_result_alloc(size_t count);
int shoal_bridge_scan_result_set(
    shoal_scan_result *result, size_t index, const uint8_t *row,
    size_t row_length, const uint8_t *column_family,
    size_t column_family_length, const uint8_t *column_qualifier,
    size_t column_qualifier_length, const uint8_t *column_visibility,
    size_t column_visibility_length, int64_t timestamp, const uint8_t *value,
    size_t value_length);
size_t shoal_bridge_scan_result_count(const shoal_scan_result *result);
int shoal_bridge_scan_result_get(const shoal_scan_result *result, size_t index,
                                 shoal_key_value_view *out_value);
void shoal_bridge_scan_result_free(shoal_scan_result *result);

shoal_scan_cursor *shoal_bridge_scan_cursor_alloc(uint64_t id);
uint64_t shoal_bridge_scan_cursor_id(const shoal_scan_cursor *cursor);
void shoal_bridge_scan_cursor_free(shoal_scan_cursor *cursor);

shoal_connector_identity_result *shoal_bridge_connector_identity_alloc(
    char *instance_name, char *instance_id, char *principal);
int shoal_bridge_connector_identity_get(
    const shoal_connector_identity_result *result,
    shoal_connector_identity_view *out_identity);
void shoal_bridge_connector_identity_free(
    shoal_connector_identity_result *result);

shoal_range_result *shoal_bridge_range_result_alloc(void);
int shoal_bridge_range_result_set_start(
    shoal_range_result *result, const uint8_t *row, size_t row_length,
    const uint8_t *column_family, size_t column_family_length,
    const uint8_t *column_qualifier, size_t column_qualifier_length,
    const uint8_t *column_visibility, size_t column_visibility_length,
    int64_t timestamp);
int shoal_bridge_range_result_set_end(
    shoal_range_result *result, const uint8_t *row, size_t row_length,
    const uint8_t *column_family, size_t column_family_length,
    const uint8_t *column_qualifier, size_t column_qualifier_length,
    const uint8_t *column_visibility, size_t column_visibility_length,
    int64_t timestamp);
void shoal_bridge_range_result_set_metadata(
    shoal_range_result *result, shoal_range_bound_kind start_kind,
    shoal_range_bound_kind end_kind, uint8_t start_inclusive,
    uint8_t end_inclusive);
int shoal_bridge_range_result_get(const shoal_range_result *result,
                                  shoal_range_view *out_range);
void shoal_bridge_range_result_free(shoal_range_result *result);

shoal_iterator_setting_result *
shoal_bridge_iterator_setting_result_alloc(size_t option_count);
int shoal_bridge_iterator_setting_result_set_identity(
    shoal_iterator_setting_result *result, const char *name,
    const char *class_name, int32_t priority);
int shoal_bridge_iterator_setting_result_set_option(
    shoal_iterator_setting_result *result, size_t index, const char *key,
    const char *value);
int shoal_bridge_iterator_setting_result_get(
    const shoal_iterator_setting_result *result,
    shoal_iterator_setting_view *out_setting);
void shoal_bridge_iterator_setting_result_free(
    shoal_iterator_setting_result *result);

shoal_table_list_result *shoal_bridge_table_list_alloc(size_t count);
int shoal_bridge_table_list_set(shoal_table_list_result *result, size_t index,
                                const char *name, const char *id);
size_t shoal_bridge_table_list_count(const shoal_table_list_result *result);
int shoal_bridge_table_list_get(const shoal_table_list_result *result,
                                size_t index, shoal_table_view *out_table);
void shoal_bridge_table_list_free(shoal_table_list_result *result);

shoal_table_constraint_list_result *
shoal_bridge_table_constraint_list_alloc(size_t count);
int shoal_bridge_table_constraint_list_set(
    shoal_table_constraint_list_result *result, size_t index, int32_t number,
    const char *class_name);
size_t shoal_bridge_table_constraint_list_count(
    const shoal_table_constraint_list_result *result);
int shoal_bridge_table_constraint_list_get(
    const shoal_table_constraint_list_result *result, size_t index,
    shoal_table_constraint_view *out_constraint);
void shoal_bridge_table_constraint_list_free(
    shoal_table_constraint_list_result *result);

shoal_write_failure *shoal_bridge_write_failure_alloc(
    shoal_write_failure_flags flags, size_t failed_extent_count,
    size_t constraint_count, size_t authorization_count, size_t cleanup_count);
int shoal_bridge_write_failure_set_failed_extent(
    shoal_write_failure *failure, size_t index, const char *server,
    const char *table_id, const uint8_t *prev_row, size_t prev_row_length,
    uint8_t has_prev_row, const uint8_t *end_row, size_t end_row_length,
    uint8_t has_end_row, size_t submitted, int64_t committed);
int shoal_bridge_write_failure_set_constraint(
    shoal_write_failure *failure, size_t index, const char *server,
    const char *constraint_class, int16_t violation_code,
    const char *description, int64_t violating_mutation_count);
int shoal_bridge_write_failure_set_authorization(
    shoal_write_failure *failure, size_t index, const char *server,
    const char *table_id, const uint8_t *prev_row, size_t prev_row_length,
    uint8_t has_prev_row, const uint8_t *end_row, size_t end_row_length,
    uint8_t has_end_row, const char *code);
int shoal_bridge_write_failure_set_cleanup(shoal_write_failure *failure,
                                           size_t index, const char *server,
                                           const char *message);
shoal_write_failure_flags shoal_bridge_write_failure_flags(
    const shoal_write_failure *failure);
size_t shoal_bridge_write_failure_failed_extent_count(
    const shoal_write_failure *failure);
int shoal_bridge_write_failure_get_failed_extent(
    const shoal_write_failure *failure, size_t index,
    shoal_failed_extent_view *out_extent);
size_t shoal_bridge_write_failure_constraint_count(
    const shoal_write_failure *failure);
int shoal_bridge_write_failure_get_constraint(
    const shoal_write_failure *failure, size_t index,
    shoal_constraint_violation_view *out_violation);
size_t shoal_bridge_write_failure_authorization_count(
    const shoal_write_failure *failure);
int shoal_bridge_write_failure_get_authorization(
    const shoal_write_failure *failure, size_t index,
    shoal_authorization_failure_view *out_failure);
size_t shoal_bridge_write_failure_cleanup_count(
    const shoal_write_failure *failure);
int shoal_bridge_write_failure_get_cleanup(
    const shoal_write_failure *failure, size_t index,
    shoal_cleanup_failure_view *out_failure);
void shoal_bridge_write_failure_free(shoal_write_failure *failure);

shoal_table_properties_result *shoal_bridge_table_properties_alloc(size_t count);
int shoal_bridge_table_properties_set(shoal_table_properties_result *result,
                                      size_t index, const char *key,
                                      const char *value);
size_t shoal_bridge_table_properties_count(
    const shoal_table_properties_result *result);
int shoal_bridge_table_properties_get(
    const shoal_table_properties_result *result, size_t index,
    shoal_table_property_view *out_property);
void shoal_bridge_table_properties_free(shoal_table_properties_result *result);

shoal_namespace_list_result *shoal_bridge_namespace_list_alloc(size_t count);
int shoal_bridge_namespace_list_set(shoal_namespace_list_result *result,
                                    size_t index, const char *name,
                                    const char *id);
size_t shoal_bridge_namespace_list_count(
    const shoal_namespace_list_result *result);
int shoal_bridge_namespace_list_get(const shoal_namespace_list_result *result,
                                    size_t index,
                                    shoal_namespace_view *out_namespace);
void shoal_bridge_namespace_list_free(shoal_namespace_list_result *result);

shoal_namespace_properties_result *
shoal_bridge_namespace_properties_alloc(size_t count);
int shoal_bridge_namespace_properties_set(
    shoal_namespace_properties_result *result, size_t index, const char *key,
    const char *value);
size_t shoal_bridge_namespace_properties_count(
    const shoal_namespace_properties_result *result);
int shoal_bridge_namespace_properties_get(
    const shoal_namespace_properties_result *result, size_t index,
    shoal_table_property_view *out_property);
void shoal_bridge_namespace_properties_free(
    shoal_namespace_properties_result *result);

shoal_versioned_properties_result *
shoal_bridge_versioned_properties_alloc(int64_t version, size_t count);
int shoal_bridge_versioned_properties_set(
    shoal_versioned_properties_result *result, size_t index, const char *key,
    const char *value);
int64_t shoal_bridge_versioned_properties_version(
    const shoal_versioned_properties_result *result);
size_t shoal_bridge_versioned_properties_count(
    const shoal_versioned_properties_result *result);
int shoal_bridge_versioned_properties_get(
    const shoal_versioned_properties_result *result, size_t index,
    shoal_table_property_view *out_property);
void shoal_bridge_versioned_properties_free(
    shoal_versioned_properties_result *result);

shoal_bytes_list_result *shoal_bridge_bytes_list_alloc(size_t count);
int shoal_bridge_bytes_list_set(shoal_bytes_list_result *result, size_t index,
                                const uint8_t *data, size_t length);
size_t shoal_bridge_bytes_list_count(const shoal_bytes_list_result *result);
int shoal_bridge_bytes_list_get(const shoal_bytes_list_result *result,
                                size_t index, shoal_bytes *out_value);
void shoal_bridge_bytes_list_free(shoal_bytes_list_result *result);

shoal_error *shoal_bridge_error_alloc(
    shoal_status code, shoal_error_source_class source,
    shoal_error_compatibility_class compatibility, int16_t compatibility_code,
    const char *message,
    size_t message_length,
    const char *security_user, size_t security_user_length,
    const char *security_code, size_t security_code_length,
    const uint8_t *visibility_terms, size_t visibility_terms_length,
    const char *visibility_reason, size_t visibility_reason_length,
    size_t visibility_offset, uint8_t has_visibility_parse);
shoal_status shoal_bridge_error_code(const shoal_error *error);
char *shoal_bridge_error_message(const shoal_error *error);
char *shoal_bridge_error_security_user(const shoal_error *error);
char *shoal_bridge_error_security_code(const shoal_error *error);
shoal_error_source_class shoal_bridge_error_source(const shoal_error *error);
const char *shoal_bridge_error_source_name(const shoal_error *error);
shoal_error_compatibility_class
shoal_bridge_error_compatibility(const shoal_error *error);
const char *shoal_bridge_error_compatibility_name(const shoal_error *error);
int16_t shoal_bridge_error_compatibility_code(const shoal_error *error);
int shoal_bridge_error_visibility_parse(
    const shoal_error *error, shoal_visibility_parse_error_view *out_details);
void shoal_bridge_error_free(shoal_error *error);
void shoal_bridge_log_callback_invoke(shoal_log_callback callback,
                                      shoal_log_level level,
                                      const char *event_name,
                                      const char *attributes_json,
                                      void *context);

void shoal_bridge_connector_config_init(shoal_connector_config *config);
void shoal_bridge_client_config_init(shoal_client_config *config);
uint32_t shoal_bridge_client_config_v1_size(void);
uint32_t shoal_bridge_connector_config_v1_size(void);
void shoal_bridge_scanner_config_init(shoal_scanner_config *config);
uint32_t shoal_bridge_scanner_config_v1_size(void);
void shoal_bridge_range_init(shoal_range *range);
uint32_t shoal_bridge_range_v1_size(void);
void shoal_bridge_batch_writer_config_init(shoal_batch_writer_config *config);
uint32_t shoal_bridge_batch_writer_config_v1_size(void);

#endif
