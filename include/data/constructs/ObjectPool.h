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

#ifndef OBJECT_ALLOCATOR_POOL_H_
#define OBJECT_ALLOCATOR_POOL_H_

#include <vector>
#include <memory>

namespace cclient {
namespace data {

template<typename T>
class ObjectAllocatorPool {
 private:

 protected:
  virtual ~ObjectAllocatorPool() {
  }
  ObjectAllocatorPool() {
  }
 public:

  virtual std::pair<char*,size_t> allocateBuffer(size_t size) = 0;

  virtual std::shared_ptr<T> allocate(size_t size) = 0;

  virtual void free(std::pair<char*, size_t> &&ptr) = 0;

};

}
}

#endif
