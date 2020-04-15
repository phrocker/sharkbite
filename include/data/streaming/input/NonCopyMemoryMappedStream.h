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
#ifndef MM_NC_ENDIAN_IN_STREAM
#define MM_NC_ENDIAN_IN_STREAM

#include <stdexcept>
#include <cstdio>
#include <iostream>
#include <cstring>
#include <netinet/in.h>
#include "InputStream.h"
#include "ByteInputStream.h"
#include "MemorymappedInputStream.h"
namespace cclient {
namespace data {
namespace streams {

#ifndef SH_UNLIKELY
#ifdef __GNUC__
#define SH_UNLIKELY(val) (__builtin_expect((val), 0))
#define SH_LIKELY(val) (__builtin_expect((val), 1))
#else
    #define SH_UNLIKELY(val) (val)
    #define SH_LIKELY(val) (val)
    #endif
#endif

#ifndef ntohll
#define ntohll(x) ( ( (uint64_t)(ntohl( (uint32_t)((x << 32) >> 32) )) << 32) | ntohl( ((uint32_t)(x >> 32)) ) )
#endif

/**
 * Purpose: converts all network ordered data into host ordered data
 * Meant to be used between thrift calls
 */
class NonCopyMemoryMappedInputStream : public MemoryMappedInputStream {
 public:

  explicit NonCopyMemoryMappedInputStream(const std::string &file)
      :
      MemoryMappedInputStream(file) {
  }

  virtual ~NonCopyMemoryMappedInputStream() {

  }

  virtual short readShort() override {
    short shortVal;
    char *ptr = (char*) &shortVal;
    if (SH_UNLIKELY((2 + offset) > filesize))
      throw std::runtime_error("Stream unavailable");
    std::cout << "offset is " << offset << std::endl;
    ptr[0] = ptr[offset + 1];
    ptr[1] = ptr[offset];
    offset += 2;
    return shortVal;
  }

  virtual unsigned short readUnsignedShort() override {
    unsigned short shortVal;
    char *ptr = (char*) &shortVal;
    if (SH_UNLIKELY((2 + offset) > filesize))
      throw std::runtime_error("Stream unavailable");
    ptr[0] = ptr[offset + 1];
    ptr[1] = ptr[offset];
    offset += 2;
    return shortVal;
  }

  virtual uint8_t readByte() override {
    uint8_t byte;
    if (SH_UNLIKELY((1 + offset) > filesize))
      throw std::runtime_error("Stream unavailable");
    byte = ptr[offset];
    offset += 1;
    return byte;
  }

  virtual int8_t readSignedByte() override {
    int8_t byte;
    if (SH_UNLIKELY((1 + offset) > filesize))
      throw std::runtime_error("Stream unavailable");
    byte = ptr[offset];
    offset += 1;
    return byte;
  }

  virtual int64_t readSignedByteAsInt() override {
    int8_t byte;
    if (SH_UNLIKELY((1 + offset) > filesize))
      throw std::runtime_error("Stream unavailable");
    byte = ptr[offset];
    offset += 1;
    return byte;
  }

  virtual int readInt() override {
    int intVal;
    char *ptr = (char*) &intVal;
    if (SH_UNLIKELY((4 + offset) > filesize))
      throw std::runtime_error("Stream unavailable");
    ptr[0] = ptr[offset + 3];
    ptr[1] = ptr[offset + 2];
    ptr[2] = ptr[offset + 1];
    ptr[3] = ptr[offset];
    offset += 4;
    return intVal;
  }

  virtual uint64_t readLong() override {
    uint64_t longVal;
    char *ptr = (char*) &longVal;
    if (SH_UNLIKELY((8 + offset) > filesize))
      throw std::runtime_error("Stream unavailable");
    ptr[0] = ptr[offset + 7];
    ptr[1] = ptr[offset + 6];
    ptr[2] = ptr[offset + 5];
    ptr[3] = ptr[offset + 4];
    ptr[4] = ptr[offset + 3];
    ptr[5] = ptr[offset + 2];
    ptr[6] = ptr[offset + 1];
    ptr[7] = ptr[offset];
    offset += 8;
    return longVal;
  }

};
}
}
}
#endif
