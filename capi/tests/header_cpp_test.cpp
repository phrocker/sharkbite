#include "shoal.h"

#include <cassert>
#include <cstdint>
#include <type_traits>

static_assert(std::is_same<shoal_status, std::int32_t>::value,
              "shoal_status must remain 32-bit");
static_assert(std::is_same<shoal_abi_capability_bits, std::uint64_t>::value,
              "capability bitset words must remain 64-bit");
static_assert(SHOAL_ABI_VERSION == 1u, "unexpected ABI version");
static_assert(SHOAL_ABI_VERSION_MAJOR == 1u, "unexpected ABI major");
static_assert(SHOAL_ABI_VERSION_MINOR == 19u, "unexpected ABI minor");
static_assert(SHOAL_ABI_VERSION_PATCH == 0u, "unexpected ABI patch");
static_assert(SHOAL_ABI_VERSION_PACKED ==
                  SHOAL_ABI_PACK_VERSION(SHOAL_ABI_VERSION_MAJOR,
                                         SHOAL_ABI_VERSION_MINOR,
                                         SHOAL_ABI_VERSION_PATCH),
              "packed ABI version drifted");
static_assert(SHOAL_ABI_CAPABILITY_CONNECTOR == 0u,
              "unexpected connector capability id");
static_assert(SHOAL_ABI_CAPABILITY_TABLE_ADMIN == 9u,
              "unexpected table admin capability id");
static_assert(SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN == 10u,
              "unexpected namespace admin capability id");
static_assert(SHOAL_ABI_CAPABILITY_SECURITY_ADMIN == 11u,
              "unexpected security admin capability id");
static_assert(SHOAL_ABI_CAPABILITY_TABLE_SPLITS == 12u,
              "unexpected table splits capability id");
static_assert(SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY == 13u,
              "unexpected connector identity capability id");
static_assert(SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS == 14u,
              "unexpected data descriptors capability id");
static_assert(SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY == 15u,
              "unexpected configuration topology capability id");
static_assert(SHOAL_ABI_CAPABILITY_RFILE == 16u,
              "unexpected RFile capability id");
static_assert(SHOAL_ABI_CAPABILITY_DATA_VALUES == 17u,
              "unexpected data values capability id");
static_assert(SHOAL_ABI_CAPABILITY_BUFFERED_WRITER == 18u,
              "unexpected buffered writer capability id");
static_assert(SHOAL_ABI_CAPABILITY_TABLE_MAINTENANCE == 19u,
              "unexpected table maintenance capability id");
static_assert(SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL == 20u,
              "unexpected connector control capability id");
static_assert(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT == 21u,
              "unexpected high-level client capability id");
static_assert(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER == 22u,
              "unexpected high-level scanner capability id");
static_assert(SHOAL_ABI_CAPABILITY_COMPATIBILITY_ERRORS == 23u,
              "unexpected compatibility errors capability id");
static_assert(SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR == 24u,
              "unexpected streaming scan cursor capability id");
static_assert(SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY == 25u,
              "unexpected column visibility capability id");
static_assert(SHOAL_ABI_CAPABILITY_OWNED_KEY == 26u,
              "unexpected owned key capability id");
static_assert(SHOAL_ABI_CAPABILITY_HDFS == 27u,
              "unexpected HDFS capability id");
static_assert(SHOAL_ABI_CAPABILITY_RFILE_LOCALITY_GROUPS == 28u,
              "unexpected RFile locality capability id");
static_assert(SHOAL_ABI_CAPABILITY_COUNT == 32u,
              "unexpected capability count");
static_assert(SHOAL_ABI_CAPABILITY_WORD0 == 0x00000000ffffffffull,
              "unexpected capability word 0");
static_assert(std::is_standard_layout<shoal_connector_identity_view>::value,
              "identity view must remain standard-layout");
static_assert(std::is_standard_layout<shoal_range_view>::value,
              "range view must remain standard-layout");
static_assert(std::is_standard_layout<shoal_iterator_setting_view>::value,
              "iterator setting view must remain standard-layout");
