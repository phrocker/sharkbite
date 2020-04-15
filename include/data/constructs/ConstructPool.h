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

#ifndef CONSTRUCT_ALLOCATOR_POOL_H_
#define CONSTRUCT_ALLOCATOR_POOL_H_

#include <vector>
#include <map>
#include <memory>
#include <stack>
#include <mutex>
#include <set>
#include <deque>
#include <cstdio>
#include "data/extern/concurrentqueue/concurrentqueue.h"
#include "ObjectPool.h"
#include "Text.h"

namespace cclient {
namespace data {

template <class T>
class SharedPool
{
 private:
  struct External_Deleter {
    explicit External_Deleter(std::weak_ptr<SharedPool<T>* > pool)
        : pool_(pool) {}

    void operator()(T* ptr) {
      if (auto pool_ptr = pool_.lock()) {
        try {
          (*pool_ptr.get())->add(std::shared_ptr<T>{ptr});
          return;
        } catch(...) {}
      }
      std::default_delete<T>{}(ptr);
    }
   private:
    std::weak_ptr<SharedPool<T>* > pool_;
  };

 public:
  using ptr_type = std::shared_ptr<T, External_Deleter >;

  SharedPool() : this_ptr_(new SharedPool<T>*(this)) {}
  virtual ~SharedPool(){}

  void add(std::shared_ptr<T> t) {
    pool_.push(std::move(t));
  }

  ptr_type acquire(ObjectAllocatorPool<Text> *x) {
    assert(!pool_.empty());
    ptr_type tmp(pool_.top().release(),
                 External_Deleter{std::weak_ptr<SharedPool<T>*>{this_ptr_}});
    tmp->setObjectPool(x);
    pool_.pop();
    return tmp;
  }

  bool empty() const {
    return pool_.empty();
  }

  size_t size() const {
    return pool_.size();
  }

 private:
  std::shared_ptr<SharedPool<T>* > this_ptr_;
  std::stack<std::shared_ptr<T> > pool_;
};

}
}

#endif
