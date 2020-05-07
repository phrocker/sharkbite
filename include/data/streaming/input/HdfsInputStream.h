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
#ifndef HADOOP_DIN_STREAM
#define HADOOP_DIN_STREAM

#include <stdexcept>
#include <string>
#include <algorithm>
#include "InputStream.h"
#include "extern/libhdfs3/client/hdfs.h"
#include "utils/Uri.h"
#include "data/constructs/client/Hdfs.h"

namespace cclient {
namespace data {
namespace streams {

/**
 * Hadoop input stream
 */
class HadoopDataInputStream : public InputStream {
 public:

  HadoopDataInputStream(InputStream *out_stream)
      :
      InputStream(out_stream),
      input_stream_ref(out_stream) {

  }

  virtual ~HadoopDataInputStream() {

  }

  virtual InputStream* seek(uint64_t pos) {
    input_stream_ref->seek(pos);
    return this;
  }

  virtual uint64_t readBytes(const uint8_t *bytes, size_t cnt) {
    return input_stream_ref->readBytes((char*) bytes, cnt);
  }

 protected:
  // output stream reference.
  InputStream *input_stream_ref;

};

class HdfsInputStream : public InputStream {

 protected:

 public:

  explicit HdfsInputStream(const std::string path);

  explicit HdfsInputStream(const std::shared_ptr<hdfs::HdfsLink> &hdfs, const std::string &path);

  virtual ~HdfsInputStream() {
    hdfsCloseFile(hdfs->getHdfsReference(), fileRef);
  }

  virtual InputStream* seek(uint64_t pos) {
    hdfsSeek(hdfs->getHdfsReference(), fileRef, pos);
    return this;
  }

  virtual uint64_t readBytes(uint8_t *bytes, size_t cnt) override{
    //tSize hdfsRead(hdfsFS fs, hdfsFile file, void * buffer, tSize length);
    return hdfsRead(hdfs->getHdfsReference(), fileRef, (void*) bytes, cnt);
  }

  virtual uint64_t readBytes(char *bytes, size_t cnt) override{
      //tSize hdfsRead(hdfsFS fs, hdfsFile file, void * buffer, tSize length);
      return hdfsRead(hdfs->getHdfsReference(), fileRef, (void*) bytes, cnt);
    }

  size_t getFileSize() const {
    return size;
  }

 protected:
  size_t size;
  hdfsFile fileRef;
  std::string file;

// output stream reference.
  std::shared_ptr<hdfs::HdfsLink> hdfs;

};

}
}
}
#endif
