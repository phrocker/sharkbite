/********************************************************************
 * 2014 -
 * open source under Apache License Version 2.0
 ********************************************************************/
/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#include "HWCrc32c.h"

#include <cassert>
#include <cstdlib>

#if defined(NATIVE_ARCH) &&                                          \
    ((defined(__X86__) || defined(__i386__) || defined(i386) ||      \
      defined(_M_IX86) || defined(__386__) || defined(__x86_64__) || \
      defined(_M_X64)))
#include <cpuid.h>
#endif

#ifndef NATIVE_ARCH
#include <zlib.h>
#endif

namespace Hdfs {
namespace Internal {

#if ((defined(__X86__) || defined(__i386__) || defined(i386) ||      \
      defined(_M_IX86) || defined(__386__) || defined(__x86_64__) || \
      defined(_M_X64)))
#if !defined(__SSE4_2__)

#if defined(NATIVE_ARCH) && defined(__LP64__)
static inline uint64_t _mm_crc32_u64(uint64_t crc, uint64_t value) {
  asm("crc32q %[value], %[crc]\n" : [ crc ] "+r"(crc) : [ value ] "rm"(value));
  return crc;
}
#endif

#if defined(NATIVE_ARCH)
static inline uint32_t _mm_crc32_u16(uint32_t crc, uint16_t value) {
  asm("crc32w %[value], %[crc]\n" : [ crc ] "+r"(crc) : [ value ] "rm"(value));
  return crc;
}

static inline uint32_t _mm_crc32_u32(uint32_t crc, uint64_t value) {
  asm("crc32l %[value], %[crc]\n" : [ crc ] "+r"(crc) : [ value ] "rm"(value));
  return crc;
}

static inline uint32_t _mm_crc32_u8(uint32_t crc, uint8_t value) {
  asm("crc32b %[value], %[crc]\n" : [ crc ] "+r"(crc) : [ value ] "rm"(value));
  return crc;
}
}
}
#endif

#else

#if defined(NATIVE_ARCH)
#include <nmmintrin.h>
#endif
#endif

bool HWCrc32c::available() {
#if ((defined(__X86__) || defined(__i386__) || defined(i386) ||      \
      defined(_M_IX86) || defined(__386__) || defined(__x86_64__) || \
      defined(_M_X64)))
#if defined(NATIVE_ARCH)
  uint32_t eax, ebx, ecx = 0, edx;
  /*
   * get the CPU features (level 1). ecx will have the SSE4.2 bit.
   * This gcc routine automatically handles saving ebx in the case where we are
   * -fpic or -fPIC
   */
  __get_cpuid(1, &eax, &ebx, &ecx, &edx);
  return (ecx & (1 << 20)) != 0;
#else
  return false;
#endif
#else
  return false;
#endif
}

void HWCrc32c::update(const void *b, int len) {
#if defined(NATIVE_ARCH)
  const char *p = static_cast<const char *>(b);
#if defined(__LP64__)
  const size_t bytes = sizeof(uint64_t);
#else
  const size_t bytes = sizeof(uint32_t);
#endif
  int align = bytes - reinterpret_cast<uint64_t>(p) % bytes;
  align = bytes == static_cast<size_t>(align) ? 0 : align;

  if (len < align) {
    align = len;
  }

  updateInt64(p, align);
  p = p + align;
  len -= align;

  if (len > 0) {
    assert(0 == reinterpret_cast<uint64_t>(p) % bytes);

    for (int i = len / bytes; i > 0; --i) {
#if defined(__LP64__)
      crc = _mm_crc32_u64(crc, *reinterpret_cast<const uint64_t *>(p));
#else
      crc = _mm_crc32_u32(crc, *reinterpret_cast<const uint32_t *>(p));
#endif
      p = p + bytes;
    }

    len &= bytes - 1;
    updateInt64(p, len);
  }
#endif
}

void HWCrc32c::updateInt64(const char *b, int len) {
  assert(len < 8);
#if defined(NATIVE_ARCH)
  switch (len) {
    case 7:
      crc = _mm_crc32_u8(crc, *reinterpret_cast<const uint8_t *>(b++));

    case 6:
      crc = _mm_crc32_u16(crc, *reinterpret_cast<const uint16_t *>(b));
      b += 2;

      /* case 5 is below: 4 + 1 */
    case 4:
      crc = _mm_crc32_u32(crc, *reinterpret_cast<const uint32_t *>(b));
      break;

    case 3:
      crc = _mm_crc32_u8(crc, *reinterpret_cast<const uint8_t *>(b++));

    case 2:
      crc = _mm_crc32_u16(crc, *reinterpret_cast<const uint16_t *>(b));
      break;

    case 5:
      crc = _mm_crc32_u32(crc, *reinterpret_cast<const uint32_t *>(b));
      b += 4;

    case 1:
      crc = _mm_crc32_u8(crc, *reinterpret_cast<const uint8_t *>(b));
      break;

    case 0:
      break;
  }
#endif
}
}
}

#endif /* _HDFS_LIBHDFS3_COMMON_HWCHECKSUM_H_ */
