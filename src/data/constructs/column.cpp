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
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#include "data/constructs/column.h"

#include <string>

#include "data/constructs/inputvalidation.h"

namespace cclient {
namespace data {

void Column::setColFamily(const char *r, uint32_t size) {
  columnFamily = std::string(r, size);
}

void Column::setColQualifier(const char *r, uint32_t size) {
  columnQualifier = std::string(r, size);
}

void Column::setColVisibility(const char *r, uint32_t size) {
  columnVisibility = std::string(r, size);
}

Column::~Column() {}

bool Column::operator<(const Column &rhs) const {
  int compare =
      compareBytes(columnFamily.data(), 0, columnFamily.size(),
                   rhs.columnFamily.data(), 0, rhs.columnFamily.size());

  if (compare < 0)
    return true;
  else if (compare > 0)
    return false;
  ;

  compare =
      compareBytes(columnQualifier.data(), 0, columnQualifier.size(),
                   rhs.columnQualifier.data(), 0, rhs.columnQualifier.size());

  if (compare < 0)
    return true;
  else if (compare > 0)
    return false;

  compare =
      compareBytes(columnVisibility.data(), 0, columnVisibility.size(),
                   rhs.columnVisibility.data(), 0, rhs.columnVisibility.size());

  if (compare < 0) return true;

  return false;
}

bool Column::operator==(const Column &rhs) const {
  int compare =
      compareBytes(columnFamily.data(), 0, columnFamily.size(),
                   rhs.columnFamily.data(), 0, rhs.columnFamily.size());

  if (compare != 0) return false;

  compare =
      compareBytes(columnQualifier.data(), 0, columnQualifier.size(),
                   rhs.columnQualifier.data(), 0, rhs.columnQualifier.size());

  if (compare != 0) return false;

  compare =
      compareBytes(columnVisibility.data(), 0, columnVisibility.size(),
                   rhs.columnVisibility.data(), 0, rhs.columnVisibility.size());

  if (compare != 0)
    return false;
  else
    return true;
}

uint64_t Column::write(cclient::data::streams::OutputStream *outStream) {
  if (columnFamily.empty()) {
    outStream->writeBoolean(false);
  } else {
    outStream->writeBoolean(true);
    outStream->writeBytes(columnFamily.data(), columnFamily.size());
  }

  if (columnQualifier.empty()) {
    outStream->writeBoolean(false);
  } else {
    outStream->writeBoolean(true);
    outStream->writeBytes(columnQualifier.data(), columnQualifier.size());
  }

  if (columnVisibility.empty()) {
    return outStream->writeBoolean(false);
  } else {
    outStream->writeBoolean(true);
    return outStream->writeBytes(columnVisibility.data(),
                                 columnVisibility.size());
  }
}

}  // namespace data
}  // namespace cclient
