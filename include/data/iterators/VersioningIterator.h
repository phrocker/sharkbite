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
#ifndef VERS_MULI_HEAPITER_H
#define VERS_MULI_HEAPITER_H

#include "HeapIterator.h"
#include "data/constructs/Range.h"
#include "data/streaming/Streams.h"
#include "data/streaming/accumulo/KeyValueIterator.h"
#include "data/streaming/input/InputStream.h"
#include "data/streaming/StreamRelocation.h"
#include "data/constructs/Key.h"
#include "data/constructs/value.h"
#include "data/extern/fastmemcpy/FastMemCpy.h"
#include "data/exceptions/IllegalStateException.h"

#ifndef SH_UNLIKELY
#ifdef __GNUC__
#define SH_UNLIKELY(val) (__builtin_expect((val), 0))
#define SH_LIKELY(val) (__builtin_expect((val), 1))
#else
      #define SH_UNLIKELY(val) (val)
      #define SH_LIKELY(val) (val)
      #endif
#endif

namespace cclient {
namespace data {

class VersioningIterator : public cclient::data::HeapIterator {
 private:
  std::shared_ptr<cclient::data::Key> topKey;
  std::shared_ptr<cclient::data::Value> topValue;
 public:
  explicit VersioningIterator(const std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>> &itrs, const std::shared_ptr<cclient::data::Range> &range = nullptr)
      :
      HeapIterator(itrs, range) {
  }

  virtual void relocate(const std::shared_ptr<cclient::data::streams::StreamRelocation> &location) override {
    cclient::data::HeapIterator::relocate(location);
    next();
  }

  VersioningIterator(const VersioningIterator &other) = default;

  VersioningIterator(VersioningIterator &&other) = default;

  VersioningIterator& operator=(VersioningIterator &&other) = default;

  VersioningIterator& operator=(const VersioningIterator &other) = default;

  virtual std::shared_ptr<Key> getTopKey() override;

  virtual std::shared_ptr<Value> getTopValue() override;

  virtual bool hasNext() override;

  virtual std::shared_ptr<KeyValue> getTop() override;

  virtual void next() override;

};

}

}

#endif
