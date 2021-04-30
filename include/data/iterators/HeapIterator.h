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
#ifndef HEAPITER_H
#define HEAPITER_H

#include <queue>
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

struct KeyValueIteratorComparator {
  bool operator()(const std::shared_ptr<cclient::data::streams::KeyValueIterator> &lhs, const std::shared_ptr<cclient::data::streams::KeyValueIterator> &rhs) const {
    return lhs->getTopKey()->compare(rhs->getTopKey()) > 0;
  }
};

class HeapIterator : public cclient::data::streams::StreamInterface, public cclient::data::streams::KeyValueIterator {
 private:

  std::shared_ptr<cclient::data::Range> block;


  protected:

  std::priority_queue<std::shared_ptr<cclient::data::streams::KeyValueIterator>, std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>>, KeyValueIteratorComparator> queues;

  std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>> iterators;

  std::shared_ptr<cclient::data::Key> nextKey;

  std::shared_ptr<cclient::data::streams::KeyValueIterator> topIterator;


 public:

  virtual bool hasNext() override;

  HeapIterator(){}

  explicit HeapIterator(const std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>> &itrs, const std::shared_ptr<cclient::data::Range> &range = nullptr) {
    iterators = itrs;
  }

  HeapIterator(const HeapIterator &other) = default;

  HeapIterator(HeapIterator &&other) = default;

  HeapIterator& operator=(HeapIterator &&other) = default;

  HeapIterator& operator=(const HeapIterator &other) = default;

  virtual void relocate(cclient::data::streams::StreamRelocation *location) override;

  virtual std::shared_ptr<Key> getTopKey() override ;

  virtual std::shared_ptr<Value> getTopValue() override ;

  virtual std::shared_ptr<KeyValue> getTop() override;

  virtual std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>> operator*() override;

  virtual void next() override;

  

   virtual   DataStream* operator++() override {
    next();
    return this;
  }

   virtual DataStream* operator++(int t) override {
    for (int i = 0; i < t; i++) {
      next();
    }
    return this;
  }

  virtual uint64_t getEntriesFiltered() override;

  virtual void close() override;

  void clear();

  void addSource(const std::shared_ptr<cclient::data::streams::KeyValueIterator> &iter);

 protected:
 void multiNext();

  

  void INLINE getTopIterator() ;

};

}

}

#endif
