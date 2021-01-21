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

#include "data/constructs/AllocatorPool.h"

#include <iostream>
namespace cclient {

namespace data {

ArrayAllocatorPool::~ArrayAllocatorPool() {
  for (auto buf : buffers256) {
    delete[] buf.first;
  }
  for (auto buf : buffers128) {
    delete[] buf.first;
  }
  for (auto buf : buffers64) {
    delete[] buf.first;
  }
  for (auto buf : buffers32) {
    delete[] buf.first;
  }
}

ArrayAllocatorPool::ArrayAllocatorPool() : keyPool(10000, this) {
  for (int i = 0; i < 100; i++) {
    buffers256.push_front(std::make_pair(new char[256], 256));
    buffers128.push_front(std::make_pair(new char[128], 128));
  }
  for (int i = 0; i < 500; i++) {
    buffers64.push_front(std::make_pair(new char[64], 64));
  }
  for (int i = 0; i < 1000; i++) {
    buffers32.push_front(std::make_pair(new char[32], 32));
  }
}

void ArrayAllocatorPool::free(
    std::pair<char *, size_t> vec,
    moodycamel::ConcurrentQueue<std::pair<char *, size_t>> *ptr) {}
void ArrayAllocatorPool::reclaim(
    moodycamel::ConcurrentQueue<std::pair<char *, size_t>> *ptr) {}

std::pair<char *, size_t> ArrayAllocatorPool::allocateBuffer(size_t size) {
  std::pair<char *, size_t> ptr;
  if (size <= 32) {
    if (!buffers32.empty()) {
      auto ret = buffers32.back();
      buffers32.pop_back();
      return ret;
    } else {
      ptr.first = new char[32];
      ptr.second = 32;
    }
  } else if (size <= 64) {
    if (!buffers64.empty()) {
      auto ret = buffers64.back();
      buffers64.pop_back();
      return ret;
    } else {
      ptr.first = new char[64];
      ptr.second = 64;
    }
  } else if (size <= 128) {
    if (!buffers128.empty()) {
      auto ret = buffers128.back();
      buffers128.pop_back();
      return ret;
    } else {
      ptr.first = new char[128];
      ptr.second = 128;
    }
  } else if (size <= 256) {
    if (!buffers256.empty()) {
      auto ret = buffers256.back();
      buffers256.pop_back();
      return ret;
    } else {
      ptr.first = new char[256];
      ptr.second = 256;
    }
  } else {
    ptr.first = new char[size];
    ptr.second = size;
  }

  return ptr;
}

std::shared_ptr<Text> ArrayAllocatorPool::allocate(size_t size) {
  std::pair<char *, size_t> ptr;
  if (size <= 32) {
    if (!buffers32.empty()) {
      auto ret = buffers32.back();
      buffers32.pop_back();
      return std::make_shared<Text>(this, ret.first, size, ret.second);
    } else {
      ptr.first = new char[32];
      ptr.second = 32;
    }
  } else if (size <= 64) {
    if (!buffers64.empty()) {
      auto ret = buffers64.back();
      buffers64.pop_back();
      return std::make_shared<Text>(this, ret.first, size, ret.second);
    } else {
      ptr.first = new char[64];
      ptr.second = 64;
    }
  } else if (size <= 128) {
    if (!buffers128.empty()) {
      auto ret = buffers128.back();
      buffers128.pop_back();
      return std::make_shared<Text>(this, ret.first, size, ret.second);
    } else {
      ptr.first = new char[128];
      ptr.second = 128;
    }
  } else if (size <= 256) {
    if (!buffers256.empty()) {
      auto ret = buffers256.back();
      buffers256.pop_back();
      return std::make_shared<Text>(this, ret.first, size, ret.second);
    } else {
      ptr.first = new char[256];
      ptr.second = 256;
    }
  } else {
    ptr.first = new char[size];
    ptr.second = size;
  }

  return std::make_shared<Text>(this, ptr.first, size, ptr.second);
}

void ArrayAllocatorPool::disown(std::pair<char *, size_t> ptr) {
  disownedBuffers.insert(ptr.first);
}

void ArrayAllocatorPool::free(std::pair<char *, size_t> &&ptr) {
  if (!ptr.first) {
    return;
  }

  size_t size = ptr.second;
  if (ptr.second > 256 ||
      (size != 32 && size != 64 && size != 128 && size != 256)) {
    delete[] ptr.first;
  } else {
    if (ptr.second <= 32) {
      buffers32.push_back(ptr);
    } else if (ptr.second <= 64) {
      buffers64.push_back(ptr);
    } else if (ptr.second <= 128) {
      buffers128.push_back(ptr);
    } else {
      buffers256.push_back(ptr);
    }
  }
}
}  // namespace data
}  // namespace cclient
