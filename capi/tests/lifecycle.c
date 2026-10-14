#include "shoal.h"
#include "test_seam.h"

#include <assert.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

static void SHOAL_CALL capture_log(shoal_log_level level,
                                   const char *event_name,
                                   const char *attributes_json,
                                   void *context) {
  size_t *count = (size_t *)context;
  assert(level == SHOAL_LOG_LEVEL_DEBUG);
  assert(strcmp(event_name, "shoal.logging.level_changed") == 0);
  assert(strstr(attributes_json, "\"level\":\"debug\"") != NULL);
  assert(strstr(attributes_json, "password") == NULL);
  (*count)++;
}

_Static_assert(SHOAL_ABI_VERSION == 1u, "unexpected compatibility ABI version");
_Static_assert(SHOAL_ABI_VERSION_MAJOR == 1u, "unexpected ABI major");
_Static_assert(SHOAL_ABI_VERSION_MINOR == 19u, "unexpected ABI minor");
_Static_assert(SHOAL_ABI_VERSION_PATCH == 0u, "unexpected ABI patch");
_Static_assert(SHOAL_ABI_VERSION_PACKED == 0x00011300u,
               "unexpected packed ABI version");
_Static_assert(SHOAL_ABI_CAPABILITY_CONNECTOR == 0u,
               "unexpected connector capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_BOOTSTRAP == 1u,
               "unexpected bootstrap capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_ERROR == 2u,
               "unexpected error capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_SCANNER == 3u,
               "unexpected scanner capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_BATCH_SCANNER == 4u,
               "unexpected batch scanner capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_OWNED_SCAN_RESULT == 5u,
               "unexpected scan result capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_MUTATION == 6u,
               "unexpected mutation capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_BATCH_WRITER == 7u,
               "unexpected batch writer capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_STRUCTURED_WRITE_FAILURE == 8u,
               "unexpected structured write failure capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_TABLE_ADMIN == 9u,
               "unexpected table admin capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN == 10u,
               "unexpected namespace admin capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_SECURITY_ADMIN == 11u,
               "unexpected security admin capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_TABLE_SPLITS == 12u,
               "unexpected table splits capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY == 13u,
               "unexpected connector identity capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS == 14u,
               "unexpected data descriptors capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY == 15u,
               "unexpected configuration topology capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_RFILE == 16u,
               "unexpected RFile capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_DATA_VALUES == 17u,
               "unexpected data values capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_TABLE_MAINTENANCE == 19u,
               "unexpected table maintenance capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL == 20u,
               "unexpected connector control capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT == 21u,
               "unexpected high-level client capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER == 22u,
               "unexpected high-level scanner capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_COMPATIBILITY_ERRORS == 23u,
               "unexpected compatibility errors capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR == 24u,
               "unexpected streaming scan cursor capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY == 25u,
               "unexpected column visibility capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_OWNED_KEY == 26u,
               "unexpected owned key capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_STORAGE_ERROR_PARITY == 30u,
               "unexpected storage/error parity capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_ZOOKEEPER_INSTANCE == 31u,
               "unexpected ZooKeeper instance capability id");
_Static_assert(SHOAL_ABI_CAPABILITY_COUNT == 32u,
               "unexpected capability count");
_Static_assert(SHOAL_ABI_CAPABILITY_WORD_COUNT == 1u,
               "unexpected capability word count");
_Static_assert(SHOAL_ABI_CAPABILITY_WORD0 == UINT64_C(0xffffffff),
               "unexpected capability word 0");

#define ASSERT_PERMISSION_VALUE(name, value)                                  \
  _Static_assert(name == value, "unexpected permission ordinal: " #name)

ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_GRANT, 0);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_CREATE_TABLE, 1);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_DROP_TABLE, 2);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_ALTER_TABLE, 3);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_CREATE_USER, 4);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_DROP_USER, 5);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_ALTER_USER, 6);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_SYSTEM, 7);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_CREATE_NAMESPACE, 8);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_DROP_NAMESPACE, 9);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_ALTER_NAMESPACE, 10);
ASSERT_PERMISSION_VALUE(SHOAL_SYSTEM_PERMISSION_OBTAIN_DELEGATION_TOKEN, 11);
ASSERT_PERMISSION_VALUE(SHOAL_TABLE_PERMISSION_READ, 2);
ASSERT_PERMISSION_VALUE(SHOAL_TABLE_PERMISSION_WRITE, 3);
ASSERT_PERMISSION_VALUE(SHOAL_TABLE_PERMISSION_BULK_IMPORT, 4);
ASSERT_PERMISSION_VALUE(SHOAL_TABLE_PERMISSION_ALTER_TABLE, 5);
ASSERT_PERMISSION_VALUE(SHOAL_TABLE_PERMISSION_GRANT, 6);
ASSERT_PERMISSION_VALUE(SHOAL_TABLE_PERMISSION_DROP_TABLE, 7);
ASSERT_PERMISSION_VALUE(SHOAL_TABLE_PERMISSION_GET_SUMMARIES, 8);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_READ, 0);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_WRITE, 1);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_ALTER_NAMESPACE, 2);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_GRANT, 3);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_ALTER_TABLE, 4);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_CREATE_TABLE, 5);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_DROP_TABLE, 6);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_BULK_IMPORT, 7);
ASSERT_PERMISSION_VALUE(SHOAL_NAMESPACE_PERMISSION_DROP_NAMESPACE, 8);

#undef ASSERT_PERMISSION_VALUE

static void expect_error(shoal_status status, shoal_status expected,
                         shoal_error **error, const char *message_part) {
  if (status != expected) {
    fprintf(stderr, "status %d, expected %d for %s\n", status, expected,
            message_part);
  }
  assert(status == expected);
  assert(error != NULL);
  assert(*error != NULL);
  assert(shoal_error_code(*error) == expected);
  assert(shoal_error_compatibility_code(*error) >= -1);
  assert(shoal_error_compatibility_code(*error) <= 13);
  assert(strstr(shoal_error_message(*error), message_part) != NULL);
  if (expected == SHOAL_STATUS_CLOSED) {
    assert(shoal_error_source(*error) ==
           SHOAL_ERROR_SOURCE_ILLEGAL_STATE_EXCEPTION);
    assert(strcmp(shoal_error_source_name(*error),
                  "IllegalStateException") == 0);
    assert(shoal_error_compatibility(*error) ==
           SHOAL_ERROR_COMPATIBILITY_RUNTIME_ERROR);
  } else if (expected == SHOAL_STATUS_CANCELLED) {
    assert(shoal_error_source(*error) ==
           SHOAL_ERROR_SOURCE_ITERATION_INTERRUPTED_EXCEPTION);
    assert(strcmp(shoal_error_source_name(*error),
                  "IterationInterruptedException") == 0);
    assert(shoal_error_compatibility(*error) ==
           SHOAL_ERROR_COMPATIBILITY_RUNTIME_ERROR);
  } else if (shoal_error_source(*error) ==
             SHOAL_ERROR_SOURCE_CLIENT_EXCEPTION) {
    assert(shoal_error_source(*error) ==
           SHOAL_ERROR_SOURCE_CLIENT_EXCEPTION);
    assert(shoal_error_compatibility(*error) ==
           SHOAL_ERROR_COMPATIBILITY_CLIENT_EXCEPTION);
    assert(strcmp(shoal_error_compatibility_name(*error),
                  "ClientException") == 0);
  } else if (shoal_error_source(*error) ==
             SHOAL_ERROR_SOURCE_VISIBILITY_PARSE_EXCEPTION) {
    assert(shoal_error_compatibility(*error) ==
           SHOAL_ERROR_COMPATIBILITY_CLIENT_EXCEPTION);
    assert(strcmp(shoal_error_compatibility_name(*error),
                  "ClientException") == 0);
  } else {
    assert(shoal_error_compatibility(*error) ==
           SHOAL_ERROR_COMPATIBILITY_RUNTIME_ERROR);
    assert(strcmp(shoal_error_compatibility_name(*error), "RuntimeError") ==
           0);
  }
  shoal_error_free(error);
  assert(*error == NULL);
}

static void expect_v1_init(const void *value, size_t allocation_size,
                           uint32_t v1_size) {
  const uint8_t *bytes = (const uint8_t *)value;
  uint32_t struct_size = 0;
  memcpy(&struct_size, value, sizeof(struct_size));
  assert(struct_size == v1_size);
  for (size_t i = sizeof(struct_size); i < v1_size; ++i) {
    assert(bytes[i] == 0);
  }
  for (size_t i = v1_size; i < allocation_size; ++i) {
    assert(bytes[i] == UINT8_C(0xa5));
  }
}

static void test_v1_initializers(void) {
#define CHECK_V1_INIT(type, init, v1_size)                                   \
  do {                                                                       \
    struct {                                                                 \
      type value;                                                            \
      uint8_t guard[16];                                                     \
    } allocation;                                                            \
    memset(&allocation, 0xa5, sizeof(allocation));                            \
    init(&allocation.value);                                                 \
    expect_v1_init(&allocation.value, sizeof(allocation), v1_size);           \
  } while (0)

  CHECK_V1_INIT(shoal_connector_config, shoal_connector_config_init,
                SHOAL_CONNECTOR_CONFIG_V1_SIZE);
  {
    struct {
      shoal_client_config value;
      uint8_t guard[16];
    } allocation;
    memset(&allocation, 0xa5, sizeof(allocation));
    shoal_client_config_init(&allocation.value);
    assert(allocation.value.struct_size == SHOAL_CLIENT_CONFIG_V1_SIZE);
    assert(allocation.value.connector == NULL);
    assert(allocation.value.table_name == NULL);
    assert(allocation.value.authorizations == NULL);
    assert(allocation.value.authorization_count == 0);
    assert(allocation.value.thread_count == 10);
    for (size_t i = 0; i < sizeof(allocation.guard); ++i) {
      assert(allocation.guard[i] == UINT8_C(0xa5));
    }
  }
  CHECK_V1_INIT(shoal_scanner_config, shoal_scanner_config_init,
                SHOAL_SCANNER_CONFIG_V1_SIZE);
  CHECK_V1_INIT(shoal_range, shoal_range_init, SHOAL_RANGE_V1_SIZE);
  CHECK_V1_INIT(shoal_batch_writer_config, shoal_batch_writer_config_init,
                SHOAL_BATCH_WRITER_CONFIG_V1_SIZE);
  CHECK_V1_INIT(shoal_connector_identity_view,
                shoal_connector_identity_view_init,
                SHOAL_CONNECTOR_IDENTITY_VIEW_V1_SIZE);
  CHECK_V1_INIT(shoal_range_view, shoal_range_view_init,
                SHOAL_RANGE_VIEW_V1_SIZE);
  CHECK_V1_INIT(shoal_iterator_setting_view,
                shoal_iterator_setting_view_init,
                SHOAL_ITERATOR_SETTING_VIEW_V1_SIZE);
  CHECK_V1_INIT(shoal_server_view, shoal_server_view_init,
                SHOAL_SERVER_VIEW_V1_SIZE);
  CHECK_V1_INIT(shoal_rfile_writer_config, shoal_rfile_writer_config_init,
                SHOAL_RFILE_WRITER_CONFIG_V1_SIZE);
  CHECK_V1_INIT(shoal_rfile_merge_config, shoal_rfile_merge_config_init,
                SHOAL_RFILE_MERGE_CONFIG_V1_SIZE);
  CHECK_V1_INIT(shoal_rfile_entry, shoal_rfile_entry_init,
                SHOAL_RFILE_ENTRY_V1_SIZE);
  CHECK_V1_INIT(shoal_rfile_entry_view, shoal_rfile_entry_view_init,
                SHOAL_RFILE_ENTRY_VIEW_V1_SIZE);
  CHECK_V1_INIT(shoal_key_value, shoal_key_value_init,
                SHOAL_KEY_VALUE_V1_SIZE);

#undef CHECK_V1_INIT
}

static void append_rfile_entry(shoal_rfile_writer *writer, const uint8_t *row,
                               size_t row_length, const uint8_t *family,
                               size_t family_length, const uint8_t *value,
                               size_t value_length, int64_t timestamp,
                               uint8_t deleted) {
  shoal_rfile_entry entry;
  shoal_error *error = NULL;
  shoal_rfile_entry_init(&entry);
  entry.key.row = (shoal_bytes){row, row_length};
  entry.key.column_family = (shoal_bytes){family, family_length};
  entry.key.timestamp = timestamp;
  entry.value = (shoal_bytes){value, value_length};
  entry.deleted = deleted;
  assert(shoal_rfile_writer_append(writer, &entry, 0, &error) ==
         SHOAL_STATUS_OK);
  assert(error == NULL);
}

