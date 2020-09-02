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
#ifndef INCLUDE_INTERCONNECT_ACCUMULO_KEYVALUEITERATOR_H_
#define INCLUDE_INTERCONNECT_ACCUMULO_KEYVALUEITERATOR_H_

#include "../../constructs/Key.h"
#include "../../constructs/KeyValue.h"
#include "../../constructs/value.h"
#include "../DataStream.h"

namespace cclient {
namespace data {
namespace streams {

class KeyValueIterator
    : public DataStream<
          std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>>> {
 public:
  explicit KeyValueIterator(KeyValueIterator* source,
                            StreamConfiguration* configuration) {
    setConfiguration(configuration);
  }

  KeyValueIterator() {}

  virtual ~KeyValueIterator() {}

  virtual bool hasNext() { return false; }

  virtual void relocate(StreamRelocation* location) {}

  virtual DataStream* begin() { return this; }

  virtual DataStream* end() { return this; }

  virtual std::shared_ptr<Key> getTopKey() { return nullptr; }

  virtual std::shared_ptr<Value> getTopValue() { return nullptr; }

  virtual std::shared_ptr<KeyValue> getTop() {
    return std::make_shared<KeyValue>(getTopKey(), getTopValue());
  }

  virtual std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>> operator*() {
    return std::make_pair(nullptr, nullptr);
  }

  virtual void next() {}

  virtual uint64_t getEntriesFiltered() { return 0; }

  virtual DataStream* operator++() { return this; }

  virtual DataStream* operator++(int t) { return this; }
};
}  // namespace streams
}  // namespace data
}  // namespace cclient
#endif /* INCLUDE_INTERCONNECT_ACCUMULO_KEYVALUEITERATOR_H_ */
