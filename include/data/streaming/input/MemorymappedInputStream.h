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

#ifndef MMAP_IN_STREAM
#define MMAP_IN_STREAM

#include <fcntl.h>
#include <sys/mman.h>
#include <sys/types.h>

#include <cstdio>
#include <cstring>
#include <stdexcept>

#include "InputStream.h"
#include "data/extern/fastmemcpy/FastMemCpy.h"

#if __linux__
#include <linux/version.h>
#if LINUX_VERSION_CODE > KERNEL_VERSION(2, 6, 22)
#define _MAP_POPULATE_AVAILABLE
#endif
#endif

#ifdef _MAP_POPULATE_AVAILABLE
#define MMAP_FLAGS (MAP_PRIVATE | MAP_POPULATE)
#else
#define MMAP_FLAGS MAP_PRIVATE
#endif

namespace cclient {
namespace data {
namespace streams {

class MemoryMappedInputStream : public InputStream {
 protected:
  size_t getFileSize(const std::string &filename) {
    struct stat st;
    stat(filename.c_str(), &st);
    return st.st_size;
  }

 public:
  explicit MemoryMappedInputStream(const std::string &file) : offset(0) {
    filesize = getFileSize(file);
    fd = open(file.c_str(), O_RDONLY, 0);
    ptr = (char *)mmap(NULL, filesize, PROT_READ, MAP_SHARED, fd, 0);
  }

  virtual ~MemoryMappedInputStream() { munmap(ptr, filesize); }

  virtual uint64_t getPos() { return offset; }
  virtual InputStream *seek(uint64_t pos) {
    offset = pos;
    return this;
  }

  virtual inline uint64_t readBytes(uint8_t *bytes, size_t cnt) {
    if ((cnt + offset) > filesize)
      throw std::runtime_error("Stream unavailable");
    memcpy_fast(bytes, (uint8_t *)ptr + offset, cnt);
    offset += cnt;
    return cnt;
  }

  virtual uint64_t readBytes(char *bytes, size_t cnt) {
    if ((cnt + offset) > filesize)
      throw std::runtime_error("Stream unavailable");
    memcpy_fast(bytes, (char *)ptr + offset, cnt);
    offset += cnt;
    return cnt;
  }

  virtual uint64_t bytesAvailable() { return (filesize - offset); }

 protected:
  // output stream reference.
  char *ptr;
  size_t filesize;
  size_t offset;
  int fd;
};
}  // namespace streams
}  // namespace data
}  // namespace cclient
#endif
