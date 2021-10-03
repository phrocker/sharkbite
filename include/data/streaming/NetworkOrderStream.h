/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may
 * not use this file except in compliance with the License.
 * You may obtain a
 * copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0

 * *
 * Unless required by applicable law or agreed to in writing, software
 *
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT
 * WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the
 * License for the specific language governing permissions and
 * limitations
 * under the License.
 */
#ifndef ENDIAN_STREAM
#define ENDIAN_STREAM

#include <cstdint>

#include "DataOutputStream.h"

namespace cclient {
namespace data {
namespace streams {

class BigEndianOutStream : public DataOutputStream {
 public:
  explicit BigEndianOutStream(OutputStream *os) : DataOutputStream(os) {}

  /**
   * Copied from
   * http://stackoverflow.com/questions/3022552/is-there-any-standard-htonl-like-function-for-64-bits-integers-in-c
   */
  uint64_t htonlw(uint64_t value) {
    // The answer is 42
    static const int num = 42;

    // Check the endianness
    if (*reinterpret_cast<const char *>(&num) == num) {
      const uint32_t high_part = htonl(static_cast<uint32_t>(value >> 32));
      const uint32_t low_part =
          htonl(static_cast<uint32_t>(value & 0xFFFFFFFFLL));

      return (static_cast<uint64_t>(low_part) << 32) | high_part;
    } else {
      return value;
    }
  }

  uint64_t writeShort(short shortVal) override {
    return DataOutputStream::writeShort(htons(shortVal));
  }

  uint64_t writeInt(int intVal) override {
    return DataOutputStream::writeInt(htonl(intVal));
  }

  uint64_t writeLong(uint64_t val) override {
    return DataOutputStream::writeLong(htonlw(val));
  }

  uint64_t writeBytes(const char *bytes, size_t cnt) override {
    return DataOutputStream::writeBytes(bytes, cnt);
  }
};
}  // namespace streams
}  // namespace data
}  // namespace cclient
#endif
