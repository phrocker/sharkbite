#ifndef SHOAL_CAPI_TEST_SEAM_H
#define SHOAL_CAPI_TEST_SEAM_H

#include <stddef.h>
#include <stdint.h>

#ifndef SHOAL_TYPES_H
typedef struct shoal_connector shoal_connector;
typedef struct shoal_batch_writer shoal_batch_writer;
#endif

enum {
  SHOAL_TEST_WRITER_SUCCESS = 0,
  SHOAL_TEST_WRITER_STRUCTURED_FAILURE = 1,
  SHOAL_TEST_WRITER_STICKY_DEADLINE = 2,
  SHOAL_TEST_WRITER_CONNECTOR_CLOSED = 3
};

int shoal_test_connector_create(shoal_connector **out_connector);
int shoal_test_scanners_create(shoal_scanner **out_scanner,
                               shoal_batch_scanner **out_batch_scanner);
int shoal_test_client_create(shoal_client **out_client);
int shoal_test_client_settings_match(
    shoal_client *client,
    char *table_name,
    shoal_bytes authorization,
    int32_t thread_count);
int shoal_test_client_columns_match(shoal_client *client, shoal_bytes family,
                                    shoal_bytes qualifier,
                                    uint8_t has_qualifier,
                                    size_t column_count);
size_t shoal_test_connector_flush_wait_count(shoal_connector *connector,
                                             uint8_t wait);
int shoal_test_connector_identity_block(                                        shoal_connector *connector,
                                        uint8_t block);
int shoal_test_connector_table_maintenance_block(
                                        shoal_connector *connector,
                                        uint8_t block);
int shoal_test_connector_last_flush_range_matches(
                                        shoal_connector *connector,
                                        shoal_bytes *start,
                                        shoal_bytes *end,
                                        uint8_t wait);
int shoal_test_connector_invalidation_matches(
                                        shoal_connector *connector,
                                        char *table_id,
                                        size_t discovery_count);
int shoal_test_connector_topology_block(shoal_connector *connector,
                                        uint8_t block);
int shoal_test_batch_writer_create(int mode, shoal_batch_writer **out_writer);
int shoal_test_accumulo_writer_create(
    int mode, shoal_accumulo_writer **out_writer);
void shoal_test_string_alloc_fail_after(size_t successful_allocations);
void shoal_test_string_alloc_reset(void);
void shoal_test_result_alloc_fail_after(size_t successful_allocations);
void shoal_test_result_alloc_reset(void);
void shoal_test_error_alloc_fail_after(size_t successful_allocations);
void shoal_test_error_alloc_reset(void);
void shoal_test_error_message_alloc_fail_after(size_t successful_allocations);
void shoal_test_error_message_alloc_reset(void);
void shoal_test_sleep_ms(int64_t milliseconds);

#endif
