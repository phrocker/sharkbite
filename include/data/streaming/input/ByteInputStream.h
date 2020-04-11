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

#ifndef BYTE_IN_STREAM
#define BYTE_IN_STREAM

#include <stdexcept>
#include <cstdio>
#include <cstring>

#include "InputStream.h"
#include "data/extern/fastmemcpy/FastMemCpy.h"

namespace cclient {
namespace data {
namespace streams {

class ByteInputStream : public InputStream {
 public:

  ByteInputStream(InputStream *out_stream)
      :
      InputStream(out_stream),
      input_stream_ref(out_stream),
      allocated(false) {
    length = 0;
    offset = 0;
  }

  ByteInputStream(char *byteArray, size_t len)
      :
      allocated(false),
      input_stream_ref(NULL) {
    iBytes = byteArray;
    length = len;
    offset = 0;

  }

  ByteInputStream()
      :
      input_stream_ref(NULL),
      allocated(false) {
  }

  ByteInputStream(char *byteArray, size_t len, bool allocate)
      :
      input_stream_ref(NULL) {
    if (allocate) {
      allocated = true;
      iBytes = new char[len];
      memcpy_fast(iBytes, byteArray, len);
    } else {
      allocated = false;
      iBytes = byteArray;  //new char[ len ];
    }
    //memcpy_fast(iBytes,byteArray,len);
    length = len;
    offset = 0;

  }

  virtual uint64_t getPos() {
    return offset;
  }

  void setArray(char *byteArray, size_t len, bool allocate) {

    if (allocate) {
      allocated = true;
      iBytes = new char[len];
      memcpy_fast(iBytes, byteArray, len);
    } else {
      iBytes = byteArray;  //new char[ len ];
    }
    //memcpy_fast(iBytes,byteArray,len);
    length = len;
    offset = 0;
  }

  virtual ~ByteInputStream() {
    if (allocated)
      delete[] iBytes;
  }

  virtual InputStream* seek(uint64_t pos) {
    if (NULL != input_stream_ref)
      input_stream_ref->seek(pos);
    else {
      offset = pos;
    }
    return this;
  }

  virtual INLINE uint64_t readBytes(uint8_t *bytes, size_t cnt) {
    if (input_stream_ref != NULL) {
      return input_stream_ref->readBytes(bytes, cnt);
      offset += cnt;
    }

    if ((cnt + offset) > length) {
      throw std::runtime_error("Stream unavailable " + std::to_string(cnt) + " " + std::to_string(offset) + " " + std::to_string(length));
    }
    memcpy_fast(bytes, iBytes + offset, cnt);
    offset += cnt;
    return cnt;
  }

  virtual INLINE uint64_t readBytes(char *bytes, size_t cnt) {
    if (input_stream_ref != NULL) {
      return input_stream_ref->readBytes(bytes, cnt);
      offset += cnt;
    }
    if ((cnt + offset) > length)
      throw std::runtime_error("Stream unavailable");
    memcpy_fast(bytes, iBytes + offset, cnt);
    offset += cnt;
    return cnt;
  }

  virtual uint64_t bytesAvailable() {
    return (length - offset);
  }

 protected:
  // output stream reference.
  InputStream *input_stream_ref;
  bool allocated;
  uint64_t length;
  uint32_t offset;
  char *iBytes;

};
}
}
}
#endif
