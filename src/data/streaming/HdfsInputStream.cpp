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

#include "data/streaming/input/HdfsInputStream.h"

#include <iostream>

namespace cclient {
namespace data {
namespace streams {

HdfsLink::HdfsLink(std::string nn, int port)
    :
    nn(nn),
    port(port) {
  std::cout << " name node is " << this->nn << " " << port << std::endl;

  reference = hdfsConnect(this->nn.c_str(), port);
}

HdfsLink::~HdfsLink() {
  hdfsDisconnect(reference);
}

hdfsFS HdfsLink::getHdfsreference() {
  return reference;
}

HdfsInputStream::HdfsInputStream(const std::string path)
    :
    InputStream(),
    hdfs(nullptr),
    file(path) {

  HdfsUri uri(path);

  hdfs = std::make_shared<HdfsLink>(file, uri.getPort());

  file = "/accumulo/tables/4/default_tablet/C00000z4.rf";

  fileRef = hdfsOpenFile(hdfs->getHdfsreference(), file.c_str(), O_RDONLY, 0, 0, 0);
  //hdfsFileInfo * hdfsGetPathInfo(hdfsFS fs, const char * path);
  auto ret = hdfsGetPathInfo(hdfs->getHdfsreference(), file.c_str());
  std::cout << file << " " << ret << std::endl;
  size = ret->mSize;
  hdfsFreeFileInfo(ret, 1);
}

HdfsInputStream::HdfsInputStream(const std::shared_ptr<HdfsLink> &hdfs, const std::string &path)
    :
    InputStream(),
    hdfs(hdfs),
    file(file) {
  fileRef = hdfsOpenFile(hdfs->getHdfsreference(), file.c_str(), O_RDONLY, 0, 0, 0);
  //hdfsFileInfo * hdfsGetPathInfo(hdfsFS fs, const char * path);
  auto ret = hdfsGetPathInfo(hdfs->getHdfsreference(), file.c_str());
  size = ret->mSize;
  std::cout << file << " " << ret << std::endl;
  hdfsFreeFileInfo(ret, 1);
}

}
}
}
