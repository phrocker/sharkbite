/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing ScannerOptions and
 * limitations under the License.
 */

#ifndef SCANNER_OPTS_H_
#define SCANNER_OPTS_H_

#include <cstdint>
#include <type_traits>

#include "utils/EnumClass.h"

enum class ScannerOptions : unsigned int {
  BASIC_SCANNER = 0x0,
  ENABLE_HEDGED_READS = 0x1,
  ENABLE_RFILE_SCANNER
};

template <>
struct EnableBitMaskOperators<ScannerOptions> {
  static const bool enable = true;
};

#endif
