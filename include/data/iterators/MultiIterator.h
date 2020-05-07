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
#ifndef MULI_HEAPITER_H
#define MULI_HEAPITER_H

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

namespace cclient {
namespace data {

class MultiIterator : public cclient::data::HeapIterator {
 public:
  explicit MultiIterator(const std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>> &itrs, const std::shared_ptr<cclient::data::Range> &range = nullptr)
      :
      HeapIterator(itrs, range) {
  }

  MultiIterator(const MultiIterator &other) = default;

  MultiIterator(MultiIterator &&other) = default;

  MultiIterator& operator=(MultiIterator &&other) = default;

  MultiIterator& operator=(const MultiIterator &other) = default;

};

}

}

#endif
