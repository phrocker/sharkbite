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
#include "data/constructs/server/RangeDefinition.h"

#include <string>
#include <vector>

#include "data/constructs/column.h"
#include "data/constructs/server/../KeyExtent.h"
#include "data/constructs/server/../Range.h"
#include "data/constructs/server/../security/AuthInfo.h"
#include "data/constructs/server/ServerDefinition.h"

namespace cclient {

namespace data {

namespace tserver {

/**
 * Constructor
 * @param creds ptr to credentials
 * @param auths pointer to auths used in range definition
 * @param host host name we're connecting to
 * @param port port connecting to
 * @param keyRange range
 * @param keyExt key extent
 * @param inCols incoming columns
 *
 */

RangeDefinition::RangeDefinition(
    cclient::data::security::AuthInfo *creds,
    cclient::data::security::Authorizations *auths, const std::string &host,
    uint32_t port, std::vector<std::shared_ptr<cclient::data::Range>> *keyRange,
    std::vector<std::shared_ptr<KeyExtent>> *keyExt,
    const std::vector<Column> &inCols)
    : ServerDefinition(creds, auths, host, port) {
  if (NULL != keyRange) {
    ranges.insert(ranges.end(), keyRange->begin(), keyRange->end());
  }

  if (NULL != keyExt)
    extents.insert(extents.end(), keyExt->begin(), keyExt->end());

  columns.insert(columns.end(), inCols.begin(), inCols.end());
}

RangeDefinition::RangeDefinition(
    cclient::data::security::AuthInfo *creds,
    cclient::data::security::Authorizations *auths, const std::string &host,
    uint32_t port, std::vector<std::shared_ptr<cclient::data::Range>> *keyRange,
    std::vector<std::shared_ptr<KeyExtent>> *keyExt)
    : RangeDefinition(creds, auths, host, port, keyRange, keyExt,
                      std::vector<Column>()) {}

/**
 * Returns ranges
 * @returns ranges
 */
std::vector<std::shared_ptr<Range>> *RangeDefinition::getRanges() {
  return &ranges;
}

/**
 * Returns key extents
 * @returns key extents;
 */
std::vector<std::shared_ptr<KeyExtent>> *RangeDefinition::getExtents() {
  return &extents;
}

bool RangeDefinition::operator==(const RangeDefinition *rhs) const {
  if (rhs == nullptr) return false;
  return *this == *rhs;
}

bool RangeDefinition::operator==(const RangeDefinition &other) const {
  if (other.ranges.size() != ranges.size()) {
    return false;
  }
  for (const auto &range : ranges) {
    bool found = false;
    for (const auto &otherRange : other.ranges) {
      if (*range == *otherRange) {
        found = true;
        break;
      }
    }
    if (!found) {
      return false;
    }
  }
  if (other.extents.size() != extents.size()) {
    return false;
  }

  for (const auto &extent : extents) {
    bool found = false;
    for (const auto &otherExtent : other.extents) {
      if (*extent == *otherExtent) {
        found = true;
        break;
      }
    }
    if (!found) {
      return false;
    }
  }

  if (other.columns.size() != columns.size()) {
    return false;
  }

  for (const auto &column : columns) {
    bool found = false;
    for (const auto &otherColumn : other.columns) {
      if (column == otherColumn) {
        found = true;
        break;
      }
    }
    if (!found) {
      return false;
    }
  }
  return true;
}

}  // namespace tserver
}  // namespace data
}  // namespace cclient
