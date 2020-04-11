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

#include "data/constructs/Text.h"
#include <iostream>
namespace cclient {
namespace data {

Text::Text(ObjectAllocatorPool<Text> *pool)
    :
    Text(pool, nullptr, 0, 0) {

}
Text::Text(char *buf, size_t currentSize, size_t originalSize)
    :
    Text(nullptr, buf, currentSize, originalSize) {
}
Text::Text(ObjectAllocatorPool<Text> *ref, char *buf, size_t currentSize, size_t originalSize)
    :
    reference(ref),
    buffer(buf),
    currentSize(currentSize),
    originalSize(originalSize) {
}

Text::~Text() {
  if (buffer) {
    if (reference) {
      reference->free(std::make_pair(buffer, originalSize));
    } else {
      delete[] buffer;
    }
  }
}

void Text::reset(char *buffer, size_t length, size_t size) {
  if (buffer) {
    if (reference) {
      reference->free(std::make_pair(this->buffer, originalSize));
    } else {
      delete[] buffer;
    }
  }
  this->buffer = buffer;
  currentSize = length;
  originalSize = size;
}

void Text::clear() {
  buffer = nullptr;
  currentSize = originalSize = 0;
}

void Text::reset() {
  if (buffer) {
    if (reference) {
      reference->free(std::make_pair(this->buffer, originalSize));
    } else {
      delete[] buffer;
    }
  }
  this->buffer = nullptr;
  currentSize = 0;
}

std::string Text::toString() {
  if (buffer)
    return std::string(buffer, currentSize);
  else
    return "";

}
std::pair<char*, size_t> Text::getBuffer() {
  return std::make_pair(buffer, currentSize);
}

size_t Text::getMaxSize() {
  return originalSize;
}

size_t Text::size() {
  return currentSize;
}

}
}
