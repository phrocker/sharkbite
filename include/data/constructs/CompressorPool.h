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

#ifndef COMPRESSSOR_POOL_H_
#define COMPRESSSOR_POOL_H_

#include <vector>
#include <map>
#include <mutex>
#include <set>
#include <deque>
#include <cstdio>
#include "data/extern/concurrentqueue/concurrentqueue.h"
#include "ObjectPool.h"
#include "compressor/compressor.h"
//#include "ConstructPool.h"
#include "Text.h"
#include "Key.h"

namespace cclient {
namespace data {

using CompressorObj = cclient::data::compression::Compressor;

class CompressorPool : public ObjectAllocatorPool<CompressorObj*> {
 private:
  std::deque<std::pair<char*, size_t>> buffers256;
  KeyManager keyPool;

  std::map<char*, std::pair<int, size_t>> referenceCount;
  inline void free(std::pair<char*, size_t> vec, moodycamel::ConcurrentQueue<std::pair<char*, size_t>> *ptr);

  inline void reclaim(moodycamel::ConcurrentQueue<std::pair<char*, size_t>> *ptr);

  static ArrayAllocators<CompressorPool>* getDelegator() {
    static ArrayAllocators<CompressorPool> delegator;
    return &delegator;
  }

 public:

  CompressorPool();

  virtual ~CompressorPool();

  virtual void disown(std::pair<char*, size_t>) override;

  virtual std::pair<char*, size_t> allocateBuffer(size_t size) override;

  virtual std::shared_ptr<Text> allocate(size_t size) override;

  virtual void free(std::pair<char*, size_t> &&ptr) override;

  std::shared_ptr<cclient::data::Key> newKey() {
    return keyPool.get(this);
  }

  static CompressorPool* newInstance() {
    return getDelegator()->get();
  }

  static void returnInstance(CompressorPool *pool) {
    getDelegator()->returnItem(pool);
  }

  static CompressorPool* getInstance() {
    // create 10000 before we revert to additional heap
    static CompressorPool pool;
    return &pool;
  }

};

}
}

#endif
