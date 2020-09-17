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
#include "data/iterators/VersioningIterator.h"

namespace cclient {
namespace data {

std::shared_ptr<Key> VersioningIterator::getTopKey() { return topKey; }

std::shared_ptr<Value> VersioningIterator::getTopValue() { return topValue; }

bool VersioningIterator::hasNext() {
  return topKey != nullptr || topIterator != nullptr;
}

std::shared_ptr<KeyValue> VersioningIterator::getTop() {
  return std::make_shared<KeyValue>(topKey, topValue);
}

void VersioningIterator::next() {
  if (!cclient::data::HeapIterator::hasNext()) {
    topKey = nullptr;
    return;
  }
  cclient::data::HeapIterator::next();
  if (cclient::data::HeapIterator::hasNext()) {
    topKey = topIterator->getTopKey();
    topValue = topIterator->getTopValue();
    if (SH_UNLIKELY(topKey->compareToVisibility(topIterator->getTopKey()) ==
                    0)) {
      do {
        cclient::data::HeapIterator::next();
      } while (nullptr != topIterator &&
               topKey->compareToVisibility(topIterator->getTopKey()) == 0);
    }
  } else {
    topKey = nullptr;
  }
}

}  // namespace data
}  // namespace cclient