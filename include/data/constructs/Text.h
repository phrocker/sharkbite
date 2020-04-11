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

#ifndef TEXT_H_
#define TEXT_H_

#include "ObjectPool.h"
#include <cstdio>

namespace cclient{
namespace data{

class Text{
 public:

  explicit Text(ObjectAllocatorPool<Text> *);
  explicit Text(char *, size_t, size_t);
  explicit Text(ObjectAllocatorPool<Text> *, char *, size_t, size_t);

  virtual ~Text();

  std::pair<char*, size_t> getBuffer();

  void reset(char *buffer, size_t length, size_t size);

  void reset();

  void clear();

  std::string toString();

  size_t getMaxSize();

  size_t size();

  bool empty(){
    return currentSize == 0;
  }

 private:


  ObjectAllocatorPool<Text> *reference;

  char *buffer;
  size_t currentSize;
  size_t originalSize;

};

}
}

#endif