static_assert(std::is_standard_layout<shoal_server_view>::value,
              "server view must remain standard-layout");
static_assert(std::is_standard_layout<shoal_rfile_writer_config>::value,
              "RFile writer config must remain standard-layout");
static_assert(std::is_standard_layout<shoal_rfile_merge_config>::value,
              "RFile merge config must remain standard-layout");
static_assert(std::is_standard_layout<shoal_rfile_entry_view>::value,
              "RFile entry view must remain standard-layout");
static_assert(std::is_standard_layout<shoal_hdfs_dir_entry_view>::value,
              "HDFS directory entry view must remain standard-layout");
static_assert(std::is_standard_layout<shoal_key_value>::value,
              "key/value input must remain standard-layout");
static_assert(std::is_standard_layout<shoal_client_config>::value,
              "client config must remain standard-layout");
static_assert(std::is_standard_layout<shoal_table_constraint_view>::value,
              "table constraint view must remain standard-layout");
static_assert(std::is_standard_layout<shoal_visibility_node_view>::value,
              "visibility node view must remain standard-layout");
static_assert(
    std::is_standard_layout<shoal_visibility_parse_error_view>::value,
    "visibility parse error view must remain standard-layout");

#define ASSERT_PERMISSION_VALUE(name, value)                                 \
  static_assert(name == value, "unexpected permission ordinal: " #name)

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

int main() {
  volatile auto rfile_locality_symbol =
      &shoal_rfile_writer_add_locality_group;
  volatile auto hdfs_view_init_symbol = &shoal_hdfs_dir_entry_view_init;
  volatile auto hdfs_create_symbol = &shoal_hdfs_client_create;
  volatile auto hdfs_close_symbol = &shoal_hdfs_client_close;
  volatile auto hdfs_free_symbol = &shoal_hdfs_client_free;
  volatile auto hdfs_open_symbol = &shoal_hdfs_client_open;
  volatile auto hdfs_create_file_symbol = &shoal_hdfs_client_create_file;
  volatile auto hdfs_list_symbol = &shoal_hdfs_client_list;
  volatile auto hdfs_stat_symbol = &shoal_hdfs_client_stat;
  volatile auto hdfs_remove_symbol = &shoal_hdfs_client_remove;
  volatile auto hdfs_rename_symbol = &shoal_hdfs_client_rename;
  volatile auto hdfs_input_read_symbol = &shoal_hdfs_input_stream_read;
  volatile auto hdfs_input_close_symbol = &shoal_hdfs_input_stream_close;
  volatile auto hdfs_input_free_symbol = &shoal_hdfs_input_stream_free;
  volatile auto hdfs_output_write_symbol = &shoal_hdfs_output_stream_write;
  volatile auto hdfs_output_close_symbol = &shoal_hdfs_output_stream_close;
  volatile auto hdfs_output_free_symbol = &shoal_hdfs_output_stream_free;
  volatile auto hdfs_list_count_symbol = &shoal_hdfs_dir_list_count;
  volatile auto hdfs_list_get_symbol = &shoal_hdfs_dir_list_get;
  volatile auto hdfs_list_free_symbol = &shoal_hdfs_dir_list_free;
  volatile auto hdfs_entry_get_symbol = &shoal_hdfs_dir_entry_result_get;
  volatile auto hdfs_entry_free_symbol = &shoal_hdfs_dir_entry_result_free;
  (void)rfile_locality_symbol;
  (void)hdfs_view_init_symbol;
  (void)hdfs_create_symbol;
  (void)hdfs_close_symbol;
  (void)hdfs_free_symbol;
  (void)hdfs_open_symbol;
  (void)hdfs_create_file_symbol;
  (void)hdfs_list_symbol;
  (void)hdfs_stat_symbol;
  (void)hdfs_remove_symbol;
  (void)hdfs_rename_symbol;
  (void)hdfs_input_read_symbol;
  (void)hdfs_input_close_symbol;
  (void)hdfs_input_free_symbol;
  (void)hdfs_output_write_symbol;
  (void)hdfs_output_close_symbol;
  (void)hdfs_output_free_symbol;
  (void)hdfs_list_count_symbol;
  (void)hdfs_list_get_symbol;
  (void)hdfs_list_free_symbol;
  (void)hdfs_entry_get_symbol;
  (void)hdfs_entry_free_symbol;
  shoal_connector *connector = nullptr;
  shoal_client *client = nullptr;
  shoal_error *error = nullptr;
  assert(shoal_error_source(nullptr) == SHOAL_ERROR_SOURCE_RUNTIME);
  assert(shoal_error_compatibility(nullptr) ==
         SHOAL_ERROR_COMPATIBILITY_RUNTIME_ERROR);
  assert(shoal_error_source_name(nullptr) != nullptr);
  assert(shoal_error_compatibility_name(nullptr) != nullptr);
  shoal_table_list_result *tables = nullptr;
  shoal_table_properties_result *properties = nullptr;
  shoal_table_view table{};
  shoal_table_property_view property{};
  shoal_namespace_list_result *namespaces = nullptr;
  shoal_namespace_properties_result *namespace_properties = nullptr;
  shoal_versioned_properties_result *versioned_properties = nullptr;
  shoal_bytes_list_result *bytes = nullptr;
  shoal_connector_identity_result *identity = nullptr;
  shoal_connector_identity_view identity_view{};
  shoal_range_result *range_result = nullptr;
  shoal_range_view range_view{};
  shoal_iterator_setting_result *iterator_result = nullptr;
  shoal_iterator_setting_view iterator_view{};
  shoal_scanner *scanner = nullptr;
  shoal_batch_scanner *batch_scanner = nullptr;
  shoal_scan_result *scan_result = nullptr;
  shoal_mutation *mutation = nullptr;
  shoal_batch_writer *writer = nullptr;
  shoal_write_failure *write_failure = nullptr;
  shoal_configuration *configuration = nullptr;
  shoal_bytes_result *bytes_result = nullptr;
  shoal_string_list_result *strings = nullptr;
  shoal_server_list_result *servers = nullptr;
  shoal_server_view server_view{};
  shoal_rfile_writer *rfile_writer = nullptr;
  shoal_rfile_reader *rfile_reader = nullptr;
  shoal_rfile_seekable *rfile_seekable = nullptr;
  shoal_rfile_entry_result *rfile_entry = nullptr;
  shoal_rfile_entry_view rfile_entry_view{};
  shoal_authorizations *authorizations = nullptr;
  shoal_key_value_result *key_value_result = nullptr;
  shoal_accumulo_writer *accumulo_writer = nullptr;
  shoal_cancellation *cancellation = nullptr;
  shoal_table_constraint_list_result *constraints = nullptr;
  shoal_table_constraint_view constraint_view{};
  shoal_key_value key_value{};
  shoal_column_visibility *visibility = nullptr;
  shoal_visibility_node *visibility_tree = nullptr;
  shoal_visibility_node *visibility_child = nullptr;
  shoal_node_expression *node_expression = nullptr;
  shoal_visibility_evaluator *visibility_evaluator = nullptr;
  shoal_visibility_node_view visibility_view{};
  shoal_visibility_parse_error_view parse_error_view{};
  shoal_owned_key *owned_key = nullptr;
  shoal_owned_key *owned_key_copy = nullptr;
  uint8_t satisfied = 0;
  int32_t comparison = 0;
  assert(shoal_abi_version() == SHOAL_ABI_VERSION);
  assert(shoal_abi_version_major() == SHOAL_ABI_VERSION_MAJOR);
  assert(shoal_abi_version_minor() == SHOAL_ABI_VERSION_MINOR);
  assert(shoal_abi_version_patch() == SHOAL_ABI_VERSION_PATCH);
  assert(shoal_abi_version_packed() == SHOAL_ABI_VERSION_PACKED);
  assert(shoal_abi_capability_count() == SHOAL_ABI_CAPABILITY_COUNT);
  assert(shoal_abi_capability_word_count() == SHOAL_ABI_CAPABILITY_WORD_COUNT);
  assert(shoal_abi_capability_word(0) == SHOAL_ABI_CAPABILITY_WORD0);
  assert(shoal_abi_capability_word(1) == 0);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_BATCH_WRITER) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_TABLE_ADMIN) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_SECURITY_ADMIN) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_TABLE_SPLITS) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY) ==
         1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS) == 1);
  assert(shoal_abi_has_capability(
             SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_RFILE) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_DATA_VALUES) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_BUFFERED_WRITER) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_TABLE_MAINTENANCE) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER) ==
         1);
  assert(shoal_abi_has_capability(
             SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_OWNED_KEY) == 1);
  assert(shoal_abi_has_capability(SHOAL_ABI_CAPABILITY_COUNT) == 0);
  assert(shoal_owned_key_create(
             {reinterpret_cast<const uint8_t *>("row"), 3}, &owned_key,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_owned_key_clone(owned_key, &owned_key_copy, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_owned_key_equal(owned_key, owned_key_copy, &satisfied,
                               &error) == SHOAL_STATUS_OK);
  assert(satisfied == 1);
  shoal_owned_key_free(&owned_key_copy);
  shoal_owned_key_free(&owned_key);
  shoal_visibility_node_view_init(&visibility_view);
  shoal_visibility_parse_error_view_init(&parse_error_view);
  assert(shoal_column_visibility_create(
             {reinterpret_cast<const uint8_t *>("A"), 1}, &visibility,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_column_visibility_expression(visibility, &bytes_result,
                                            &error) == SHOAL_STATUS_OK);
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_column_visibility_tree(visibility, &visibility_tree, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_column_visibility_normalized(visibility, &visibility_child,
                                            &error) == SHOAL_STATUS_OK);
  assert(shoal_visibility_node_compare(visibility_tree, visibility_child,
                                       &comparison, &error) ==
         SHOAL_STATUS_OK);
  shoal_visibility_node_free(&visibility_child);
  assert(shoal_visibility_node_get(visibility_tree, &visibility_view, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_visibility_node_expression(visibility_tree, &bytes_result,
                                          &error) == SHOAL_STATUS_OK);
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_visibility_node_term(
             visibility_tree,
             {reinterpret_cast<const uint8_t *>("A"), 1}, &node_expression,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_node_expression_size(node_expression) == 1);
  assert(shoal_node_expression_term(node_expression, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_node_expression_buffer(node_expression, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_bytes_result_free(&bytes_result);
  shoal_node_expression_free(&node_expression);
  assert(shoal_node_expression_create(
             {reinterpret_cast<const uint8_t *>("A"), 1}, 0, 1,
             &node_expression, &error) == SHOAL_STATUS_OK);
  shoal_node_expression_free(&node_expression);
  assert(shoal_column_visibility_flatten(visibility, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_bytes_result_free(&bytes_result);
  assert(shoal_visibility_node_child(visibility_tree, 0, &visibility_child,
                                     &error) == SHOAL_STATUS_INVALID_ARGUMENT);
  assert(shoal_error_visibility_parse(error, &parse_error_view) ==
         SHOAL_STATUS_NOT_FOUND);
  shoal_error_free(&error);
  assert(shoal_visibility_evaluator_create(nullptr, &visibility_evaluator,
                                           &error) == SHOAL_STATUS_OK);
  assert(shoal_visibility_evaluator_evaluate(
             visibility_evaluator,
             {reinterpret_cast<const uint8_t *>(""), 0}, &satisfied,
             &error) == SHOAL_STATUS_OK);
  assert(shoal_visibility_evaluator_evaluate_tree(
             visibility_evaluator,
             {reinterpret_cast<const uint8_t *>("A"), 1}, visibility_tree,
             &satisfied, &error) == SHOAL_STATUS_OK);
  assert(shoal_visibility_evaluator_set_authorizations(
             visibility_evaluator, nullptr, &error) == SHOAL_STATUS_OK);
  assert(shoal_visibility_evaluator_authorizations(
             visibility_evaluator, &authorizations, &error) == SHOAL_STATUS_OK);
  shoal_authorizations_free(&authorizations);
  shoal_visibility_evaluator_free(&visibility_evaluator);
  shoal_visibility_node_free(&visibility_tree);
  shoal_column_visibility_free(&visibility);
  assert(shoal_versioned_properties_version(versioned_properties) == 0);
  assert(shoal_versioned_properties_count(versioned_properties) == 0);
  assert(shoal_versioned_properties_get(versioned_properties, 0, &property,
                                        &error) ==
         SHOAL_STATUS_INVALID_ARGUMENT);
  assert(error != nullptr);
  shoal_error_free(&error);
  assert(shoal_cancellation_create(&cancellation, &error) == SHOAL_STATUS_OK);
  assert(shoal_scanner_scan_with_cancellation(
             scanner, nullptr, 0, cancellation, &scan_result, &error) ==
         SHOAL_STATUS_INVALID_HANDLE);
  shoal_error_free(&error);
  assert(shoal_batch_scanner_scan_with_cancellation(
             batch_scanner, nullptr, 0, 0, cancellation, &scan_result,
             &error) == SHOAL_STATUS_INVALID_HANDLE);
  shoal_error_free(&error);
  assert(shoal_connector_invalidate_table(connector, "5", &error) ==
         SHOAL_STATUS_INVALID_HANDLE);
  shoal_error_free(&error);
  assert(shoal_connector_invalidate_discovery(connector, &error) ==
         SHOAL_STATUS_INVALID_HANDLE);
  shoal_error_free(&error);
  assert(shoal_cancellation_cancel(cancellation, &error) == SHOAL_STATUS_OK);
  shoal_client_config client_config{};
  shoal_client_config_init(&client_config);
  assert(client_config.struct_size == SHOAL_CLIENT_CONFIG_V1_SIZE);
  assert(client_config.thread_count == 10);
  const auto client_select_column = &shoal_client_select_column;
  const auto client_scan_range = &shoal_client_scan_range;
  const auto client_scan_range_cancel =
      &shoal_client_scan_range_with_cancellation;
  const auto client_scan_ranges = &shoal_client_scan_ranges;
  const auto client_scan_ranges_cancel =
      &shoal_client_scan_ranges_with_cancellation;
  const auto scanner_stream = &shoal_scanner_stream;
  const auto scanner_stream_cancel = &shoal_scanner_stream_with_cancellation;
  const auto batch_scanner_stream = &shoal_batch_scanner_stream;
  const auto batch_scanner_stream_cancel =
      &shoal_batch_scanner_stream_with_cancellation;
  const auto client_stream_range = &shoal_client_stream_range;
  const auto client_stream_range_cancel =
      &shoal_client_stream_range_with_cancellation;
  const auto client_stream_ranges = &shoal_client_stream_ranges;
  const auto client_stream_ranges_cancel =
      &shoal_client_stream_ranges_with_cancellation;
  const auto cursor_next = &shoal_scan_cursor_next;
  const auto cursor_close = &shoal_scan_cursor_close;
  const auto cursor_free = &shoal_scan_cursor_free;
  (void)client_select_column;
  (void)client_scan_range;
  (void)client_scan_range_cancel;
  (void)client_scan_ranges;
  (void)client_scan_ranges_cancel;
  (void)scanner_stream;
  (void)scanner_stream_cancel;
  (void)batch_scanner_stream;
  (void)batch_scanner_stream_cancel;
  (void)client_stream_range;
  (void)client_stream_range_cancel;
  (void)client_stream_ranges;
  (void)client_stream_ranges_cancel;
  (void)cursor_next;
  (void)cursor_close;
  (void)cursor_free;
  assert(shoal_client_create(&client_config, &client, &error) ==
         SHOAL_STATUS_INVALID_ARGUMENT);
  shoal_error_free(&error);
  assert(shoal_configuration_create(&configuration, &error) == SHOAL_STATUS_OK);
  const std::uint8_t key_data[] = {'k', '\0'};
  const std::uint8_t value_data[] = {'v', '\0'};
  shoal_bytes key{key_data, sizeof(key_data)};
  shoal_bytes value{value_data, sizeof(value_data)};
  assert(shoal_configuration_set(configuration, key, value, &error) ==
         SHOAL_STATUS_OK);
  assert(shoal_configuration_get(configuration, key, &bytes_result, &error) ==
         SHOAL_STATUS_OK);
  shoal_bytes copied = shoal_bytes_result_get(bytes_result);
  assert(copied.length == sizeof(value_data));
  shoal_server_view_init(&server_view);
  assert(server_view.struct_size == SHOAL_SERVER_VIEW_V1_SIZE);
  shoal_rfile_entry_view_init(&rfile_entry_view);
  assert(rfile_entry_view.struct_size == SHOAL_RFILE_ENTRY_VIEW_V1_SIZE);
  shoal_key_value_init(&key_value);
  assert(key_value.struct_size == SHOAL_KEY_VALUE_V1_SIZE);
  shoal_table_constraint_view_init(&constraint_view);
  assert(constraint_view.struct_size == SHOAL_TABLE_CONSTRAINT_VIEW_V1_SIZE);
  assert(shoal_connector_list_table_constraints(
             connector, "events", 0, &constraints, &error) ==
         SHOAL_STATUS_INVALID_HANDLE);
  shoal_error_free(&error);
  assert(shoal_table_constraint_list_get(constraints, 0, &constraint_view,
                                         &error) ==
         SHOAL_STATUS_INVALID_ARGUMENT);
  shoal_error_free(&error);
  assert(error == nullptr);
  shoal_connector_identity_view_init(&identity_view);
  assert(identity_view.struct_size == SHOAL_CONNECTOR_IDENTITY_VIEW_V1_SIZE);
  assert(shoal_connector_get_identity(connector, 0, &identity, &error) ==
         SHOAL_STATUS_INVALID_HANDLE);
  shoal_error_free(&error);
  shoal_range_view_init(&range_view);
  assert(shoal_range_get(range_result, &range_view, &error) ==
         SHOAL_STATUS_INVALID_ARGUMENT);
  shoal_error_free(&error);
  shoal_iterator_setting_view_init(&iterator_view);
  assert(shoal_iterator_setting_get(iterator_result, &iterator_view, &error) ==
         SHOAL_STATUS_INVALID_ARGUMENT);
  shoal_error_free(&error);
  assert(shoal_connector_identity_get(identity, &identity_view, &error) ==
         SHOAL_STATUS_INVALID_ARGUMENT);
  shoal_error_free(&error);
  (void)table;
  (void)property;
  shoal_connector_free(&connector);
  shoal_client_free(&client);
  shoal_table_list_free(&tables);
  shoal_table_properties_free(&properties);
  shoal_namespace_list_free(&namespaces);
  shoal_namespace_properties_free(&namespace_properties);
  shoal_versioned_properties_free(&versioned_properties);
  shoal_bytes_list_free(&bytes);
  shoal_connector_identity_free(&identity);
  shoal_range_free(&range_result);
  shoal_iterator_setting_free(&iterator_result);
  shoal_scanner_free(&scanner);
  shoal_batch_scanner_free(&batch_scanner);
  shoal_scan_result_free(&scan_result);
  shoal_mutation_free(&mutation);
  shoal_batch_writer_free(&writer);
  shoal_write_failure_free(&write_failure);
  shoal_configuration_free(&configuration);
  shoal_bytes_result_free(&bytes_result);
  shoal_string_list_free(&strings);
  shoal_server_list_free(&servers);
  shoal_rfile_writer_free(&rfile_writer);
  shoal_rfile_reader_free(&rfile_reader);
  shoal_rfile_seekable_free(&rfile_seekable);
  shoal_rfile_entry_result_free(&rfile_entry);
  shoal_authorizations_free(&authorizations);
  shoal_key_value_result_free(&key_value_result);
  shoal_accumulo_writer_free(&accumulo_writer);
  shoal_cancellation_free(&cancellation);
  shoal_table_constraint_list_free(&constraints);
  shoal_error_free(&error);
  return 0;
}
