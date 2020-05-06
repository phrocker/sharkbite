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
#include <arpa/inet.h>
#include "data/streaming/EndianTranslation.h"

namespace cclient {
namespace data {
namespace streams {
uint64_t EndianTranslationStream::htonlw(uint64_t value) {
  // The answer is 42
  static const int num = 42;

  // Check the endianness
  if (*reinterpret_cast<const char*>(&num) == num) {
    const uint32_t high_part = htonl(static_cast<uint32_t>(value >> 32));
    const uint32_t low_part = htonl(static_cast<uint32_t>(value & 0xFFFFFFFFLL));

    return (static_cast<uint64_t>(low_part) << 32) | high_part;
  } else {
    return value;
  }
}

EndianTranslationStream::~EndianTranslationStream(){
}

uint64_t EndianTranslationStream::writeShort(short shortVal) {

  return HadoopDataOutputStream::writeShort(htons(shortVal));
}

uint64_t EndianTranslationStream::writeInt(int intVal) {

  return HadoopDataOutputStream::writeInt(htonl(intVal));
}

uint64_t EndianTranslationStream::writeLong(uint64_t val) {

  return HadoopDataOutputStream::writeLong(htonlw(val));
}

uint64_t EndianTranslationStream::writeEncodedLong(const int64_t val) {
  return HadoopDataOutputStream::writeEncodedLong((int64_t) htonlw(val));
}

}
}
}
