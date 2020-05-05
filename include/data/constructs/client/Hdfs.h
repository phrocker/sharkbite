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
#ifndef HADOOP_HDFS
#define HADOOP_HDFS

#include <stdexcept>
#include <string>
#include <vector>
#include <algorithm>
#include "InputStream.h"
#include "extern/libhdfs3/client/hdfs.h"
#include "utils/Uri.h"

namespace cclient {
namespace data {
namespace hdfs {

class HdfsDirEnt {
 private:
  bool isDir;
  std::string name;
  size_t size;
  uint8_t repFactor;
  std::string owner;
  std::string group;
  uint16_t permissions;
  uint64_t lastAccess;

 public:

  explicit HdfsDirEnt(const hdfsFileInfo*);

  bool isDirectory() const {
    return isDir;
  }

  std::string getName() const {
    return name;
  }

  size_t getSize() const {
    return size;
  }

  uint8_t getreplication() const {
    return repFactor;
  }

  std::string getOwner() const {
    return owner;
  }

  std::string getGroup() const {
    return group;
  }

  uint16_t getPermissions() const {
    return permissions;
  }

};

class HdfsLink {
 public:
  explicit HdfsLink(std::string nn, int port);

  ~HdfsLink();

  int mkdir(const std::string &dir);

  std::vector<HdfsDirEnt> list(const std::string &dir);

  hdfsFS getHdfsreference();

 private:
  std::string nn;
  int port;
  hdfsFS reference;
};

}
}
}
#endif
