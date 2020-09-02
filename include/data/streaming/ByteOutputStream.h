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
#ifndef BYTE_STREAM
#define BYTE_STREAM
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wweak-vtables"

#include <arpa/inet.h>
#include <stdint.h>

#include <iostream>
#include <stdexcept>
#include <typeinfo>
#include <vector>

#include "OutputStream.h"

namespace cclient {
namespace data {
namespace streams {

/**
 * Designed after hadoop output streams
 */
class ByteOutputStream : public OutputStream {
 public:
  ByteOutputStream(size_t initial_size, OutputStream *out_stream = NULL);

  virtual ~ByteOutputStream();
  void flush();
  virtual uint64_t getPos();

  void getByteArray(char *inArray, size_t inArraySize);

  char *getByteArray();

  char *getByteArrayAtPosition();
  size_t getSize();
  void setOutputStreamRef(OutputStream *out_stream);

  virtual uint64_t write(const char *bytes, long cnt);

  virtual uint64_t writeByte(int byte);

  virtual uint64_t writeString(std::string s);

  virtual uint64_t write(const uint8_t *bytes, long cnt);

  virtual uint64_t writeBytes(const char *bytes, size_t cnt) {
    return writeBytes((const uint8_t *)bytes, cnt);
  }

  virtual uint64_t writeBytes(const uint8_t *bytes, size_t cnt);

  virtual uint64_t writeByte(const uint8_t byte);

  virtual uint64_t writeShort(const short shortVal);
  virtual uint64_t writeInt(const int intVal);

  virtual uint64_t writeLong(const uint64_t val);

  virtual uint64_t writeBoolean(const bool val);

  virtual uint64_t writeEncodedLong(const int64_t n = 0) {
    return OutputStream::writeEncodedLong(n);
  }

  void ensure(size_t size, size_t ptr = 0);

 protected:
  // offset of the stream.
  uint32_t offset;
  // size of the steram.
  size_t size;
  // buffered array.
  // char *array;
  std::vector<char> array;
  // output stream reference.
  OutputStream *output_stream_ref;
};

class BigEndianByteStream : public ByteOutputStream {
 public:
  BigEndianByteStream(size_t initial_size, OutputStream *out_stream = NULL)
      : ByteOutputStream(initial_size, out_stream) {}

  uint64_t writeShort(short shortVal) {
    return ByteOutputStream::writeShort(htons(shortVal));
  }

  uint64_t writeInt(int intVal) {
    return ByteOutputStream::writeInt(htonl(intVal));
  }

  uint64_t writeLong(uint64_t val) {
    return ByteOutputStream::writeLong(htonlw(val));
  }

  virtual uint64_t writeVLong(const int64_t n = 0) {
    return ByteOutputStream::writeHadoopLong((int64_t)htonlw(n));
  }

  virtual uint64_t writeEncodedLong(const int64_t n = 0) {
    return ByteOutputStream::writeEncodedLong((int64_t)htonlw(n));
  }

  virtual uint64_t writeBytes(const char *bytes, size_t cnt) {
    return ByteOutputStream::writeBytes(bytes, cnt);
  }

 private:
  /**
   Removed the check for endianness as we know
   that we will be writing to a network ordering
   from a little endian order.
   **/
  inline uint64_t htonlw(uint64_t val) {
    const uint32_t high_part = htonl(static_cast<uint32_t>(val >> 32));
    const uint32_t low_part = htonl(static_cast<uint32_t>(val & 0xFFFFFFFFLL));

    return (static_cast<uint64_t>(low_part) << 32) | high_part;
  }
};
}  // namespace streams
}  // namespace data
}  // namespace cclient

#endif
