#include "shoal.h"

#include <assert.h>
#include <stdint.h>

#if defined(_WIN32)
#include <windows.h>

typedef HMODULE shoal_library_handle;
typedef void *shoal_symbol;

static shoal_library_handle load_library(const char *path) {
  return LoadLibraryA(path);
}

static shoal_symbol load_symbol(shoal_library_handle handle,
                                const char *symbol_name) {
  return (void *)GetProcAddress(handle, symbol_name);
}

static void close_library(shoal_library_handle handle) {
  if (handle != NULL) {
    FreeLibrary(handle);
  }
}
#else
#include <dlfcn.h>

typedef void *shoal_library_handle;
typedef void *shoal_symbol;

static shoal_library_handle load_library(const char *path) {
  return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

static shoal_symbol load_symbol(shoal_library_handle handle,
                                const char *symbol_name) {
  return dlsym(handle, symbol_name);
}

static void close_library(shoal_library_handle handle) {
  if (handle != NULL) {
    dlclose(handle);
  }
}
#endif

typedef uint32_t(SHOAL_CALL *shoal_u32_query_fn)(void);
typedef uint8_t(SHOAL_CALL *shoal_has_capability_fn)(
    shoal_abi_capability_id capability_id);
typedef shoal_abi_capability_bits(SHOAL_CALL *shoal_capability_word_fn)(
    uint32_t word_index);

int main(int argc, char **argv) {
  assert(argc == 2);
  assert(argv != NULL);
  assert(argv[1] != NULL);

  shoal_library_handle library = load_library(argv[1]);
  assert(library != NULL);

  shoal_symbol version_symbol = load_symbol(library, "shoal_abi_version");
  shoal_symbol major_symbol = load_symbol(library, "shoal_abi_version_major");
  shoal_symbol minor_symbol = load_symbol(library, "shoal_abi_version_minor");
  shoal_symbol patch_symbol = load_symbol(library, "shoal_abi_version_patch");
  shoal_symbol packed_symbol = load_symbol(library, "shoal_abi_version_packed");
  shoal_symbol capability_count_symbol =
      load_symbol(library, "shoal_abi_capability_count");
  shoal_symbol capability_word_count_symbol =
      load_symbol(library, "shoal_abi_capability_word_count");
  shoal_symbol capability_word_symbol =
      load_symbol(library, "shoal_abi_capability_word");
  shoal_symbol has_capability_symbol =
      load_symbol(library, "shoal_abi_has_capability");

  assert(version_symbol != NULL);
  assert(major_symbol != NULL);
  assert(minor_symbol != NULL);
  assert(patch_symbol != NULL);
  assert(packed_symbol != NULL);
  assert(capability_count_symbol != NULL);
  assert(capability_word_count_symbol != NULL);
  assert(capability_word_symbol != NULL);
  assert(has_capability_symbol != NULL);
  assert(load_symbol(library, "shoal_abi_missing_symbol") == NULL);

  shoal_u32_query_fn version = (shoal_u32_query_fn)version_symbol;
  shoal_u32_query_fn major = (shoal_u32_query_fn)major_symbol;
  shoal_u32_query_fn minor = (shoal_u32_query_fn)minor_symbol;
  shoal_u32_query_fn patch = (shoal_u32_query_fn)patch_symbol;
  shoal_u32_query_fn packed = (shoal_u32_query_fn)packed_symbol;
  shoal_u32_query_fn capability_count =
      (shoal_u32_query_fn)capability_count_symbol;
  shoal_u32_query_fn capability_word_count =
      (shoal_u32_query_fn)capability_word_count_symbol;
  shoal_capability_word_fn capability_word =
      (shoal_capability_word_fn)capability_word_symbol;
  shoal_has_capability_fn has_capability =
      (shoal_has_capability_fn)has_capability_symbol;

  assert(version() == SHOAL_ABI_VERSION);
  assert(major() == SHOAL_ABI_VERSION_MAJOR);
  assert(minor() == SHOAL_ABI_VERSION_MINOR);
  assert(patch() == SHOAL_ABI_VERSION_PATCH);
  assert(packed() == SHOAL_ABI_VERSION_PACKED);
  assert(capability_count() == SHOAL_ABI_CAPABILITY_COUNT);
  assert(capability_word_count() == SHOAL_ABI_CAPABILITY_WORD_COUNT);
  assert(capability_word(0) == SHOAL_ABI_CAPABILITY_WORD0);
  assert(capability_word(1) == 0);
  assert(has_capability(SHOAL_ABI_CAPABILITY_CONNECTOR) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_TABLE_ADMIN) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_SECURITY_ADMIN) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_TABLE_SPLITS) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_RFILE) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_DATA_VALUES) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY) == 1);
  assert(has_capability(SHOAL_ABI_CAPABILITY_COUNT) == 0);
  assert(has_capability(63u) == 0);
  assert(has_capability(64u) == 0);

  close_library(library);
  return 0;
}