static void test_rfile_abi(void) {
  static const char path_one[] = "shoal-capi-rfile-one.rf";
  static const char path_two[] = "shoal-capi-rfile-two.rf";
  static const uint8_t row_a[] = {'a', '\0'};
  static const uint8_t row_b[] = {'b'};
  static const uint8_t row_c[] = {'c'};
  static const uint8_t family[] = {'f', '\0'};
  static const uint8_t value_a[] = {'v', '\0', 'a'};
  static const uint8_t value_b[] = {'v', 'b'};
  static const uint8_t value_c[] = {'v', 'c'};
  shoal_rfile_writer_config writer_config;
  shoal_rfile_merge_config merge_config;
  shoal_rfile_writer *writer = NULL;
  shoal_rfile_reader *reader = NULL;
  shoal_rfile_seekable *seekable = NULL;
  shoal_rfile_entry_result *entry_result = NULL;
  shoal_bytes_result *bytes_result = NULL;
  shoal_range_result *range_result = NULL;
  shoal_error *error = NULL;

  remove(path_one);
  remove(path_two);
  assert(shoal_rfile_reader_open(path_one, 0, &reader, &error) ==
         SHOAL_STATUS_NOT_FOUND);
  assert(reader == NULL && error != NULL);
  shoal_error_free(&error);
  shoal_rfile_writer_config_init(&writer_config);
  assert(shoal_rfile_writer_create(path_one, &writer_config, -1, &writer,
                                   &error) ==
         SHOAL_STATUS_INVALID_ARGUMENT);
  assert(writer == NULL && error != NULL);
  shoal_error_free(&error);
  writer_config.codec = "not-a-codec";
  assert(shoal_rfile_writer_create(path_one, &writer_config, 0, &writer,
                                   &error) == SHOAL_STATUS_UNSUPPORTED);
  assert(writer == NULL && error != NULL);
  shoal_error_free(&error);
  writer_config.codec = "gz";
  writer_config.block_size = 64;
  assert(shoal_rfile_writer_create(path_one, &writer_config, 0, &writer,
                                   &error) == SHOAL_STATUS_OK);
  append_rfile_entry(writer, row_a, sizeof(row_a), family, sizeof(family),
                     value_a, sizeof(value_a), 7, 0);
  append_rfile_entry(writer, row_b, sizeof(row_b), family, sizeof(family),
                     value_b, sizeof(value_b), 6, 1);
  assert(shoal_rfile_writer_entries(writer) == 2);
  assert(shoal_rfile_writer_close(writer, &error) == SHOAL_STATUS_OK);
  assert(shoal_rfile_writer_close(writer, &error) == SHOAL_STATUS_OK);
  shoal_rfile_writer_free(&writer);
  shoal_rfile_writer_free(&writer);
  assert(writer == NULL);

  shoal_rfile_writer_config_init(&writer_config);
  assert(shoal_rfile_writer_create(path_two, &writer_config, 0, &writer,
                                   &error) == SHOAL_STATUS_OK);
  append_rfile_entry(writer, row_c, sizeof(row_c), family, sizeof(family),
                     value_c, sizeof(value_c), 5, 0);
  assert(shoal_rfile_writer_close(writer, &error) == SHOAL_STATUS_OK);
  shoal_rfile_writer_free(&writer);

  assert(shoal_rfile_reader_open(path_one, 0, &reader, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_rfile_reader_has_top(reader) == 0);
  assert(shoal_rfile_reader_top(reader, &entry_result, &error) ==
         SHOAL_STATUS_NOT_FOUND);
  assert(entry_result == NULL && error != NULL);
  shoal_error_free(&error);

  shoal_range range;
  shoal_range_init(&range);
  shoal_bytes families[] = {{family, sizeof(family)}};
  assert(shoal_rfile_seekable_create(&range, families, 1, 1, &seekable,
                                     &error) == SHOAL_STATUS_OK);
  assert(shoal_rfile_seekable_column_family_count(seekable) == 1);
  assert(shoal_rfile_seekable_is_inclusive(seekable) == 1);
  assert(shoal_rfile_seekable_get_column_family(seekable, 0, &bytes_result,
                                                &error) == SHOAL_STATUS_OK);
  shoal_bytes copied = shoal_bytes_result_get(bytes_result);
  assert(copied.length == sizeof(family));
  assert(memcmp(copied.data, family, sizeof(family)) == 0);
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_rfile_seekable_get_range(seekable, &range_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_range_view range_view;
  shoal_range_view_init(&range_view);
  assert(shoal_range_get(range_result, &range_view, &error) == SHOAL_STATUS_OK);
  shoal_range_free(&range_result);
  assert(shoal_rfile_reader_seek(reader, seekable, 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_rfile_reader_has_top(reader) == 1);

  shoal_test_result_alloc_fail_after(0);
  assert(shoal_rfile_reader_top(reader, &entry_result, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(entry_result == NULL && error != NULL);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();

  assert(shoal_rfile_reader_top(reader, &entry_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_rfile_entry_view entry_view;
  shoal_rfile_entry_view_init(&entry_view);
  assert(shoal_rfile_entry_result_get(entry_result, &entry_view, &error) ==
         SHOAL_STATUS_OK);
  assert(entry_view.key.row.length == sizeof(row_a));
  assert(memcmp(entry_view.key.row.data, row_a, sizeof(row_a)) == 0);
  assert(entry_view.key.column_family.length == sizeof(family));
  assert(entry_view.key.timestamp == 7);
  assert(entry_view.value.length == sizeof(value_a));
  assert(memcmp(entry_view.value.data, value_a, sizeof(value_a)) == 0);
  assert(entry_view.deleted == 0);
  shoal_rfile_entry_result_free(&entry_result);
  shoal_rfile_entry_result_free(&entry_result);
  assert(shoal_rfile_reader_top_key(reader, &entry_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_rfile_entry_view_init(&entry_view);
  assert(shoal_rfile_entry_result_get(entry_result, &entry_view, &error) ==
         SHOAL_STATUS_OK);
  assert(entry_view.value.length == 0);
  shoal_rfile_entry_result_free(&entry_result);
  assert(shoal_rfile_reader_top_value(reader, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  copied = shoal_bytes_result_get(bytes_result);
  assert(copied.length == sizeof(value_a));
  assert(memcmp(copied.data, value_a, sizeof(value_a)) == 0);
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_rfile_reader_next(reader, 0, &error) == SHOAL_STATUS_OK);
  assert(shoal_rfile_reader_top(reader, &entry_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_rfile_entry_view_init(&entry_view);
  assert(shoal_rfile_entry_result_get(entry_result, &entry_view, &error) ==
         SHOAL_STATUS_OK);
  assert(entry_view.deleted == 1);
  shoal_rfile_entry_result_free(&entry_result);
  assert(shoal_rfile_reader_next(reader, 0, &error) == SHOAL_STATUS_OK);
  assert(shoal_rfile_reader_has_top(reader) == 0);
  assert(shoal_rfile_reader_close(reader, &error) == SHOAL_STATUS_OK);
  assert(shoal_rfile_reader_close(reader, &error) == SHOAL_STATUS_OK);
  shoal_rfile_reader_free(&reader);
  shoal_rfile_reader_free(&reader);
  shoal_rfile_seekable_free(&seekable);
  shoal_rfile_seekable_free(&seekable);

  const char *paths[] = {path_two, path_one};
  shoal_rfile_merge_config_init(&merge_config);
  merge_config.versions = 1;
  assert(shoal_rfile_reader_open_many(paths, 2, &merge_config, 0, &reader,
                                     &error) == SHOAL_STATUS_OK);
  assert(shoal_rfile_reader_has_top(reader) == 1);
  assert(shoal_rfile_reader_top(reader, &entry_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_rfile_entry_view_init(&entry_view);
  assert(shoal_rfile_entry_result_get(entry_result, &entry_view, &error) ==
         SHOAL_STATUS_OK);
  assert(entry_view.key.row.length == sizeof(row_a));
  shoal_rfile_entry_result_free(&entry_result);
  shoal_rfile_reader_free(&reader);

  assert(shoal_rfile_reader_open_sequential(path_two, 0, &reader, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_rfile_reader_has_top(reader) == 1);
  shoal_rfile_reader_free(&reader);
  remove(path_one);
  remove(path_two);
}

static void test_data_value_abi(void) {
  uint8_t row[] = {'r', '\0'};
  uint8_t family[] = {'f'};
  uint8_t qualifier[] = {'q'};
  uint8_t visibility[] = {'A'};
  uint8_t value[] = {'v', '\0', 'x'};
  shoal_key key = {
      {row, sizeof(row)},
      {family, sizeof(family)},
      {qualifier, sizeof(qualifier)},
      {visibility, sizeof(visibility)},
      7,
  };
  shoal_error *error = NULL;
  shoal_bytes_result *text = NULL;
  expect_error(shoal_key_to_string(NULL, &text, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "key");
  assert(shoal_key_to_string(&key, &text, &error) == SHOAL_STATUS_OK);
  shoal_bytes rendered = shoal_bytes_result_get(text);
  assert(rendered.length > sizeof(row));
  assert(memchr(rendered.data, '\0', rendered.length) == NULL);
  shoal_bytes_result_free(&text);

  shoal_range range;
  shoal_range_init(&range);
  range.start.kind = SHOAL_RANGE_BOUND_ROW;
  range.start.row = (shoal_bytes){(const uint8_t *)"b", 1};
  range.end.kind = SHOAL_RANGE_BOUND_ROW;
  range.end.row = (shoal_bytes){(const uint8_t *)"y", 1};
  range.start_inclusive = 1;
  range.end_inclusive = 0;
  uint8_t predicate = 9;
  expect_error(shoal_range_before_start_key(&range, NULL, &predicate, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "key");
  assert(predicate == 0);
  assert(shoal_range_before_start_key(&range, &key, &predicate, &error) ==
         SHOAL_STATUS_OK);
  assert(predicate == 0);
  assert(shoal_range_after_end_key(&range, &key, &predicate, &error) ==
         SHOAL_STATUS_OK);
  assert(predicate == 0);
  assert(shoal_range_to_string(&range, &text, &error) == SHOAL_STATUS_OK);
  rendered = shoal_bytes_result_get(text);
  assert(rendered.length >= 6);
  assert(memcmp(rendered.data, "Range ", 6) == 0);
  shoal_bytes_result_free(&text);

  shoal_key_value input;
  shoal_key_value_init(&input);
  input.key = key;
  input.value = (shoal_bytes){value, sizeof(value)};
  shoal_key_value_result *key_value = NULL;
  uint32_t key_value_size = input.struct_size;
  input.struct_size = SHOAL_KEY_VALUE_V1_SIZE - 1;
  expect_error(shoal_key_value_create(&input, &key_value, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "too small");
  input.struct_size = key_value_size;
  assert(shoal_key_value_create(&input, &key_value, &error) ==
         SHOAL_STATUS_OK);
  row[0] = 'z';
  value[0] = 'z';
  shoal_key_value_view view;
  memset(&view, 0, sizeof(view));
  assert(shoal_key_value_result_get(key_value, &view, &error) ==
         SHOAL_STATUS_OK);
  assert(view.row.length == sizeof(row) && view.row.data[0] == 'r');
  assert(view.value.length == sizeof(value) && view.value.data[0] == 'v');
  shoal_key_value_result_free(&key_value);
  shoal_key_value_result_free(&key_value);

  row[0] = 'r';
  value[0] = 'v';
  shoal_test_result_alloc_fail_after(0);
  assert(shoal_key_value_create(&input, &key_value, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(key_value == NULL && error != NULL);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();

  uint8_t label_binary[] = {'b', '\0'};
  shoal_bytes labels[] = {
      {(const uint8_t *)"z", 1},
      {label_binary, sizeof(label_binary)},
      {(const uint8_t *)"a", 1},
      {(const uint8_t *)"a", 1},
  };
  shoal_authorizations *auths = NULL;
  shoal_authorizations *same = NULL;
  shoal_bytes_list_result *listed = NULL;
  expect_error(shoal_authorizations_create(NULL, 1, &auths, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "labels");
  assert(shoal_authorizations_create(labels, 4, &auths, &error) ==
         SHOAL_STATUS_OK);
  label_binary[0] = 'x';
  assert(shoal_authorizations_count(auths) == 3);
  assert(shoal_authorizations_empty(auths) == 0);
  shoal_bytes original_binary = {(const uint8_t *)"b\0", 2};
  assert(shoal_authorizations_contains(auths, original_binary) == 1);
  assert(shoal_authorizations_contains(
             auths, (shoal_bytes){(const uint8_t *)"missing", 7}) == 0);
  assert(shoal_authorizations_list(auths, &listed, &error) == SHOAL_STATUS_OK);
  assert(shoal_bytes_list_count(listed) == 3);
  shoal_bytes listed_value = {0};
  assert(shoal_bytes_list_get(listed, 0, &listed_value, &error) ==
         SHOAL_STATUS_OK);
  assert(listed_value.length == 1 && listed_value.data[0] == 'a');
  shoal_bytes_list_free(&listed);
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_authorizations_list(auths, &listed, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "authorization list");
  assert(listed == NULL);
  shoal_test_result_alloc_reset();
  label_binary[0] = 'b';
  assert(shoal_authorizations_create(labels, 4, &same, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_authorizations_equal(auths, same) == 1);
  assert(shoal_authorization_character_is_valid('A') == 1);
  assert(shoal_authorization_character_is_valid(':') == 1);
  assert(shoal_authorization_character_is_valid('!') == 0);
  shoal_authorizations_free(&same);
  shoal_authorizations_free(&auths);
  shoal_authorizations_free(&auths);
}

static void test_buffered_writer_abi(shoal_connector *connector) {
  shoal_error *error = NULL;
  shoal_write_failure *failure = NULL;
  shoal_accumulo_writer *writer = NULL;
  shoal_batch_writer_config config;
  shoal_batch_writer_config_init(&config);
  config.table_name = "events";

  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_connector_create_accumulo_writer(
                   connector, &config, &writer, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "buffered writer");
  assert(writer == NULL);
  shoal_test_result_alloc_reset();

  assert(shoal_connector_create_accumulo_writer(
             connector, &config, &writer, &error) == SHOAL_STATUS_OK);
  assert(writer != NULL && error == NULL);
  static const uint8_t row_a[] = {'a', '\0'};
  static const uint8_t family[] = {'f'};
  static const uint8_t qualifier[] = {'q'};
  static const uint8_t value[] = {'v', '\0'};
  shoal_bytes row_a_bytes = {row_a, sizeof(row_a)};
  shoal_bytes family_bytes = {family, sizeof(family)};
  shoal_bytes qualifier_bytes = {qualifier, sizeof(qualifier)};
  shoal_bytes empty = {NULL, 0};
  shoal_bytes value_bytes = {value, sizeof(value)};
  expect_error(shoal_accumulo_writer_put(
                   writer, row_a_bytes, family_bytes, qualifier_bytes, empty,
                   0, value_bytes, 0, &failure, &error),
               SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error,
               "discovery unavailable");
  assert(failure == NULL);
  assert(shoal_accumulo_writer_close(writer, 0, &failure, &error) ==
         SHOAL_STATUS_OK);
  shoal_accumulo_writer_free(&writer);
  shoal_accumulo_writer_free(&writer);

  assert(shoal_test_accumulo_writer_create(
      SHOAL_TEST_WRITER_SUCCESS, &writer));
  shoal_bytes malformed = {NULL, 1};
  expect_error(shoal_accumulo_writer_put(
                   writer, malformed, family_bytes, qualifier_bytes, empty, 0,
                   value_bytes, 0, &failure, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "row");
  expect_error(shoal_accumulo_writer_put(
                   writer, row_a_bytes, family_bytes, qualifier_bytes, empty,
                   0, value_bytes, -1, &failure, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "timeout");
  assert(shoal_accumulo_writer_put(
             writer, row_a_bytes, family_bytes, qualifier_bytes, empty, 0,
             value_bytes, 0, &failure, &error) == SHOAL_STATUS_OK);
  assert(shoal_accumulo_writer_put(
             writer, row_a_bytes, family_bytes, qualifier_bytes, empty, 7,
             value_bytes, 0, &failure, &error) == SHOAL_STATUS_OK);
  static const uint8_t row_b[] = {'b'};
  shoal_bytes row_b_bytes = {row_b, sizeof(row_b)};
  assert(shoal_accumulo_writer_put_delete(
             writer, row_b_bytes, family_bytes, qualifier_bytes, empty, 0, 0,
             &failure, &error) == SHOAL_STATUS_OK);
  shoal_key key = {
      row_a_bytes,
      family_bytes,
      qualifier_bytes,
      empty,
      INT64_MAX,
  };
  assert(shoal_accumulo_writer_delete(writer, &key, 0, &failure, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_accumulo_writer_close(writer, 0, &failure, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_accumulo_writer_close(writer, 0, &failure, &error) ==
         SHOAL_STATUS_OK);
  shoal_accumulo_writer_free(&writer);

  assert(shoal_test_accumulo_writer_create(
      SHOAL_TEST_WRITER_STRUCTURED_FAILURE, &writer));
  assert(shoal_accumulo_writer_put(
             writer, row_a_bytes, family_bytes, qualifier_bytes, empty, 1,
             value_bytes, 0, &failure, &error) == SHOAL_STATUS_OK);
  assert(shoal_accumulo_writer_put(
             writer, row_b_bytes, family_bytes, qualifier_bytes, empty, 2,
             value_bytes, 0, &failure, &error) == SHOAL_STATUS_OK);
  expect_error(shoal_accumulo_writer_close(writer, 0, &failure, &error),
               SHOAL_STATUS_AMBIGUOUS_WRITE, &error, "batch writer failed");
  assert(failure != NULL);
  assert(shoal_write_failure_get_flags(failure) != 0);
  shoal_write_failure_free(&failure);
  shoal_accumulo_writer_free(&writer);
}

static void test_column_visibility(void) {
  uint8_t expression_data[] = "A&(B|C)";
  shoal_bytes expression = {expression_data, sizeof(expression_data) - 1};
  shoal_column_visibility *visibility = NULL;
  shoal_column_visibility *bad_visibility = NULL;
  shoal_visibility_node *tree = NULL;
  shoal_visibility_node *child = NULL;
  shoal_visibility_node *normalized = NULL;
  shoal_node_expression *term = NULL;
  shoal_visibility_evaluator *evaluator = NULL;
  shoal_authorizations *auths = NULL;
  shoal_authorizations *auths_copy = NULL;
  shoal_bytes_result *bytes_result = NULL;
  shoal_error *error = NULL;
  uint8_t satisfied = 0;
  int32_t comparison = 99;
  shoal_visibility_node_view view;

  assert(shoal_column_visibility_create(expression, &visibility, &error) ==
         SHOAL_STATUS_OK);
  expression_data[0] = 'Z';
  assert(shoal_column_visibility_expression(visibility, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_bytes bytes = shoal_bytes_result_get(bytes_result);
  assert(bytes.length == sizeof(expression_data) - 1);
  assert(memcmp(bytes.data, "A&(B|C)", bytes.length) == 0);
  shoal_bytes_result_free(&bytes_result);

  assert(shoal_column_visibility_tree(visibility, &tree, &error) ==
         SHOAL_STATUS_OK);
  shoal_visibility_node_view_init(&view);
  assert(shoal_visibility_node_get(tree, &view, &error) == SHOAL_STATUS_OK);
  assert(view.node_type == SHOAL_VISIBILITY_AND);
  assert(view.child_count == 2);
  assert(view.span_length == sizeof(expression_data) - 1);
  assert(!view.empty);
  assert(shoal_visibility_node_term(
             tree, (shoal_bytes){(const uint8_t *)"A&(B|C)", 7}, &term,
             &error) == SHOAL_STATUS_INVALID_ARGUMENT);
  shoal_visibility_parse_error_view nonterm_view;
  shoal_visibility_parse_error_view_init(&nonterm_view);
  assert(shoal_error_visibility_parse(error, &nonterm_view) ==
         SHOAL_STATUS_OK);
  assert(nonterm_view.offset == 0);
  assert(nonterm_view.terms.length == 7);
  assert(strstr(nonterm_view.reason, "AND node has no term") != NULL);
  shoal_error_free(&error);
  assert(shoal_visibility_node_child(tree, 0, &child, &error) ==
         SHOAL_STATUS_OK);
  shoal_visibility_node_view_init(&view);
  assert(shoal_visibility_node_get(child, &view, &error) == SHOAL_STATUS_OK);
  assert(view.node_type == SHOAL_VISIBILITY_TERM);
  assert(shoal_visibility_node_term(
             child, (shoal_bytes){(const uint8_t *)"A&(B|C)", 7}, &term,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_node_expression_size(term) == 1);
  assert(shoal_node_expression_buffer(term, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  bytes = shoal_bytes_result_get(bytes_result);
  assert(bytes.length == 1 && bytes.data[0] == 'A');
  shoal_bytes_result_free(&bytes_result);
  shoal_node_expression_free(&term);
  shoal_visibility_node_free(&child);

  assert(shoal_column_visibility_normalized(visibility, &normalized, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_visibility_node_compare(tree, normalized, &comparison, &error) ==
         SHOAL_STATUS_OK);
  assert(comparison == 0);
  assert(shoal_column_visibility_flatten(visibility, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  bytes = shoal_bytes_result_get(bytes_result);
  assert(bytes.length == 7 && memcmp(bytes.data, "A&(B|C)", 7) == 0);
  shoal_bytes_result_free(&bytes_result);

  shoal_bytes labels[] = {
      {(const uint8_t *)"A", 1},
      {(const uint8_t *)"C", 1},
  };
  assert(shoal_authorizations_create(labels, 2, &auths, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_visibility_evaluator_create(auths, &evaluator, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_visibility_evaluator_evaluate(
             evaluator, (shoal_bytes){(const uint8_t *)"A&(B|C)", 7},
             &satisfied, &error) == SHOAL_STATUS_OK);
  assert(satisfied);
  assert(shoal_visibility_evaluator_evaluate_tree(
             evaluator, (shoal_bytes){(const uint8_t *)"A&(B|C)", 7}, tree,
             &satisfied, &error) == SHOAL_STATUS_OK);
  assert(satisfied);
  assert(shoal_visibility_evaluator_authorizations(evaluator, &auths_copy,
                                                   &error) == SHOAL_STATUS_OK);
  assert(shoal_authorizations_count(auths_copy) == 2);
  shoal_authorizations_free(&auths_copy);
  assert(shoal_visibility_evaluator_set_authorizations(evaluator, NULL,
                                                       &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_visibility_evaluator_evaluate(
             evaluator, (shoal_bytes){(const uint8_t *)"A", 1}, &satisfied,
             &error) == SHOAL_STATUS_OK);
  assert(!satisfied);

  assert(shoal_visibility_evaluator_evaluate_tree(
             evaluator, (shoal_bytes){(const uint8_t *)"different", 9}, tree,
             &satisfied, &error) == SHOAL_STATUS_INVALID_ARGUMENT);
  assert(error != NULL);
  assert(shoal_error_source(error) ==
         SHOAL_ERROR_SOURCE_VISIBILITY_PARSE_EXCEPTION);
  shoal_visibility_parse_error_view parse_view;
  shoal_visibility_parse_error_view_init(&parse_view);
  assert(shoal_error_visibility_parse(error, &parse_view) == SHOAL_STATUS_OK);
  assert(parse_view.offset == 0);
  assert(parse_view.terms.length == 9);
  assert(strstr(parse_view.reason, "different expression") != NULL);
  shoal_error_free(&error);

  assert(shoal_column_visibility_create(
             (shoal_bytes){(const uint8_t *)"A&", 2}, &bad_visibility,
             &error) == SHOAL_STATUS_INVALID_ARGUMENT);
  assert(error != NULL);
  shoal_visibility_parse_error_view_init(&parse_view);
  assert(shoal_error_visibility_parse(error, &parse_view) == SHOAL_STATUS_OK);
  assert(parse_view.terms.length == 2);
  assert(memcmp(parse_view.terms.data, "A&", 2) == 0);
  shoal_error_free(&error);

  assert(shoal_node_expression_create(
             (shoal_bytes){(const uint8_t *)"wxyz", 4}, 1, 2, &term,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_node_expression_term(term, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  bytes = shoal_bytes_result_get(bytes_result);
  assert(bytes.length == 2 && memcmp(bytes.data, "xy", 2) == 0);
  shoal_bytes_result_free(&bytes_result);
  shoal_node_expression_free(&term);

  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_column_visibility_tree(visibility, &child, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate visibility node");
  shoal_test_result_alloc_reset();
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_column_visibility_create(
                   (shoal_bytes){(const uint8_t *)"A", 1}, &bad_visibility,
                   &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error,
               "allocate column visibility handle");
  shoal_test_result_alloc_reset();
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_node_expression_create(
                   (shoal_bytes){(const uint8_t *)"A", 1}, 0, 1, &term,
                   &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error,
               "allocate node expression handle");
  shoal_test_result_alloc_reset();
  shoal_visibility_evaluator_free(&evaluator);
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_visibility_evaluator_create(auths, &evaluator, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error,
               "allocate visibility evaluator handle");
  shoal_test_result_alloc_reset();
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_column_visibility_flatten(visibility, &bytes_result,
                                               &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate bytes result");
  shoal_test_result_alloc_reset();

  memset(&view, 0, sizeof(view));
  expect_error(shoal_visibility_node_get(tree, &view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "must be initialized");
  memset(&parse_view, 0, sizeof(parse_view));
  assert(shoal_error_visibility_parse(NULL, &parse_view) ==
         SHOAL_STATUS_INVALID_ARGUMENT);
  bytes_result = (shoal_bytes_result *)(uintptr_t)1;
  expect_error(shoal_column_visibility_expression(NULL, &bytes_result, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "handle is NULL");
  assert(bytes_result == NULL);
  bytes_result = (shoal_bytes_result *)(uintptr_t)1;
  expect_error(shoal_column_visibility_flatten(NULL, &bytes_result, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "handle is NULL");
  assert(bytes_result == NULL);
  bytes_result = (shoal_bytes_result *)(uintptr_t)1;
  expect_error(shoal_node_expression_term(NULL, &bytes_result, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "handle is NULL");
  assert(bytes_result == NULL);
  bytes_result = (shoal_bytes_result *)(uintptr_t)1;
  expect_error(shoal_visibility_node_expression(NULL, &bytes_result, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "handle is NULL");
  assert(bytes_result == NULL);

  shoal_visibility_evaluator_free(&evaluator);
  shoal_visibility_evaluator_free(&evaluator);
  shoal_authorizations_free(&auths);
  shoal_visibility_node_free(&normalized);
  shoal_visibility_node_free(&tree);
  shoal_column_visibility_free(&visibility);
  shoal_column_visibility_free(&visibility);
}

int main(void) {
  test_column_visibility();
  shoal_connector *connector = NULL;
  shoal_connector *admin_connector = NULL;
  shoal_client *client = NULL;
  shoal_client *admin_client = NULL;
  shoal_cancellation *cancellation = NULL;
  shoal_scanner *scanner = NULL;
  shoal_batch_scanner *batch_scanner = NULL;
  shoal_scan_result *result = NULL;
  shoal_scan_cursor *cursor = NULL;
  shoal_table_list_result *table_list = NULL;
  shoal_mutation *mutation = NULL;
  shoal_batch_writer *writer = NULL;
  shoal_accumulo_writer *client_writer = NULL;
  shoal_write_failure *write_failure = NULL;
  shoal_table_properties_result *properties = NULL;
  shoal_namespace_list_result *namespace_list = NULL;
  shoal_namespace_properties_result *namespace_properties = NULL;
  shoal_versioned_properties_result *versioned_properties = NULL;
  shoal_bytes_list_result *bytes_list = NULL;
  shoal_connector_identity_result *identity = NULL;
  shoal_range_result *range_result = NULL;
  shoal_iterator_setting_result *iterator_result = NULL;
  shoal_configuration *configuration = NULL;
  shoal_bytes_result *bytes_result = NULL;
  shoal_string_list_result *string_list = NULL;
  shoal_server_list_result *server_list = NULL;
  shoal_error *error = NULL;

  test_v1_initializers();
  test_rfile_abi();
  test_data_value_abi();
  assert(shoal_abi_version() == SHOAL_ABI_VERSION);
  assert(shoal_abi_version_major() == SHOAL_ABI_VERSION_MAJOR);
  assert(shoal_abi_version_minor() == SHOAL_ABI_VERSION_MINOR);
  assert(shoal_abi_version_patch() == SHOAL_ABI_VERSION_PATCH);
  assert(shoal_abi_version_packed() == SHOAL_ABI_VERSION_PACKED);
  assert(shoal_abi_capability_count() == SHOAL_ABI_CAPABILITY_COUNT);
  assert(shoal_abi_capability_word_count() == SHOAL_ABI_CAPABILITY_WORD_COUNT);
  assert(shoal_abi_capability_word(0) == SHOAL_ABI_CAPABILITY_WORD0);
  assert(shoal_abi_capability_word(1) == 0);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_CONNECTOR) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_BOOTSTRAP) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_ERROR) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_SCANNER) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_BATCH_SCANNER) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_OWNED_SCAN_RESULT) ==
         1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_MUTATION) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_BATCH_WRITER) == 1);
  assert(shoal_abi_has_capability(
             SHOAL_ABI_CAPABILITY_STRUCTURED_WRITE_FAILURE) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_TABLE_ADMIN) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY) ==
         1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_RFILE) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_DATA_VALUES) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_BUFFERED_WRITER) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER) ==
         1);
  assert(shoal_abi_has_capability(
             SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_COUNT) == 0);
  assert(shoal_abi_has_capability(63u) == 0);
  assert(shoal_abi_has_capability(64u) == 0);
  expect_error(shoal_connector_create(NULL, &connector, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "config");
  assert(connector == NULL);

  shoal_connector_config config;
  shoal_connector_config_init(&config);
  assert(config.struct_size == SHOAL_CONNECTOR_CONFIG_V1_SIZE);

  config.bootstrap = SHOAL_BOOTSTRAP_STATIC;
  config.instance_name = "accumulo";
  config.instance_id = "00000000-0000-0000-0000-000000000001";
  config.principal = "root";
  {
    static const uint8_t password[] = {'s', 'e', 'c', '\0', 'r', 'e', 't'};
    config.password = password;
    config.password_length = sizeof(password);
  }
  struct {
    shoal_connector_config value;
    uint8_t future[16];
  } future_connector_config;
  memset(&future_connector_config, 0xa5, sizeof(future_connector_config));
  future_connector_config.value = config;
  future_connector_config.value.struct_size =
      (uint32_t)sizeof(future_connector_config);
  shoal_connector *future_connector = NULL;
  assert(shoal_connector_create(&future_connector_config.value,
                                &future_connector, &error) == SHOAL_STATUS_OK);
  assert(future_connector != NULL && error == NULL);
  assert(shoal_connector_close(future_connector, &error) == SHOAL_STATUS_OK);
  shoal_connector_free(&future_connector);

  config.accumulo_version = "2.1.6";
  expect_error(shoal_connector_create(&config, &connector, &error),
               SHOAL_STATUS_UNSUPPORTED, &error, "Accumulo 4");
  assert(connector == NULL);

  config.accumulo_version = NULL;
  assert(shoal_connector_create(&config, &connector, &error) ==
         SHOAL_STATUS_OK);
  assert(connector != NULL);
  assert(error == NULL);
  test_buffered_writer_abi(connector);

  assert(shoal_test_connector_create(&admin_connector));
  assert(admin_connector != NULL);

  expect_error(shoal_cancellation_create(NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "out_cancellation is required");
  expect_error(shoal_cancellation_cancel(NULL, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error,
               "cancellation handle is NULL");
  shoal_test_result_alloc_fail_after(0);
  assert(shoal_cancellation_create(&cancellation, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(cancellation == NULL);
  assert(error != NULL);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();
  assert(shoal_cancellation_create(&cancellation, &error) == SHOAL_STATUS_OK);
  assert(cancellation != NULL && error == NULL);
  uint8_t cancelled = 2;
  assert(shoal_cancellation_is_cancelled(cancellation, &cancelled, &error) ==
         SHOAL_STATUS_OK);
  assert(cancelled == 0 && error == NULL);
  expect_error(shoal_cancellation_is_cancelled(cancellation, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "out_cancelled is required");
  expect_error(shoal_scanner_scan_with_cancellation(
                   NULL, NULL, 0, cancellation, &result, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "scanner handle is NULL");
  assert(result == NULL);
  expect_error(shoal_batch_scanner_scan_with_cancellation(
                   NULL, NULL, 0, 0, cancellation, &result, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error,
               "batch scanner handle is NULL");
  assert(result == NULL);
  assert(shoal_cancellation_cancel(cancellation, &error) == SHOAL_STATUS_OK);
  assert(shoal_cancellation_cancel(cancellation, &error) == SHOAL_STATUS_OK);
  assert(shoal_cancellation_is_cancelled(cancellation, &cancelled, &error) ==
         SHOAL_STATUS_OK);
  assert(cancelled == 1 && error == NULL);
  shoal_cancellation_free(&cancellation);
  assert(cancellation == NULL);
  shoal_cancellation_free(&cancellation);

  expect_error(shoal_connector_invalidate_table(
                   admin_connector, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "table_id is required");
  expect_error(shoal_connector_invalidate_table(
                   admin_connector, "", &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "table_id is required");
  char invalidated_table_id[] = {'5', '\0'};
  assert(shoal_connector_invalidate_table(
             admin_connector, invalidated_table_id, &error) ==
         SHOAL_STATUS_OK);
  invalidated_table_id[0] = '6';
  assert(shoal_connector_invalidate_discovery(admin_connector, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_test_connector_invalidation_matches(admin_connector, "5", 1));

  shoal_client_config client_config;
  shoal_client_config_init(&client_config);
  assert(client_config.struct_size == SHOAL_CLIENT_CONFIG_V1_SIZE);
  assert(client_config.thread_count == 10);
  expect_error(shoal_client_create(NULL, &client, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "client config is required");
  expect_error(shoal_client_create(&client_config, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "out_client is required");
  client_config.struct_size = SHOAL_CLIENT_CONFIG_V1_SIZE - 1;
  expect_error(shoal_client_create(&client_config, &client, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "struct_size");
  shoal_client_config_init(&client_config);
  expect_error(shoal_client_create(&client_config, &client, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "connector config is required");
  client_config.connector = &config;
  client_config.thread_count = 0;
  expect_error(shoal_client_create(&client_config, &client, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "thread_count must be positive");
  char initial_table[] = "events";
  uint8_t initial_authorization_data[] = {'A', 0, 'B'};
  shoal_bytes initial_authorization = {
      initial_authorization_data, sizeof(initial_authorization_data)};
  client_config.thread_count = 10;
  client_config.table_name = initial_table;
  client_config.authorizations = &initial_authorization;
  client_config.authorization_count = 1;
  shoal_test_result_alloc_fail_after(0);
  assert(shoal_client_create(&client_config, &client, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(client == NULL && error != NULL);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();
  assert(shoal_client_create(&client_config, &client, &error) ==
         SHOAL_STATUS_OK);
  assert(client != NULL && error == NULL);
  initial_table[0] = 'x';
  initial_authorization_data[0] = 'Z';
  const uint8_t expected_initial_authorization_data[] = {'A', 0, 'B'};
  shoal_bytes expected_initial_authorization = {
      expected_initial_authorization_data,
      sizeof(expected_initial_authorization_data)};
  assert(shoal_test_client_settings_match(
      client, "events", expected_initial_authorization, 10));
  expect_error(shoal_client_set_threads(client, 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "thread_count must be positive");
  assert(shoal_client_set_table(client, NULL, &error) == SHOAL_STATUS_OK);
  assert(shoal_test_client_settings_match(
      client, "events", expected_initial_authorization, 10));
  expect_error(shoal_client_set_authorizations(client, NULL, 1, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "authorizations");
  char updated_table[] = "analytics";
  uint8_t updated_authorization_data[] = {'x', 0, 'y'};
  shoal_bytes updated_authorization = {
      updated_authorization_data, sizeof(updated_authorization_data)};
  assert(shoal_client_set_table(client, updated_table, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_client_set_authorizations(
             client, &updated_authorization, 1, &error) == SHOAL_STATUS_OK);
  assert(shoal_client_set_threads(client, 17, &error) == SHOAL_STATUS_OK);
  updated_table[0] = 'z';
  updated_authorization_data[0] = 'z';
  const uint8_t expected_updated_authorization_data[] = {'x', 0, 'y'};
  shoal_bytes expected_updated_authorization = {
      expected_updated_authorization_data,
      sizeof(expected_updated_authorization_data)};
  assert(shoal_test_client_settings_match(
      client, "analytics", expected_updated_authorization, 17));
  expect_error(shoal_client_create_batch_writer(client, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "out_writer is required");
  shoal_test_result_alloc_fail_after(0);
  expect_error(
      shoal_client_create_batch_writer(client, &client_writer, &error),
      SHOAL_STATUS_OUT_OF_MEMORY, &error, "buffered writer handle");
  assert(client_writer == NULL);
  shoal_test_result_alloc_reset();
  assert(shoal_client_create_batch_writer(client, &client_writer, &error) ==
         SHOAL_STATUS_OK);
  assert(client_writer != NULL && error == NULL);
  shoal_accumulo_writer_free(&client_writer);
  assert(shoal_client_close(client, &error) == SHOAL_STATUS_OK);
  assert(shoal_client_close(client, &error) == SHOAL_STATUS_OK);
  expect_error(shoal_client_set_table(client, "later", &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_client_create_scanner(client, &scanner, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_client_create_batch_writer(client, &client_writer, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  shoal_client_free(&client);
  assert(client == NULL);
  shoal_client_free(&client);

  assert(shoal_test_client_create(&admin_client));
  assert(admin_client != NULL);
  expect_error(shoal_client_create_scanner(admin_client, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "out_scanner is required");
  assert(shoal_client_create_scanner(admin_client, &scanner, &error) ==
         SHOAL_STATUS_OK);
  assert(scanner != NULL && error == NULL);
  shoal_scanner_free(&scanner);
  uint8_t client_family_data[] = {'f', 0, 'm'};
  uint8_t client_qualifier_data[] = {'q', 0};
  shoal_bytes client_family = {
      client_family_data, sizeof(client_family_data)};
  shoal_bytes client_qualifier = {
      client_qualifier_data, sizeof(client_qualifier_data)};
  shoal_bytes empty_bytes = {NULL, 0};
  assert(shoal_client_select_column(
             admin_client, client_family, NULL, &error) == SHOAL_STATUS_OK);
  client_family_data[0] = 'x';
  const uint8_t expected_client_family_data[] = {'f', 0, 'm'};
  shoal_bytes expected_client_family = {
      expected_client_family_data, sizeof(expected_client_family_data)};
  assert(shoal_test_client_columns_match(
      admin_client, expected_client_family, empty_bytes, 0, 1));
  client_family_data[0] = 'f';
  assert(shoal_client_select_column(
             admin_client, client_family, &client_qualifier, &error) ==
         SHOAL_STATUS_OK);
  client_family_data[0] = 'x';
  client_qualifier_data[0] = 'x';
  const uint8_t expected_client_qualifier_data[] = {'q', 0};
  shoal_bytes expected_client_qualifier = {
      expected_client_qualifier_data, sizeof(expected_client_qualifier_data)};
  assert(shoal_test_client_columns_match(
      admin_client, expected_client_family, expected_client_qualifier, 1, 2));
  shoal_bytes malformed_column = {NULL, 1};
  expect_error(shoal_client_select_column(
                   admin_client, malformed_column, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "column family");

  shoal_range client_ranges[2];
  shoal_range_init(&client_ranges[0]);
  shoal_range_init(&client_ranges[1]);
  expect_error(shoal_client_scan_range(
                   admin_client, NULL, 0, &result, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "range is required");
  expect_error(shoal_client_scan_range(
                   admin_client, &client_ranges[0], -1, &result, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "timeout_ms must not be negative");
  expect_error(shoal_client_scan_ranges(
                   admin_client, NULL, 0, 0, &result, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "at least one range is required");
  assert(shoal_client_scan_range(
             admin_client, &client_ranges[0], 0, &result, &error) ==
         SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 1);
  shoal_scan_result_free(&result);
  assert(shoal_client_scan_ranges(
             admin_client, client_ranges, 2, 0, &result, &error) ==
         SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 2);
  shoal_scan_result_free(&result);
  uint8_t exhausted = 0;
  expect_error(shoal_client_stream_range(
                   admin_client, NULL, 0, &cursor, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "range is required");
  assert(cursor == NULL);
  assert(shoal_client_stream_range(
             admin_client, &client_ranges[0], 0, &cursor, &error) ==
         SHOAL_STATUS_OK);
  assert(cursor != NULL && error == NULL);
  expect_error(shoal_scan_cursor_next(
                   cursor, 0, &result, &exhausted, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "max_entries");
  assert(result == NULL && exhausted == 0);
  assert(shoal_scan_cursor_next(
             cursor, 2, &result, &exhausted, &error) == SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 2);
  assert(exhausted == 0);
  shoal_scan_result_free(&result);
  assert(shoal_scan_cursor_next(
             cursor, 2, &result, &exhausted, &error) == SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 1);
  assert(exhausted == 1);
  shoal_scan_result_free(&result);
  assert(shoal_scan_cursor_next(
             cursor, 2, &result, &exhausted, &error) == SHOAL_STATUS_OK);
  assert(result == NULL && exhausted == 1);
  assert(shoal_scan_cursor_close(cursor, &error) == SHOAL_STATUS_OK);
  assert(shoal_scan_cursor_close(cursor, &error) == SHOAL_STATUS_OK);
  shoal_scan_cursor_free(&cursor);
  assert(cursor == NULL);
  shoal_scan_cursor_free(&cursor);

  assert(shoal_client_stream_ranges(
             admin_client, client_ranges, 2, 0, &cursor, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_scan_cursor_next(
             cursor, 8, &result, &exhausted, &error) == SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 2);
  assert(exhausted == 1);
  shoal_scan_cursor_free(&cursor);
  shoal_key_value_view entry;
  memset(&entry, 0, sizeof(entry));
  assert(shoal_scan_result_get(result, 1, &entry, &error) == SHOAL_STATUS_OK);
  assert(entry.timestamp == -7);
  assert(entry.value.length == 3);
  assert(entry.value.data[0] == 0 && entry.value.data[1] == 1 &&
         entry.value.data[2] == 0);
  shoal_scan_result_free(&result);

  assert(shoal_test_scanners_create(&scanner, &batch_scanner) == 1);
  assert(scanner != NULL && batch_scanner != NULL);
  assert(shoal_scanner_stream(
             scanner, &client_ranges[0], 0, &cursor, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_scan_cursor_next(
             cursor, 8, &result, &exhausted, &error) == SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 1);
  assert(exhausted == 1);
  shoal_scan_result_free(&result);
  shoal_scan_cursor_free(&cursor);

  assert(shoal_cancellation_create(&cancellation, &error) == SHOAL_STATUS_OK);
  assert(shoal_scanner_stream_with_cancellation(
             scanner, &client_ranges[0], 0, cancellation, &cursor, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_scan_cursor_next(
             cursor, 8, &result, &exhausted, &error) == SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 1);
  shoal_scan_result_free(&result);
  shoal_scan_cursor_free(&cursor);

  assert(shoal_batch_scanner_stream(
             batch_scanner, client_ranges, 2, 0, &cursor, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_scan_cursor_next(
             cursor, 8, &result, &exhausted, &error) == SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 2);
  assert(exhausted == 1);
  shoal_scan_result_free(&result);
  shoal_scan_cursor_free(&cursor);

  assert(shoal_batch_scanner_stream_with_cancellation(
             batch_scanner, client_ranges, 2, 0, cancellation, &cursor,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_scan_cursor_next(
             cursor, 8, &result, &exhausted, &error) == SHOAL_STATUS_OK);
  assert(result != NULL && shoal_scan_result_count(result) == 2);
  shoal_scan_result_free(&result);
  shoal_scan_cursor_free(&cursor);
  shoal_cancellation_free(&cancellation);
  assert(shoal_scanner_close(scanner, &error) == SHOAL_STATUS_OK);
  assert(shoal_batch_scanner_close(batch_scanner, &error) == SHOAL_STATUS_OK);
  shoal_scanner_free(&scanner);
  shoal_batch_scanner_free(&batch_scanner);

  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_client_stream_range(
                   admin_client, &client_ranges[0], 0, &cursor, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "scan cursor handle");
  assert(cursor == NULL);
  shoal_test_result_alloc_reset();
  assert(shoal_client_stream_range(
             admin_client, &client_ranges[0], 0, &cursor, &error) ==
         SHOAL_STATUS_OK);
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_scan_cursor_next(
                   cursor, 1, &result, &exhausted, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "scan result");
  assert(result == NULL);
  shoal_test_result_alloc_reset();
  shoal_scan_cursor_free(&cursor);
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_client_scan_range(
                   admin_client, &client_ranges[0], 0, &result, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "scan result");
  assert(result == NULL);
  shoal_test_result_alloc_reset();
  assert(shoal_cancellation_create(&cancellation, &error) == SHOAL_STATUS_OK);
  assert(shoal_cancellation_cancel(cancellation, &error) == SHOAL_STATUS_OK);
  expect_error(shoal_client_stream_range_with_cancellation(
                   admin_client, &client_ranges[0], 0, cancellation, &cursor,
                   &error),
               SHOAL_STATUS_CANCELLED, &error, "context canceled");
  assert(cursor == NULL);
  expect_error(shoal_client_scan_range_with_cancellation(
                   admin_client, &client_ranges[0], 0, cancellation, &result,
                   &error),
               SHOAL_STATUS_CANCELLED, &error, "context canceled");
  assert(result == NULL);
  expect_error(shoal_client_scan_ranges_with_cancellation(
                   admin_client, client_ranges, 2, 0, cancellation, &result,
                   &error),
               SHOAL_STATUS_CANCELLED, &error, "context canceled");
  assert(result == NULL);
  shoal_cancellation_free(&cancellation);
  expect_error(shoal_client_list_tables(admin_client, 0, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "out_result is required");
  assert(shoal_client_list_tables(
             admin_client, 0, &table_list, &error) == SHOAL_STATUS_OK);
  assert(table_list != NULL && shoal_table_list_count(table_list) == 2);
  shoal_table_list_free(&table_list);
  shoal_test_result_alloc_fail_after(0);
  assert(shoal_client_list_tables(
             admin_client, 0, &table_list, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(table_list == NULL && error != NULL);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();
  assert(shoal_client_stream_range(
             admin_client, &client_ranges[0], 0, &cursor, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_client_close(admin_client, &error) == SHOAL_STATUS_OK);
  expect_error(shoal_scan_cursor_next(
                   cursor, 1, &result, &exhausted, &error),
               SHOAL_STATUS_CANCELLED, &error, "context canceled");
  shoal_scan_cursor_free(&cursor);
  expect_error(shoal_client_scan_range(
                   admin_client, &client_ranges[0], 0, &result, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_client_select_column(
                   admin_client, expected_client_family, NULL, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  shoal_client_free(&admin_client);
  assert(admin_client == NULL);

  assert(shoal_configuration_create(&configuration, &error) == SHOAL_STATUS_OK);
  assert(configuration != NULL && error == NULL);
  const uint8_t binary_name[] = {'n', '\0', 'k'};
  const uint8_t binary_value[] = {'v', '\0', 'x'};
  shoal_bytes name = {binary_name, sizeof(binary_name)};
  shoal_bytes value = {binary_value, sizeof(binary_value)};
  assert(shoal_configuration_set(configuration, name, value, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_configuration_get(configuration, name, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_bytes got = shoal_bytes_result_get(bytes_result);
  assert(got.length == sizeof(binary_value));
  assert(memcmp(got.data, binary_value, sizeof(binary_value)) == 0);
  shoal_bytes_result_free(&bytes_result);
  shoal_bytes_result_free(&bytes_result);
  const uint8_t number_value[] = {'4', '2'};
  shoal_bytes number_name = {(const uint8_t *)"number", 6};
  shoal_bytes number = {number_value, sizeof(number_value)};
  uint32_t parsed = 0;
  assert(shoal_configuration_set(configuration, number_name, number, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_configuration_get_uint32(configuration, number_name, &parsed,
                                        &error) == SHOAL_STATUS_OK);
  assert(parsed == 42);
  shoal_bytes missing = {(const uint8_t *)"missing", 7};
  assert(shoal_configuration_get_or(configuration, missing, value,
                                    &bytes_result, &error) == SHOAL_STATUS_OK);
  got = shoal_bytes_result_get(bytes_result);
  assert(got.length == sizeof(binary_value));
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_configuration_get_uint32_or(configuration, missing, 17, &parsed,
                                           &error) == SHOAL_STATUS_OK);
  assert(parsed == 17);
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_configuration_get(configuration, name, &bytes_result,
                                       &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate bytes result");
  shoal_test_result_alloc_reset();
  shoal_configuration_free(&configuration);
  shoal_configuration_free(&configuration);

  assert(shoal_connector_get_root(connector, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  got = shoal_bytes_result_get(bytes_result);
  assert(got.length > 10);
  assert(memcmp(got.data, "/accumulo/", 10) == 0);
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_connector_get_zookeepers(connector, &string_list, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_string_list_count(string_list) == 0);
  shoal_string_list_free(&string_list);
  assert(shoal_connector_get_configuration(connector, &configuration, &error) ==
         SHOAL_STATUS_OK);
  assert(configuration != NULL);
  shoal_configuration_free(&configuration);
  expect_error(shoal_connector_get_root_tablet_location(
                   connector, 0, &bytes_result, &error),
               SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error,
               "discovery unavailable");
  expect_error(shoal_connector_get_manager_locations(
                   connector, 0, &string_list, &error),
               SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error,
               "discovery unavailable");
  expect_error(shoal_connector_get_servers(connector, 0, &server_list, &error),
               SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error,
               "discovery unavailable");

  expect_error(shoal_connector_get_identity(NULL, 0, &identity, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "NULL");
  expect_error(shoal_connector_get_identity(connector, -1, &identity, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "timeout");
  expect_error(shoal_connector_get_identity(connector, 0, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "out_result");
  assert(shoal_connector_get_identity(connector, 0, &identity, &error) ==
         SHOAL_STATUS_OK);
  assert(identity != NULL && error == NULL);
  shoal_connector_identity_view identity_view;
  memset(&identity_view, 0, sizeof(identity_view));
  expect_error(shoal_connector_identity_get(identity, &identity_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "struct_size");
  shoal_connector_identity_view_init(&identity_view);
  assert(identity_view.struct_size == SHOAL_CONNECTOR_IDENTITY_VIEW_V1_SIZE);
  assert(shoal_connector_identity_get(identity, &identity_view, &error) ==
         SHOAL_STATUS_OK);
  assert(strcmp(identity_view.instance_name, "accumulo") == 0);
  assert(strcmp(identity_view.instance_id,
                "00000000-0000-0000-0000-000000000001") == 0);
  assert(strcmp(identity_view.principal, "root") == 0);
  struct {
    shoal_connector_identity_view view;
    uint8_t future[16];
  } future_identity;
  memset(&future_identity, 0xa5, sizeof(future_identity));
  shoal_connector_identity_view_init(&future_identity.view);
  future_identity.view.struct_size = (uint32_t)sizeof(future_identity);
  assert(shoal_connector_identity_get(identity, &future_identity.view,
                                      &error) == SHOAL_STATUS_OK);
  for (size_t i = 0; i < sizeof(future_identity.future); ++i) {
    assert(future_identity.future[i] == UINT8_C(0xa5));
  }
  expect_error(shoal_connector_identity_get(identity, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "out_identity");
  expect_error(shoal_connector_identity_get(NULL, &identity_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "identity result");
  shoal_connector_identity_free(&identity);
  assert(identity == NULL);
  shoal_connector_identity_free(&identity);
  expect_error(shoal_zookeeper_resolve_instance(
                   NULL, "zk:2181", 1000, 1000, NULL, &identity, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "instance_name");
  expect_error(shoal_zookeeper_resolve_instance(
                   "accumulo", NULL, 1000, 1000, NULL, &identity, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "zookeeper_servers");
  expect_error(shoal_zookeeper_resolve_instance(
                   "accumulo", "zk:2181", -1, 1000, NULL, &identity, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "session_timeout_ms");
  expect_error(shoal_zookeeper_resolve_instance(
                   "accumulo", "zk:2181", 1000, -1, NULL, &identity, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "bootstrap_timeout_ms");
  expect_error(shoal_zookeeper_resolve_instance(
                   "accumulo", "zk:2181", 1000, 1000, NULL, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "out_result");

  for (size_t allocation = 0; allocation < 3; ++allocation) {
    shoal_test_string_alloc_fail_after(allocation);
    expect_error(shoal_connector_get_identity(admin_connector, 0, &identity,
                                              &error),
                 SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate");
    assert(identity == NULL);
    shoal_test_string_alloc_reset();
  }
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_connector_get_identity(admin_connector, 0, &identity,
                                            &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error,
               "allocate connector identity result");
  assert(identity == NULL);
  shoal_test_result_alloc_reset();
  assert(shoal_test_connector_identity_block(admin_connector, 1));
  expect_error(shoal_connector_get_identity(admin_connector, 1, &identity,
                                            &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline");
  assert(identity == NULL);
  assert(shoal_test_connector_identity_block(admin_connector, 0));

  assert(shoal_connector_get_root_tablet_location(
             admin_connector, 0, &bytes_result, &error) == SHOAL_STATUS_OK);
  got = shoal_bytes_result_get(bytes_result);
  assert(got.length == strlen("tablet.example:9997"));
  assert(memcmp(got.data, "tablet.example:9997", got.length) == 0);
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_connector_get_manager_locations(
             admin_connector, 0, &string_list, &error) == SHOAL_STATUS_OK);
  assert(shoal_string_list_count(string_list) == 1);
  assert(shoal_string_list_get(string_list, 0, &got, &error) ==
         SHOAL_STATUS_OK);
  assert(got.length == strlen("manager.example:9999"));
  shoal_string_list_free(&string_list);
  assert(shoal_connector_get_servers(admin_connector, 0, &server_list,
                                     &error) == SHOAL_STATUS_OK);
  assert(shoal_server_list_count(server_list) == 1);
  shoal_server_view server_view;
  shoal_server_view_init(&server_view);
  assert(shoal_server_list_get(server_list, 0, &server_view, &error) ==
         SHOAL_STATUS_OK);
  assert(server_view.port == 9997);
  assert(server_view.kind.length == strlen("tserver"));
  assert(server_view.group.length == strlen("default"));
  assert(server_view.host.length == strlen("server.example"));
  shoal_server_list_free(&server_list);
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_connector_get_manager_locations(
                   admin_connector, 0, &string_list, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate string list");
  expect_error(shoal_connector_get_servers(admin_connector, 0, &server_list,
                                           &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate server list");
  shoal_test_result_alloc_reset();
  assert(shoal_test_connector_topology_block(admin_connector, 1));
  expect_error(shoal_connector_get_root_tablet_location(
                   admin_connector, 1, &bytes_result, &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline");
  expect_error(shoal_connector_get_manager_locations(
                   admin_connector, 1, &string_list, &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline");
  expect_error(shoal_connector_get_servers(admin_connector, 1, &server_list,
                                           &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline");
  assert(shoal_test_connector_topology_block(admin_connector, 0));

  uint8_t start_row[] = {'s', '\0', 'r'};
  uint8_t start_cf[] = {'c', 'f'};
  uint8_t start_cq[] = {'c', 'q'};
  uint8_t start_cv[] = {'v'};
  uint8_t end_row[] = {'t', '\0', 'r'};
  uint8_t end_cf[] = {'e', 'f'};
  uint8_t end_cq[] = {'e', 'q'};
  uint8_t end_cv[] = {'e', 'v'};
  shoal_range descriptor_range;
  shoal_range_init(&descriptor_range);
  descriptor_range.start.kind = SHOAL_RANGE_BOUND_KEY;
  descriptor_range.start.key.row =
      (shoal_bytes){start_row, sizeof(start_row)};
  descriptor_range.start.key.column_family =
      (shoal_bytes){start_cf, sizeof(start_cf)};
  descriptor_range.start.key.column_qualifier =
      (shoal_bytes){start_cq, sizeof(start_cq)};
  descriptor_range.start.key.column_visibility =
      (shoal_bytes){start_cv, sizeof(start_cv)};
  descriptor_range.start.key.timestamp = 17;
  descriptor_range.end.kind = SHOAL_RANGE_BOUND_KEY;
  descriptor_range.end.key.row = (shoal_bytes){end_row, sizeof(end_row)};
  descriptor_range.end.key.column_family =
      (shoal_bytes){end_cf, sizeof(end_cf)};
  descriptor_range.end.key.column_qualifier =
      (shoal_bytes){end_cq, sizeof(end_cq)};
  descriptor_range.end.key.column_visibility =
      (shoal_bytes){end_cv, sizeof(end_cv)};
  descriptor_range.end.key.timestamp = 23;
  descriptor_range.start_inclusive = 1;
  descriptor_range.end_inclusive = 0;
  expect_error(shoal_range_create(NULL, &range_result, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "range is required");
  expect_error(shoal_range_create(&descriptor_range, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "out_result");
  uint32_t descriptor_range_size = descriptor_range.struct_size;
  descriptor_range.struct_size = SHOAL_RANGE_V1_SIZE - 1;
  expect_error(shoal_range_create(&descriptor_range, &range_result, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "struct_size");
  descriptor_range.struct_size = descriptor_range_size;
  struct {
    shoal_range value;
    uint8_t future[16];
  } future_descriptor_range;
  memset(&future_descriptor_range, 0xa5, sizeof(future_descriptor_range));
  future_descriptor_range.value = descriptor_range;
  future_descriptor_range.value.struct_size =
      (uint32_t)sizeof(future_descriptor_range);
  assert(shoal_range_create(&future_descriptor_range.value, &range_result,
                            &error) == SHOAL_STATUS_OK);
  shoal_range_free(&range_result);
  assert(shoal_range_create(&descriptor_range, &range_result, &error) ==
         SHOAL_STATUS_OK);
  assert(range_result != NULL && error == NULL);
  memset(start_row, 'x', sizeof(start_row));
  memset(start_cf, 'x', sizeof(start_cf));
  memset(start_cq, 'x', sizeof(start_cq));
  memset(start_cv, 'x', sizeof(start_cv));
  memset(end_row, 'x', sizeof(end_row));
  memset(end_cf, 'x', sizeof(end_cf));
  memset(end_cq, 'x', sizeof(end_cq));
  memset(end_cv, 'x', sizeof(end_cv));
  shoal_range_view range_view;
  memset(&range_view, 0, sizeof(range_view));
  expect_error(shoal_range_get(range_result, &range_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "struct_size");
  shoal_range_view_init(&range_view);
  assert(shoal_range_get(range_result, &range_view, &error) ==
         SHOAL_STATUS_OK);
  static const uint8_t expected_start_row[] = {'s', '\0', 'r'};
  static const uint8_t expected_start_cf[] = {'c', 'f'};
  static const uint8_t expected_start_cq[] = {'c', 'q'};
  static const uint8_t expected_start_cv[] = {'v'};
  static const uint8_t expected_end_row[] = {'t', '\0', 'r'};
  static const uint8_t expected_end_cf[] = {'e', 'f'};
  static const uint8_t expected_end_cq[] = {'e', 'q'};
  static const uint8_t expected_end_cv[] = {'e', 'v'};
  assert(range_view.has_start_key == 1 && range_view.has_end_key == 1);
  assert(range_view.start_kind == SHOAL_RANGE_BOUND_KEY &&
         range_view.end_kind == SHOAL_RANGE_BOUND_KEY);
  assert(range_view.start_inclusive == 1 && range_view.end_inclusive == 0);
  assert(range_view.start_key.row.length == sizeof(expected_start_row));
  assert(memcmp(range_view.start_key.row.data, expected_start_row,
                sizeof(expected_start_row)) == 0);
  assert(range_view.start_key.column_family.length ==
         sizeof(expected_start_cf));
  assert(memcmp(range_view.start_key.column_family.data, expected_start_cf,
                sizeof(expected_start_cf)) == 0);
  assert(range_view.start_key.column_qualifier.length ==
         sizeof(expected_start_cq));
  assert(memcmp(range_view.start_key.column_qualifier.data, expected_start_cq,
                sizeof(expected_start_cq)) == 0);
  assert(range_view.start_key.column_visibility.length ==
         sizeof(expected_start_cv));
  assert(memcmp(range_view.start_key.column_visibility.data, expected_start_cv,
                sizeof(expected_start_cv)) == 0);
  assert(range_view.start_key.timestamp == 17);
  assert(range_view.end_key.row.length == sizeof(expected_end_row));
  assert(memcmp(range_view.end_key.row.data, expected_end_row,
                sizeof(expected_end_row)) == 0);
  assert(range_view.end_key.column_family.length == sizeof(expected_end_cf));
  assert(memcmp(range_view.end_key.column_family.data, expected_end_cf,
                sizeof(expected_end_cf)) == 0);
  assert(range_view.end_key.column_qualifier.length == sizeof(expected_end_cq));
  assert(memcmp(range_view.end_key.column_qualifier.data, expected_end_cq,
                sizeof(expected_end_cq)) == 0);
  assert(range_view.end_key.column_visibility.length == sizeof(expected_end_cv));
  assert(memcmp(range_view.end_key.column_visibility.data, expected_end_cv,
                sizeof(expected_end_cv)) == 0);
  assert(range_view.end_key.timestamp == 23);
  struct {
    shoal_range_view view;
    uint8_t future[16];
  } future_range;
  memset(&future_range, 0xa5, sizeof(future_range));
  shoal_range_view_init(&future_range.view);
  future_range.view.struct_size = (uint32_t)sizeof(future_range);
  assert(shoal_range_get(range_result, &future_range.view, &error) ==
         SHOAL_STATUS_OK);
  for (size_t i = 0; i < sizeof(future_range.future); ++i) {
    assert(future_range.future[i] == UINT8_C(0xa5));
  }
  expect_error(shoal_range_get(NULL, &range_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "range result");
  expect_error(shoal_range_get(range_result, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "out_range");
  shoal_range_free(&range_result);
  shoal_range_free(&range_result);
  assert(range_result == NULL);

  shoal_range infinite_range;
  shoal_range_init(&infinite_range);
  infinite_range.start.kind = SHOAL_RANGE_BOUND_UNBOUNDED;
  infinite_range.end.kind = SHOAL_RANGE_BOUND_UNBOUNDED;
  assert(shoal_range_create(&infinite_range, &range_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_range_view_init(&range_view);
  assert(shoal_range_get(range_result, &range_view, &error) ==
         SHOAL_STATUS_OK);
  assert(range_view.start_kind == SHOAL_RANGE_BOUND_UNBOUNDED &&
         range_view.end_kind == SHOAL_RANGE_BOUND_UNBOUNDED);
  assert(range_view.has_start_key == 0 && range_view.has_end_key == 0);
  shoal_range_free(&range_result);

  shoal_range empty_row_range;
  shoal_range_init(&empty_row_range);
  empty_row_range.start.kind = SHOAL_RANGE_BOUND_ROW;
  empty_row_range.end.kind = SHOAL_RANGE_BOUND_ROW;
  empty_row_range.start_inclusive = 1;
  empty_row_range.end_inclusive = 1;
  assert(shoal_range_create(&empty_row_range, &range_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_range_view_init(&range_view);
  assert(shoal_range_get(range_result, &range_view, &error) ==
         SHOAL_STATUS_OK);
  assert(range_view.start_kind == SHOAL_RANGE_BOUND_ROW &&
         range_view.end_kind == SHOAL_RANGE_BOUND_ROW);
  assert(range_view.has_start_key == 1 && range_view.has_end_key == 1);
  assert(range_view.start_key.row.length == 0 &&
         range_view.end_key.row.length == 0);
  shoal_range_free(&range_result);
  descriptor_range.start_inclusive = 2;
  expect_error(shoal_range_create(&descriptor_range, &range_result, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "flags");
  descriptor_range.start_inclusive = 1;
  for (size_t allocation = 0; allocation < 9; ++allocation) {
    shoal_test_result_alloc_fail_after(allocation);
    expect_error(shoal_range_create(&descriptor_range, &range_result, &error),
                 SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate range");
    assert(range_result == NULL);
    shoal_test_result_alloc_reset();
  }

  char iterator_name[] = "age";
  char iterator_class[] = "com.example.Age";
  char option_z_key[] = "zeta";
  char option_z_value[] = "last";
  char option_a_key[] = "alpha";
  char option_a_value[] = "first";
  shoal_iterator_option iterator_options[] = {
      {option_z_key, option_z_value}, {option_a_key, option_a_value}};
  shoal_iterator_setting iterator_setting = {
      iterator_name, iterator_class, 19, iterator_options, 2};
  expect_error(shoal_iterator_setting_create(NULL, &iterator_result, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "iterator setting is required");
  expect_error(
      shoal_iterator_setting_create(&iterator_setting, NULL, &error),
      SHOAL_STATUS_INVALID_ARGUMENT, &error, "out_result");
  assert(shoal_iterator_setting_create(&iterator_setting, &iterator_result,
                                       &error) == SHOAL_STATUS_OK);
  memset(iterator_name, 'x', sizeof(iterator_name) - 1);
  memset(iterator_class, 'x', sizeof(iterator_class) - 1);
  memset(option_a_value, 'x', sizeof(option_a_value) - 1);
  shoal_iterator_setting_view iterator_view;
  memset(&iterator_view, 0, sizeof(iterator_view));
  expect_error(
      shoal_iterator_setting_get(iterator_result, &iterator_view, &error),
      SHOAL_STATUS_INVALID_ARGUMENT, &error, "struct_size");
  shoal_iterator_setting_view_init(&iterator_view);
  assert(shoal_iterator_setting_get(iterator_result, &iterator_view, &error) ==
         SHOAL_STATUS_OK);
  assert(strcmp(iterator_view.name, "age") == 0);
  assert(strcmp(iterator_view.class_name, "com.example.Age") == 0);
  assert(iterator_view.priority == 19 && iterator_view.option_count == 2);
  assert(strcmp(iterator_view.options[0].key, "alpha") == 0);
  assert(strcmp(iterator_view.options[0].value, "first") == 0);
  assert(strcmp(iterator_view.options[1].key, "zeta") == 0);
  assert(strcmp(iterator_view.options[1].value, "last") == 0);
  struct {
    shoal_iterator_setting_view view;
    uint8_t future[16];
  } future_iterator;
  memset(&future_iterator, 0xa5, sizeof(future_iterator));
  shoal_iterator_setting_view_init(&future_iterator.view);
  future_iterator.view.struct_size = (uint32_t)sizeof(future_iterator);
  assert(shoal_iterator_setting_get(iterator_result, &future_iterator.view,
                                    &error) == SHOAL_STATUS_OK);
  for (size_t i = 0; i < sizeof(future_iterator.future); ++i) {
    assert(future_iterator.future[i] == UINT8_C(0xa5));
  }
  expect_error(shoal_iterator_setting_get(NULL, &iterator_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "iterator setting result");
  expect_error(shoal_iterator_setting_get(iterator_result, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "out_setting");
  shoal_iterator_setting_free(&iterator_result);
  shoal_iterator_setting_free(&iterator_result);
  assert(iterator_result == NULL);
  shoal_iterator_setting invalid_iterator = {NULL, "class", 1, NULL, 0};
  expect_error(shoal_iterator_setting_create(&invalid_iterator,
                                             &iterator_result, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "name");
  for (size_t allocation = 0; allocation < 8; ++allocation) {
    shoal_test_result_alloc_fail_after(allocation);
    expect_error(shoal_iterator_setting_create(&iterator_setting,
                                               &iterator_result, &error),
                 SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate iterator");
    assert(iterator_result == NULL);
    shoal_test_result_alloc_reset();
  }
  shoal_test_string_alloc_fail_after(0);
  expect_error(shoal_iterator_setting_create(&iterator_setting,
                                             &iterator_result, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "iterator name");
  shoal_test_string_alloc_reset();

  shoal_table_view table_view;
  assert(shoal_connector_list_tables(admin_connector, 0, &table_list, &error) ==
         SHOAL_STATUS_OK);
  assert(table_list != NULL && error == NULL);
  assert(shoal_table_list_count(table_list) == 2);
  assert(shoal_table_list_get(table_list, 0, &table_view, &error) ==
         SHOAL_STATUS_OK);
  assert(strcmp(table_view.name, "analytics.orders") == 0);
  assert(strcmp(table_view.id, "2") == 0);
  assert(shoal_table_list_get(table_list, 1, &table_view, &error) ==
         SHOAL_STATUS_OK);
  assert(strcmp(table_view.name, "events") == 0);
  assert(strcmp(table_view.id, "1") == 0);
  expect_error(shoal_table_list_get(table_list, 2, &table_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "out of bounds");
  shoal_table_list_free(&table_list);
  assert(table_list == NULL);
  shoal_table_list_free(&table_list);

  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_connector_list_tables(admin_connector, 0, &table_list,
                                           &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error,
               "allocate table list result");
  assert(table_list == NULL);
  shoal_test_result_alloc_reset();

  shoal_test_result_alloc_fail_after(0);
  shoal_test_error_alloc_fail_after(0);
  assert(shoal_connector_list_tables(admin_connector, 0, &table_list, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(table_list == NULL && error == NULL);
  shoal_test_result_alloc_reset();
  shoal_test_error_alloc_reset();
  shoal_test_result_alloc_fail_after(0);
  shoal_test_error_alloc_fail_after(1);
  expect_error(shoal_connector_list_tables(admin_connector, 0, &table_list,
                                           &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error,
               "allocate table list result");
  assert(table_list == NULL);
  shoal_test_result_alloc_reset();
  shoal_test_error_alloc_reset();
  assert(shoal_connector_list_tables(admin_connector, 0, &table_list, &error) ==
         SHOAL_STATUS_OK);
  assert(table_list != NULL && error == NULL);
  shoal_table_list_free(&table_list);
  assert(table_list == NULL);

  shoal_test_string_alloc_fail_after(1);
  expect_error(shoal_connector_list_tables(admin_connector, 0, &table_list,
                                           &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "table 0 id");
  assert(table_list == NULL);
  shoal_test_string_alloc_reset();

  uint8_t exists = 99;
  assert(shoal_connector_table_exists(admin_connector, "events", 0, &exists,
                                      &error) == SHOAL_STATUS_OK);
  assert(exists == 1 && error == NULL);
  exists = 99;
  assert(shoal_connector_table_exists(admin_connector, "missing", 0, &exists,
                                      &error) == SHOAL_STATUS_OK);
  assert(exists == 0 && error == NULL);
  expect_error(shoal_connector_table_exists(admin_connector, "block", 1,
                                            &exists, &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline exceeded");

  assert(shoal_connector_create_table(admin_connector, "created", 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_connector_table_exists(admin_connector, "created", 0, &exists,
                                      &error) == SHOAL_STATUS_OK);
  assert(exists == 1);
  expect_error(shoal_connector_create_table(admin_connector, "events", 0,
                                            &error),
               SHOAL_STATUS_ALREADY_EXISTS, &error, "table exists");
  assert(shoal_connector_rename_table(admin_connector, "created", "renamed", 0,
                                      &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_table_exists(admin_connector, "created", 0, &exists,
                                      &error) == SHOAL_STATUS_OK);
  assert(exists == 0);
  assert(shoal_connector_table_exists(admin_connector, "renamed", 0, &exists,
                                      &error) == SHOAL_STATUS_OK);
  assert(exists == 1);
  expect_error(shoal_connector_delete_table(admin_connector, "missing", 0,
                                            &error),
               SHOAL_STATUS_NOT_FOUND, &error, "table not found");
  assert(shoal_connector_delete_table(admin_connector, "renamed", 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_connector_table_exists(admin_connector, "renamed", 0, &exists,
                                      &error) == SHOAL_STATUS_OK);
  assert(exists == 0);

  expect_error(shoal_connector_flush_table(admin_connector, "events", 2, 0,
                                           &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "wait must be 0 or 1");
  expect_error(shoal_connector_flush_table(admin_connector, "events", 0, -1,
                                           &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "timeout_ms must not be negative");
  shoal_test_error_message_alloc_fail_after(0);
  assert(shoal_connector_flush_table(admin_connector, "events", 2, 0, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(error == NULL);
  shoal_test_error_message_alloc_reset();
  assert(shoal_connector_flush_table(admin_connector, "events", 0, 0, &error) ==
         SHOAL_STATUS_OK);
  assert(error == NULL);
  assert(shoal_connector_flush_table(admin_connector, "events", 1, 0, &error) ==
         SHOAL_STATUS_OK);
  assert(error == NULL);
  assert(shoal_test_connector_flush_wait_count(admin_connector, 0) == 1);
  assert(shoal_test_connector_flush_wait_count(admin_connector, 1) == 1);
  expect_error(shoal_connector_flush_table(admin_connector, "down", 0, 0,
                                           &error),
               SHOAL_STATUS_UNAVAILABLE, &error, "manager unavailable");

  uint8_t range_start_data[] = {'a', 0, 'b'};
  uint8_t range_end_data[] = {'z', 0, 'y'};
  const uint8_t expected_start_data[] = {'a', 0, 'b'};
  const uint8_t expected_end_data[] = {'z', 0, 'y'};
  shoal_bytes range_start = {range_start_data, sizeof(range_start_data)};
  shoal_bytes range_end = {range_end_data, sizeof(range_end_data)};
  shoal_bytes expected_start = {expected_start_data,
                                sizeof(expected_start_data)};
  shoal_bytes expected_end = {expected_end_data, sizeof(expected_end_data)};
  shoal_bytes empty_bound = {NULL, 0};
  expect_error(shoal_connector_flush_table_range(
                   NULL, "events", NULL, NULL, 0, 0, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "connector handle is NULL");
  expect_error(shoal_connector_flush_table_range(
                   admin_connector, NULL, NULL, NULL, 0, 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "table_name is required");
  expect_error(shoal_connector_flush_table_range(
                   admin_connector, "events", &range_start, &range_end, 2, 0,
                   &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "wait must be 0 or 1");
  expect_error(shoal_connector_flush_table_range(
                   admin_connector, "events", &range_start, &range_end, 0, -1,
                   &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "timeout_ms must not be negative");
  shoal_bytes invalid_bound = {NULL, 1};
  expect_error(shoal_connector_flush_table_range(
                   admin_connector, "events", &invalid_bound, NULL, 0, 0,
                   &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "start_row is NULL with non-zero length");
  expect_error(shoal_connector_flush_table_range(
                   admin_connector, "events", &range_end, &range_start, 0, 0,
                   &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "invalid table range");
  assert(shoal_connector_flush_table_range(
             admin_connector, "events", &range_start, &range_end, 1, 0,
             &error) == SHOAL_STATUS_OK);
  range_start_data[0] = 'q';
  range_end_data[0] = 'q';
  assert(shoal_test_connector_last_flush_range_matches(
      admin_connector, &expected_start, &expected_end, 1));
  assert(shoal_connector_flush_table_range(
             admin_connector, "events", NULL, &empty_bound, 0, 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_test_connector_last_flush_range_matches(
      admin_connector, NULL, &empty_bound, 0));
  assert(shoal_test_connector_table_maintenance_block(admin_connector, 1));
  expect_error(shoal_connector_flush_table_range(
                   admin_connector, "events", NULL, NULL, 0, 1, &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline exceeded");
  assert(shoal_test_connector_table_maintenance_block(admin_connector, 0));

  int32_t constraint_number = 0;
  expect_error(shoal_connector_add_table_constraint(
                   admin_connector, "events", NULL, 0, &constraint_number,
                   &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "class_name is required");
  expect_error(shoal_connector_add_table_constraint(
                   admin_connector, "events", "org.example.New", -1,
                   &constraint_number, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "timeout_ms must not be negative");
  assert(shoal_connector_add_table_constraint(
             admin_connector, "events", "org.example.New", 0,
             &constraint_number, &error) == SHOAL_STATUS_OK);
  assert(constraint_number == 2);
  assert(shoal_connector_add_table_constraint(
             admin_connector, "events", "org.example.New", 0,
             &constraint_number, &error) == SHOAL_STATUS_OK);
  assert(constraint_number == 2);

  shoal_table_constraint_list_result *constraints = NULL;
  shoal_table_constraint_view constraint_view;
  shoal_table_constraint_view_init(&constraint_view);
  assert(constraint_view.struct_size == SHOAL_TABLE_CONSTRAINT_VIEW_V1_SIZE);
  assert(shoal_connector_list_table_constraints(
             admin_connector, "events", 0, &constraints, &error) ==
         SHOAL_STATUS_OK);
  assert(constraints != NULL && error == NULL);
  assert(shoal_table_constraint_list_count(constraints) == 3);
  assert(shoal_table_constraint_list_get(
             constraints, 0, &constraint_view, &error) == SHOAL_STATUS_OK);
  assert(constraint_view.number == 1);
  assert(strcmp(constraint_view.class_name, "org.example.First") == 0);
  const char *first_constraint_class = constraint_view.class_name;
  assert(shoal_table_constraint_list_get(
             constraints, 1, &constraint_view, &error) == SHOAL_STATUS_OK);
  assert(constraint_view.number == 2);
  assert(strcmp(constraint_view.class_name, "org.example.New") == 0);
  assert(strcmp(first_constraint_class, "org.example.First") == 0);
  constraint_view.struct_size = 0;
  expect_error(shoal_table_constraint_list_get(
                   constraints, 0, &constraint_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "constraint view is missing or too small");
  shoal_table_constraint_view_init(&constraint_view);
  expect_error(shoal_table_constraint_list_get(
                   constraints, 3, &constraint_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "invalid constraint list access");
  shoal_table_constraint_list_free(&constraints);
  assert(constraints == NULL);
  shoal_table_constraint_list_free(&constraints);

  shoal_test_result_alloc_fail_after(0);
  assert(shoal_connector_list_table_constraints(
             admin_connector, "events", 0, &constraints, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(constraints == NULL && error != NULL);
  assert(shoal_error_code(error) == SHOAL_STATUS_OUT_OF_MEMORY);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();
  shoal_test_result_alloc_fail_after(2);
  assert(shoal_connector_list_table_constraints(
             admin_connector, "events", 0, &constraints, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(constraints == NULL && error != NULL);
  assert(shoal_error_code(error) == SHOAL_STATUS_OUT_OF_MEMORY);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();
  assert(shoal_connector_remove_table_constraint(
             admin_connector, "events", 2, 0, &error) == SHOAL_STATUS_OK);
  expect_error(shoal_connector_remove_table_constraint(
                   admin_connector, "events", 0, 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "constraint number must be positive");
  assert(shoal_connector_list_table_constraints(
             admin_connector, "events", 0, &constraints, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_table_constraint_list_count(constraints) == 2);
  shoal_table_constraint_list_free(&constraints);

  assert(shoal_connector_set_table_property(
             admin_connector, "events", "table.custom.alpha", "alpha", 0,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_set_table_property(
             admin_connector, "events", "table.custom.empty", "", 0,
             &error) == SHOAL_STATUS_OK);
  expect_error(shoal_connector_set_table_property(
                   admin_connector, "events", "invalid", "x", 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "invalid property");
  assert(shoal_connector_remove_table_property(
             admin_connector, "events", "table.custom.alpha", 0,
             &error) == SHOAL_STATUS_OK);

  shoal_table_property_view property_view;
  assert(shoal_connector_effective_table_properties(admin_connector, "events",
                                                    0, &properties,
                                                    &error) ==
         SHOAL_STATUS_OK);
  assert(properties != NULL && error == NULL);
  assert(shoal_table_properties_count(properties) == 2);
  assert(shoal_table_properties_get(properties, 0, &property_view, &error) ==
         SHOAL_STATUS_OK);
  const char *table_property_key = property_view.key;
  const char *table_property_value = property_view.value;
  assert(table_property_key != NULL);
  assert(table_property_value != NULL);
  assert(strcmp(property_view.key, "table.custom.empty") == 0);
  assert(strcmp(property_view.value, "") == 0);
  assert(shoal_table_properties_get(properties, 1, &property_view, &error) ==
         SHOAL_STATUS_OK);
  assert(strcmp(table_property_key, "table.custom.empty") == 0);
  assert(strcmp(table_property_value, "") == 0);
  assert(strcmp(property_view.key, "table.custom.mode") == 0);
  assert(strcmp(property_view.value, "stream") == 0);
  expect_error(shoal_table_properties_get(properties, 2, &property_view,
                                          &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "out of bounds");
  assert(strcmp(table_property_key, "table.custom.empty") == 0);
  assert(strcmp(table_property_value, "") == 0);
  shoal_table_properties_free(&properties);
  assert(properties == NULL);
  shoal_table_properties_free(&properties);
  shoal_test_result_alloc_fail_after(0);
  shoal_test_error_alloc_fail_after(1);
  shoal_test_error_message_alloc_fail_after(0);
  assert(shoal_connector_effective_table_properties(
             admin_connector, "events", 0, &properties, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(properties == NULL && error == NULL);
  shoal_test_result_alloc_reset();
  shoal_test_error_alloc_reset();
  shoal_test_error_message_alloc_reset();
  assert(shoal_connector_effective_table_properties(
             admin_connector, "events", 0, &properties, &error) ==
         SHOAL_STATUS_OK);
  assert(properties != NULL && error == NULL);
  shoal_table_properties_free(&properties);
  assert(properties == NULL);
  shoal_test_string_alloc_fail_after(3);
  expect_error(shoal_connector_effective_table_properties(
                   admin_connector, "events", 0, &properties, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "property 1 value");
  assert(properties == NULL);
  shoal_test_string_alloc_reset();
  expect_error(shoal_connector_effective_table_properties(
                   admin_connector, "down", 0, &properties, &error),
               SHOAL_STATUS_UNAVAILABLE, &error,
               "client service unavailable");
  expect_error(shoal_connector_effective_table_properties(
                   admin_connector, "denied", 0, &properties, &error),
               SHOAL_STATUS_PERMISSION_DENIED, &error, "permission denied");

  assert(shoal_connector_list_namespaces(admin_connector, 0, &namespace_list,
                                         &error) == SHOAL_STATUS_OK);
  assert(shoal_namespace_list_count(namespace_list) == 2);
  shoal_namespace_view namespace_view = {0};
  assert(shoal_namespace_list_get(namespace_list, 0, &namespace_view, &error) ==
         SHOAL_STATUS_OK);
  assert(strcmp(namespace_view.name, "") == 0);
  assert(strcmp(namespace_view.id, "+default") == 0);
  shoal_namespace_list_free(&namespace_list);
  assert(namespace_list == NULL);
  shoal_namespace_list_free(&namespace_list);
  shoal_test_result_alloc_fail_after(0);
  expect_error(shoal_connector_list_namespaces(
                   admin_connector, 0, &namespace_list, &error),
               SHOAL_STATUS_OUT_OF_MEMORY, &error, "allocate");
  shoal_test_result_alloc_reset();

  exists = 0;
  assert(shoal_connector_namespace_exists(admin_connector, "", 0, &exists,
                                          &error) == SHOAL_STATUS_OK);
  assert(exists == 1);
  assert(shoal_connector_namespace_exists(admin_connector, "analytics", 0,
                                          &exists, &error) == SHOAL_STATUS_OK);
  assert(exists == 1);
  expect_error(shoal_connector_namespace_exists(admin_connector, NULL, 0,
                                                &exists, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "namespace_name");
  expect_error(shoal_connector_delete_namespace(admin_connector, NULL, 0,
                                                &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "namespace_name");
  expect_error(shoal_connector_delete_namespace(admin_connector, "", 0,
                                                &error),
               SHOAL_STATUS_NAMESPACE_NOT_EMPTY, &error,
               "namespace not empty");
  expect_error(shoal_connector_delete_namespace(admin_connector, "analytics",
                                                0, &error),
               SHOAL_STATUS_NAMESPACE_NOT_EMPTY, &error,
               "namespace not empty");
  expect_error(shoal_connector_rename_namespace(admin_connector, NULL, "work",
                                                0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "namespace_name");
  expect_error(shoal_connector_rename_namespace(admin_connector, "analytics",
                                                "", 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "new_namespace_name");
  expect_error(shoal_connector_rename_namespace(admin_connector, "", "analytics",
                                                0, &error),
               SHOAL_STATUS_ALREADY_EXISTS, &error, "namespace exists");
  assert(shoal_connector_create_namespace(admin_connector, "scratch", 0,
                                          &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_rename_namespace(admin_connector, "scratch", "work",
                                          0, &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_set_namespace_property(
             admin_connector, "", "table.custom.default", "enabled", 0,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_namespace_properties(
             admin_connector, "", 0, &namespace_properties, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_namespace_properties_count(namespace_properties) == 1);
  assert(shoal_namespace_properties_get(namespace_properties, 0, &property_view,
                                        &error) == SHOAL_STATUS_OK);
  assert(strcmp(property_view.key, "table.custom.default") == 0);
  assert(strcmp(property_view.value, "enabled") == 0);
  shoal_namespace_properties_free(&namespace_properties);
  assert(shoal_connector_remove_namespace_property(
             admin_connector, "", "table.custom.default", 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_connector_set_namespace_property(
             admin_connector, "work", "table.custom.mode", "", 0, &error) ==
         SHOAL_STATUS_OK);
  expect_error(shoal_connector_set_namespace_property(
                   admin_connector, NULL, "table.custom.mode", "", 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "namespace_name");
  expect_error(shoal_connector_remove_namespace_property(
                   admin_connector, NULL, "table.custom.mode", 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "namespace_name");
  assert(shoal_connector_namespace_properties(
             admin_connector, "work", 0, &namespace_properties, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_namespace_properties_count(namespace_properties) == 1);
  assert(shoal_namespace_properties_get(namespace_properties, 0, &property_view,
                                        &error) == SHOAL_STATUS_OK);
  assert(strcmp(property_view.key, "table.custom.mode") == 0);
  assert(strcmp(property_view.value, "") == 0);
  shoal_namespace_properties_free(&namespace_properties);
  shoal_namespace_properties_free(&namespace_properties);
  assert(shoal_connector_effective_namespace_properties(
             admin_connector, "work", 0, &namespace_properties, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_namespace_properties_count(namespace_properties) == 1);
  shoal_namespace_properties_free(&namespace_properties);
  expect_error(shoal_connector_effective_namespace_properties(
                   admin_connector, NULL, 0, &namespace_properties, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "namespace_name");
  expect_error(shoal_connector_namespace_properties(
                   admin_connector, NULL, 0, &namespace_properties, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "namespace_name");
  assert(shoal_connector_remove_namespace_property(
             admin_connector, "work", "table.custom.mode", 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_connector_namespace_properties(
             admin_connector, "work", 0, &namespace_properties, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_namespace_properties_count(namespace_properties) == 0);
  shoal_namespace_properties_free(&namespace_properties);
  assert(shoal_connector_versioned_namespace_properties(
             admin_connector, "analytics", 0, &versioned_properties, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_versioned_properties_version(versioned_properties) == 7);
  assert(shoal_versioned_properties_count(versioned_properties) == 1);
  assert(shoal_versioned_properties_get(versioned_properties, 0,
                                        &property_view, &error) ==
         SHOAL_STATUS_OK);
  const char *versioned_property_key = property_view.key;
  const char *versioned_property_value = property_view.value;
  assert(versioned_property_key != NULL);
  assert(versioned_property_value != NULL);
  assert(strcmp(property_view.key, "table.custom.namespace") == 0);
  assert(strcmp(property_view.value, "enabled") == 0);
  expect_error(shoal_versioned_properties_get(versioned_properties, 1,
                                              &property_view, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error,
               "invalid versioned property result access");
  assert(strcmp(versioned_property_key, "table.custom.namespace") == 0);
  assert(strcmp(versioned_property_value, "enabled") == 0);
  shoal_versioned_properties_free(&versioned_properties);
  assert(versioned_properties == NULL);
  shoal_versioned_properties_free(&versioned_properties);
  expect_error(shoal_connector_versioned_namespace_properties(
                   admin_connector, NULL, 0, &versioned_properties, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "namespace_name");
  assert(shoal_connector_delete_namespace(admin_connector, "work", 0,
                                          &error) == SHOAL_STATUS_OK);
  expect_error(shoal_connector_create_namespace(admin_connector, "block", 1,
                                                &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline");

  shoal_bytes empty_password = {NULL, 0};
  assert(shoal_connector_create_user(admin_connector, "alice", &empty_password,
                                     0, &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_change_user_authorizations(
             admin_connector, "alice", NULL, 0, 0, &error) == SHOAL_STATUS_OK);
  assert(error == NULL);
  assert(shoal_connector_get_user_authorizations(
             admin_connector, "alice", 0, &bytes_list, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_bytes_list_count(bytes_list) == 0);
  shoal_bytes_list_free(&bytes_list);
  shoal_bytes_list_free(&bytes_list);
  expect_error(shoal_connector_change_user_authorizations(
                   admin_connector, "alice", NULL, 1, 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "authorizations");
  const uint8_t auth_a[] = {'A', 0, 'B'};
  shoal_bytes auths[] = {{auth_a, sizeof(auth_a)}};
  expect_error(shoal_connector_change_user_authorizations(
                   admin_connector, "alice", auths, SIZE_MAX / 2, 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "count is too large");
  assert(shoal_connector_change_user_authorizations(
             admin_connector, "alice", auths, 1, 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_connector_get_user_authorizations(
             admin_connector, "alice", 0, &bytes_list, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_bytes_list_count(bytes_list) == 1);
  shoal_bytes bytes_view = {0};
  assert(shoal_bytes_list_get(bytes_list, 0, &bytes_view, &error) ==
         SHOAL_STATUS_OK);
  assert(bytes_view.length == sizeof(auth_a));
  assert(memcmp(bytes_view.data, auth_a, sizeof(auth_a)) == 0);
  shoal_bytes_list_free(&bytes_list);
  shoal_bytes_list_free(&bytes_list);
  assert(shoal_connector_grant_table_permission(
             admin_connector, "alice", "events", SHOAL_TABLE_PERMISSION_READ,
             0, &error) == SHOAL_STATUS_OK);
  uint8_t has_permission = 0;
  expect_error(shoal_connector_has_table_permission(
                   admin_connector, "alice", NULL, SHOAL_TABLE_PERMISSION_READ,
                   0, &has_permission, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "target_name");
  expect_error(shoal_connector_grant_table_permission(
                   admin_connector, "alice", NULL, SHOAL_TABLE_PERMISSION_READ,
                   0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "target_name");
  expect_error(shoal_connector_revoke_table_permission(
                   admin_connector, "alice", NULL, SHOAL_TABLE_PERMISSION_READ,
                   0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "target_name");
  expect_error(shoal_connector_has_namespace_permission(
                   admin_connector, "alice", NULL,
                   SHOAL_NAMESPACE_PERMISSION_READ, 0, &has_permission, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "target_name");
  expect_error(shoal_connector_grant_namespace_permission(
                   admin_connector, "alice", NULL,
                   SHOAL_NAMESPACE_PERMISSION_READ, 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "target_name");
  expect_error(shoal_connector_revoke_namespace_permission(
                   admin_connector, "alice", NULL,
                   SHOAL_NAMESPACE_PERMISSION_READ, 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "target_name");
  assert(shoal_connector_has_table_permission(
             admin_connector, "alice", "events", SHOAL_TABLE_PERMISSION_READ,
             0, &has_permission, &error) == SHOAL_STATUS_OK);
  assert(has_permission == 1);
  assert(shoal_connector_revoke_table_permission(
             admin_connector, "alice", "events", SHOAL_TABLE_PERMISSION_READ,
             0, &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_has_table_permission(
             admin_connector, "alice", "events", SHOAL_TABLE_PERMISSION_READ,
             0, &has_permission, &error) == SHOAL_STATUS_OK);
  assert(has_permission == 0);
  assert(shoal_connector_grant_system_permission(
             admin_connector, "alice", SHOAL_SYSTEM_PERMISSION_CREATE_TABLE, 0,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_has_system_permission(
             admin_connector, "alice", SHOAL_SYSTEM_PERMISSION_CREATE_TABLE, 0,
             &has_permission, &error) == SHOAL_STATUS_OK);
  assert(has_permission == 1);
  assert(shoal_connector_revoke_system_permission(
             admin_connector, "alice", SHOAL_SYSTEM_PERMISSION_CREATE_TABLE, 0,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_has_system_permission(
             admin_connector, "alice", SHOAL_SYSTEM_PERMISSION_CREATE_TABLE, 0,
             &has_permission, &error) == SHOAL_STATUS_OK);
  assert(has_permission == 0);
  assert(shoal_connector_grant_namespace_permission(
             admin_connector, "alice", "analytics",
             SHOAL_NAMESPACE_PERMISSION_READ, 0, &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_has_namespace_permission(
             admin_connector, "alice", "analytics",
             SHOAL_NAMESPACE_PERMISSION_READ, 0, &has_permission, &error) ==
         SHOAL_STATUS_OK);
  assert(has_permission == 1);
  assert(shoal_connector_revoke_namespace_permission(
             admin_connector, "alice", "analytics",
             SHOAL_NAMESPACE_PERMISSION_READ, 0, &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_has_namespace_permission(
             admin_connector, "alice", "analytics",
             SHOAL_NAMESPACE_PERMISSION_READ, 0, &has_permission, &error) ==
         SHOAL_STATUS_OK);
  assert(has_permission == 0);
  assert(shoal_connector_grant_namespace_permission(
             admin_connector, "alice", "", SHOAL_NAMESPACE_PERMISSION_READ, 0,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_has_namespace_permission(
             admin_connector, "alice", "", SHOAL_NAMESPACE_PERMISSION_READ, 0,
             &has_permission, &error) == SHOAL_STATUS_OK);
  assert(has_permission == 1);
  assert(shoal_connector_revoke_namespace_permission(
             admin_connector, "alice", "", SHOAL_NAMESPACE_PERMISSION_READ, 0,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_change_password(admin_connector, "missing",
                                         &empty_password, 0, &error) ==
         SHOAL_STATUS_USER_NOT_FOUND);
  assert(error != NULL);
  assert(strstr(shoal_error_message(error), "security error") != NULL);
  assert(strcmp(shoal_error_security_user(error), "missing") == 0);
  assert(strcmp(shoal_error_security_code(error), "USER_DOESNT_EXIST") == 0);
  shoal_error_free(&error);
  shoal_error_free(&error);
  assert(shoal_connector_grant_table_permission(
             admin_connector, "alice", "events", SHOAL_TABLE_PERMISSION_READ,
             0, &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_drop_user(admin_connector, "alice", 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_connector_create_user(admin_connector, "alice", &empty_password,
                                     0, &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_has_table_permission(
             admin_connector, "alice", "events", SHOAL_TABLE_PERMISSION_READ,
             0, &has_permission, &error) == SHOAL_STATUS_OK);
  assert(has_permission == 0);
  assert(shoal_connector_drop_user(admin_connector, "alice", 0, &error) ==
         SHOAL_STATUS_OK);

  assert(shoal_connector_list_table_splits(admin_connector, "events", 0,
                                           &bytes_list, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_bytes_list_count(bytes_list) == 1);
  shoal_bytes_list_free(&bytes_list);
  const uint8_t split_one[] = {0, 'x'};
  const uint8_t split_two[] = {'z'};
  shoal_bytes splits[] = {{split_one, sizeof(split_one)},
                          {split_two, sizeof(split_two)}};
  assert(shoal_connector_add_table_splits(admin_connector, "events", splits, 2,
                                          0, &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_list_table_splits(admin_connector, "events", 0,
                                           &bytes_list, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_bytes_list_count(bytes_list) == 3);
  shoal_bytes_list_free(&bytes_list);
  assert(shoal_connector_create_table(admin_connector, "split-source", 0,
                                      &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_add_table_splits(admin_connector, "split-source",
                                          splits, 2, 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_connector_rename_table(admin_connector, "split-source",
                                      "split-target", 0, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_connector_list_table_splits(admin_connector, "split-target", 0,
                                           &bytes_list, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_bytes_list_count(bytes_list) == 2);
  shoal_bytes_list_free(&bytes_list);
  assert(shoal_connector_delete_table(admin_connector, "split-target", 0,
                                      &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_create_table(admin_connector, "split-target", 0,
                                      &error) == SHOAL_STATUS_OK);
  assert(shoal_connector_list_table_splits(admin_connector, "split-target", 0,
                                           &bytes_list, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_bytes_list_count(bytes_list) == 0);
  shoal_bytes_list_free(&bytes_list);
  assert(shoal_connector_delete_table(admin_connector, "split-target", 0,
                                      &error) == SHOAL_STATUS_OK);
  expect_error(shoal_connector_add_table_splits(admin_connector, "events",
                                                NULL, 0, 0, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "table split");

  shoal_scanner_config scanner_config;
  shoal_scanner_config_init(&scanner_config);
  assert(scanner_config.struct_size == SHOAL_SCANNER_CONFIG_V1_SIZE);
  scanner_config.table_name = "events";
  uint32_t scanner_config_size = scanner_config.struct_size;
  scanner_config.struct_size = SHOAL_SCANNER_CONFIG_V1_SIZE - 1;
  expect_error(
      shoal_connector_create_scanner(connector, &scanner_config, &scanner,
                                     &error),
      SHOAL_STATUS_INVALID_ARGUMENT, &error, "struct_size");
  scanner_config.struct_size = scanner_config_size;
  struct {
    shoal_scanner_config value;
    uint8_t future[16];
  } future_scanner_config;
  memset(&future_scanner_config, 0xa5, sizeof(future_scanner_config));
  future_scanner_config.value = scanner_config;
  future_scanner_config.value.struct_size =
      (uint32_t)sizeof(future_scanner_config);
  expect_error(
      shoal_connector_create_scanner(connector, &future_scanner_config.value,
                                     &scanner, &error),
      SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error, "discovery unavailable");
  for (size_t i = 0; i < sizeof(future_scanner_config.future); ++i) {
    assert(future_scanner_config.future[i] == UINT8_C(0xa5));
  }

  expect_error(
      shoal_connector_create_scanner(connector, &scanner_config, &scanner,
                                     &error),
      SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error, "discovery unavailable");
  assert(scanner == NULL);
  expect_error(shoal_connector_create_batch_scanner(
                   connector, &scanner_config, &batch_scanner, &error),
               SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error,
               "discovery unavailable");
  assert(batch_scanner == NULL);

  scanner_config.table_id = "1";
  expect_error(
      shoal_connector_create_scanner(connector, &scanner_config, &scanner,
                                     &error),
      SHOAL_STATUS_INVALID_ARGUMENT, &error, "exactly one");
  scanner_config.table_id = NULL;

  scanner_config.authorization_count = 1;
  expect_error(
      shoal_connector_create_scanner(connector, &scanner_config, &scanner,
                                     &error),
      SHOAL_STATUS_INVALID_ARGUMENT, &error, "authorizations");
  scanner_config.authorization_count = 0;

  scanner_config.use_multi_scan = 2;
  expect_error(
      shoal_connector_create_scanner(connector, &scanner_config, &scanner,
                                     &error),
      SHOAL_STATUS_INVALID_ARGUMENT, &error, "use_multi_scan");
  scanner_config.use_multi_scan = 0;

  shoal_range range;
  shoal_range_init(&range);
  assert(range.struct_size == SHOAL_RANGE_V1_SIZE);

  assert(shoal_scan_result_count(NULL) == 0);
  expect_error(shoal_scan_result_get(NULL, 0, NULL, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "result");
  shoal_scan_result_free(&result);
  assert(result == NULL);
  shoal_scan_result_free(&result);

  expect_error(shoal_scanner_close(NULL, &error), SHOAL_STATUS_INVALID_HANDLE,
               &error, "NULL");
  expect_error(shoal_batch_scanner_close(NULL, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "NULL");
  shoal_scanner_free(&scanner);
  shoal_batch_scanner_free(&batch_scanner);

  shoal_batch_writer_config writer_config;
  shoal_batch_writer_config_init(&writer_config);
  assert(writer_config.struct_size == SHOAL_BATCH_WRITER_CONFIG_V1_SIZE);
  writer_config.table_name = "events";
  uint32_t writer_config_size = writer_config.struct_size;
  writer_config.struct_size = SHOAL_BATCH_WRITER_CONFIG_V1_SIZE - 1;
  expect_error(shoal_connector_create_batch_writer(
                   connector, &writer_config, &writer, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "struct_size");
  writer_config.struct_size = writer_config_size;
  struct {
    shoal_batch_writer_config value;
    uint8_t future[16];
  } future_writer_config;
  memset(&future_writer_config, 0xa5, sizeof(future_writer_config));
  future_writer_config.value = writer_config;
  future_writer_config.value.struct_size =
      (uint32_t)sizeof(future_writer_config);
  expect_error(shoal_connector_create_batch_writer(
                   connector, &future_writer_config.value, &writer, &error),
               SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error,
               "discovery unavailable");
  for (size_t i = 0; i < sizeof(future_writer_config.future); ++i) {
    assert(future_writer_config.future[i] == UINT8_C(0xa5));
  }
  expect_error(shoal_connector_create_batch_writer(
                   connector, &writer_config, &writer, &error),
               SHOAL_STATUS_DISCOVERY_UNAVAILABLE, &error,
               "discovery unavailable");
  assert(writer == NULL);

  writer_config.durability = 99;
  expect_error(shoal_connector_create_batch_writer(
                   connector, &writer_config, &writer, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "durability");
  writer_config.durability = SHOAL_DURABILITY_DEFAULT;
  writer_config.max_latency_ms = -1;
  expect_error(shoal_connector_create_batch_writer(
                   connector, &writer_config, &writer, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "max_latency_ms");
  writer_config.max_latency_ms = 0;

  static const uint8_t mutation_row[] = {'r', '\0', 'w'};
  static const uint8_t family[] = {'c', 'f'};
  static const uint8_t qualifier[] = {'c', 'q'};
  static const uint8_t visibility[] = {'A', '&', 'B'};
  static const uint8_t mutation_value[] = {'v', '\0', 'l'};
  shoal_bytes row_bytes = {mutation_row, sizeof(mutation_row)};
  shoal_bytes family_bytes = {family, sizeof(family)};
  shoal_bytes qualifier_bytes = {qualifier, sizeof(qualifier)};
  shoal_bytes visibility_bytes = {visibility, sizeof(visibility)};
  shoal_bytes value_bytes = {mutation_value, sizeof(mutation_value)};
  size_t mutation_size = SIZE_MAX;

  assert(shoal_mutation_create(row_bytes, &mutation, &error) ==
         SHOAL_STATUS_OK);
  assert(mutation != NULL && error == NULL);
  assert(shoal_mutation_size(mutation, &mutation_size, &error) ==
         SHOAL_STATUS_OK);
  assert(mutation_size == 0);
  assert(shoal_mutation_put(mutation, family_bytes, qualifier_bytes,
                            visibility_bytes, 42, value_bytes,
                            &error) == SHOAL_STATUS_OK);
  assert(shoal_mutation_delete_latest(mutation, family_bytes, qualifier_bytes,
                                      visibility_bytes,
                                      &error) == SHOAL_STATUS_OK);
  assert(shoal_mutation_delete(mutation, family_bytes, qualifier_bytes,
                               visibility_bytes, 43,
                               &error) == SHOAL_STATUS_OK);
  assert(shoal_mutation_size(mutation, &mutation_size, &error) ==
         SHOAL_STATUS_OK);
  assert(mutation_size == 3);

  shoal_bytes malformed = {NULL, 1};
  expect_error(shoal_mutation_put_latest(
                   mutation, family_bytes, qualifier_bytes, visibility_bytes,
                   malformed, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "value");

  assert(shoal_test_batch_writer_create(SHOAL_TEST_WRITER_SUCCESS, &writer));
  assert(writer != NULL);
  shoal_mutation *empty_mutation = NULL;
  static const uint8_t empty_row[] = {'e'};
  shoal_bytes empty_row_bytes = {empty_row, sizeof(empty_row)};
  assert(shoal_mutation_create(empty_row_bytes, &empty_mutation, &error) ==
         SHOAL_STATUS_OK);
  expect_error(shoal_batch_writer_add(writer, empty_mutation, 0,
                                      &write_failure, &error),
               SHOAL_STATUS_OPERATION_FAILED, &error,
               "at least one update");
  assert(write_failure == NULL);
  shoal_mutation_free(&empty_mutation);
  assert(shoal_batch_writer_add(writer, mutation, 0, &write_failure, &error) ==
         SHOAL_STATUS_OK);
  assert(write_failure == NULL && error == NULL);
  size_t writer_size = SIZE_MAX;
  assert(shoal_batch_writer_size(writer, 0, &writer_size, &error) ==
         SHOAL_STATUS_OK);
  assert(writer_size == 1 && error == NULL);
  assert(shoal_batch_writer_flush(writer, 0, &write_failure, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_batch_writer_size(writer, 0, &writer_size, &error) ==
         SHOAL_STATUS_OK);
  assert(writer_size == 0 && error == NULL);
  assert(shoal_batch_writer_close(writer, 0, &write_failure, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_batch_writer_close(writer, 0, &write_failure, &error) ==
         SHOAL_STATUS_OK);
  shoal_batch_writer_free(&writer);
  assert(writer == NULL);

  size_t log_count = 0;
  assert(shoal_logging_set_callback(capture_log, &log_count, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_logging_set_level(SHOAL_LOG_LEVEL_DEBUG, &error) ==
         SHOAL_STATUS_OK);
  assert(log_count == 1);
  assert(shoal_logging_get_level() == SHOAL_LOG_LEVEL_DEBUG);
  expect_error(shoal_logging_set_level(99, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "log level");
  assert(shoal_logging_set_level(SHOAL_LOG_LEVEL_OFF, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_logging_set_callback(NULL, NULL, &error) == SHOAL_STATUS_OK);
  expect_error(shoal_hdfs_client_mkdir(NULL, "/warehouse", 0, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "handle");
  expect_error(shoal_hdfs_client_chown(NULL, "/warehouse", "alice",
                                       "analytics", 0, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "handle");

  assert(shoal_test_batch_writer_create(
      SHOAL_TEST_WRITER_STRUCTURED_FAILURE, &writer));
  expect_error(shoal_batch_writer_flush(writer, 0, &write_failure, &error),
               SHOAL_STATUS_AMBIGUOUS_WRITE, &error, "batch writer failed");
  assert(write_failure != NULL);
  assert(shoal_write_failure_get_flags(write_failure) ==
         (SHOAL_WRITE_FAILURE_AMBIGUOUS_COMMIT |
          SHOAL_WRITE_FAILURE_RETRY_EXHAUSTED |
          SHOAL_WRITE_FAILURE_AUTOMATIC_FLUSH));
  assert(shoal_write_failure_failed_extent_count(write_failure) == 1);
  assert(shoal_write_failure_constraint_count(write_failure) == 1);
  assert(shoal_write_failure_authorization_count(write_failure) == 1);
  assert(shoal_write_failure_cleanup_count(write_failure) == 1);
  shoal_failed_extent_view structured_extent;
  assert(shoal_write_failure_get_failed_extent(
             write_failure, 0, &structured_extent, &error) == SHOAL_STATUS_OK);
  assert(strcmp(structured_extent.server, "server:9997") == 0);
  assert(structured_extent.submitted == 3);
  assert(structured_extent.committed == 2);
  shoal_write_failure_free(&write_failure);
  shoal_batch_writer_free(&writer);

  {
    static const struct {
      size_t fail_after;
      const char *message_part;
    } write_failure_alloc_cases[] = {
        {0, "failed extent 0 server"},
        {1, "failed extent 0 table id"},
        {2, "constraint 0 server"},
        {3, "constraint 0 class"},
        {4, "constraint 0 description"},
        {5, "authorization 0 server"},
        {6, "authorization 0 table id"},
        {7, "authorization 0 code"},
        {8, "cleanup 0 server"},
        {9, "cleanup 0 message"},
    };
    for (size_t i = 0; i < sizeof(write_failure_alloc_cases) /
                               sizeof(write_failure_alloc_cases[0]);
         ++i) {
      shoal_test_string_alloc_fail_after(write_failure_alloc_cases[i].fail_after);
      assert(shoal_test_batch_writer_create(
          SHOAL_TEST_WRITER_STRUCTURED_FAILURE, &writer));
      expect_error(shoal_batch_writer_flush(writer, 0, &write_failure, &error),
                   SHOAL_STATUS_OUT_OF_MEMORY, &error,
                   write_failure_alloc_cases[i].message_part);
      assert(write_failure == NULL);
      shoal_batch_writer_free(&writer);
      assert(writer == NULL);
      shoal_test_string_alloc_reset();
    }
  }

  assert(shoal_test_batch_writer_create(SHOAL_TEST_WRITER_STICKY_DEADLINE,
                                        &writer));
  expect_error(shoal_batch_writer_close(writer, 1, &write_failure, &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline exceeded");
  expect_error(shoal_batch_writer_close(writer, 1000, &write_failure, &error),
               SHOAL_STATUS_DEADLINE_EXCEEDED, &error, "deadline exceeded");
  shoal_batch_writer_free(&writer);

  assert(shoal_test_batch_writer_create(SHOAL_TEST_WRITER_CONNECTOR_CLOSED,
                                        &writer));
  expect_error(shoal_batch_writer_add(writer, mutation, 0, &write_failure,
                                      &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  assert(write_failure == NULL);
  shoal_batch_writer_free(&writer);

  shoal_mutation_free(&mutation);
  assert(mutation == NULL);
  shoal_mutation_free(&mutation);

  expect_error(shoal_batch_writer_close(NULL, 0, &write_failure, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "NULL");
  assert(write_failure == NULL);
  assert(shoal_write_failure_get_flags(NULL) == 0);
  assert(shoal_write_failure_failed_extent_count(NULL) == 0);
  shoal_failed_extent_view failed_extent;
  expect_error(shoal_write_failure_get_failed_extent(
                   NULL, 0, &failed_extent, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "write failure");
  shoal_batch_writer_free(&writer);
  shoal_write_failure_free(&write_failure);

  assert(shoal_connector_close(connector, &error) == SHOAL_STATUS_OK);
  assert(error == NULL);
  assert(shoal_connector_close(connector, &error) == SHOAL_STATUS_OK);
  assert(error == NULL);

  assert(shoal_connector_close(admin_connector, &error) == SHOAL_STATUS_OK);
  expect_error(shoal_connector_list_tables(admin_connector, 0, &table_list,
                                           &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_connector_list_namespaces(
                   admin_connector, 0, &namespace_list, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_connector_get_user_authorizations(
                   admin_connector, "root", 0, &bytes_list, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_connector_list_table_splits(
                   admin_connector, "events", 0, &bytes_list, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_connector_get_identity(admin_connector, 0, &identity,
                                            &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_connector_flush_table_range(
                   admin_connector, "events", NULL, NULL, 0, 0, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_connector_list_table_constraints(
                   admin_connector, "events", 0, &constraints, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_connector_invalidate_table(admin_connector, "5", &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");
  expect_error(shoal_connector_invalidate_discovery(admin_connector, &error),
               SHOAL_STATUS_CLOSED, &error, "connector is closed");

  shoal_connector_free(&connector);
  assert(connector == NULL);
  shoal_connector_free(&connector);
  shoal_connector_free(&admin_connector);
  assert(admin_connector == NULL);
  shoal_connector_free(&admin_connector);

  shoal_connector_config_init(&config);
  config.struct_size = SHOAL_CONNECTOR_CONFIG_V1_SIZE - 1;
  expect_error(shoal_connector_create(&config, &connector, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "struct_size");

  uint8_t owned_row_data[] = {'r', 0, 'w'};
  uint8_t owned_family_data[] = {'c', 'f'};
  uint8_t owned_qualifier_data[] = {'c', 'q'};
  uint8_t owned_visibility_data[] = {'A', '&', 'B'};
  shoal_owned_key *owned_key = NULL;
  shoal_owned_key *owned_key_copy = NULL;
  expect_error(shoal_owned_key_create(
                   (shoal_bytes){NULL, 1}, &owned_key, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "key row");
  assert(shoal_owned_key_create_full(
             (shoal_bytes){owned_row_data, sizeof(owned_row_data)},
             (shoal_bytes){owned_family_data, sizeof(owned_family_data)},
             (shoal_bytes){owned_qualifier_data, sizeof(owned_qualifier_data)},
             (shoal_bytes){owned_visibility_data,
                           sizeof(owned_visibility_data)},
             42, &owned_key, &error) == SHOAL_STATUS_OK);
  owned_row_data[0] = 'X';
  shoal_bytes_result *owned_bytes = NULL;
  assert(shoal_owned_key_row(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  shoal_bytes owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 3 && owned_value.data[0] == 'r' &&
         owned_value.data[1] == 0 && owned_value.data[2] == 'w');
  shoal_bytes_result_free(&owned_bytes);
  assert(shoal_owned_key_column_family(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 2 && owned_value.data[0] == 'c' &&
         owned_value.data[1] == 'f');
  shoal_bytes_result_free(&owned_bytes);
  assert(shoal_owned_key_column_qualifier(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 2 && owned_value.data[0] == 'c' &&
         owned_value.data[1] == 'q');
  shoal_bytes_result_free(&owned_bytes);
  assert(shoal_owned_key_column_visibility(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 3 && owned_value.data[0] == 'A' &&
         owned_value.data[1] == '&' && owned_value.data[2] == 'B');
  shoal_bytes_result_free(&owned_bytes);
  assert(shoal_owned_key_clone(owned_key, &owned_key_copy, &error) ==
         SHOAL_STATUS_OK);
  uint8_t predicate_value = 0;
  int32_t owned_order = 99;
  assert(shoal_owned_key_compare(owned_key, owned_key_copy, &owned_order,
                                 &error) == SHOAL_STATUS_OK);
  assert(owned_order == 0);
  assert(shoal_owned_key_compare_visibility(
             owned_key, owned_key_copy, &owned_order, &error) ==
         SHOAL_STATUS_OK);
  assert(owned_order == 0);
  assert(shoal_owned_key_equal(owned_key, owned_key_copy, &predicate_value,
                               &error) == SHOAL_STATUS_OK);
  assert(predicate_value == 1);
  assert(shoal_owned_key_not_equal(owned_key, owned_key_copy,
                                   &predicate_value, &error) ==
         SHOAL_STATUS_OK);
  assert(predicate_value == 0);
  assert(shoal_owned_key_less(owned_key, owned_key_copy, &predicate_value,
                              &error) == SHOAL_STATUS_OK);
  assert(predicate_value == 0);
  assert(shoal_owned_key_less_or_equal(
             owned_key, owned_key_copy, &predicate_value, &error) ==
         SHOAL_STATUS_OK);
  assert(predicate_value == 1);
  assert(shoal_owned_key_set_column_visibility(
             owned_key_copy, (shoal_bytes){(const uint8_t *)"A&C", 3},
             &error) == SHOAL_STATUS_OK);
  assert(shoal_owned_key_compare(owned_key, owned_key_copy, &owned_order,
                                 &error) == SHOAL_STATUS_OK);
  assert(owned_order < 0);
  assert(shoal_owned_key_compare(owned_key_copy, owned_key, &owned_order,
                                 &error) == SHOAL_STATUS_OK);
  assert(owned_order > 0);
  assert(shoal_owned_key_compare_visibility(
             owned_key, owned_key_copy, &owned_order, &error) ==
         SHOAL_STATUS_OK);
  assert(owned_order < 0);
  assert(shoal_owned_key_less(owned_key, owned_key_copy, &predicate_value,
                              &error) == SHOAL_STATUS_OK);
  assert(predicate_value == 1);
  assert(shoal_owned_key_equal(owned_key, owned_key_copy, &predicate_value,
                               &error) == SHOAL_STATUS_OK);
  assert(predicate_value == 0);
  assert(shoal_owned_key_set_column_visibility(
             owned_key_copy,
             (shoal_bytes){owned_visibility_data,
                           sizeof(owned_visibility_data)},
             &error) == SHOAL_STATUS_OK);
  assert(shoal_owned_key_set_timestamp(owned_key_copy, 41, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_compare(owned_key, owned_key_copy, &owned_order,
                                 &error) == SHOAL_STATUS_OK);
  assert(owned_order < 0);
  assert(shoal_owned_key_compare_visibility(
             owned_key, owned_key_copy, &owned_order, &error) ==
         SHOAL_STATUS_OK);
  assert(owned_order == 0);
  assert(shoal_owned_key_set_timestamp(owned_key_copy, 42, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_set_deleted(owned_key_copy, 1, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_compare(owned_key, owned_key_copy, &owned_order,
                                 &error) == SHOAL_STATUS_OK);
  assert(owned_order > 0);
  assert(shoal_owned_key_compare(owned_key_copy, owned_key, &owned_order,
                                 &error) == SHOAL_STATUS_OK);
  assert(owned_order < 0);
  assert(shoal_owned_key_empty(owned_key, &predicate_value, &error) ==
         SHOAL_STATUS_OK);
  assert(predicate_value == 0);
  int64_t owned_timestamp = 0;
  assert(shoal_owned_key_timestamp(owned_key, &owned_timestamp, &error) ==
         SHOAL_STATUS_OK);
  assert(owned_timestamp == 42);
  assert(shoal_owned_key_set_timestamp(owned_key, 41, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_timestamp(owned_key, &owned_timestamp, &error) ==
         SHOAL_STATUS_OK);
  assert(owned_timestamp == 41);
  expect_error(shoal_owned_key_set_deleted(owned_key, 2, &error),
               SHOAL_STATUS_INVALID_ARGUMENT, &error, "0 or 1");
  assert(shoal_owned_key_set_deleted(owned_key, 1, &error) == SHOAL_STATUS_OK);
  assert(shoal_owned_key_is_deleted(owned_key, &predicate_value, &error) ==
         SHOAL_STATUS_OK);
  assert(predicate_value == 1);
  assert(shoal_owned_key_set_row(
             owned_key, (shoal_bytes){(const uint8_t *)"a", 1}, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_set_column_family(
             owned_key, (shoal_bytes){(const uint8_t *)"bc", 2}, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_set_column_qualifier(
             owned_key, (shoal_bytes){(const uint8_t *)"def", 3}, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_set_column_visibility(
             owned_key, (shoal_bytes){(const uint8_t *)"G&H1", 4}, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_row(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 1 && owned_value.data[0] == 'a');
  shoal_bytes_result_free(&owned_bytes);
  assert(shoal_owned_key_column_family(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 2 && memcmp(owned_value.data, "bc", 2) == 0);
  shoal_bytes_result_free(&owned_bytes);
  assert(shoal_owned_key_column_qualifier(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 3 && memcmp(owned_value.data, "def", 3) == 0);
  shoal_bytes_result_free(&owned_bytes);
  assert(shoal_owned_key_column_visibility(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 4 && memcmp(owned_value.data, "G&H1", 4) == 0);
  shoal_bytes_result_free(&owned_bytes);
  size_t owned_size = 0;
  assert(shoal_owned_key_size(owned_key, &owned_size, &error) ==
         SHOAL_STATUS_OK);
  assert(owned_size == 18);
  assert(shoal_owned_key_length(owned_key, &owned_size, &error) ==
         SHOAL_STATUS_OK);
  assert(owned_size == 18);
  assert(shoal_owned_key_row_size(owned_key, &owned_size, &error) ==
         SHOAL_STATUS_OK && owned_size == 1);
  assert(shoal_owned_key_column_family_size(owned_key, &owned_size, &error) ==
         SHOAL_STATUS_OK && owned_size == 2);
  assert(shoal_owned_key_column_qualifier_size(owned_key, &owned_size,
                                               &error) == SHOAL_STATUS_OK &&
         owned_size == 3);
  assert(shoal_owned_key_column_visibility_size(owned_key, &owned_size,
                                                &error) == SHOAL_STATUS_OK &&
         owned_size == 4);
  assert(shoal_owned_key_row(owned_key_copy, &owned_bytes, &error) ==
         SHOAL_STATUS_OK);
  owned_value = shoal_bytes_result_get(owned_bytes);
  assert(owned_value.length == 3 && owned_value.data[0] == 'r' &&
         owned_value.data[1] == 0 && owned_value.data[2] == 'w');
  shoal_bytes_result_free(&owned_bytes);
  shoal_owned_key_free(&owned_key_copy);
  shoal_test_result_alloc_fail_after(0);
  assert(shoal_owned_key_clone(owned_key, &owned_key_copy, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(owned_key_copy == NULL);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();
  shoal_test_result_alloc_fail_after(0);
  assert(shoal_owned_key_row(owned_key, &owned_bytes, &error) ==
         SHOAL_STATUS_OUT_OF_MEMORY);
  assert(owned_bytes == NULL);
  shoal_error_free(&error);
  shoal_test_result_alloc_reset();
  owned_bytes = (shoal_bytes_result *)(uintptr_t)1;
  expect_error(shoal_owned_key_row(NULL, &owned_bytes, &error),
               SHOAL_STATUS_INVALID_HANDLE, &error, "handle");
  assert(owned_bytes == NULL);
  shoal_owned_key_free(&owned_key_copy);
  shoal_owned_key_free(&owned_key_copy);
  shoal_owned_key_free(&owned_key);
  shoal_owned_key_free(&owned_key);

  return 0;
}
