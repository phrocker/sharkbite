#ifndef SHOAL_H
#define SHOAL_H

#include "shoal_types.h"

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Legacy compatibility-major query preserved for existing callers. Use the
 * version and capability queries below when negotiating additive features.
 */
SHOAL_API uint32_t SHOAL_CALL shoal_abi_version(void);

/*
 * These queries are deterministic, allocation-free, thread-safe, and valid
 * before any connector or other handle is created. When a caller may run
 * against an older library, every additive symbol not guaranteed by that
 * library must be dynamically resolved before use. Capability checks govern
 * feature availability but do not make hard symbol references load-safe.
 */
SHOAL_API uint32_t SHOAL_CALL shoal_abi_version_major(void);
SHOAL_API uint32_t SHOAL_CALL shoal_abi_version_minor(void);
SHOAL_API uint32_t SHOAL_CALL shoal_abi_version_patch(void);
SHOAL_API uint32_t SHOAL_CALL shoal_abi_version_packed(void);

/*
 * Capability identifiers are append-only. word_count reports how many 64-bit
 * words the current library uses. shoal_abi_capability_word() returns 0 for
 * word_index values beyond that range. shoal_abi_has_capability() returns 1
 * for supported capabilities and 0 for unsupported or unknown identifiers.
 */
SHOAL_API uint32_t SHOAL_CALL shoal_abi_capability_count(void);
SHOAL_API uint32_t SHOAL_CALL shoal_abi_capability_word_count(void);
SHOAL_API shoal_abi_capability_bits SHOAL_CALL
shoal_abi_capability_word(uint32_t word_index);
SHOAL_API uint8_t SHOAL_CALL
shoal_abi_has_capability(shoal_abi_capability_id capability_id);

/*
 * Compatibility error classification is allocation-free and deterministic.
 * Source class records the closest Sharkbite C++ error category. Compatibility
 * class records the Python exception a compatibility binding must raise:
 * ClientException is distinct from RuntimeError. Returned names are immutable
 * library-owned C strings valid for the lifetime of the loaded library.
 * Concurrent getters on one owned error are safe; free must be externally
 * serialized and remains NULL-safe and idempotent.
 */
SHOAL_API shoal_error_source_class SHOAL_CALL
shoal_error_source(const shoal_error *error);
SHOAL_API const char *SHOAL_CALL
shoal_error_source_name(const shoal_error *error);
SHOAL_API shoal_error_compatibility_class SHOAL_CALL
shoal_error_compatibility(const shoal_error *error);
SHOAL_API const char *SHOAL_CALL
shoal_error_compatibility_name(const shoal_error *error);
SHOAL_API int16_t SHOAL_CALL
shoal_error_compatibility_code(const shoal_error *error);

/*
 * Data-value operations are local and do not take deadlines. Every byte input
 * is copied before return. Authorizations handles are immutable after
 * construction, so concurrent getters are safe; free must be externally
 * serialized and clears the caller's handle. All result frees are NULL-safe
 * and idempotent.
 */
SHOAL_API void SHOAL_CALL
shoal_key_value_init(shoal_key_value *value);
SHOAL_API shoal_status SHOAL_CALL
shoal_key_to_string(const shoal_key *key, shoal_bytes_result **out_result,
                    shoal_error **out_error);

/*
 * Owned keys copy every byte input and never expose Go memory. Getters return
 * independently owned byte results. Getters, setters, cloning, comparison and
 * size queries may run concurrently on live handles. Free is NULL-safe,
 * idempotent and must be externally serialized with calls on that handle.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_create(shoal_bytes row, shoal_owned_key **out_key,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_create_full(shoal_bytes row, shoal_bytes column_family,
                            shoal_bytes column_qualifier,
                            shoal_bytes column_visibility, int64_t timestamp,
                            shoal_owned_key **out_key,
                            shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_clone(shoal_owned_key *key, shoal_owned_key **out_key,
                      shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_empty(shoal_owned_key *key, uint8_t *out_value,
                      shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_set_row(shoal_owned_key *key, shoal_bytes value,
                        shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_set_column_family(shoal_owned_key *key, shoal_bytes value,
                                  shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_set_column_qualifier(shoal_owned_key *key, shoal_bytes value,
                                     shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_set_column_visibility(shoal_owned_key *key, shoal_bytes value,
                                      shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_row(shoal_owned_key *key, shoal_bytes_result **out_result,
                    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_column_family(shoal_owned_key *key,
                              shoal_bytes_result **out_result,
                              shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_column_qualifier(shoal_owned_key *key,
                                 shoal_bytes_result **out_result,
                                 shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_column_visibility(shoal_owned_key *key,
                                  shoal_bytes_result **out_result,
                                  shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_set_timestamp(shoal_owned_key *key, int64_t timestamp,
                              shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_timestamp(shoal_owned_key *key, int64_t *out_timestamp,
                          shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_is_deleted(shoal_owned_key *key, uint8_t *out_value,
                           shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_set_deleted(shoal_owned_key *key, uint8_t deleted,
                            shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_compare(shoal_owned_key *left, shoal_owned_key *right,
                        int32_t *out_order, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_compare_visibility(shoal_owned_key *left,
                                   shoal_owned_key *right,
                                   int32_t *out_order,
                                   shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_less(shoal_owned_key *left, shoal_owned_key *right,
                     uint8_t *out_value, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_less_or_equal(shoal_owned_key *left, shoal_owned_key *right,
                              uint8_t *out_value, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_equal(shoal_owned_key *left, shoal_owned_key *right,
                      uint8_t *out_value, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_not_equal(shoal_owned_key *left, shoal_owned_key *right,
                          uint8_t *out_value, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_size(shoal_owned_key *key, size_t *out_size,
                     shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_length(shoal_owned_key *key, size_t *out_size,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_row_size(shoal_owned_key *key, size_t *out_size,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_column_family_size(shoal_owned_key *key, size_t *out_size,
                                   shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_column_qualifier_size(shoal_owned_key *key, size_t *out_size,
                                      shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_owned_key_column_visibility_size(shoal_owned_key *key, size_t *out_size,
                                       shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_owned_key_free(shoal_owned_key **key);
SHOAL_API shoal_status SHOAL_CALL
shoal_range_after_end_key(const shoal_range *range, const shoal_key *key,
                          uint8_t *out_value, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_range_before_start_key(const shoal_range *range, const shoal_key *key,
                             uint8_t *out_value, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_range_to_string(const shoal_range *range,
                      shoal_bytes_result **out_result,
                      shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_key_value_create(const shoal_key_value *value,
                       shoal_key_value_result **out_result,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_key_value_result_get(const shoal_key_value_result *result,
                           shoal_key_value_view *out_value,
                           shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_key_value_result_free(shoal_key_value_result **result);

SHOAL_API shoal_status SHOAL_CALL
shoal_authorizations_create(const shoal_bytes *labels, size_t label_count,
                            shoal_authorizations **out_authorizations,
                            shoal_error **out_error);
SHOAL_API uint8_t SHOAL_CALL
shoal_authorizations_contains(const shoal_authorizations *authorizations,
                              shoal_bytes label);
SHOAL_API size_t SHOAL_CALL
shoal_authorizations_count(const shoal_authorizations *authorizations);
SHOAL_API shoal_status SHOAL_CALL
shoal_authorizations_list(const shoal_authorizations *authorizations,
                          shoal_bytes_list_result **out_result,
                          shoal_error **out_error);
SHOAL_API uint8_t SHOAL_CALL
shoal_authorizations_empty(const shoal_authorizations *authorizations);
SHOAL_API uint8_t SHOAL_CALL
shoal_authorizations_equal(const shoal_authorizations *left,
                           const shoal_authorizations *right);
SHOAL_API uint8_t SHOAL_CALL
shoal_authorization_character_is_valid(uint8_t character);
SHOAL_API void SHOAL_CALL
shoal_authorizations_free(shoal_authorizations **authorizations);

/*
 * Column-visibility inputs are binary-safe and copied before return. Visibility,
 * node, and node-expression handles are immutable and support concurrent
 * getters. Evaluators synchronize evaluation with authorization replacement.
 * Free is NULL-safe/idempotent and must be serialized with calls using the same
 * handle. Every returned result or handle is independently owned.
 */
