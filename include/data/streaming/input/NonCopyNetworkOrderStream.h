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

#include <netinet/in.h>

#include <cstdio>
#include <cstring>
#include <iostream>
#include <stdexcept>

#include "ByteInputStream.h"
#include "InputStream.h"
#include "data/constructs/MovablePointer.h"
#include "data/extern/concurrentqueue/concurrentqueue.h"
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
#define ntohll(x)                                           \
  (((uint64_t)(ntohl((uint32_t)((x << 32) >> 32))) << 32) | \
   ntohl(((uint32_t)(x >> 32))))
#endif

/**
 * Purpose: converts all network ordered data into host ordered data
 * Meant to be used between thrift calls
 */
class NonCopyEndianInputStream : public ByteInputStream {
 private:
  movable_ptr<cclient::data::streams::ByteOutputStream> bosref;
  movable_ptr<
      moodycamel::ConcurrentQueue<cclient::data::streams::ByteOutputStream *>>
      queueref;

 public:
  explicit NonCopyEndianInputStream(InputStream *out_stream)
      : ByteInputStream(out_stream), bosref(nullptr), queueref(nullptr) {}

  explicit NonCopyEndianInputStream(char *byteArray, size_t len)
      : ByteInputStream(byteArray, len), bosref(nullptr), queueref(nullptr) {}

  explicit NonCopyEndianInputStream(char *byteArray, size_t len, bool allocated)
      : ByteInputStream(byteArray, len, allocated),
        bosref(nullptr),
        queueref(nullptr) {}

  explicit NonCopyEndianInputStream(
      cclient::data::streams::ByteOutputStream *ref,
      moodycamel::ConcurrentQueue<cclient::data::streams::ByteOutputStream *>
          *queueref)
      : ByteInputStream(ref->getByteArray(), ref->getSize(), false),
        bosref(ref),
        queueref(queueref) {}

  NonCopyEndianInputStream()
      : ByteInputStream(), bosref(nullptr), queueref(nullptr) {}

  virtual ~NonCopyEndianInputStream() {
    if (bosref.pointer != nullptr && queueref.pointer != nullptr) {
      bosref->flush();
      // deete
      // delete bosref;
      queueref->enqueue(bosref.pointer);
    }
  }

  virtual INLINE short readShort() override {
    short shortVal;
    char *ptr = (char *)&shortVal;
    if (SH_UNLIKELY((2 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    ptr[0] = iBytes[offset + 1];
    ptr[1] = iBytes[offset];
    offset += 2;
    return shortVal;
  }

  virtual INLINE unsigned short readUnsignedShort() override {
    unsigned short shortVal;
    char *ptr = (char *)&shortVal;
    if (SH_UNLIKELY((2 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    ptr[0] = iBytes[offset + 1];
    ptr[1] = iBytes[offset];
    offset += 2;
    return shortVal;
  }

  virtual INLINE uint8_t readByte() override {
    uint8_t byte;
    if (SH_UNLIKELY((1 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    byte = iBytes[offset];
    offset += 1;
    return byte;
  }

  virtual INLINE int8_t readSignedByte() override {
    int8_t byte;
    if (SH_UNLIKELY((1 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    byte = iBytes[offset];
    offset += 1;
    return byte;
  }

  virtual INLINE int64_t readSignedByteAsInt() override {
    int8_t byte;
    if (SH_UNLIKELY((1 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    byte = iBytes[offset];
    offset += 1;
    return byte;
  }

  virtual INLINE int readInt() override {
    int intVal;
    char *ptr = (char *)&intVal;
    if (SH_UNLIKELY((4 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    ptr[0] = iBytes[offset + 3];
    ptr[1] = iBytes[offset + 2];
    ptr[2] = iBytes[offset + 1];
    ptr[3] = iBytes[offset];
    offset += 4;
    return intVal;
  }

  virtual INLINE uint64_t readLong() override {
    uint64_t longVal;
    char *ptr = (char *)&longVal;
    if (SH_UNLIKELY((8 + offset) > length))
      throw std::runtime_error("Stream unavailable");
    ptr[0] = iBytes[offset + 7];
    ptr[1] = iBytes[offset + 6];
    ptr[2] = iBytes[offset + 5];
    ptr[3] = iBytes[offset + 4];
    ptr[4] = iBytes[offset + 3];
    ptr[5] = iBytes[offset + 2];
    ptr[6] = iBytes[offset + 1];
    ptr[7] = iBytes[offset];
    offset += 8;
    return longVal;
  }
};
}  // namespace streams
}  // namespace data
}  // namespace cclient
#endif
