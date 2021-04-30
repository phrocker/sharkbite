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
#include "data/iterators/HeapIterator.h"

namespace cclient {
namespace data {

void HeapIterator::relocate(
    const std::shared_ptr<cclient::data::streams::StreamRelocation> &location) {
  for (auto &itr : iterators) {
    itr->relocate(location);
    addSource(itr);
  }
}

std::shared_ptr<Key> HeapIterator::getTopKey() {
  return topIterator->getTopKey();
}

std::shared_ptr<Value> HeapIterator::getTopValue() {
  return topIterator->getTopValue();
}

std::shared_ptr<KeyValue> HeapIterator::getTop() {
  return std::make_shared<KeyValue>(topIterator->getTopKey(),
                                    topIterator->getTopValue());
}

std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>>
HeapIterator::operator*() {
  return std::make_pair(topIterator->getTopKey(), topIterator->getTopValue());
}

bool HeapIterator::hasNext() {
  return topIterator != nullptr && topIterator->hasNext();
}

void HeapIterator::next() { multiNext(); }

void HeapIterator::multiNext() {
  if (SH_UNLIKELY(nullptr == topIterator)) {
    throw cclient::exceptions::IllegalStateException(
        "Called next() without a top");
  }
  topIterator->next();
  if (!topIterator->hasNext()) {
    if (nullptr == nextKey) {
      topIterator = nullptr;
      return;
    }
    getTopIterator();
  } else {
    if (nullptr == nextKey) {
      return;
    }
    if (nextKey->compare(topIterator->getTopKey()) < 0) {
      auto top = queues.top();
      queues.pop();
      queues.push(topIterator);
      topIterator = top;
      nextKey = queues.top()->getTopKey();
    }
  }
}

void HeapIterator::close() {
  for (auto &iter : iterators) {
    iter->close();
  }
}

uint64_t HeapIterator::getEntriesFiltered() {
  uint64_t filtered = 0;
  for (const auto &iter : iterators) {
    filtered += iter->getEntriesFiltered();
  }
  return filtered;
}

void HeapIterator::addSource(
    const std::shared_ptr<cclient::data::streams::KeyValueIterator> &iter) {
  if (iter->hasNext()) {
    queues.push(iter);
    if (nullptr != topIterator) {
      queues.push(topIterator);
    }
    getTopIterator();
  }
}

void HeapIterator::getTopIterator() {
  // remove the top iterator
  topIterator = queues.top();
  queues.pop();
  if (!queues.empty()) {
    nextKey = queues.top()->getTopKey();
  } else {
    nextKey = nullptr;
  }
}

void HeapIterator::clear() {
  queues = std::priority_queue<
      std::shared_ptr<cclient::data::streams::KeyValueIterator>,
      std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>>,
      KeyValueIteratorComparator>();
  topIterator = nullptr;
  nextKey = nullptr;
}

}  // namespace data
}  // namespace cclient