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

#include "data/constructs/Range.h"

namespace cclient {
namespace data {

Range::~Range() {

}

Range::Range()
    :
    start(NULL),
    stop(NULL),
    startKeyInclusive(true),
    stopKeyInclusive(false) {
  infiniteStartKey = true;
  infiniteStopKey = true;
}

Range::Range(const std::string &row)
    :
    Range(row, true, row, true) {
}

Range::Range(const std::string &startRow, bool startInclusive, const std::string &endRow, bool endKeyInclusive, bool update)
    :
    startKeyInclusive(startInclusive),
    stopKeyInclusive(endKeyInclusive) {

  start = std::make_shared<cclient::data::Key>();
  stop = std::make_shared<cclient::data::Key>();
  start->setRow(startRow);
  stop->setRow(endRow);
  infiniteStartKey = startRow.empty();
  infiniteStopKey = endRow.empty();

  if (infiniteStopKey){
    stop = nullptr;
  }

  if (infiniteStartKey){
    start = nullptr;
  }

  if (stopKeyInclusive && stop != nullptr && update) {
    std::pair<char*, size_t> row = stop->getRow();
    char *newRow = new char[row.second + 1];
    memset(newRow, 0x00, row.second + 1);
    memcpy(newRow, row.first, row.second);
    stop->setRow(newRow, row.second + 1);
    delete[] newRow;
  }
  if (!infiniteStartKey && !infiniteStopKey && (stop != nullptr && *stop.get() < start.get())) {
    throw new cclient::exceptions::IllegalArgumentException("Start key must be less than end key in range");
  }
}

Range::Range(std::shared_ptr<Key> startKey, bool startInclusive, std::shared_ptr<Key> endKey, bool endKeyInclusive, bool update)
    :
    start(startKey),
    startKeyInclusive(startInclusive),
    stop(endKey),
    stopKeyInclusive(endKeyInclusive) {
  infiniteStartKey = startKey == NULL;
  infiniteStopKey = stop == NULL;

  if (stopKeyInclusive && stop != nullptr && update) {
    std::pair<char*, size_t> row = stop->getRow();
    char *newRow = new char[row.second + 1];
    memset(newRow, 0x00, row.second + 1);
    memcpy(newRow, row.first, row.second);
    stop->setRow(newRow, row.second + 1);
    delete[] newRow;
  }
  if (!infiniteStartKey && !infiniteStopKey && (stop != nullptr && *stop.get() < start.get())) {
    throw new cclient::exceptions::IllegalArgumentException("Start key must be less than end key in range");
  }
}

bool Range::afterEndKey(const std::shared_ptr<Key> &key) const {
  if (infiniteStartKey)
    return false;

  if (stopKeyInclusive) {
    return *stop.get() < key.get();
  }

  return *stop.get() <= key.get();

}

bool Range::beforeStartKey(const std::shared_ptr<Key> &key) const {
  if (infiniteStartKey) {
    return false;
  }

  if (startKeyInclusive)
    return *key.get() < start.get();
  return *key.get() <= start.get();
}

std::shared_ptr<Range> Range::intersect(const std::shared_ptr<Range> &range) const {
  auto sk = range->getStartKey();
  bool ski = range->getStartKeyInclusive();

  auto ek = range->getStopKey();
  bool eki = range->getStopKeyInclusive();

  if (range->getStartKey() == nullptr) {
    if (getStartKey() != nullptr) {
      sk = getStartKey();
      ski = getStartKeyInclusive();
    }
  } else if (afterEndKey(range->getStartKey()) || (getStopKey() != nullptr && *range->getStartKey().get() == *getStopKey().get() && !(range->getStartKeyInclusive() && getStopKeyInclusive()))) {
    return nullptr;
  } else if (beforeStartKey(range->getStartKey())) {
    sk = getStartKey();
    ski = getStartKeyInclusive();
  }
  if (range->getStopKey() == nullptr) {
    if (getStopKey() != nullptr) {
      ek = getStopKey();
      eki = getStopKeyInclusive();
    }
  } else if (beforeStartKey(range->getStopKey()) || (getStartKey() != nullptr && *range->getStopKey().get() == *getStartKey().get() && !(range->getStopKeyInclusive() && getStartKeyInclusive()))) {
    return nullptr;
  } else if (afterEndKey(range->getStopKey())) {
    ek = getStopKey();
    eki = getStopKeyInclusive();
  }
  return std::make_shared<Range>(sk, ski, ek, eki, false);
}

} /* namespace data */
} /* namespace cclient */
