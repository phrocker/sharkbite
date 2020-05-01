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

class HdfsLink {
 public:
  explicit HdfsLink(std::string nn, int port);

  ~HdfsLink();

  hdfsFS getHdfsreference();

 private:
  std::string nn;
  int port;
  hdfsFS reference;
};

class HdfsUri {

 private:

  std::string protocol_;
  std::string host_;
  std::string path_;
  int port;
  void parse(const std::string &url_s) {
    const std::string prot_end("://");
    std::string::const_iterator prot_i = search(url_s.begin(), url_s.end(), prot_end.begin(), prot_end.end());
    protocol_.reserve(std::distance(url_s.begin(), prot_i));
    std::transform(url_s.begin(), prot_i, back_inserter(protocol_), std::ptr_fun<int, int>(tolower));  // protocol is icase
    if (prot_i == url_s.end())
      return;
    std::advance(prot_i, prot_end.length());
    std::string::const_iterator path_i = find(prot_i, url_s.end(), '/');
    host_.reserve(distance(prot_i, path_i));
    std::transform(prot_i, path_i, back_inserter(host_), std::ptr_fun<int, int>(tolower));  // host is icase

    size_t colon_pos;

    std::string portstr = (colon_pos = host_.find(":")) != std::string::npos ? host_.substr(colon_pos + 1) : "";
    std::cout << portstr << std::endl;
    host_ = host_.substr(0,host_.find(":"));
    if (!portstr.empty()) {
      port = atoi(portstr.c_str());
      std::cout << "port is " << port << std::endl;
    } else {
      port = 8080;
    }

  }

 public:
  explicit HdfsUri(const std::string file) {
    parse(file);
  }

  std::string getProtocol() const {
    return protocol_;
  }

  std::string getHost() const {
    return host_;
  }

  std::string getPath() const {
    return path_;
  }

  int getPort() const {
    return port;
  }

};

class HdfsInputStream : public InputStream {

 protected:

 public:

  explicit HdfsInputStream(const std::string path);

  explicit HdfsInputStream(const std::shared_ptr<HdfsLink> &hdfs, const std::string &path);

  virtual ~HdfsInputStream() {
    hdfsCloseFile(hdfs->getHdfsreference(), fileRef);
  }

  virtual InputStream* seek(uint64_t pos) {
    hdfsSeek(hdfs->getHdfsreference(), fileRef, pos);
    return this;
  }

  virtual uint64_t readBytes(uint8_t *bytes, size_t cnt) override{
    //tSize hdfsRead(hdfsFS fs, hdfsFile file, void * buffer, tSize length);
    return hdfsRead(hdfs->getHdfsreference(), fileRef, (void*) bytes, cnt);
  }

  virtual uint64_t readBytes(char *bytes, size_t cnt) override{
      //tSize hdfsRead(hdfsFS fs, hdfsFile file, void * buffer, tSize length);
      return hdfsRead(hdfs->getHdfsreference(), fileRef, (void*) bytes, cnt);
    }

  size_t getFileSize() const {
    return size;
  }

 protected:
  size_t size;
  hdfsFile fileRef;
  std::string file;

// output stream reference.
  std::shared_ptr<HdfsLink> hdfs;

};

}
}
}
#endif
