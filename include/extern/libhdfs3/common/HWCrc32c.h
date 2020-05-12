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
#ifndef _HDFS_LIBHDFS3_COMMON_HWCHECKSUM_H_
#define _HDFS_LIBHDFS3_COMMON_HWCHECKSUM_H_

#include "Checksum.h"

#ifndef NATIVE_ARCH
#include <zlib.h>
#endif

namespace Hdfs {
namespace Internal {

/**
 * Calculate CRC with hardware support.
 */
class HWCrc32c : public Checksum {
 public:
  /**
   * Constructor.
   */
  HWCrc32c() {
#if defined(NATIVE_ARCH)
        crc = 0xFFFFFFFF;
#else
    crc = crc32(0L, Z_NULL, 0);
#endif
  }

  uint32_t getValue() {
#if defined(NATIVE_ARCH)
    return ~crc;
#else
    return crc;
#endif
  }

  /**
   * @ref Checksum#reset()
   */
  void reset() {
#if defined(NATIVE_ARCH)
        crc = 0xFFFFFFFF;
#else
    crc = crc32(0L, Z_NULL, 0);
#endif
  }

  /**
   * @ref Checksum#update(const void *, int)
   */
  void update(const void *b, int len);

  /**
   * Destory an HWCrc32 instance.
   */
  ~HWCrc32c() {
  }

  /**
   * To test if the hardware support this function.
   * @return true if the hardware support to calculate the CRC.
   */
  static bool available();

 private:
  void updateInt64(const char *b, int len);

 private:

  uint32_t crc;
};

}
}

#endif /* _HDFS_LIBHDFS3_COMMON_HWCHECKSUM_H_ */
