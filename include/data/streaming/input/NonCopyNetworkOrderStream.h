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
#ifndef NC_ENDIAN_IN_STREAM
#define NC_ENDIAN_IN_STREAM

#include <stdexcept>
#include <cstdio>
#include <iostream>
#include <cstring>
#include <netinet/in.h>
#include "InputStream.h"
#include "ByteInputStream.h"

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
class NonCopyEndianInputStream : public ByteInputStream {
 public:

  NonCopyEndianInputStream(InputStream *out_stream)
      :
      ByteInputStream(out_stream) {

  }

  NonCopyEndianInputStream(char *byteArray, size_t len)
      :
      ByteInputStream(byteArray, len) {

  }

  NonCopyEndianInputStream(char *byteArray, size_t len, bool allocated)
      :
      ByteInputStream(byteArray, len, allocated) {

  }

  NonCopyEndianInputStream()
      :
      ByteInputStream() {
  }

  virtual ~NonCopyEndianInputStream() {

  }

  virtual short readShort() override {
    short shortVal;
    char *ptr = (char*)&shortVal;
    if (SH_UNLIKELY( (2 + offset) > length) )
          throw std::runtime_error ("Stream unavailable");
    ptr[0] = iBytes[offset];
    ptr[1] = iBytes[offset + 1];
    offset += 2;
    return ntohs(shortVal);
  }

  virtual unsigned short readUnsignedShort() override {
    unsigned short shortVal;
    char *ptr = (char*)&shortVal;
    if (SH_UNLIKELY( (2 + offset) > length) )
          throw std::runtime_error ("Stream unavailable");
    ptr[0] = iBytes[offset];
    ptr[1] = iBytes[offset + 1];
    offset += 2;
    return ntohs(shortVal);
  }

  virtual uint8_t readByte() {
    uint8_t byte;
    if (SH_UNLIKELY( (1 + offset) > length) )
          throw std::runtime_error ("Stream unavailable");
    byte = iBytes[offset];
    offset += 1;
    return byte;
  }

  virtual int8_t readSignedByte() {
    int8_t byte;
    if (SH_UNLIKELY( (1 + offset) > length) )
          throw std::runtime_error ("Stream unavailable");
    byte = iBytes[offset];
    offset += 1;
    return byte;
  }

  virtual int64_t readSignedByteAsInt() {
    int8_t byte;
    if (SH_UNLIKELY( (1 + offset) > length) )
          throw std::runtime_error ("Stream unavailable");
    byte = iBytes[offset];
    offset += 1;
    return byte;
  }

  virtual int readInt() override {
    int intVal;
    char *ptr = (char*)&intVal;
    if (SH_UNLIKELY( (4 + offset) > length))
          throw std::runtime_error ("Stream unavailable");
    ptr[0] = iBytes[offset];
    ptr[1] = iBytes[offset + 1];
    ptr[2] = iBytes[offset + 2];
    ptr[3] = iBytes[offset + 3];
    offset += 4;
    return ntohl(intVal);
  }

  virtual uint64_t readLong() override {
    uint64_t longVal;
    char *ptr = (char*)&longVal;
    if ( SH_UNLIKELY( (8 + offset) > length))
          throw std::runtime_error ("Stream unavailable");
    ptr[0] = iBytes[offset];
    ptr[1] = iBytes[offset + 1];
    ptr[2] = iBytes[offset + 2];
    ptr[3] = iBytes[offset + 3];
    ptr[4] = iBytes[offset + 4];
    ptr[5] = iBytes[offset + 5];
    ptr[6] = iBytes[offset + 6];
    ptr[7] = iBytes[offset + 7];
    offset += 8;
    return ntohll(longVal);
  }

};
}
}
}
#endif
