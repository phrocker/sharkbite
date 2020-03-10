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
#ifndef ENDIAN_IN_STREAM
#define ENDIAN_IN_STREAM

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

#ifndef ntohll
  #define ntohll(x) ( ( (uint64_t)(ntohl( (uint32_t)((x << 32) >> 32) )) << 32) | ntohl( ((uint32_t)(x >> 32)) ) )
#endif

/**
 * Purpose: converts all network ordered data into host ordered data
 * Meant to be used between thrift calls
 */
class EndianInputStream : public ByteInputStream {
 public:

  EndianInputStream(InputStream *out_stream)
      :
      ByteInputStream(out_stream) {

  }

  EndianInputStream(char *byteArray, size_t len)
      :
      ByteInputStream(byteArray, len) {

  }

  EndianInputStream(char *byteArray, size_t len, bool allocated)
      :
      ByteInputStream(byteArray, len, allocated) {

  }

  EndianInputStream()
      :
      ByteInputStream() {
  }

  virtual ~EndianInputStream() {

  }

  virtual short readShort() override {
    return ntohs(ByteInputStream::readShort());
  }

  virtual unsigned short readUnsignedShort() override {
    unsigned short shortVal;
    readBytes((char*) &shortVal, 2);
    return ntohs(shortVal);
  }

  virtual int readInt() override {
    return ntohl(ByteInputStream::readInt());
  }

  virtual uint64_t readLong() override {
    auto jd = ByteInputStream::readLong();
    auto ret = ntohll(jd);
    return ret;
  }

};
}
}
}
#endif
