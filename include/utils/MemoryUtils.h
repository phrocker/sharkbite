/**
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#pragma once

#include "data/extern/fastmemcpy/FastMemCpy.h"
#include "data/extern/knnspeed/avxmem.h"

namespace cclient {
namespace MemoryUtils {

//void * AVX_memcpy(void *dest, void *src, size_t numbytes);
//int AVX_memcmp(const void *str1, const void *str2, size_t numbytes, int equality);
INLINE int compare(const void *str1, const void *str2, size_t numbytes, int equality = 0) {
  return AVX_memcmp(str1, str2, numbytes, equality);
}

INLINE int compare(const void *str1, size_t str1_numbytes, const void *str2, size_t str2_numbytes, int equality = 0) {

  if (str1_numbytes < str2_numbytes) {
    return -1;
  } else if (str1_numbytes > str2_numbytes) {
    return 1;
  }
  return AVX_memcmp(str1, str2, str1_numbytes, equality);
}

//static void* memcpy_fast(void *destination, const void *source, size_t size)

}
}
