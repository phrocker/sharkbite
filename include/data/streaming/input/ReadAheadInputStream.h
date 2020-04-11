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
#ifndef RHNC_ENDIAN_IN_STREAM
#define RHNC_ENDIAN_IN_STREAM

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

/**
 * Purpose: converts all network ordered data into host ordered data
 * Meant to be used between thrift calls
 */
class ReadAheadInputStream : public ByteInputStream {
 public:

  explicit ReadAheadInputStream(InputStream *out_stream, size_t readahead, size_t maxBuffer, size_t lng)
      :
      ByteInputStream(out_stream),
      readAheadSize(readahead),
      readAheadStart(0),
      currentReadAheadSize(0),
      currentBatchOffset(0){
    if (dynamic_cast<std::ifstream*>(out_stream) != nullptr)
      dynamic_cast<std::ifstream*>(out_stream)->rdbuf()->pubsetbuf(0,0);
    readAheadSizeMax=maxBuffer;
    iBytes = new char[readAheadSizeMax];
    allocated = true;
    fillReadAhead(0);
    length = lng;
  }

  virtual ~ReadAheadInputStream() {
  }


  virtual INLINE uint64_t readBytes(uint8_t *bytes, size_t cnt) override{
      auto rem = currentReadAheadSize - currentBatchOffset;
      if (rem >=cnt){
        memcpy_fast(bytes,iBytes+currentBatchOffset,cnt);
      }
      else{
        memcpy_fast(bytes,iBytes+currentBatchOffset,rem);
        input_stream_ref->readBytes(bytes+rem, cnt-rem);
      }
      offset += cnt;
      currentBatchOffset+=cnt;
      return cnt;
    }

    virtual INLINE uint64_t readBytes(char *bytes, size_t cnt) override{
      auto rem = currentReadAheadSize - currentBatchOffset;
      if (rem >=cnt){
        memcpy_fast(bytes,iBytes+currentBatchOffset,cnt);
      }
      else{
        memcpy_fast(bytes,iBytes+currentBatchOffset,rem);
        input_stream_ref->readBytes(bytes+rem, cnt-rem);
      }
      offset += cnt;
      currentBatchOffset+=cnt;
      return cnt;
    }

  virtual InputStream* seek(uint64_t pos) override {
    // readAheadStart to readAheadStart+readAhead
    if (pos < readAheadStart || pos >= readAheadStart + readAheadSize)
    {
      fillReadAhead(pos);

    }
    offset = pos;

    return this;
  }

  virtual INLINE short readShort() override {
    short shortVal;
    char *ptr = (char*) &shortVal;
    if (SH_UNLIKELY((2 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    if (SH_UNLIKELY((2 + offset) > (readAheadStart + currentReadAheadSize)))
      fillReadAhead(offset);
    ptr[0] = iBytes[currentBatchOffset+1];
    ptr[1] = iBytes[currentBatchOffset];
    offset += 2;
    currentBatchOffset+=2;
    return shortVal;
  }

  virtual INLINE unsigned short readUnsignedShort() override {
    unsigned short shortVal;
    char *ptr = (char*) &shortVal;
    if (SH_UNLIKELY((2 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    if (SH_UNLIKELY((2 + offset) > (readAheadStart + currentReadAheadSize)))
      fillReadAhead(offset);
    ptr[0] = iBytes[currentBatchOffset + 1];
    ptr[1] = iBytes[currentBatchOffset];
    offset += 2;
    currentBatchOffset+=2;
    return shortVal;
  }

  virtual INLINE uint8_t readByte() override {
    uint8_t byte;
    if (SH_UNLIKELY((1 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    if (SH_UNLIKELY((1 + offset) > (readAheadStart + currentReadAheadSize)))
      fillReadAhead(offset);
    byte = iBytes[currentBatchOffset];
    offset += 1;
    currentBatchOffset+=1;
    return byte;
  }

  virtual INLINE int8_t readSignedByte() override {
    int8_t byte;
    if (SH_UNLIKELY((1 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    if (SH_UNLIKELY((1 + offset) > (readAheadStart + currentReadAheadSize)))
      fillReadAhead(offset);
    byte = iBytes[currentBatchOffset];
    offset += 1;
    currentBatchOffset+=1;
    return byte;
  }

  virtual INLINE int64_t readSignedByteAsInt() override {
    int8_t byte;
    if (SH_UNLIKELY((1 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    if (SH_UNLIKELY((1 + offset) > (readAheadStart + currentReadAheadSize)))
      fillReadAhead(offset);
    byte = iBytes[currentBatchOffset];
    offset += 1;
    currentBatchOffset+=1;
    return byte;
  }

  virtual INLINE int readInt() override {
    int intVal;
    char *ptr = (char*) &intVal;
    if (SH_UNLIKELY((4 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    if (SH_UNLIKELY((4 + offset) > (readAheadStart + currentReadAheadSize)))
      fillReadAhead(offset);
    ptr[0] = iBytes[currentBatchOffset + 3];
    ptr[1] = iBytes[currentBatchOffset + 2];
    ptr[2] = iBytes[currentBatchOffset + 1];
    ptr[3] = iBytes[currentBatchOffset];
    offset += 4;
    currentBatchOffset+=4;
    return intVal;
  }

  virtual INLINE uint64_t readLong() override {
    uint64_t longVal;
    char *ptr = (char*) &longVal;
    if (SH_UNLIKELY((8 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    if (SH_UNLIKELY((8 + offset) > (readAheadStart + currentReadAheadSize)))
      fillReadAhead(offset);
    ptr[0] = iBytes[currentBatchOffset + 7];
    ptr[1] = iBytes[currentBatchOffset + 6];
    ptr[2] = iBytes[currentBatchOffset + 5];
    ptr[3] = iBytes[currentBatchOffset + 4];
    ptr[4] = iBytes[currentBatchOffset + 3];
    ptr[5] = iBytes[currentBatchOffset + 2];
    ptr[6] = iBytes[currentBatchOffset + 1];
    ptr[7] = iBytes[currentBatchOffset];
    currentBatchOffset+=8;
    offset += 8;
    return longVal;
  }
 private:
  size_t readAheadStart;
  size_t currentReadAheadSize;
  size_t readAheadSize;  // size of readahead
  size_t readAheadSizeMax;  // size of readahead
  size_t currentBatchOffset;
  void fillReadAhead(uint64_t whence = 0,size_t rqsize=0) {
    readAheadStart = whence;
    offset = whence;
    currentBatchOffset=0;
    auto size= rqsize > 0 ? rqsize : readAheadSize;
    if (size > readAheadSizeMax){
      readAheadSizeMax=size;
      delete [] iBytes;
      std::cout << "resizing to " << size << " " << readAheadSizeMax << std::endl;
      iBytes = new char[ readAheadSizeMax];
    }
    input_stream_ref->seek(whence);
    if (whence + size > length) {
      currentReadAheadSize = length - whence;
    } else {
      currentReadAheadSize = size;
    }
    input_stream_ref->readBytes(iBytes, currentReadAheadSize);

  }
};
}
}
}
#endif
