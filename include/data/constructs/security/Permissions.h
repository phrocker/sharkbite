#ifndef PERMISSIONS_H_
#define PERMISSIONS_H_

#include "utils/EnumClass.h"
#include <cstdint>
#include <type_traits>

namespace cclient{

namespace data{

enum class SystemPermissions : int8_t {
  GRANT=0,
  CREATE_TABLE,
  DROP_TABLE,
  ALTER_TABLE,
  CREATE_USER,
  DROP_USER,
  ALTER_USER,
  SYSTEM,
  CREATE_NAMESPACE,
  DROP_NAMESPACE,
  ALTER_NAMESPACE,
  OBTAIN_DELEGATION_TOKEN
};

enum class TablePermissions : int8_t{
  READ=2,
  WRITE=3,
  BULK_IMPORT=4,
  ALTER_TABLE=5,
  GRANT=6,
  DROP_TABLE=7,
  GET_SUMMARIES=8
};

enum class NamespacePermissions : int8_t {
  READ=0,
  WRITE,
  ALTER_NAMESPACE,
  GRANT,
  ALTER_TABLE,
  CREATE_TABLE,
  DROP_TABLE,
  BULK_IMPORT,
  DROP_NAMESPACE
};


}
}

template<>
struct EnableBitMaskOperators<cclient::data::SystemPermissions> {
  static const bool enable = true;
};


template<>
struct EnableBitMaskOperators<cclient::data::TablePermissions> {
  static const bool enable = true;
};


template<>
struct EnableBitMaskOperators<cclient::data::NamespacePermissions> {
  static const bool enable = true;
};


#endif