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
#include <mutex>
#include <set>
#include <deque>
#include <cstdio>
#include "data/extern/concurrentqueue/concurrentqueue.h"
#include "ObjectPool.h"
//#include "ConstructPool.h"
#include "Text.h"
#include "Key.h"

namespace cclient {
namespace data {

template<typename T>
class ArrayAllocators {

  std::mutex guard;

  std::deque<T*> pool;

 public:
  ArrayAllocators() {

  }

  ~ArrayAllocators() {

  }

  T* get() {
    std::lock_guard<std::mutex> lock(guard);
    if (pool.empty()) {
      return new T();
    }
    auto ret = pool.back();
    pool.pop_back();
    return ret;
  }

  void returnItem(T *item) {
    std::lock_guard<std::mutex> lock(guard);
    pool.push_back(item);
  }

};

class KeyManager {
 private:
  std::vector<cclient::data::Key*> freeObjPool;
  size_t maxsize;
 public:

  explicit KeyManager(size_t maxsize,ObjectAllocatorPool<Text> *txt)
      :
      maxsize(maxsize) {
      for(size_t i=0; i < maxsize; i++){
        freeObjPool.emplace_back(new cclient::data::Key(txt) );
      }

  }

  ~KeyManager() {
    for (auto o : freeObjPool)
      delete o;
  }

  inline void add(cclient::data::Key *o) {
    if (SH_LIKELY(o && freeObjPool.size() < maxsize)) {
      freeObjPool.push_back(o);
    } else {
      delete o;
    }
  }

  std::shared_ptr<cclient::data::Key> get(ObjectAllocatorPool<Text> *txt) {
    cclient::data::Key *o;
    if (freeObjPool.empty())
      return std::make_shared<cclient::data::Key>(txt);
    else {
      o = freeObjPool.back();
      freeObjPool.pop_back();
    }
    return std::shared_ptr<cclient::data::Key>(o, std::bind(&KeyManager::add, this, std::placeholders::_1));
  }
};

class ArrayAllocatorPool : public ObjectAllocatorPool<Text> {
 private:
  std::deque<std::pair<char*, size_t>> buffers256;
  std::deque<std::pair<char*, size_t>> buffers128;
  std::deque<std::pair<char*, size_t>> buffers64;
  std::deque<std::pair<char*, size_t>> buffers32;
  std::set<char*> disownedBuffers;
  KeyManager keyPool;

  std::map<char*, std::pair<int, size_t>> referenceCount;
  inline void free(std::pair<char*, size_t> vec, moodycamel::ConcurrentQueue<std::pair<char*, size_t>> *ptr);

  inline void reclaim(moodycamel::ConcurrentQueue<std::pair<char*, size_t>> *ptr);

  static ArrayAllocators<ArrayAllocatorPool>* getDelegator() {
    static ArrayAllocators<ArrayAllocatorPool> delegator;
    return &delegator;
  }

 public:

  ArrayAllocatorPool();

  virtual ~ArrayAllocatorPool();

  virtual void disown(std::pair<char*, size_t>) override;

  virtual std::pair<char*, size_t> allocateBuffer(size_t size) override;

  virtual std::shared_ptr<Text> allocate(size_t size) override;

  virtual void free(std::pair<char*, size_t> &&ptr) override;

  std::shared_ptr<cclient::data::Key> newKey() {
    return keyPool.get(this);
  }

  static ArrayAllocatorPool* newInstance() {
    return getDelegator()->get();
  }

  static void returnInstance(ArrayAllocatorPool *pool) {
    getDelegator()->returnItem(pool);
  }

  static ArrayAllocatorPool* getInstance() {
    // create 10000 before we revert to additional heap
    static ArrayAllocatorPool pool;
    return &pool;
  }

};

}
}

#endif