SHOAL_API void SHOAL_CALL
shoal_visibility_node_view_init(shoal_visibility_node_view *view);
SHOAL_API void SHOAL_CALL shoal_visibility_parse_error_view_init(
    shoal_visibility_parse_error_view *view);
SHOAL_API shoal_status SHOAL_CALL
shoal_column_visibility_create(shoal_bytes expression,
                               shoal_column_visibility **out_visibility,
                               shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_column_visibility_expression(const shoal_column_visibility *visibility,
                                   shoal_bytes_result **out_result,
                                   shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_column_visibility_tree(const shoal_column_visibility *visibility,
                             shoal_visibility_node **out_node,
                             shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_column_visibility_normalized(const shoal_column_visibility *visibility,
                                   shoal_visibility_node **out_node,
                                   shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_column_visibility_flatten(const shoal_column_visibility *visibility,
                                shoal_bytes_result **out_result,
                                shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_column_visibility_free(shoal_column_visibility **visibility);

SHOAL_API shoal_status SHOAL_CALL
shoal_node_expression_create(shoal_bytes expression, size_t offset,
                             size_t size,
                             shoal_node_expression **out_expression,
                             shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_node_expression_term(const shoal_node_expression *expression,
                           shoal_bytes_result **out_result,
                           shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_node_expression_buffer(const shoal_node_expression *expression,
                             shoal_bytes_result **out_result,
                             shoal_error **out_error);
SHOAL_API size_t SHOAL_CALL
shoal_node_expression_size(const shoal_node_expression *expression);
SHOAL_API void SHOAL_CALL
shoal_node_expression_free(shoal_node_expression **expression);

SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_node_get(const shoal_visibility_node *node,
                          shoal_visibility_node_view *out_node,
                          shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_node_expression(const shoal_visibility_node *node,
                                 shoal_bytes_result **out_result,
                                 shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_node_child(const shoal_visibility_node *node, size_t index,
                            shoal_visibility_node **out_child,
                            shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_node_term(const shoal_visibility_node *node,
                           shoal_bytes expression,
                           shoal_node_expression **out_expression,
                           shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_node_compare(const shoal_visibility_node *left,
                              const shoal_visibility_node *right,
                              int32_t *out_comparison,
                              shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_visibility_node_free(shoal_visibility_node **node);

SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_evaluator_create(
    const shoal_authorizations *authorizations,
    shoal_visibility_evaluator **out_evaluator, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_evaluator_authorizations(
    const shoal_visibility_evaluator *evaluator,
    shoal_authorizations **out_authorizations, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_evaluator_set_authorizations(
    shoal_visibility_evaluator *evaluator,
    const shoal_authorizations *authorizations, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_evaluator_evaluate(
    shoal_visibility_evaluator *evaluator, shoal_bytes expression,
    uint8_t *out_satisfied, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_visibility_evaluator_evaluate_tree(
    shoal_visibility_evaluator *evaluator, shoal_bytes expression,
    const shoal_visibility_node *node, uint8_t *out_satisfied,
    shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_visibility_evaluator_free(shoal_visibility_evaluator **evaluator);

SHOAL_API void SHOAL_CALL
shoal_rfile_writer_config_init(shoal_rfile_writer_config *config);
SHOAL_API void SHOAL_CALL
shoal_rfile_merge_config_init(shoal_rfile_merge_config *config);
SHOAL_API void SHOAL_CALL shoal_rfile_entry_init(shoal_rfile_entry *entry);
SHOAL_API void SHOAL_CALL
shoal_rfile_entry_view_init(shoal_rfile_entry_view *view);

/*
 * Standalone RFile operations copy all caller inputs. timeout_ms is zero for
 * no deadline and must not be negative. Handles own their files; close is
 * idempotent, cancels active operations, and free performs bounded best-effort
 * close before setting the caller's handle to NULL.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_writer_create(const char *path,
                          const shoal_rfile_writer_config *config,
                          int64_t timeout_ms, shoal_rfile_writer **out_writer,
                          shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_writer_append(shoal_rfile_writer *writer,
                          const shoal_rfile_entry *entry, int64_t timeout_ms,
                          shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_writer_add_locality_group(shoal_rfile_writer *writer,
                                      const char *name, int64_t timeout_ms,
                                      shoal_error **out_error);
SHOAL_API int64_t SHOAL_CALL
shoal_rfile_writer_entries(shoal_rfile_writer *writer);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_writer_close(shoal_rfile_writer *writer, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_rfile_writer_free(shoal_rfile_writer **writer);

SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_open(const char *path, int64_t timeout_ms,
                        shoal_rfile_reader **out_reader,
                        shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_open_sequential(const char *path, int64_t timeout_ms,
                                   shoal_rfile_reader **out_reader,
                                   shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_open_many(const char *const *paths, size_t path_count,
                             const shoal_rfile_merge_config *config,
                             int64_t timeout_ms,
                             shoal_rfile_reader **out_reader,
                             shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_seek(shoal_rfile_reader *reader,
                        const shoal_rfile_seekable *seekable,
                        int64_t timeout_ms, shoal_error **out_error);
SHOAL_API uint8_t SHOAL_CALL
shoal_rfile_reader_has_top(shoal_rfile_reader *reader);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_top(shoal_rfile_reader *reader,
                       shoal_rfile_entry_result **out_result,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_top_key(shoal_rfile_reader *reader,
                           shoal_rfile_entry_result **out_result,
                           shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_top_value(shoal_rfile_reader *reader,
                             shoal_bytes_result **out_result,
                             shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_next(shoal_rfile_reader *reader, int64_t timeout_ms,
                        shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_reader_close(shoal_rfile_reader *reader, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_rfile_reader_free(shoal_rfile_reader **reader);

SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_seekable_create(const shoal_range *range,
                            const shoal_bytes *column_families,
                            size_t column_family_count, uint8_t inclusive,
                            shoal_rfile_seekable **out_seekable,
                            shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_seekable_get_range(const shoal_rfile_seekable *seekable,
                               shoal_range_result **out_result,
                               shoal_error **out_error);
SHOAL_API size_t SHOAL_CALL
shoal_rfile_seekable_column_family_count(
    const shoal_rfile_seekable *seekable);
SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_seekable_get_column_family(
    const shoal_rfile_seekable *seekable, size_t index,
    shoal_bytes_result **out_result, shoal_error **out_error);
SHOAL_API uint8_t SHOAL_CALL
shoal_rfile_seekable_is_inclusive(const shoal_rfile_seekable *seekable);
SHOAL_API void SHOAL_CALL
shoal_rfile_seekable_free(shoal_rfile_seekable **seekable);

SHOAL_API shoal_status SHOAL_CALL
shoal_rfile_entry_result_get(const shoal_rfile_entry_result *result,
                             shoal_rfile_entry_view *out_entry,
                             shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_rfile_entry_result_free(shoal_rfile_entry_result **result);

/*
 * HDFS clients use the process's Hadoop configuration and established
 * authentication. No credential bytes cross this ABI. All paths and write
 * bytes are copied before return. Handles are owned; close is idempotent,
 * cancels active operations, and free clears the caller's handle.
 */
SHOAL_API void SHOAL_CALL
shoal_hdfs_dir_entry_view_init(shoal_hdfs_dir_entry_view *view);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_create(const char *namenode, int64_t timeout_ms,
                         shoal_hdfs_client **out_client,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_close(shoal_hdfs_client *client, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_hdfs_client_free(shoal_hdfs_client **client);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_open(shoal_hdfs_client *client, const char *path,
                       int64_t timeout_ms,
                       shoal_hdfs_input_stream **out_stream,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_create_file(shoal_hdfs_client *client, const char *path,
                              int64_t timeout_ms,
                              shoal_hdfs_output_stream **out_stream,
                              shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_list(shoal_hdfs_client *client, const char *path,
                       int64_t timeout_ms,
                       shoal_hdfs_dir_list_result **out_result,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_stat(shoal_hdfs_client *client, const char *path,
                       int64_t timeout_ms,
                       shoal_hdfs_dir_entry_result **out_result,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_remove(shoal_hdfs_client *client, const char *path,
                         uint8_t recursive, int64_t timeout_ms,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_rename(shoal_hdfs_client *client, const char *old_path,
                         const char *new_path, int64_t timeout_ms,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_mkdir(shoal_hdfs_client *client, const char *path,
                        int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_client_chown(shoal_hdfs_client *client, const char *path,
                        const char *owner, const char *group,
                        int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_input_stream_read(shoal_hdfs_input_stream *stream, size_t length,
                             int64_t timeout_ms,
                             shoal_bytes_result **out_result,
                             shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_input_stream_close(shoal_hdfs_input_stream *stream,
                              shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_hdfs_input_stream_free(shoal_hdfs_input_stream **stream);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_output_stream_write(shoal_hdfs_output_stream *stream,
                               shoal_bytes value, int64_t timeout_ms,
                               size_t *out_written, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_output_stream_close(shoal_hdfs_output_stream *stream,
                               shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_hdfs_output_stream_free(shoal_hdfs_output_stream **stream);
SHOAL_API size_t SHOAL_CALL
shoal_hdfs_dir_list_count(const shoal_hdfs_dir_list_result *result);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_dir_list_get(const shoal_hdfs_dir_list_result *result, size_t index,
                        shoal_hdfs_dir_entry_view *out_entry,
                        shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_hdfs_dir_list_free(shoal_hdfs_dir_list_result **result);
SHOAL_API shoal_status SHOAL_CALL
shoal_hdfs_dir_entry_result_get(const shoal_hdfs_dir_entry_result *result,
                                shoal_hdfs_dir_entry_view *out_entry,
                                shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_hdfs_dir_entry_result_free(shoal_hdfs_dir_entry_result **result);

/*
 * Configuration inputs are binary-safe and copied before return. Handles are
 * owned, safe for concurrent getters/setters, and released idempotently by
 * shoal_configuration_free. Result views borrow owned result memory.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_configuration_create(shoal_configuration **out_configuration,
                           shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_configuration_set(shoal_configuration *configuration, shoal_bytes name,
                        shoal_bytes value, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_configuration_get(shoal_configuration *configuration, shoal_bytes name,
                        shoal_bytes_result **out_result,
                        shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_configuration_get_or(shoal_configuration *configuration,
                           shoal_bytes name, shoal_bytes default_value,
                           shoal_bytes_result **out_result,
                           shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_configuration_get_uint32(shoal_configuration *configuration,
                               shoal_bytes name, uint32_t *out_value,
                               shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_configuration_get_uint32_or(shoal_configuration *configuration,
                                  shoal_bytes name, uint32_t default_value,
                                  uint32_t *out_value,
                                  shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_configuration_free(shoal_configuration **configuration);

SHOAL_API shoal_bytes SHOAL_CALL
shoal_bytes_result_get(const shoal_bytes_result *result);
SHOAL_API void SHOAL_CALL shoal_bytes_result_free(shoal_bytes_result **result);
SHOAL_API size_t SHOAL_CALL
shoal_string_list_count(const shoal_string_list_result *result);
SHOAL_API shoal_status SHOAL_CALL
shoal_string_list_get(const shoal_string_list_result *result, size_t index,
                      shoal_bytes *out_value, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_string_list_free(shoal_string_list_result **result);
SHOAL_API void SHOAL_CALL shoal_server_view_init(shoal_server_view *view);
SHOAL_API size_t SHOAL_CALL
shoal_server_list_count(const shoal_server_list_result *result);
SHOAL_API shoal_status SHOAL_CALL
shoal_server_list_get(const shoal_server_list_result *result, size_t index,
                      shoal_server_view *out_server, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_server_list_free(shoal_server_list_result **result);

SHOAL_API void SHOAL_CALL
shoal_connector_config_init(shoal_connector_config *config);
SHOAL_API void SHOAL_CALL
shoal_client_config_init(shoal_client_config *config);

SHOAL_API void SHOAL_CALL
shoal_scanner_config_init(shoal_scanner_config *config);

SHOAL_API void SHOAL_CALL shoal_range_init(shoal_range *range);

SHOAL_API void SHOAL_CALL shoal_range_view_init(shoal_range_view *view);

SHOAL_API void SHOAL_CALL
shoal_iterator_setting_view_init(shoal_iterator_setting_view *view);

SHOAL_API void SHOAL_CALL
shoal_batch_writer_config_init(shoal_batch_writer_config *config);

/*
 * Data-descriptor constructors copy all caller-owned strings and bytes into
 * owned results. Range views preserve each bound's ROW, KEY, or UNBOUNDED
 * kind, including bounded empty rows. Views borrow result memory until free.
 * These local operations do not use connectors or I/O, cannot block, and
 * therefore have no deadline or cancellation parameter. Concurrent getters
 * are safe while the caller guarantees that free does not race with them.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_range_create(const shoal_range *range, shoal_range_result **out_result,
                   shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_range_get(const shoal_range_result *result, shoal_range_view *out_range,
                shoal_error **out_error);
SHOAL_API void SHOAL_CALL shoal_range_free(shoal_range_result **result);

SHOAL_API shoal_status SHOAL_CALL
shoal_iterator_setting_create(
    const shoal_iterator_setting *setting,
    shoal_iterator_setting_result **out_result, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_iterator_setting_get(const shoal_iterator_setting_result *result,
                           shoal_iterator_setting_view *out_setting,
                           shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_iterator_setting_free(shoal_iterator_setting_result **result);

/*
 * Creates a connector and stores its owned handle in out_connector.
 *
 * out_connector is set to NULL before validation. out_error may be NULL; when
 * supplied, it is set to NULL on success or to an owned error on failure.
 * Initialize both output variables to NULL before calling.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_create(const shoal_connector_config *config,
                       shoal_connector **out_connector,
                       shoal_error **out_error);

/*
 * Closes connector-owned transports and bootstrap resources. Close is
 * idempotent while the handle remains alive, cancels active identity, table,
 * namespace, security, and split-administration calls, and waits for those
 * plus any in-flight scanner or batch-scanner calls to finish before tearing
 * down connector-owned resources.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_close(shoal_connector *connector, shoal_error **out_error);

/*
 * Closes and releases an owned connector handle, then sets *connector to NULL.
 * Passing NULL or a pointer whose value is NULL is a no-op. Call close first
 * when its error must be observed. Any scanner or batch-scanner created from
 * this connector permanently rejects new operations as soon as close/free
 * starts, while in-flight scan calls are allowed to finish before final
 * teardown.
 */
SHOAL_API void SHOAL_CALL shoal_connector_free(shoal_connector **connector);

/*
 * The high-level client owns its connector and copies configuration, table,
 * and authorization inputs. Setters are synchronized with concurrent
 * list/create calls. Scanner and writer creation snapshot the current state.
 * Close is idempotent, prevents new calls, and coordinates with active work.
 * Free performs bounded close, clears the caller's handle, and is NULL-safe.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_client_create(const shoal_client_config *config,
                    shoal_client **out_client, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_set_threads(shoal_client *client, int32_t thread_count,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_set_table(shoal_client *client, const char *table_name,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_set_authorizations(shoal_client *client,
                                const shoal_bytes *authorizations,
                                size_t authorization_count,
                                shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_list_tables(shoal_client *client, int64_t timeout_ms,
                         shoal_table_list_result **out_result,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_create_scanner(shoal_client *client,
                            shoal_scanner **out_scanner,
                            shoal_error **out_error);
/*
 * Appends one copied binary column selection to future client scanners and
 * scans. A NULL qualifier selects the whole family; a non-NULL empty
 * qualifier selects that exact empty qualifier.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_client_select_column(shoal_client *client, shoal_bytes family,
                           const shoal_bytes *qualifier,
                           shoal_error **out_error);
/*
 * Executes an owned-result scan from one atomic client settings snapshot.
 * timeout_ms is zero for no deadline and must not be negative. Cancellation
 * variants require a live one-shot cancellation handle. Client close cancels
 * and joins these calls. Inputs are copied before the call can outlive them.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_client_scan_range(shoal_client *client, const shoal_range *range,
                        int64_t timeout_ms,
                        shoal_scan_result **out_result,
                        shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL shoal_client_scan_range_with_cancellation(
    shoal_client *client, const shoal_range *range, int64_t timeout_ms,
    shoal_cancellation *cancellation, shoal_scan_result **out_result,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_scan_ranges(shoal_client *client, const shoal_range *ranges,
                         size_t range_count, int64_t timeout_ms,
                         shoal_scan_result **out_result,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL shoal_client_scan_ranges_with_cancellation(
    shoal_client *client, const shoal_range *ranges, size_t range_count,
    int64_t timeout_ms, shoal_cancellation *cancellation,
    shoal_scan_result **out_result, shoal_error **out_error);

/*
 * High-level streaming scans hold one copied client-settings snapshot until
 * cursor close or exhaustion. Client close cancels and joins every cursor.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_client_stream_range(shoal_client *client, const shoal_range *range,
                          int64_t timeout_ms, shoal_scan_cursor **out_cursor,
                          shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL shoal_client_stream_range_with_cancellation(
    shoal_client *client, const shoal_range *range, int64_t timeout_ms,
    shoal_cancellation *cancellation, shoal_scan_cursor **out_cursor,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_stream_ranges(shoal_client *client, const shoal_range *ranges,
                           size_t range_count, int64_t timeout_ms,
                           shoal_scan_cursor **out_cursor,
                           shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL shoal_client_stream_ranges_with_cancellation(
    shoal_client *client, const shoal_range *ranges, size_t range_count,
    int64_t timeout_ms, shoal_cancellation *cancellation,
    shoal_scan_cursor **out_cursor, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_create_batch_writer(shoal_client *client,
                                 shoal_accumulo_writer **out_writer,
                                 shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_client_close(shoal_client *client, shoal_error **out_error);
SHOAL_API void SHOAL_CALL shoal_client_free(shoal_client **client);

/*
 * Returns the immutable instance name, instance ID, and authenticated
 * principal captured when the connector was created. timeout_ms is zero for
 * no deadline and must not be negative. The result owns all three strings.
 * Initialize out_identity with shoal_connector_identity_view_init before
 * calling shoal_connector_identity_get. Pointers in the view are borrowed
 * from the result and remain valid until the result is freed.
 */
SHOAL_API void SHOAL_CALL
shoal_connector_identity_view_init(shoal_connector_identity_view *view);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_get_identity(shoal_connector *connector, int64_t timeout_ms,
                             shoal_connector_identity_result **out_result,
                             shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_identity_get(const shoal_connector_identity_result *result,
                             shoal_connector_identity_view *out_identity,
                             shoal_error **out_error);

SHOAL_API void SHOAL_CALL
shoal_connector_identity_free(shoal_connector_identity_result **result);

/*
 * Resolves a ZooKeeper-backed instance identity without retaining a native
 * session. zookeeper_servers is a comma-separated list. A zero session timeout
 * selects Shoal's documented 30-second default; a zero bootstrap timeout
 * selects 30 seconds. The result owns the instance name and ID and has an
 * empty principal. This operation is intended for compatibility Instance
 * objects that must resolve identity before credentials are available.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_zookeeper_resolve_instance(
    const char *instance_name, const char *zookeeper_servers,
    int64_t session_timeout_ms, int64_t bootstrap_timeout_ms,
    const char *instance_secret, shoal_connector_identity_result **out_result,
    shoal_error **out_error);

/*
 * Live topology calls use timeout_ms (zero means no deadline), participate in
 * connector cancellation, and return fully owned snapshots. Immutable wiring
 * calls are also coordinated with concurrent connector close.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_get_root_tablet_location(
    shoal_connector *connector, int64_t timeout_ms,
    shoal_bytes_result **out_result, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_get_manager_locations(
    shoal_connector *connector, int64_t timeout_ms,
    shoal_string_list_result **out_result, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_get_zookeepers(shoal_connector *connector,
                               shoal_string_list_result **out_result,
                               shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_get_configuration(shoal_connector *connector,
                                  shoal_configuration **out_configuration,
                                  shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_get_servers(shoal_connector *connector, int64_t timeout_ms,
                            shoal_server_list_result **out_result,
                            shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_get_root(shoal_connector *connector,
                         shoal_bytes_result **out_result,
                         shoal_error **out_error);

/*
 * timeout_ms is zero for no deadline and must not be negative. The returned
 * list owns every table name/ID pair and stays valid until freed. Entries are
 * sorted by qualified table name.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_list_tables(shoal_connector *connector, int64_t timeout_ms,
                            shoal_table_list_result **out_result,
                            shoal_error **out_error);

SHOAL_API size_t SHOAL_CALL
shoal_table_list_count(const shoal_table_list_result *result);

SHOAL_API shoal_status SHOAL_CALL
shoal_table_list_get(const shoal_table_list_result *result, size_t index,
                     shoal_table_view *out_table, shoal_error **out_error);

SHOAL_API void SHOAL_CALL
shoal_table_list_free(shoal_table_list_result **result);

/*
 * timeout_ms is zero for no deadline and must not be negative. out_exists is
 * set to 0 before validation and to 1 only when the table is present.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_table_exists(shoal_connector *connector,
                             const char *table_name, int64_t timeout_ms,
                             uint8_t *out_exists, shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_create_table(shoal_connector *connector,
                             const char *table_name, int64_t timeout_ms,
                             shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_delete_table(shoal_connector *connector,
                             const char *table_name, int64_t timeout_ms,
                             shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_rename_table(shoal_connector *connector,
                             const char *table_name,
                             const char *new_table_name, int64_t timeout_ms,
                             shoal_error **out_error);

/*
 * wait must be 0 or 1. When set, the call waits until Accumulo reports the
 * full-table flush complete or the operation deadline expires.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_flush_table(shoal_connector *connector,
                            const char *table_name, uint8_t wait,
                            int64_t timeout_ms, shoal_error **out_error);

/*
 * A NULL bound pointer is unbounded; a non-NULL shoal_bytes with length zero
 * is a bounded empty row. Bounds are copied before return. Reversed bounds and
 * malformed byte views are invalid arguments.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_flush_table_range(
    shoal_connector *connector, const char *table_name,
    const shoal_bytes *start_row, const shoal_bytes *end_row, uint8_t wait,
    int64_t timeout_ms, shoal_error **out_error);

/*
 * Constraint class/table strings are copied before use. List results own every
 * class name; initialized views borrow result memory until list free. All live
 * operations accept deadlines and are canceled and joined by connector close.
 */
SHOAL_API void SHOAL_CALL
shoal_table_constraint_view_init(shoal_table_constraint_view *view);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_add_table_constraint(
    shoal_connector *connector, const char *table_name,
    const char *class_name, int64_t timeout_ms, int32_t *out_number,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_list_table_constraints(
    shoal_connector *connector, const char *table_name, int64_t timeout_ms,
    shoal_table_constraint_list_result **out_result,
    shoal_error **out_error);
SHOAL_API size_t SHOAL_CALL
shoal_table_constraint_list_count(
    const shoal_table_constraint_list_result *result);
SHOAL_API shoal_status SHOAL_CALL
shoal_table_constraint_list_get(
    const shoal_table_constraint_list_result *result, size_t index,
    shoal_table_constraint_view *out_constraint, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_table_constraint_list_free(
    shoal_table_constraint_list_result **result);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_remove_table_constraint(
    shoal_connector *connector, const char *table_name, int32_t number,
    int64_t timeout_ms, shoal_error **out_error);

/*
 * property_value is required but may be the empty string; use
 * shoal_connector_remove_table_property to remove a property entirely.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_set_table_property(shoal_connector *connector,
                                   const char *table_name,
                                   const char *property_name,
                                   const char *property_value,
                                   int64_t timeout_ms,
                                   shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_remove_table_property(shoal_connector *connector,
                                      const char *table_name,
                                      const char *property_name,
                                      int64_t timeout_ms,
                                      shoal_error **out_error);

/*
 * timeout_ms is zero for no deadline and must not be negative. The returned
 * key/value pairs own their storage, are sorted by property key, and preserve
 * explicit empty string values until freed.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_effective_table_properties(
    shoal_connector *connector, const char *table_name, int64_t timeout_ms,
    shoal_table_properties_result **out_result, shoal_error **out_error);

SHOAL_API size_t SHOAL_CALL
shoal_table_properties_count(const shoal_table_properties_result *result);

SHOAL_API shoal_status SHOAL_CALL
shoal_table_properties_get(const shoal_table_properties_result *result,
                           size_t index, shoal_table_property_view *out_entry,
                           shoal_error **out_error);

SHOAL_API void SHOAL_CALL
shoal_table_properties_free(shoal_table_properties_result **result);

/* Namespace names and IDs are owned by the result and sorted by name. */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_list_namespaces(shoal_connector *connector, int64_t timeout_ms,
                                shoal_namespace_list_result **out_result,
                                shoal_error **out_error);
SHOAL_API size_t SHOAL_CALL
shoal_namespace_list_count(const shoal_namespace_list_result *result);
SHOAL_API shoal_status SHOAL_CALL
shoal_namespace_list_get(const shoal_namespace_list_result *result,
                         size_t index, shoal_namespace_view *out_namespace,
                         shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_namespace_list_free(shoal_namespace_list_result **result);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_namespace_exists(shoal_connector *connector,
                                 const char *namespace_name,
                                 int64_t timeout_ms, uint8_t *out_exists,
                                 shoal_error **out_error);
/*
 * namespace_name must be non-NULL. An empty namespace_name identifies
 * Accumulo's default namespace for existence, deletion, rename-source,
 * permission, and property operations. Creation and rename destinations
 * require non-empty names.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_create_namespace(shoal_connector *connector,
                                 const char *namespace_name,
                                 int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_delete_namespace(shoal_connector *connector,
                                 const char *namespace_name,
                                 int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_rename_namespace(shoal_connector *connector,
                                 const char *namespace_name,
                                 const char *new_namespace_name,
                                 int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_set_namespace_property(
    shoal_connector *connector, const char *namespace_name,
    const char *property_name, const char *property_value, int64_t timeout_ms,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_remove_namespace_property(
    shoal_connector *connector, const char *namespace_name,
    const char *property_name, int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_effective_namespace_properties(
    shoal_connector *connector, const char *namespace_name,
    int64_t timeout_ms, shoal_namespace_properties_result **out_result,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_namespace_properties(
    shoal_connector *connector, const char *namespace_name,
    int64_t timeout_ms, shoal_namespace_properties_result **out_result,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_versioned_namespace_properties(
    shoal_connector *connector, const char *namespace_name,
    int64_t timeout_ms, shoal_versioned_properties_result **out_result,
    shoal_error **out_error);
SHOAL_API size_t SHOAL_CALL
shoal_namespace_properties_count(
    const shoal_namespace_properties_result *result);
SHOAL_API shoal_status SHOAL_CALL
shoal_namespace_properties_get(
    const shoal_namespace_properties_result *result, size_t index,
    shoal_table_property_view *out_entry, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_namespace_properties_free(shoal_namespace_properties_result **result);
SHOAL_API int64_t SHOAL_CALL
shoal_versioned_properties_version(
    const shoal_versioned_properties_result *result);
SHOAL_API size_t SHOAL_CALL
shoal_versioned_properties_count(
    const shoal_versioned_properties_result *result);
SHOAL_API shoal_status SHOAL_CALL
shoal_versioned_properties_get(
    const shoal_versioned_properties_result *result, size_t index,
    shoal_table_property_view *out_entry, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_versioned_properties_free(shoal_versioned_properties_result **result);

/*
 * Password, authorization, and split inputs are copied before return.
 * A non-NULL password struct with {NULL, 0} represents an empty password.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_create_user(shoal_connector *connector, const char *user,
                            const shoal_bytes *password, int64_t timeout_ms,
                            shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_drop_user(shoal_connector *connector, const char *user,
                          int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_change_password(shoal_connector *connector, const char *user,
                                const shoal_bytes *password,
                                int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_change_user_authorizations(
    shoal_connector *connector, const char *user,
    const shoal_bytes *authorizations, size_t authorization_count,
    int64_t timeout_ms, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_get_user_authorizations(
    shoal_connector *connector, const char *user, int64_t timeout_ms,
    shoal_bytes_list_result **out_result, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_has_system_permission(
    shoal_connector *connector, const char *user,
    shoal_system_permission permission, int64_t timeout_ms,
    uint8_t *out_has_permission, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_has_table_permission(
    shoal_connector *connector, const char *user, const char *table_name,
    shoal_table_permission permission, int64_t timeout_ms,
    uint8_t *out_has_permission, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_has_namespace_permission(
    shoal_connector *connector, const char *user, const char *namespace_name,
    shoal_namespace_permission permission, int64_t timeout_ms,
    uint8_t *out_has_permission, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_grant_system_permission(
    shoal_connector *connector, const char *user,
    shoal_system_permission permission, int64_t timeout_ms,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_revoke_system_permission(
    shoal_connector *connector, const char *user,
    shoal_system_permission permission, int64_t timeout_ms,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_grant_table_permission(
    shoal_connector *connector, const char *user, const char *table_name,
    shoal_table_permission permission, int64_t timeout_ms,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_revoke_table_permission(
    shoal_connector *connector, const char *user, const char *table_name,
    shoal_table_permission permission, int64_t timeout_ms,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_grant_namespace_permission(
    shoal_connector *connector, const char *user, const char *namespace_name,
    shoal_namespace_permission permission, int64_t timeout_ms,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_revoke_namespace_permission(
    shoal_connector *connector, const char *user, const char *namespace_name,
    shoal_namespace_permission permission, int64_t timeout_ms,
    shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_list_table_splits(shoal_connector *connector,
                                  const char *table_name, int64_t timeout_ms,
                                  shoal_bytes_list_result **out_result,
                                  shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_add_table_splits(shoal_connector *connector,
                                 const char *table_name,
                                 const shoal_bytes *splits,
                                 size_t split_count, int64_t timeout_ms,
                                 shoal_error **out_error);
SHOAL_API size_t SHOAL_CALL
shoal_bytes_list_count(const shoal_bytes_list_result *result);
SHOAL_API shoal_status SHOAL_CALL
shoal_bytes_list_get(const shoal_bytes_list_result *result, size_t index,
                     shoal_bytes *out_value, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_bytes_list_free(shoal_bytes_list_result **result);

/*
 * Creates a scanner. The configuration and all nested values are copied.
 * Scanner handles remain valid after connector free, but once connector close
 * or free starts, new operations fail with CLOSED while already-started scans
 * are allowed to finish before connector teardown.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_create_scanner(shoal_connector *connector,
                               const shoal_scanner_config *config,
                               shoal_scanner **out_scanner,
                               shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_create_batch_scanner(shoal_connector *connector,
                                     const shoal_scanner_config *config,
                                     shoal_batch_scanner **out_scanner,
                                     shoal_error **out_error);

/*
 * Close is idempotent and cancels then joins in-flight calls. Concurrent scan
 * and close calls are supported. Free/use of the same handle concurrently is
 * not supported.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_scanner_close(shoal_scanner *scanner, shoal_error **out_error);

SHOAL_API void SHOAL_CALL shoal_scanner_free(shoal_scanner **scanner);

SHOAL_API shoal_status SHOAL_CALL
shoal_batch_scanner_close(shoal_batch_scanner *scanner,
                          shoal_error **out_error);

SHOAL_API void SHOAL_CALL
shoal_batch_scanner_free(shoal_batch_scanner **scanner);

/*
 * timeout_ms is zero for no deadline and must not be negative. On any call
 * that produced entries, out_result may be non-NULL even when the status is a
 * failure (for example CLEANUP_FAILED or an error after partial batch results).
 * The caller owns every non-NULL result and must free it.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_scanner_scan(shoal_scanner *scanner, const shoal_range *range,
                   int64_t timeout_ms, shoal_scan_result **out_result,
                   shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_batch_scanner_scan(shoal_batch_scanner *scanner,
                         const shoal_range *ranges, size_t range_count,
                         int64_t timeout_ms, shoal_scan_result **out_result,
                         shoal_error **out_error);

/*
 * Cancellation handles are one-shot and thread-safe. Cancel is idempotent.
 * Cancelable scans register only for the duration of that call. Free cancels
 * and joins registered calls, clears the caller's handle, and is NULL-safe and
 * idempotent. A canceled handle remains canceled and may be queried or freed.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_cancellation_create(shoal_cancellation **out_cancellation,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_cancellation_cancel(shoal_cancellation *cancellation,
                         shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_cancellation_is_cancelled(const shoal_cancellation *cancellation,
                               uint8_t *out_cancelled,
                               shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_cancellation_free(shoal_cancellation **cancellation);
SHOAL_API shoal_status SHOAL_CALL
shoal_scanner_scan_with_cancellation(
    shoal_scanner *scanner, const shoal_range *range, int64_t timeout_ms,
    shoal_cancellation *cancellation, shoal_scan_result **out_result,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_batch_scanner_scan_with_cancellation(
    shoal_batch_scanner *scanner, const shoal_range *ranges,
    size_t range_count, int64_t timeout_ms, shoal_cancellation *cancellation,
    shoal_scan_result **out_result, shoal_error **out_error);

/*
 * Streaming cursors keep at most one Go scan batch resident. Every returned
 * shoal_scan_result is independently owned and may outlive the cursor. A
 * successful next call returns up to max_entries and sets out_exhausted when
 * no further entries exist; exact chunk boundaries may require one final call
 * that returns NULL plus out_exhausted=1. Cursor iteration is serialized.
 * Close is concurrent-safe, cancels an in-flight next call, joins cleanup, and
 * is idempotent. Free clears the caller's handle and is NULL-safe.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_scanner_stream(shoal_scanner *scanner, const shoal_range *range,
                     int64_t timeout_ms, shoal_scan_cursor **out_cursor,
                     shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL shoal_scanner_stream_with_cancellation(
    shoal_scanner *scanner, const shoal_range *range, int64_t timeout_ms,
    shoal_cancellation *cancellation, shoal_scan_cursor **out_cursor,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_batch_scanner_stream(shoal_batch_scanner *scanner,
                           const shoal_range *ranges, size_t range_count,
                           int64_t timeout_ms,
                           shoal_scan_cursor **out_cursor,
                           shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL shoal_batch_scanner_stream_with_cancellation(
    shoal_batch_scanner *scanner, const shoal_range *ranges,
    size_t range_count, int64_t timeout_ms,
    shoal_cancellation *cancellation, shoal_scan_cursor **out_cursor,
    shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_scan_cursor_next(shoal_scan_cursor *cursor, size_t max_entries,
                       shoal_scan_result **out_result, uint8_t *out_exhausted,
                       shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_scan_cursor_close(shoal_scan_cursor *cursor, shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_scan_cursor_free(shoal_scan_cursor **cursor);

/*
 * Connector invalidation is local and performs no network I/O. Inputs are
 * copied before return. Calls coordinate with connector close and fail with
 * SHOAL_STATUS_CLOSED after close begins.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_invalidate_table(shoal_connector *connector,
                                const char *table_id,
                                shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_invalidate_discovery(shoal_connector *connector,
                                    shoal_error **out_error);

SHOAL_API size_t SHOAL_CALL
shoal_scan_result_count(const shoal_scan_result *result);

SHOAL_API shoal_status SHOAL_CALL
shoal_scan_result_get(const shoal_scan_result *result, size_t index,
                      shoal_key_value_view *out_value,
                      shoal_error **out_error);

SHOAL_API void SHOAL_CALL
shoal_scan_result_free(shoal_scan_result **result);

/*
 * Mutation inputs are copied before each call returns. A mutation may be
 * reused or freed immediately after a successful BatchWriter add.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_mutation_create(shoal_bytes row, shoal_mutation **out_mutation,
                      shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_mutation_put(shoal_mutation *mutation, shoal_bytes column_family,
                   shoal_bytes column_qualifier,
                   shoal_bytes column_visibility, int64_t timestamp,
                   shoal_bytes value, shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_mutation_put_latest(shoal_mutation *mutation, shoal_bytes column_family,
                          shoal_bytes column_qualifier,
                          shoal_bytes column_visibility, shoal_bytes value,
                          shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_mutation_delete(shoal_mutation *mutation, shoal_bytes column_family,
                      shoal_bytes column_qualifier,
                      shoal_bytes column_visibility, int64_t timestamp,
                      shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_mutation_delete_latest(shoal_mutation *mutation,
                             shoal_bytes column_family,
                             shoal_bytes column_qualifier,
                             shoal_bytes column_visibility,
                             shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_mutation_size(const shoal_mutation *mutation, size_t *out_size,
                    shoal_error **out_error);

SHOAL_API void SHOAL_CALL shoal_mutation_free(shoal_mutation **mutation);

SHOAL_API shoal_status SHOAL_CALL
shoal_connector_create_batch_writer(
    shoal_connector *connector, const shoal_batch_writer_config *config,
    shoal_batch_writer **out_writer, shoal_error **out_error);

/*
 * timeout_ms is zero for no deadline and must not be negative. out_failure is
 * optional and receives owned structured details when the operation reaches
 * the write path. Free every non-NULL failure object.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_batch_writer_add(shoal_batch_writer *writer,
                       const shoal_mutation *mutation, int64_t timeout_ms,
                       shoal_write_failure **out_failure,
                       shoal_error **out_error);

SHOAL_API shoal_status SHOAL_CALL
shoal_batch_writer_flush(shoal_batch_writer *writer, int64_t timeout_ms,
                         shoal_write_failure **out_failure,
                         shoal_error **out_error);

/*
 * Returns the exact number of mutations currently buffered in the writer.
 * The value is a point-in-time snapshot and may change immediately when other
 * threads use the writer. timeout_ms is zero for no deadline.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_batch_writer_size(shoal_batch_writer *writer, int64_t timeout_ms,
                        size_t *out_size, shoal_error **out_error);

/*
 * Close is idempotent, prevents new operations, cancels and joins in-flight
 * calls, then flushes remaining mutations. Repeated calls return the first
 * close result, including a deadline or cancellation failure.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_batch_writer_close(shoal_batch_writer *writer, int64_t timeout_ms,
                         shoal_write_failure **out_failure,
                         shoal_error **out_error);

SHOAL_API void SHOAL_CALL
shoal_batch_writer_free(shoal_batch_writer **writer);

/*
 * Logging control is process-wide, matching Sharkbite's static configuration.
 * Invalid levels are rejected. The current level is allocation-free and safe
 * to query concurrently.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_logging_set_level(shoal_log_level level, shoal_error **out_error);
SHOAL_API shoal_log_level SHOAL_CALL shoal_logging_get_level(void);
/*
 * Installs a process-wide structured callback. callback == NULL restores the
 * default slog sink. event_name and attributes_json are callback-lifetime
 * UTF-8 strings; callbacks may run concurrently on client goroutines. A
 * callback must not reconfigure logging. Clearing the callback waits for
 * in-flight callback invocations before returning.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_logging_set_callback(shoal_log_callback callback, void *context,
                           shoal_error **out_error);

/*
 * The buffered writer is an owned, lazy high-level writer. It copies config
 * and byte inputs, creates its underlying batch writer on the first update,
 * and buffers one mutation until the row changes or close is called. Calls on
 * one handle are serialized. close cancels and joins concurrent updates;
 * connector close cancels them as well. timeout_ms is zero for no deadline.
 * Free is NULL-safe and idempotent; callers must externally serialize free
 * against access through aliases to the same C handle.
 */
SHOAL_API shoal_status SHOAL_CALL
shoal_connector_create_accumulo_writer(
    shoal_connector *connector, const shoal_batch_writer_config *config,
    shoal_accumulo_writer **out_writer, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_accumulo_writer_put(
    shoal_accumulo_writer *writer, shoal_bytes row, shoal_bytes column_family,
    shoal_bytes column_qualifier, shoal_bytes column_visibility,
    int64_t timestamp, shoal_bytes value, int64_t timeout_ms,
    shoal_write_failure **out_failure, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_accumulo_writer_put_delete(
    shoal_accumulo_writer *writer, shoal_bytes row, shoal_bytes column_family,
    shoal_bytes column_qualifier, shoal_bytes column_visibility,
    int64_t timestamp, int64_t timeout_ms,
    shoal_write_failure **out_failure, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_accumulo_writer_delete(
    shoal_accumulo_writer *writer, const shoal_key *key, int64_t timeout_ms,
    shoal_write_failure **out_failure, shoal_error **out_error);
SHOAL_API shoal_status SHOAL_CALL
shoal_accumulo_writer_close(shoal_accumulo_writer *writer,
                            int64_t timeout_ms,
                            shoal_write_failure **out_failure,
                            shoal_error **out_error);
SHOAL_API void SHOAL_CALL
shoal_accumulo_writer_free(shoal_accumulo_writer **writer);

SHOAL_API shoal_write_failure_flags SHOAL_CALL
shoal_write_failure_get_flags(const shoal_write_failure *failure);

SHOAL_API size_t SHOAL_CALL
shoal_write_failure_failed_extent_count(const shoal_write_failure *failure);

SHOAL_API shoal_status SHOAL_CALL
shoal_write_failure_get_failed_extent(
    const shoal_write_failure *failure, size_t index,
    shoal_failed_extent_view *out_extent, shoal_error **out_error);

SHOAL_API size_t SHOAL_CALL shoal_write_failure_constraint_count(
    const shoal_write_failure *failure);

SHOAL_API shoal_status SHOAL_CALL
shoal_write_failure_get_constraint(
    const shoal_write_failure *failure, size_t index,
    shoal_constraint_violation_view *out_violation, shoal_error **out_error);

SHOAL_API size_t SHOAL_CALL shoal_write_failure_authorization_count(
    const shoal_write_failure *failure);

SHOAL_API shoal_status SHOAL_CALL
shoal_write_failure_get_authorization(
    const shoal_write_failure *failure, size_t index,
    shoal_authorization_failure_view *out_failure, shoal_error **out_error);

SHOAL_API size_t SHOAL_CALL
shoal_write_failure_cleanup_count(const shoal_write_failure *failure);

SHOAL_API shoal_status SHOAL_CALL
shoal_write_failure_get_cleanup(const shoal_write_failure *failure,
                                size_t index,
                                shoal_cleanup_failure_view *out_failure,
                                shoal_error **out_error);

SHOAL_API void SHOAL_CALL
shoal_write_failure_free(shoal_write_failure **failure);

/* Returns the stable status stored in error, or INVALID_ARGUMENT for NULL. */
SHOAL_API shoal_status SHOAL_CALL shoal_error_code(const shoal_error *error);

/*
 * Returns a borrowed, NUL-terminated message. It remains valid until error is
 * freed. The returned bytes are read-only and must not be freed directly.
 */
SHOAL_API const char *SHOAL_CALL
shoal_error_message(const shoal_error *error);

/* Borrowed structured Accumulo security details, or NULL when not applicable. */
SHOAL_API const char *SHOAL_CALL
shoal_error_security_user(const shoal_error *error);
SHOAL_API const char *SHOAL_CALL
shoal_error_security_code(const shoal_error *error);
SHOAL_API shoal_status SHOAL_CALL shoal_error_visibility_parse(
    const shoal_error *error, shoal_visibility_parse_error_view *out_details);

/*
 * Releases an owned error and sets *error to NULL. Passing NULL or a pointer
 * whose value is NULL is a no-op.
 */
SHOAL_API void SHOAL_CALL shoal_error_free(shoal_error **error);

#ifdef __cplusplus
}
#endif

#endif
