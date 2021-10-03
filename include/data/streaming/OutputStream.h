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
#ifndef OUTPUT_STREAM
#define OUTPUT_STREAM
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wweak-vtables"

#include <arpa/inet.h>
#include <stdint.h>

#include <memory>
#include <ostream>

namespace cclient {
namespace data {
namespace streams {

class OutputStream {
 public:
  explicit OutputStream(std::unique_ptr<std::ostream> ptr, uint64_t pos)
      : ostream_ref(ptr.get()), position(pos), copy(false),owned_ostream_ref(std::move(ptr)) {
  }

  explicit OutputStream(std::ostream *ptr, uint64_t pos);

  OutputStream();

  virtual ~OutputStream();

  virtual void flush();

  virtual uint64_t getPos();

  virtual uint64_t write(const char *bytes, long cnt);

  virtual uint64_t writeByte(int byte);

  virtual uint64_t writeString(std::string s);

  virtual uint64_t write(const uint8_t *bytes, long cnt);

  virtual uint64_t writeBytes(const uint8_t *bytes, size_t cnt);

  virtual uint64_t writeBytes(const char *bytes, size_t cnt);

  virtual uint64_t writeByte(const uint8_t byte);

  virtual uint64_t writeShort(const short shortVal);

  virtual uint64_t writeInt(const int intVal);

  virtual uint64_t writeLong(const uint64_t val);

  virtual uint64_t writeVLong(const int64_t val);

  virtual uint64_t writeBoolean(const bool val);

  virtual uint64_t writeHadoopLong(const int64_t n = 0);

  virtual uint64_t writeEncodedLong(const int64_t n = 0);

  virtual uint32_t bytesWritten();

  friend class HadoopDataOutputStream;
  friend class DataOutputStream;

 protected:
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

  virtual bool canReclaim() { return true; }

  int numberOfLeadingZeros(uint64_t i) {
    // HD, Figure 5-6
    if (i == 0) return 64;
    int n = 1;
    unsigned int x = i >> 32;
    if (x == 0) {
      n += 32;
      x = (int)i;
    }
    if (x >> (unsigned int)16 == 0) {
      n += 16;
      x <<= 16;
    }
    if (x >> (unsigned int)24 == 0) {
      n += 8;
      x <<= 8;
    }
    if (x >> (unsigned int)28 == 0) {
      n += 4;
      x <<= 4;
    }
    if (x >> (unsigned int)30 == 0) {
      n += 2;
      x <<= 2;
    }
    n -= x >> (unsigned int)31;
    return n;
  }

  /**
   * Constructor, which is to be used by DataOutputStream. Note that it is
   * a friend class
   * @param ptr stream pointer
   */
  explicit OutputStream(OutputStream *ptr)
      : ostream_ref(ptr == NULL ? NULL : ptr->ostream_ref),
        position(ptr == NULL ? 0 : ptr->position),
        copy(true) {}

  std::unique_ptr<std::ostream> owned_ostream_ref;

  // ostream reference.
  std::ostream *ostream_ref;
  // position pointer.
  uint64_t position;
  // identify that we have copied a stream
  // useful when deleting position
  bool copy;
};
}  // namespace streams
}  // namespace data
}  // namespace cclient
#endif
