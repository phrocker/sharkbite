#include "cabi_symbol_fixture.h"

// shoal_comment_only();
static const char *fixture_name = "shoal_string_only";

#if 0
shoal_disabled_only();
#endif

int compiled_fixture_reference_count(void) {
  void (*address_ref)(void) = &shoal_live_address;
  shoal_live_call();
  return fixture_name != 0 && address_ref != 0;
}
