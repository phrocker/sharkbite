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
#ifndef HADOOP_DOUT_STREAM
#define HADOOP_DOUT_STREAM

#include <cstdint>

#include "ByteOutputStream.h"
#include "OutputStream.h"
#include "Streams.h"
#include "data/constructs/client/Hdfs.h"
#include "extern/libhdfs3/client/hdfs.h"
#include "utils/Uri.h"

namespace cclient {
namespace data {
namespace streams {

class HadoopDataOutputStream : public DataOutputStream {
 public:
  explicit HadoopDataOutputStream(OutputStream *out_stream)
      : DataOutputStream(out_stream), output_stream_ref(out_stream) {}

  virtual ~HadoopDataOutputStream();
  virtual uint64_t getPos();
  uint64_t write(const char *bytes, long cnt);
  virtual uint64_t writeByte(int byte);
  virtual uint64_t writeString(std::string s);
  virtual uint64_t write(const uint8_t *bytes, long cnt);
  virtual uint64_t writeBytes(const uint8_t *bytes, size_t cnt);
  virtual uint64_t writeByte(const uint8_t byte);
  virtual uint64_t writeShort(const short shortVal);
  virtual uint64_t writeInt(const int intVal);
  virtual uint64_t writeLong(const uint64_t val);
  virtual uint64_t writeBoolean(const bool val);
  virtual uint64_t writeHadoopLong(const int64_t n);

 protected:
  // output stream reference.
  cclient::data::streams::OutputStream *output_stream_ref;
};

class HdfsOutputStream : public OutputStream {
 public:
  explicit HdfsOutputStream(const std::string &path);

  explicit HdfsOutputStream(const std::shared_ptr<hdfs::HdfsLink> &hdfs,
                            const std::string &path);

  ~HdfsOutputStream() {
    hdfsFlush(hdfs->getHdfsReference(), fileRef);
    hdfsCloseFile(hdfs->getHdfsReference(), fileRef);
  }

  /*  int close() {
      hdfsCloseFile(hdfs->getHdfsReference(), fileRef);
      fileRef = nullptr;
      return 0;
    }*/

  /**
   * Copied from
   * http://stackoverflow.com/questions/3022552/is-there-any-standard-htonl-like-function-for-64-bits-integers-in-c
   */

  uint64_t htonlw(uint64_t value);

  virtual uint64_t writeString(std::string s) override;

  virtual uint64_t writeShort(short shortVal) override;

  virtual uint64_t writeInt(int intVal) override;

  virtual uint64_t writeLong(uint64_t val) override;

  virtual uint64_t writeByte(int byte) override;

  virtual uint64_t writeBoolean(const bool val) override;

  virtual uint64_t writeBytes(const char *bytes, size_t cnt) override;

  virtual uint64_t writeBytes(const uint8_t *bytes, size_t cnt) override;

  virtual uint64_t write(const uint8_t *bytes, long cnt) override;

  virtual uint64_t write(const char *bytes, long cnt) override;

  virtual uint64_t writeEncodedLong(const int64_t n) override;

  virtual uint64_t writeHadoopLong(const int64_t n) override;

  virtual uint64_t writeVLong(const int64_t val) override;
  /*
    virtual uint64_t write(const uint8_t *bytes, long cnt) override {
      written += cnt;
      return hdfsWrite(hdfs->getHdfsReference(), fileRef,
                       static_cast<const void *>(bytes), cnt);
    }

    virtual uint64_t getPos() override { return written; }

    uint64_t write(const char *bytes, long cnt) override {
      written += cnt;
      return hdfsWrite(hdfs->getHdfsReference(), fileRef,
                       static_cast<const void *>(bytes), cnt);
    }

    virtual uint64_t writeByte(int byte) override {
      return writeBytes(reinterpret_cast<uint8_t *>(&byte), 1);
    }

    virtual uint64_t writeByte(const uint8_t byte) override {
      return writeBytes(&byte, 1);
    }

    virtual uint64_t writeEncodedLong(const int64_t val) override{
      return OutputStream::writeEncodedLong((int64_t)htonlw(val));
    }

    virtual uint64_t writeHadoopLong(const int64_t val) override{
      return OutputStream::writeHadoopLong((int64_t)htonlw(val));
    }
  */

 protected:
  size_t size;
  hdfsFile fileRef;
  std::string file;

  // output stream reference.
  std::shared_ptr<hdfs::HdfsLink> hdfs;
};

}  // namespace streams
}  // namespace data
}  // namespace cclient
#endif
