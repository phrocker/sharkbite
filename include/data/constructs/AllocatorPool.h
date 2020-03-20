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

#ifndef ARRAY_ALLOCATOR_POOL_H_
#define ARRAY_ALLOCATOR_POOL_H_

#include <vector>
#include <map>
#include <deque>
#include <cstdio>
#include "data/extern/concurrentqueue/concurrentqueue.h"
#include "ObjectPool.h"
#include "Text.h"

namespace cclient {
namespace data {

class ArrayAllocatorPool : public ObjectAllocatorPool<Text> {
 private:
  std::deque<std::pair<char*,size_t>> buffers256;
  std::deque<std::pair<char*,size_t>> buffers128;
  std::deque<std::pair<char*,size_t>> buffers64;
  std::deque<std::pair<char*,size_t>> buffers32;


  std::map<char*,std::pair<int,size_t>> referenceCount;
  inline void free(std::pair<char*,size_t> vec, moodycamel::ConcurrentQueue<std::pair<char*,size_t>> *ptr);

  inline void reclaim(moodycamel::ConcurrentQueue<std::pair<char*,size_t>> *ptr);

 protected:
  virtual ~ArrayAllocatorPool();
  ArrayAllocatorPool();
 public:


  virtual std::pair<char*,size_t> allocateBuffer(size_t size) override;

  virtual std::shared_ptr<Text> allocate(size_t size) override;

  virtual void free(std::pair<char*,size_t> &&ptr) override;

  static ArrayAllocatorPool* getInstance() {
    // create 10000 before we revert to additional heap
    static ArrayAllocatorPool pool;
    return &pool;
  }

};

}
}

#endif
